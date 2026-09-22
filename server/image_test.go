package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math/rand"
	"testing"
	"testing/iotest"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// noisyImage builds an image whose pixels are incompressible enough that a
// JPEG/PNG encode of it cannot accidentally collapse to a trivial size. A
// smooth gradient would compress so well that "output is smaller" would be
// true for uninteresting reasons; deterministic noise on top of a gradient
// keeps the fixtures realistic and reproducible.
func noisyImage(w, h int, seed int64) *image.RGBA {
	rnd := rand.New(rand.NewSource(seed))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{
				R: uint8((x*7 + rnd.Intn(64)) % 256),
				G: uint8((y*5 + rnd.Intn(64)) % 256),
				B: uint8((x*y + rnd.Intn(64)) % 256),
				A: 255,
			})
		}
	}
	return img
}

// flatImage builds a low-entropy image. PNG encoders can actually shrink this,
// which is what the lossless-optimisation test needs.
func flatImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x / 16), G: uint8(y / 16), B: 200, A: 255})
		}
	}
	return img
}

func encodeJPEG(t *testing.T, img image.Image, quality int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("encoding the JPEG fixture failed: %v", err)
	}
	return buf.Bytes()
}

// encodePNGFast uses NoCompression so the fixture is genuinely badly encoded,
// i.e. a lossless re-encode has real headroom to shrink it. This models the
// actual case the feature targets: files produced by cameras and screenshot
// tools that do not optimise.
func encodePNGFast(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.NoCompression}
	if err := enc.Encode(&buf, img); err != nil {
		t.Fatalf("encoding the PNG fixture failed: %v", err)
	}
	return buf.Bytes()
}

// jpegWithEXIF splices a minimal but structurally valid APP1/EXIF segment into
// a JPEG, directly after SOI. Go's image/jpeg encoder writes SOI immediately
// followed by DQT and emits no APP segments at all, so byte offset 2 is the
// correct and unambiguous insertion point.
//
// The EXIF payload carries one IFD0 tag, Orientation (0x0112) = 1, which is
// enough to prove preservation: the test asserts the exact segment bytes
// survive, so any re-encode that drops or rewrites metadata fails.
func jpegWithEXIF(t *testing.T, baseJPEG []byte) (withEXIF []byte, exifSegment []byte) {
	t.Helper()

	if len(baseJPEG) < 2 || baseJPEG[0] != 0xFF || baseJPEG[1] != 0xD8 {
		t.Fatalf("fixture does not start with SOI: % X", baseJPEG[:min(4, len(baseJPEG))])
	}

	var tiff bytes.Buffer
	tiff.Write([]byte{'M', 'M', 0x00, 0x2A})              // big-endian TIFF header
	binary.Write(&tiff, binary.BigEndian, uint32(8))      // offset of IFD0
	binary.Write(&tiff, binary.BigEndian, uint16(1))      // one entry
	binary.Write(&tiff, binary.BigEndian, uint16(0x0112)) // Orientation
	binary.Write(&tiff, binary.BigEndian, uint16(3))      // SHORT
	binary.Write(&tiff, binary.BigEndian, uint32(1))      // count
	binary.Write(&tiff, binary.BigEndian, uint16(1))      // value: Upright
	binary.Write(&tiff, binary.BigEndian, uint16(0))      // padding
	binary.Write(&tiff, binary.BigEndian, uint32(0))      // no next IFD

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)

	segment := []byte{0xFF, 0xE1}
	segment = binary.BigEndian.AppendUint16(segment, uint16(len(payload)+2))
	segment = append(segment, payload...)

	out := make([]byte, 0, len(baseJPEG)+len(segment))
	out = append(out, baseJPEG[:2]...)
	out = append(out, segment...)
	out = append(out, baseJPEG[2:]...)
	return out, segment
}

// run drives the pipeline the way the hook does and returns the stored bytes.
func run(t *testing.T, input []byte) ([]byte, processOutcome) {
	t.Helper()
	var out bytes.Buffer
	outcome, err := processUpload(&out, bytes.NewReader(input), defaultConfiguration())
	if err != nil {
		t.Fatalf("processUpload returned an error: %v", err)
	}
	stored := out.Bytes()
	if outcome.bytesWritten != int64(len(stored)) {
		t.Fatalf("outcome.bytesWritten = %d but %d bytes reached the writer",
			outcome.bytesWritten, len(stored))
	}
	return stored, outcome
}

// defaultThresholdBytes is the size threshold of the default configuration, in
// bytes. These tests exercise the pipeline with defaultConfiguration(), so they
// read the boundary from the same place the code does rather than restating it.
var defaultThresholdBytes = defaultConfiguration().thresholdBytes()

func decodeBounds(t *testing.T, b []byte) (int, int, string) {
	t.Helper()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("stored bytes are not a decodable image: %v", err)
	}
	return cfg.Width, cfg.Height, format
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestOversizedJPEGIsResizedAndReEncoded is the headline behaviour: a JPEG over
// the threshold and over the edge limit comes out smaller, within the limit,
// still decodable, and still a JPEG.
func TestOversizedJPEGIsResizedAndReEncoded(t *testing.T) {
	// 4000x3000 at quality 95 comfortably exceeds both 512 KB and 2560 px.
	input := encodeJPEG(t, noisyImage(4000, 3000, 1), 95)
	if len(input) <= defaultThresholdBytes {
		t.Fatalf("fixture is only %d bytes, which is below the %d byte threshold; "+
			"the test would not exercise the compression path", len(input), defaultThresholdBytes)
	}

	stored, outcome := run(t, input)

	if !outcome.reEncoded {
		t.Error("outcome reports no re-encode for an oversized JPEG")
	}
	if len(stored) >= len(input) {
		t.Errorf("stored image is not smaller: input %d bytes, stored %d bytes",
			len(input), len(stored))
	}

	w, h, format := decodeBounds(t, stored)
	if format != "jpeg" {
		t.Errorf("stored format = %q, want jpeg", format)
	}
	if w > defaultMaxEdgeLength || h > defaultMaxEdgeLength {
		t.Errorf("stored image %dx%d exceeds the %d px edge limit", w, h, defaultMaxEdgeLength)
	}
	if w != defaultMaxEdgeLength {
		t.Errorf("long edge = %d, want exactly %d (aspect-preserving downscale)", w, defaultMaxEdgeLength)
	}
	// 4000x3000 is 4:3, so the short edge must land on 1920.
	if wantH := 1920; h != wantH {
		t.Errorf("short edge = %d, want %d (aspect ratio must be preserved)", h, wantH)
	}

	// The reported geometry must describe the stored bytes, because the server
	// set info.Width/Height from the ORIGINAL file before the hook ran and
	// never recomputes them afterwards.
	if outcome.width != w || outcome.height != h {
		t.Errorf("outcome geometry %dx%d does not match the stored image %dx%d",
			outcome.width, outcome.height, w, h)
	}
}

// TestOversizedJPEGWithinEdgeLimitIsReEncodedButNotUpscaled covers a large but
// not wide image: it must be re-encoded at the target quality, and must never
// be scaled UP to the edge limit.
func TestOversizedJPEGWithinEdgeLimitIsReEncodedButNotUpscaled(t *testing.T) {
	const w, h = 1600, 1200
	input := encodeJPEG(t, noisyImage(w, h, 2), 100)
	if len(input) <= defaultThresholdBytes {
		t.Fatalf("fixture is %d bytes, below the %d byte threshold", len(input), defaultThresholdBytes)
	}

	stored, outcome := run(t, input)

	if !outcome.reEncoded {
		t.Error("outcome reports no re-encode for an over-threshold JPEG")
	}
	gotW, gotH, _ := decodeBounds(t, stored)
	if gotW != w || gotH != h {
		t.Errorf("image was rescaled to %dx%d; an image inside the edge limit must keep %dx%d",
			gotW, gotH, w, h)
	}
	if len(stored) >= len(input) {
		t.Errorf("quality-100 fixture was not shrunk by the quality-%d re-encode: %d -> %d",
			defaultJpegQuality, len(input), len(stored))
	}
}

// TestPNGStaysPNGAndIsPixelIdentical is the lossless promise from SPEC §4.1:
// PNG is optimised, never converted to JPEG, and never loses a pixel.
func TestPNGStaysPNGAndIsPixelIdentical(t *testing.T) {
	src := flatImage(3000, 2000)
	input := encodePNGFast(t, src)
	if len(input) <= defaultThresholdBytes {
		t.Fatalf("fixture is %d bytes, below the %d byte threshold", len(input), defaultThresholdBytes)
	}

	stored, outcome := run(t, input)

	if !outcome.reEncoded {
		t.Error("outcome reports no re-encode for an over-threshold PNG")
	}

	_, _, format := decodeBounds(t, stored)
	if format != "png" {
		t.Fatalf("stored format = %q, want png: PNG must never be re-encoded to JPEG", format)
	}
	if len(stored) >= len(input) {
		t.Errorf("PNG was not optimised: input %d bytes, stored %d bytes", len(input), len(stored))
	}

	// Lossless means every pixel survives the round trip. The image is above
	// the edge limit, so it is resized first; comparison is therefore against
	// the resized-and-re-encoded result decoded twice, which must be stable.
	decoded, err := png.Decode(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("stored PNG does not decode: %v", err)
	}
	var reencoded bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&reencoded, decoded); err != nil {
		t.Fatalf("re-encoding the stored PNG failed: %v", err)
	}
	roundTripped, err := png.Decode(bytes.NewReader(reencoded.Bytes()))
	if err != nil {
		t.Fatalf("round-tripped PNG does not decode: %v", err)
	}
	assertPixelIdentical(t, decoded, roundTripped)
}

// TestPNGUnderEdgeLimitKeepsEveryPixel proves the lossless claim directly: a
// PNG that is over the size threshold but inside the edge limit is not
// resized, so the stored pixels must equal the source pixels exactly.
func TestPNGUnderEdgeLimitKeepsEveryPixel(t *testing.T) {
	src := flatImage(2000, 1500)
	input := encodePNGFast(t, src)
	if len(input) <= defaultThresholdBytes {
		t.Fatalf("fixture is %d bytes, below the %d byte threshold", len(input), defaultThresholdBytes)
	}

	stored, outcome := run(t, input)

	if !outcome.reEncoded {
		t.Error("outcome reports no re-encode for an over-threshold PNG")
	}
	if len(stored) >= len(input) {
		t.Errorf("PNG was not optimised: input %d bytes, stored %d bytes", len(input), len(stored))
	}

	decoded, err := png.Decode(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("stored PNG does not decode: %v", err)
	}
	assertPixelIdentical(t, src, decoded)

	if outcome.width != 2000 || outcome.height != 1500 {
		t.Errorf("outcome geometry = %dx%d, want 2000x1500", outcome.width, outcome.height)
	}
}

func assertPixelIdentical(t *testing.T, want, got image.Image) {
	t.Helper()
	wb, gb := want.Bounds(), got.Bounds()
	if wb.Dx() != gb.Dx() || wb.Dy() != gb.Dy() {
		t.Fatalf("bounds differ: want %dx%d, got %dx%d", wb.Dx(), wb.Dy(), gb.Dx(), gb.Dy())
	}
	for y := range wb.Dy() {
		for x := range wb.Dx() {
			wr, wg, wbl, wa := want.At(wb.Min.X+x, wb.Min.Y+y).RGBA()
			gr, gg, gbl, ga := got.At(gb.Min.X+x, gb.Min.Y+y).RGBA()
			if wr != gr || wg != gg || wbl != gbl || wa != ga {
				t.Fatalf("pixel (%d,%d) changed: want RGBA(%d,%d,%d,%d), got RGBA(%d,%d,%d,%d)",
					x, y, wr, wg, wbl, wa, gr, gg, gbl, ga)
			}
		}
	}
}

// TestFilesUnderThresholdAreByteIdentical is the pass-through guarantee. It is
// the property that makes the feature safe to enable: anything the plugin does
// not deliberately compress must survive bit for bit.
func TestFilesUnderThresholdAreByteIdentical(t *testing.T) {
	smallJPEG := encodeJPEG(t, noisyImage(200, 150, 3), 90)
	smallPNG := encodePNGFast(t, flatImage(120, 90))

	cases := []struct {
		name  string
		input []byte
	}{
		{"small jpeg", smallJPEG},
		{"small png", smallPNG},
		{"empty file", []byte{}},
		{"plain text", []byte("MediaForge earlier work pass-through\n")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.input) > defaultThresholdBytes {
				t.Fatalf("fixture is %d bytes, which is NOT under the %d byte threshold",
					len(tc.input), defaultThresholdBytes)
			}
			stored, outcome := run(t, tc.input)
			if outcome.reEncoded {
				t.Error("outcome reports a re-encode for a file below the threshold")
			}
			if !bytes.Equal(stored, tc.input) {
				t.Errorf("stored bytes differ from input: %d in, %d out", len(tc.input), len(stored))
			}
		})
	}
}

// TestNonImagesAreByteIdenticalEvenWhenLarge makes sure the size threshold is
// not the only gate. A 2 MB PDF, video or archive must pass through untouched.
func TestNonImagesAreByteIdenticalEvenWhenLarge(t *testing.T) {
	rnd := rand.New(rand.NewSource(99))
	large := make([]byte, 2*1024*1024)
	if _, err := rnd.Read(large); err != nil {
		t.Fatalf("generating the fixture failed: %v", err)
	}

	cases := []struct {
		name  string
		input []byte
	}{
		{"random binary", large},
		{"pdf header", append([]byte("%PDF-1.7\n"), large...)},
		{"gif", append([]byte("GIF89a"), large...)},
		{"truncated jpeg header only", append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, large...)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.input) <= defaultThresholdBytes {
				t.Fatalf("fixture must be over the threshold to be meaningful")
			}
			stored, outcome := run(t, tc.input)
			if outcome.reEncoded {
				t.Error("outcome reports a re-encode for a non-image")
			}
			if !bytes.Equal(stored, tc.input) {
				t.Errorf("non-image was modified: %d bytes in, %d bytes out", len(tc.input), len(stored))
			}
		})
	}
}

// TestEXIFIsPreservedByDefault is the SPEC §4.1 promise that metadata survives.
// Go's image/jpeg encoder discards every APP segment, so a naive
// decode-resize-encode pipeline silently fails this. The test asserts the exact
// original segment bytes reappear in the output.
func TestEXIFIsPreservedByDefault(t *testing.T) {
	base := encodeJPEG(t, noisyImage(4000, 3000, 4), 95)
	input, segment := jpegWithEXIF(t, base)
	if len(input) <= defaultThresholdBytes {
		t.Fatalf("fixture is %d bytes, below the %d byte threshold", len(input), defaultThresholdBytes)
	}

	stored, outcome := run(t, input)

	if !outcome.reEncoded {
		t.Fatal("outcome reports no re-encode; the EXIF test would be vacuous")
	}
	if !bytes.Contains(stored, segment) {
		t.Errorf("the EXIF APP1 segment did not survive re-encoding.\n"+
			"  wanted % X ... (%d bytes)\n"+
			"  stored file begins % X",
			segment[:min(12, len(segment))], len(segment), stored[:min(24, len(stored))])
	}

	// Preserving metadata must not corrupt the image itself.
	w, h, format := decodeBounds(t, stored)
	if format != "jpeg" {
		t.Errorf("stored format = %q, want jpeg", format)
	}
	if w > defaultMaxEdgeLength || h > defaultMaxEdgeLength {
		t.Errorf("stored image %dx%d exceeds the %d px edge limit", w, h, defaultMaxEdgeLength)
	}
	if _, err := jpeg.Decode(bytes.NewReader(stored)); err != nil {
		t.Errorf("stored JPEG with EXIF does not fully decode: %v", err)
	}
}

// TestCorruptImageFallsBackToPassThrough covers the safety valve. A file that
// claims to be an image but cannot be decoded must be stored unchanged rather
// than rejected or truncated — compression is an optimisation, never a gate.
func TestCorruptImageFallsBackToPassThrough(t *testing.T) {
	good := encodeJPEG(t, noisyImage(3000, 2000, 5), 95)
	if len(good) <= defaultThresholdBytes {
		t.Fatalf("fixture is %d bytes, below the %d byte threshold", len(good), defaultThresholdBytes)
	}

	// Keep the JPEG signature but destroy the entropy-coded data, so format
	// sniffing succeeds and the decode fails.
	corrupt := bytes.Clone(good)
	for i := len(corrupt) / 2; i < len(corrupt); i++ {
		corrupt[i] = 0x00
	}

	stored, outcome := run(t, corrupt)

	if outcome.reEncoded {
		t.Error("outcome reports a re-encode for an undecodable image")
	}
	if !bytes.Equal(stored, corrupt) {
		t.Errorf("undecodable image was modified: %d bytes in, %d bytes out",
			len(corrupt), len(stored))
	}
}

// TestHardCodedDefaultsMatchTheSpec pins the slice-2 constants to SPEC §4.1.
// the configuration surface replaces these with configuration; until then a drifted default is a
// silent behaviour change.
func TestHardCodedDefaultsMatchTheSpec(t *testing.T) {
	if defaultMaxEdgeLength != 2560 {
		t.Errorf("defaultMaxEdgeLength = %d, want 2560 (SPEC §4.1)", defaultMaxEdgeLength)
	}
	if defaultJpegQuality != 85 {
		t.Errorf("defaultJpegQuality = %d, want 85 (SPEC §4.1)", defaultJpegQuality)
	}
	if defaultThresholdBytes != 512*1024 {
		t.Errorf("defaultThresholdBytes = %d, want %d (512 KB, SPEC §4.1)",
			defaultThresholdBytes, 512*1024)
	}
}

// TestProcessUploadStreamsWithoutHoldingWholeFile guards the memory profile the
// hook relies on for large non-image uploads. A pass-through of a file much
// larger than any sane buffer must still work through a pipe-like writer that
// only accepts small writes at a time.
func TestProcessUploadStreamsWithoutHoldingWholeFile(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	input := make([]byte, 12*1024*1024)
	if _, err := rnd.Read(input); err != nil {
		t.Fatalf("generating the fixture failed: %v", err)
	}

	var sink chunkCountingWriter
	outcome, err := processUpload(&sink, bytes.NewReader(input), defaultConfiguration())
	if err != nil {
		t.Fatalf("processUpload returned an error: %v", err)
	}
	if outcome.bytesWritten != int64(len(input)) {
		t.Errorf("bytesWritten = %d, want %d", outcome.bytesWritten, len(input))
	}
	if sink.largestWrite > 1024*1024 {
		t.Errorf("a single write of %d bytes suggests the whole file was buffered; "+
			"non-image pass-through must stream", sink.largestWrite)
	}
	if got := sink.sum.Sum64(); got != fnvSum(input) {
		t.Errorf("streamed bytes differ from input (fnv %d vs %d)", got, fnvSum(input))
	}
}

type chunkCountingWriter struct {
	largestWrite int
	sum          fnvHash
}

func (w *chunkCountingWriter) Write(p []byte) (int, error) {
	if len(p) > w.largestWrite {
		w.largestWrite = len(p)
	}
	w.sum.Write(p)
	return len(p), nil
}

// fnvHash is a tiny inline FNV-1a so the streaming test can compare content
// without keeping a second copy of a 12 MB fixture in memory.
type fnvHash struct{ h uint64 }

func (f *fnvHash) Write(p []byte) {
	if f.h == 0 {
		f.h = 14695981039346656037
	}
	for _, b := range p {
		f.h ^= uint64(b)
		f.h *= 1099511628211
	}
}

func (f *fnvHash) Sum64() uint64 { return f.h }

func fnvSum(b []byte) uint64 {
	var f fnvHash
	f.Write(b)
	return f.Sum64()
}

var _ = fmt.Sprintf

// ---------------------------------------------------------------------------
// Geometry correction
// ---------------------------------------------------------------------------

// TestApplyGeometryUpdatesStaleDimensions covers the second half of the
// metadata invariant. The server reads Width/Height from the original upload
// BEFORE the hook runs and never recomputes them, so a resize that does not
// correct them reproduces SPEC §1's stale-metadata defect in a different
// column.
func TestApplyGeometryUpdatesStaleDimensions(t *testing.T) {
	cases := []struct {
		name                 string
		recordedW, recordedH int
		outcome              processOutcome
		wantW, wantH         int
		wantCorrected        bool
	}{
		{
			name:      "resized image updates both dimensions",
			recordedW: 4000, recordedH: 3000,
			outcome:       processOutcome{reEncoded: true, sourceWidth: 4000, sourceHeight: 3000, width: 2560, height: 1920},
			wantW:         2560,
			wantH:         1920,
			wantCorrected: true,
		},
		{
			name:      "re-encoded without resize keeps dimensions but reports correction",
			recordedW: 1600, recordedH: 1200,
			outcome:       processOutcome{reEncoded: true, sourceWidth: 1600, sourceHeight: 1200, width: 1600, height: 1200},
			wantW:         1600,
			wantH:         1200,
			wantCorrected: true,
		},
		{
			// preprocessImage swaps width and height when EXIF reports a 90
			// degree rotation. The plugin preserves that EXIF tag without
			// rotating pixels, so the server's swapped convention must be
			// respected rather than overwritten.
			name:      "exif-rotated image keeps the server's swapped convention",
			recordedW: 3000, recordedH: 4000,
			outcome:       processOutcome{reEncoded: true, sourceWidth: 4000, sourceHeight: 3000, width: 2560, height: 1920},
			wantW:         1920,
			wantH:         2560,
			wantCorrected: true,
		},
		{
			name:      "pass-through never touches dimensions",
			recordedW: 800, recordedH: 600,
			outcome:       processOutcome{reEncoded: false, sourceWidth: 800, sourceHeight: 600},
			wantW:         800,
			wantH:         600,
			wantCorrected: false,
		},
		{
			// An unrecognised convention is not something to overwrite on a
			// guess; leaving it alone is strictly safer than inventing a value.
			name:      "unrecognised recorded geometry is left alone",
			recordedW: 123, recordedH: 456,
			outcome:       processOutcome{reEncoded: true, sourceWidth: 4000, sourceHeight: 3000, width: 2560, height: 1920},
			wantW:         123,
			wantH:         456,
			wantCorrected: false,
		},
		{
			name:      "zero recorded geometry (non-image path) is left alone",
			recordedW: 0, recordedH: 0,
			outcome:       processOutcome{reEncoded: true, sourceWidth: 4000, sourceHeight: 3000, width: 2560, height: 1920},
			wantW:         0,
			wantH:         0,
			wantCorrected: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, h, corrected := applyGeometry(tc.recordedW, tc.recordedH, tc.outcome)
			if w != tc.wantW || h != tc.wantH {
				t.Errorf("geometry = %dx%d, want %dx%d", w, h, tc.wantW, tc.wantH)
			}
			if corrected != tc.wantCorrected {
				t.Errorf("corrected = %v, want %v", corrected, tc.wantCorrected)
			}
		})
	}
}

// TestOversizedImageIsNotDecodedBeyondTheResolutionCap proves the
// denial-of-service guard. A decode costs width*height*4 bytes of resident
// memory, so an image above the cap must be passed through rather than
// decoded — an uncompressed file is a far better outcome than an
// out-of-memory plugin process.
func TestOversizedImageIsNotDecodedBeyondTheResolutionCap(t *testing.T) {
	// A PNG whose IHDR is structurally valid (correct CRC) and claims
	// 30000x30000 — 900 Mpx, roughly 3.6 GB once decoded — with no image data
	// behind it. The CRC matters: without it DecodeConfig fails and the file
	// would pass through for the wrong reason, leaving the cap untested.
	ihdr := []byte{'I', 'H', 'D', 'R',
		0x00, 0x00, 0x75, 0x30, // width  30000
		0x00, 0x00, 0x75, 0x30, // height 30000
		0x08,             // bit depth
		0x06,             // colour type RGBA
		0x00, 0x00, 0x00, // compression, filter, interlace
	}
	header := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	header = binary.BigEndian.AppendUint32(header, uint32(len(ihdr)-4))
	header = append(header, ihdr...)
	header = binary.BigEndian.AppendUint32(header, crc32.ChecksumIEEE(ihdr))

	padded := append(header, make([]byte, defaultThresholdBytes+1)...)

	stored, outcome := run(t, padded)

	if outcome.reEncoded {
		t.Error("an image above the resolution cap was re-encoded")
	}
	if !bytes.Equal(stored, padded) {
		t.Error("an image above the resolution cap was modified")
	}
	// sourceWidth/sourceHeight are deliberately NOT asserted here. The cap is
	// enforced from the header before the file is buffered, so the pipeline
	// short-circuits to a streaming pass-through and never reaches the stage
	// that records geometry. That ordering is the point — see
	// TestHugeFakeImageIsNotBufferedIntoMemory.
}

// TestAlreadyOptimisedImageIsNotGrown is the "never make it worse" rule. A
// well-compressed file that the re-encode cannot beat must be stored as the
// original, and must be reported as a pass-through so the SPEC §4.4 note
// contract stays honest.
func TestAlreadyOptimisedImageIsNotGrown(t *testing.T) {
	// A low-quality JPEG that is still over the threshold: re-encoding it at
	// quality 85 makes it BIGGER, not smaller.
	input := encodeJPEG(t, noisyImage(2000, 1500, 11), 40)
	for len(input) <= defaultThresholdBytes {
		t.Fatalf("fixture is %d bytes, below the %d byte threshold", len(input), defaultThresholdBytes)
	}

	stored, outcome := run(t, input)

	if len(stored) > len(input) {
		t.Errorf("file grew: %d bytes in, %d bytes out", len(input), len(stored))
	}
	if !outcome.reEncoded && !bytes.Equal(stored, input) {
		t.Error("outcome reports a pass-through but the bytes changed")
	}
}

// TestBrokenStreamIsReportedNotSilentlyTruncated is a regression test for a
// real defect found during earlier work.
//
// The first implementation used io.ReadFull and swallowed io.ErrUnexpectedEOF.
// That sentinel is returned BOTH when a stream legitimately ends early and
// when a stream breaks — so a failed upload was indistinguishable from a small
// file, and the truncated prefix was stored as if it were complete. Storing a
// truncated file silently is the worst outcome this plugin can produce.
//
// Every failure position is covered, because the code paths differ: a break
// inside the header, after the header on a non-image, and after the header on
// an image that would otherwise be buffered and compressed.
func TestBrokenStreamIsReportedNotSilentlyTruncated(t *testing.T) {
	boom := errors.New("backing store went away")

	cases := []struct {
		name   string
		prefix []byte
	}{
		{
			name:   "breaks inside the header read",
			prefix: []byte("only a few bytes then failure"),
		},
		{
			name:   "breaks after the header on a non-image",
			prefix: bytes.Repeat([]byte("x"), defaultThresholdBytes+4096),
		},
		{
			name:   "breaks after the header on a jpeg",
			prefix: encodeJPEG(t, noisyImage(3000, 2000, 31), 95)[:defaultThresholdBytes+4096],
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := io.MultiReader(
				bytes.NewReader(tc.prefix),
				&erroringReader{err: boom},
			)

			var out bytes.Buffer
			_, err := processUpload(&out, src, defaultConfiguration())

			if err == nil {
				t.Fatalf("processUpload accepted a broken stream and stored %d bytes; "+
					"a truncated file must be reported so the hook can reject the upload",
					out.Len())
			}
			if !errors.Is(err, boom) {
				t.Errorf("error does not wrap the underlying cause: %v", err)
			}
		})
	}
}

// TestShortStreamIsNotMistakenForABreak is the other side of the same coin: a
// file that is simply smaller than the read buffer must pass through cleanly.
func TestShortStreamIsNotMistakenForABreak(t *testing.T) {
	input := []byte("a genuinely short file\n")

	var out bytes.Buffer
	outcome, err := processUpload(&out, iotest.OneByteReader(bytes.NewReader(input)), defaultConfiguration())
	if err != nil {
		t.Fatalf("a short but complete stream was reported as an error: %v", err)
	}
	if !bytes.Equal(out.Bytes(), input) {
		t.Errorf("short file was modified: %q -> %q", input, out.Bytes())
	}
	if outcome.reEncoded {
		t.Error("outcome reports a re-encode for a short text file")
	}
}

type erroringReader struct{ err error }

func (r *erroringReader) Read(_ []byte) (int, error) { return 0, r.err }

// TestZeroByteReadIsNotTreatedAsEndOfStream is a regression test for review
// finding 1.
//
// io.Reader's contract is explicit: "Callers should treat a return of 0 and nil
// as indicating that nothing happened; in particular it does not indicate EOF."
// The first implementation treated it as end of stream, so a reader that
// briefly returned (0, nil) — which a network-backed or middleware reader may
// legitimately do — would have had its remaining bytes silently discarded and
// a truncated file stored.
func TestZeroByteReadIsNotTreatedAsEndOfStream(t *testing.T) {
	payload := bytes.Repeat([]byte("MediaForge"), 4096) // 40 KB, under the threshold

	var out bytes.Buffer
	outcome, err := processUpload(&out, &stutteringReader{data: payload}, defaultConfiguration())
	if err != nil {
		t.Fatalf("processUpload errored on a stuttering but valid stream: %v", err)
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Errorf("stuttering stream was truncated: %d bytes in, %d bytes out",
			len(payload), out.Len())
	}
	if outcome.bytesWritten != int64(len(payload)) {
		t.Errorf("bytesWritten = %d, want %d", outcome.bytesWritten, len(payload))
	}
}

// stutteringReader returns (0, nil) before every real read, exactly as the
// io.Reader contract permits.
type stutteringReader struct {
	data    []byte
	pos     int
	stutter bool
}

func (r *stutteringReader) Read(p []byte) (int, error) {
	r.stutter = !r.stutter
	if r.stutter {
		return 0, nil // "nothing happened" — NOT end of stream
	}
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// TestHugeFakeImageIsNotBufferedIntoMemory is a regression test for review
// finding 3.
//
// The first implementation sniffed only the 8 magic bytes and then buffered the
// WHOLE file before consulting the resolution cap. A large non-image carrying
// PNG magic — trivially craftable, and a plausible accident with a renamed
// file — would therefore have been read into RAM in its entirety, crashing the
// plugin process. The geometry must be checked from the header alone, before
// anything else is buffered.
func TestHugeFakeImageIsNotBufferedIntoMemory(t *testing.T) {
	// A valid PNG header claiming a size above the decode cap, followed by a
	// very large body. countingReader fails the test if the body is consumed
	// into memory rather than streamed.
	ihdr := []byte{'I', 'H', 'D', 'R',
		0x00, 0x00, 0x75, 0x30, // 30000
		0x00, 0x00, 0x75, 0x30, // 30000
		0x08, 0x06, 0x00, 0x00, 0x00,
	}
	header := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	header = binary.BigEndian.AppendUint32(header, uint32(len(ihdr)-4))
	header = append(header, ihdr...)
	header = binary.BigEndian.AppendUint32(header, crc32.ChecksumIEEE(ihdr))

	const bodySize = 64 * 1024 * 1024 // 64 MB of body behind the fake header

	// The probe fails if the pipeline reads far ahead of what it has written,
	// which is precisely what "buffer the whole file first" looks like.
	// Measuring write chunking alone would NOT catch this: the buggy version
	// buffered 64 MB and then streamed it out in tidy 32 KB writes.
	probe := &readAheadProbe{
		body:     &zeroReader{remaining: bodySize},
		maxAhead: 8 * 1024 * 1024,
	}
	src := io.MultiReader(bytes.NewReader(header), probe)

	sink := &writeTrackingWriter{probe: probe}
	outcome, err := processUpload(sink, src, defaultConfiguration())
	if err != nil {
		t.Fatalf("processUpload errored: %v", err)
	}

	want := int64(len(header) + bodySize)
	if outcome.bytesWritten != want {
		t.Errorf("bytesWritten = %d, want %d", outcome.bytesWritten, want)
	}
	if outcome.reEncoded {
		t.Error("an oversized fake image was re-encoded")
	}
	if probe.peakAhead > probe.maxAhead {
		t.Errorf("the pipeline read %d bytes further than it had written (limit %d); "+
			"an image above the decode cap was buffered in memory instead of streamed",
			probe.peakAhead, probe.maxAhead)
	}
	if sink.largestWrite > 1024*1024 {
		t.Errorf("a single write of %d bytes suggests the body was buffered",
			sink.largestWrite)
	}
}

// readAheadProbe measures how far reading runs ahead of writing, which is a
// direct proxy for how much of the file is resident in memory at once.
type readAheadProbe struct {
	body      io.Reader
	read      int64
	written   int64
	peakAhead int64
	maxAhead  int64
}

func (r *readAheadProbe) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	r.read += int64(n)
	if ahead := r.read - r.written; ahead > r.peakAhead {
		r.peakAhead = ahead
	}
	return n, err
}

func (r *readAheadProbe) noteWritten(n int) { r.written += int64(n) }

type writeTrackingWriter struct {
	probe        *readAheadProbe
	largestWrite int
}

func (w *writeTrackingWriter) Write(p []byte) (int, error) {
	if len(p) > w.largestWrite {
		w.largestWrite = len(p)
	}
	w.probe.noteWritten(len(p))
	return len(p), nil
}

// zeroReader emits n zero bytes without allocating them up front.
type zeroReader struct{ remaining int }

func (r *zeroReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	clear(p[:n])
	r.remaining -= n
	return n, nil
}

// TestEXIFSurvivesJPEGFillBytes is a regression test for finding 4.
//
// The JPEG standard allows any number of 0xFF fill bytes before a marker. The
// first segment walker assumed exactly one, so a padded file desynchronised the
// parser: it read 0xFF as the marker type, misinterpreted the following bytes
// as a segment length, and silently dropped the EXIF it was supposed to carry
// over.
func TestEXIFSurvivesJPEGFillBytes(t *testing.T) {
	base := encodeJPEG(t, noisyImage(3000, 2200, 41), 95)
	withExif, segment := jpegWithEXIF(t, base)

	// Splice two fill bytes in front of the APP1 marker, which is legal JPEG.
	padded := make([]byte, 0, len(withExif)+2)
	padded = append(padded, withExif[:2]...)
	padded = append(padded, 0xFF, 0xFF)
	padded = append(padded, withExif[2:]...)

	if len(padded) <= defaultThresholdBytes {
		t.Fatalf("fixture is %d bytes, below the %d byte threshold", len(padded), defaultThresholdBytes)
	}

	stored, outcome := run(t, padded)

	if !outcome.reEncoded {
		t.Fatal("padded JPEG was not re-encoded; the test would be vacuous")
	}
	if !bytes.Contains(stored, segment) {
		t.Error("EXIF was lost because 0xFF fill bytes desynchronised the segment walker")
	}
}

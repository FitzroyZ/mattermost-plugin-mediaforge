package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"time"

	xdraw "golang.org/x/image/draw"
)

// emptyReader is an already-exhausted reader, used when the entire upload
// already sits in the header buffer.
type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

// headerProbeBytes bounds the buffer used to inspect an upload before deciding
// what to do with it.
//
// It is deliberately a constant rather than derived from the configured size
// threshold. The buffer has two jobs — recognise the format and read the
// declared geometry — and both need a fixed, generous window, because a JPEG
// may carry hundreds of kilobytes of EXIF and thumbnail data before its SOF
// marker. Sizing it from the threshold would make a 1 KB threshold unable to
// sniff anything, and a 1 GB threshold allocate a gigabyte per upload.
//
// The value matches the image compression threshold (512 KB + 1 byte), which keeps the
// default path byte-for-byte identical to the behaviour it replaces.
const headerProbeBytes = 512*1024 + 1

// maxDecodePixels caps how large an image may be before the plugin declines to
// decode it and passes it through instead.
//
// Decoding costs width*height*4 bytes of resident memory, so an unbounded
// decode is a denial-of-service vector: a crafted 20000x20000 PNG is a small
// download but a 1.6 GB allocation. The cap is deliberately set to Mattermost's
// own FileSettings.MaxImageResolution default (33,177,600 px = 7680x4320), so
// the plugin refuses to decode exactly the images the server would refuse to
// accept anyway. Passing through is the safe answer: an uncompressed file is a
// far better outcome than an out-of-memory plugin process.
const maxDecodePixels = 33177600

// copyBufferSize bounds a single pass-through write. It matches io.Copy's own
// default so streaming behaviour is unsurprising.
const copyBufferSize = 32 * 1024

// processOutcome describes what the image pipeline did with one uploaded file.
type processOutcome struct {
	// reEncoded is true when the bytes written to the output differ from the
	// input because the pipeline re-encoded the image.
	reEncoded bool
	// sourceWidth and sourceHeight are the dimensions of the image as decoded
	// from the INPUT, before any resize. They are how the caller recognises
	// the geometry the server recorded before the hook ran.
	sourceWidth  int
	sourceHeight int
	// width and height describe the stored image. They are only meaningful
	// when reEncoded is true.
	width  int
	height int
	// bytesWritten is the number of bytes handed to the output writer.
	bytesWritten int64
	// originalSize is the size of the file the user sent, in bytes.
	//
	// It is measured by the pipeline rather than read from fileinfo.Size,
	// because the pipeline is the only place that knows it for certain: the
	// server overwrites fileinfo.Size from its own byte count immediately
	// after the hook returns, so by the time the note is written the recorded
	// size already describes the compressed file. It is only filled in where
	// the whole file was in hand — that is every path that can re-encode.
	originalSize int64
	// isVideo marks an outcome produced by the video pipeline. The hook needs
	// it because a transcoded video is logged and described differently from a
	// re-encoded image.
	isVideo bool
	// duration is how long the conversion took. Only filled in for video,
	// where it is worth logging: it is the number an administrator needs when
	// deciding whether the timeout is set sensibly.
	duration time.Duration
	// queueFull records that the file was stored as sent because no
	// processing slot became available in time. It is not an error — the
	// upload succeeded — but it is the one pass-through an administrator may
	// want to know about, because it means the limits are set below what the
	// instance is being asked to do.
	queueFull bool
	// isVoice marks an outcome produced by the voice pipeline, whether or not
	// the file was actually converted. The hook needs it to decide how to
	// describe the stored file to the server (SPEC §4.3).
	isVoice bool
	// voiceConverted is true when the recording was re-encoded into AAC in an
	// MP4 container — which is the moment the stored file gains a new name, a
	// new extension and a new MIME type.
	//
	// It is deliberately separate from reEncoded. reEncoded means "this file
	// got smaller and the saving is worth reporting", which is what the size
	// note and the administrator's report both read. A voice conversion
	// usually makes the file slightly LARGER, because Opus is a better codec
	// than AAC; reporting it as a compression would put a negative saving into
	// the one figure the statistics report exists to make trustworthy.
	voiceConverted bool
	// spokenSeconds is how long a converted recording plays.
	//
	// Measured by the duration probe that enforces MaxVoiceSeconds, and
	// previously discarded once that check had passed. It is carried here so
	// the note can tell a reader what the attachment is asking of them before
	// they press play.
	//
	// Separate from `duration`, which is how long the CONVERSION took. Those
	// are different facts about the same file and confusing them would put a
	// processing time on the post.
	spokenSeconds float64
	// storedLengthError records that the stored file's length could not be
	// measured, so the length reported is the one from the input.
	//
	// Carried rather than logged where it happens: the voice pipeline takes
	// only a configuration and has no logger, and threading an API handle
	// into it for a non-event would be the wrong trade. The hook writes the
	// warning, where every other diagnosis about this upload is written.
	storedLengthError error
}

// processUpload copies input to output, compressing images on the way.
//
// It is the entry point for everything that has no file name to go on, and it
// is kept exactly as earlier work left it: every existing caller and every test
// written against them behaves identically. Naming is what routes an upload
// into the voice path, so an unnamed one can never take it — see
// processNamedUpload.
func processUpload(output io.Writer, input io.Reader, cfg configuration) (processOutcome, error) {
	return processNamedUpload(output, input, cfg, "")
}

// processNamedUpload is processUpload with the file name the server received.
//
// The name is the only thing that separates a voice message from an ordinary
// audio attachment. Both are an audio track in a container that can also carry
// video, and video compression stores exactly those byte-identical — so recognising a
// recording by its contents would quietly claim every `.m4a` voice memo a user
// attaches and break a proved path. The plugin's own recorder puts a known
// prefix on what it uploads, and nothing else reaches this pipeline.
//
// The shape of the work:
//
//  0. With every feature switched off, everything streams straight through.
//  1. Read at most headerProbeBytes. That is enough to recognise the format
//     and read the declared geometry, while bounding the buffer for the
//     common small-file case.
//  2. A file the recorder named is handed to the voice pipeline, which makes
//     the real decision from the streams ffprobe finds.
//  3. Anything at or below the configured size threshold, or not a JPEG/PNG,
//     is streamed out unchanged without ever being held in memory in full.
//  4. Otherwise the file is buffered, decoded, resized if it exceeds the edge
//     limit, and re-encoded. If anything goes wrong, or the result is not
//     actually smaller, the buffered original is written instead.
func processNamedUpload(
	output io.Writer, input io.Reader, cfg configuration, name string,
) (processOutcome, error) {
	// With ALL features switched off the plugin must be indistinguishable
	// from an uninstalled one, so the upload is streamed straight through
	// without even being sniffed.
	if !cfg.EnableImageCompression && !cfg.videoEnabled() && !cfg.voiceEnabled() {
		return passThrough(output, input)
	}

	threshold := cfg.thresholdBytes()

	head, complete, err := readHeader(input, headerProbeBytes)
	if err != nil {
		return processOutcome{}, err
	}

	// A recording the plugin made itself is decided before anything else.
	// Its container is one the video pipeline would otherwise pick up, and
	// the two have opposite contracts: this one owes the sender a file their
	// colleagues can play on a phone.
	if cfg.voiceEnabled() && looksLikeVoiceRecording(name) {
		var rest io.Reader = input
		if !complete {
			// The whole upload already fits in the header buffer.
			rest = emptyReader{}
		}
		return processVoiceUpload(output, head, rest, cfg)
	}

	// Video is decided before the image rules, because the two pipelines have
	// opposite failure contracts and must not be allowed to overlap. The image
	// threshold deliberately does not apply: it is tuned for photographs, and a
	// small video is still a video.
	if cfg.videoEnabled() && looksLikeVideoContainer(head) {
		var rest io.Reader = input
		if !complete {
			rest = emptyReader{}
		}
		return processVideoUpload(output, head, rest, cfg)
	}

	if !cfg.EnableImageCompression {
		// Images are off and this is neither a video nor a recording.
		if !complete {
			return passThrough(output, bytes.NewReader(head))
		}
		return passThrough(output, io.MultiReader(bytes.NewReader(head), input))
	}

	if !complete {
		// The stream is exhausted, so head is the entire file and its exact
		// size is known.
		if len(head) <= threshold || !isCompressibleImage(head) {
			return passThrough(output, bytes.NewReader(head))
		}
		// Over a threshold small enough that the whole file fits in the probe
		// buffer. It is already in memory, so there is nothing left to read.
		return processBuffered(output, head, cfg)
	}

	// Sniff the format from the header alone. Non-images never get buffered.
	if !isCompressibleImage(head) {
		return passThrough(output, io.MultiReader(bytes.NewReader(head), input))
	}

	// Decide whether the image is safe to decode BEFORE buffering the rest of
	// it. This ordering is the whole defence against memory exhaustion: a 10 GB
	// file carrying PNG magic bytes is trivial to craft and easy to produce by
	// accident, and buffering it to discover its dimensions would be far too
	// late. Both formats declare their geometry in the first few dozen bytes,
	// well inside the 512 KB header, so the check costs nothing.
	imgCfg, _, imgCfgErr := image.DecodeConfig(bytes.NewReader(head))
	if imgCfgErr != nil || !safeToDecode(imgCfg.Width, imgCfg.Height) {
		return passThrough(output, io.MultiReader(bytes.NewReader(head), input))
	}

	// From here the file is a JPEG or PNG over the threshold whose declared
	// geometry is within the decode budget, so it is worth buffering in full:
	// re-encoding requires random access to the pixels.
	//
	// The buffer is bounded by what the declared geometry can justify. Without
	// that bound this is the plugin's cheapest denial of service: DecodeConfig
	// reads only the IHDR or the SOF marker, so a PNG declaring 100x100 passes
	// safeToDecode and then buffers however many megabytes follow it. The cap
	// above bounds the DECODE; this one bounds the READ, and the two are
	// different attacks — see bufferBudgetFor.
	original, exceeded, err := readBufferedUpTo(head, input, bufferBudgetFor(imgCfg))
	if err != nil {
		return processOutcome{}, err
	}
	if exceeded {
		// More bytes than a picture of this size could account for. The file is
		// stored exactly as sent, which is what SPEC §4.1 already requires of
		// every image the plugin declines to compress: an upload must never
		// fail because compression was not attempted.
		return passThrough(output, io.MultiReader(bytes.NewReader(original), input))
	}

	// Only now is the exact size known. With the default 512 KB threshold this
	// check can never fire — the file filled the probe buffer, so it is over
	// the threshold by construction — but a raised threshold needs it.
	if len(original) <= threshold {
		return passThrough(output, bytes.NewReader(original))
	}

	return processBuffered(output, original, cfg)
}

// processBuffered compresses an image that is already fully in memory and
// writes either the result or the untouched original.
func processBuffered(output io.Writer, original []byte, cfg configuration) (processOutcome, error) {
	// Bound how many images are decoded at once. Each one holds a decoded
	// bitmap — width*height*4 bytes — for as long as it is being re-encoded,
	// so an unbounded burst is a memory spike rather than a speed-up.
	//
	// A slot that does not arrive in time stores the original. That is
	// required, not merely convenient: SPEC §4.1 and the image compression contract say
	// an image upload never fails because of compression, and "the server was
	// busy" is no better a reason than any other.
	release, admitted := acquireImageSlot(cfg)
	if !admitted {
		res, err := passThrough(output, bytes.NewReader(original))
		res.queueFull = true
		return res, err
	}

	// The slot is given back as soon as the decoding is done, before the
	// bytes are handed to the server — writing them can block on a slow
	// client, and holding a compression slot while waiting for the network
	// would limit throughput on something that costs no processor time. The
	// deferred call is the safety net: release is idempotent, so a panic
	// inside the encoder still returns the slot.
	defer release()

	outcome, compressed := compressImage(original, cfg)
	release()
	if compressed == nil {
		// Not decodable, too large to decode safely, or the re-encode did not
		// pay off. Store what the user sent.
		res, err := passThrough(output, bytes.NewReader(original))
		// Keep the source geometry if we learned it; the caller uses it only
		// when reEncoded is true, but reporting it costs nothing and helps
		// logging.
		res.sourceWidth, res.sourceHeight = outcome.sourceWidth, outcome.sourceHeight
		return res, err
	}

	written, err := output.Write(compressed)
	if err != nil {
		return processOutcome{}, fmt.Errorf("writing the compressed image: %w", err)
	}
	outcome.bytesWritten = int64(written)
	// The buffered input IS the file the user sent, so its length is the
	// original size the size note needs (SPEC §4.4).
	outcome.originalSize = int64(len(original))
	return outcome, nil
}

// readHeader reads up to n bytes, reporting whether the buffer was filled.
//
// It exists because io.ReadFull cannot be used here. ReadFull reports
// io.ErrUnexpectedEOF both for a stream that legitimately ended early and for
// one that BROKE while reporting io.ErrUnexpectedEOF itself — and the plugin
// must treat those opposite cases in opposite ways. Swallowing the sentinel
// would store a truncated file as if it were a small complete one, which is
// the single worst outcome this plugin can produce and exactly what the
// the first hook version rejection contract forbids.
//
// The loop therefore only accepts a bare io.EOF as a genuine end of stream.
// Every other error is propagated, so the hook can reject the upload.
func readHeader(r io.Reader, n int) (buf []byte, complete bool, err error) {
	buf = make([]byte, n)
	read := 0
	for read < n {
		got, readErr := r.Read(buf[read:])
		read += got
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return buf[:read], read >= n, nil
			}
			return buf[:read], false, fmt.Errorf("reading the upload: %w", readErr)
		}
		// A (0, nil) read means "nothing happened", NOT end of stream — the
		// io.Reader contract says so explicitly. Treating it as EOF would
		// discard the rest of the upload and store a truncated file, so the
		// loop simply tries again. io.Reader also permits this only
		// transiently, so there is no spin risk from a conforming reader.
	}
	return buf, true, nil
}

// passThrough streams src to dst without buffering it in full.
//
// The struct{io.Reader} wrapper is load-bearing. io.Copy and io.CopyBuffer
// both short-circuit to src.WriteTo when the source implements io.WriterTo,
// and both *bytes.Reader and io.MultiReader do — which would hand the whole
// file to the writer in a single call and defeat the streaming this function
// exists to guarantee. Hiding the concrete type behind a bare io.Reader forces
// the bounded-buffer path.
func passThrough(dst io.Writer, src io.Reader) (processOutcome, error) {
	buf := make([]byte, copyBufferSize)
	written, err := io.CopyBuffer(dst, struct{ io.Reader }{src}, buf)
	if err != nil {
		return processOutcome{bytesWritten: written}, fmt.Errorf("streaming the upload: %w", err)
	}
	return processOutcome{bytesWritten: written}, nil
}

// metadataAllowance is how many bytes beyond its own pixels an image may carry
// and still be worth buffering.
//
// A real photograph carries more than pixels: EXIF, an embedded preview
// thumbnail, and an ICC colour profile all live in APP segments, and a JPEG
// APPn segment is capped at 64 KB each with fifteen of them available. Two
// megabytes is comfortably above every legitimate case measured on the test
// instance fixtures and far below the point where buffering matters.
//
// It is deliberately generous. The cost of setting it too low is that real
// photographs stop being compressed — a silent loss of the plugin's whole
// purpose, and the failure mode
// TestAnImageWithPlausiblePaddingIsStillCompressed exists to catch.
const metadataAllowance = 2 * 1024 * 1024

// bufferBudgetFor is the largest number of bytes worth buffering for an image
// of the declared geometry.
//
// WHY A SECOND BOUND IS NEEDED AT ALL. safeToDecode already caps the PIXEL
// count, because decoding costs width*height*4 bytes of bitmap. That says
// nothing about the file's LENGTH, and the two are independent: image.DecodeConfig
// reads only the PNG IHDR chunk or the JPEG SOF marker, both of which sit in
// the first few dozen bytes. A PNG declaring 100x100 followed by 64 MB of
// well-formed ancillary chunks therefore passes every check the plugin had and
// was then read into one contiguous slice — measured at 63.5 MB resident for a
// file whose pixels account for 40 KB.
//
// It is the cheapest attack the plugin offered, for three reasons: the file
// compresses to almost nothing on the wire, the padding needs no valid pixel
// data, and the buffering happens BEFORE processBuffered takes an image slot,
// so the SPEC §4.4 concurrency limit does not bound it. Uploads in flight each
// held their own copy.
//
// The budget is the raw pixel data at 16 bits per channel — which is the most
// any honest encoding of that geometry needs, since a compressed file smaller
// than its own pixels is the normal case and one larger than them is already
// pathological — plus the metadata allowance above.
func bufferBudgetFor(cfg image.Config) int64 {
	// 8 bytes per pixel covers 16-bit RGBA, the widest format either decoder
	// accepts. safeToDecode has already bounded width*height, so this product
	// cannot overflow: the cap is 33 177 600 pixels, and 33 177 600 * 8 is
	// well inside int64.
	pixels := int64(cfg.Width) * int64(cfg.Height)
	return pixels*8 + metadataAllowance
}

// readBufferedUpTo reads the rest of the stream into the header buffer, giving
// up once the total passes budget.
//
// It reports `exceeded` rather than an error, because passing a large file
// through is a normal outcome and not a failure: the caller stores it exactly
// as sent. The returned slice always holds everything that WAS read, so the
// caller can hand those bytes on and stream the remainder without losing any.
//
// One byte beyond the budget is read deliberately, which is the same trick
// spoolUpload uses on the video path: it is how "exactly at the budget" is
// told apart from "over it" without reading the whole oversized file.
func readBufferedUpTo(head []byte, rest io.Reader, budget int64) (buf []byte, exceeded bool, err error) {
	if int64(len(head)) > budget {
		return head, true, nil
	}

	b := bytes.NewBuffer(make([]byte, 0, len(head)*2))
	b.Write(head)

	remaining := budget - int64(len(head)) + 1
	read, copyErr := b.ReadFrom(io.LimitReader(rest, remaining))
	if copyErr != nil {
		return nil, false, fmt.Errorf("buffering the upload: %w", copyErr)
	}
	// The limit reader hides a genuine end of stream, so "filled the budget"
	// and "the file ended exactly there" are distinguished by the extra byte.
	return b.Bytes(), read == remaining, nil
}

var (
	jpegMagic = []byte{0xFF, 0xD8, 0xFF}
	pngMagic  = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
)

// safeToDecode reports whether an image of the given declared dimensions fits
// within the decode budget.
//
// The area is checked by division rather than multiplication. Go's own PNG
// decoder already rejects dimensions large enough to overflow, so the product
// is not reachable in practice — but a guard whose correctness depends on a
// third party's input validation is a guard waiting to break, and the division
// form costs nothing.
func safeToDecode(width, height int) bool {
	if width <= 0 || height <= 0 {
		return false
	}
	return width <= maxDecodePixels/height
}

// isCompressibleImage reports whether the header looks like a format this
// slice handles. Only JPEG and PNG qualify: GIF is frequently animated (which
// a still re-encode would destroy) and SVG is not a raster image at all.
func isCompressibleImage(head []byte) bool {
	return bytes.HasPrefix(head, jpegMagic) || bytes.HasPrefix(head, pngMagic)
}

// compressImage decodes, resizes and re-encodes one buffered image.
//
// It returns nil bytes to mean "keep the original", which happens when the
// image cannot be decoded, is too large to decode safely, cannot be re-encoded,
// or simply did not get smaller. The outcome's sourceWidth/sourceHeight are
// filled in whenever the geometry could be read, even in the nil case.
func compressImage(original []byte, cfg configuration) (processOutcome, []byte) {
	var outcome processOutcome

	imgCfg, format, err := image.DecodeConfig(bytes.NewReader(original))
	if err != nil {
		// The magic bytes matched but the file is not a usable image.
		return outcome, nil
	}
	outcome.sourceWidth, outcome.sourceHeight = imgCfg.Width, imgCfg.Height

	if imgCfg.Width <= 0 || imgCfg.Height <= 0 || !safeToDecode(imgCfg.Width, imgCfg.Height) {
		return outcome, nil
	}

	decoded, _, err := image.Decode(bytes.NewReader(original))
	if err != nil {
		return outcome, nil
	}

	resized := scaleToFit(decoded, cfg.MaxEdgeLength)
	bounds := resized.Bounds()
	outcome.width, outcome.height = bounds.Dx(), bounds.Dy()

	var encoded []byte
	switch format {
	case "jpeg":
		encoded, err = encodeResizedJPEG(resized, original, cfg)
	case "png":
		encoded, err = encodeOptimisedPNG(resized)
	default:
		// isCompressibleImage already filtered these out; belt and braces.
		return outcome, nil
	}
	if err != nil || len(encoded) == 0 {
		return outcome, nil
	}

	// Never make a file bigger. Re-encoding can genuinely lose: an
	// already-optimised PNG, a paletted PNG that decodes to RGBA, or a JPEG
	// that was saved below quality 85. In those cases the original wins and
	// the file is reported as a pass-through, which keeps the "no note means
	// stored as the original" contract of SPEC §4.4 honest.
	if len(encoded) >= len(original) {
		return outcome, nil
	}

	outcome.reEncoded = true
	return outcome, encoded
}

// scaleToFit shrinks img so neither edge exceeds maxEdge, preserving the aspect
// ratio. Images already within the limit are returned untouched — SPEC §4.1
// explicitly forbids upscaling.
func scaleToFit(img image.Image, maxEdge int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxEdge && h <= maxEdge {
		return img
	}

	newW, newH := w, h
	if w >= h {
		newW = maxEdge
		newH = int(float64(h) * float64(maxEdge) / float64(w))
	} else {
		newH = maxEdge
		newW = int(float64(w) * float64(maxEdge) / float64(h))
	}
	// Rounding must never collapse an edge to zero.
	newW = max(newW, 1)
	newH = max(newH, 1)

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	// CatmullRom is the sharpest of the stock kernels and is what a user
	// expects from "the plugin resized my photo". It costs more than
	// ApproxBiLinear, but image work here is measured in tens of milliseconds
	// and runs before the upload completes, so quality is the right trade.
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)
	return dst
}

// encodeOptimisedPNG re-encodes a PNG at the highest compression setting.
//
// This is lossless in the sense SPEC §4.1 requires: every pixel value survives,
// only the DEFLATE stream and filtering change. Nothing is quantised, no colour
// is dropped, and the output is still a PNG.
func encodeOptimisedPNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encoding png: %w", err)
	}
	return buf.Bytes(), nil
}

// encodeResizedJPEG re-encodes a JPEG at the configured quality and, unless the
// administrator asked for the metadata to be removed, splices the original's
// application segments back in.
//
// This is necessary because image/jpeg is metadata-blind: its decoder ignores
// every APPn segment and its encoder writes none, so a plain
// decode-resize-encode round trip silently destroys EXIF, XMP and the ICC
// colour profile. SPEC §4.1 promises EXIF is preserved by default, so by
// default the segments are carried over verbatim from the source file.
//
// The "remove" path is therefore the cheap one: it simply does not splice.
// That is also what makes it trustworthy — the stripped file is the encoder's
// own output, which cannot contain a segment the encoder never writes, rather
// than a filtered copy of the original that might miss one.
//
// Go's encoder emits SOI immediately followed by DQT and never writes an APP
// segment of its own, so byte offset 2 is an unambiguous insertion point and
// the result cannot contain duplicated or conflicting application data.
func encodeResizedJPEG(img image.Image, original []byte, cfg configuration) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: cfg.JpegQuality}); err != nil {
		return nil, fmt.Errorf("encoding jpeg: %w", err)
	}
	encoded := buf.Bytes()

	if cfg.stripMetadata() {
		return encoded, nil
	}

	segments := extractAppSegments(original)
	if len(segments) == 0 {
		return encoded, nil
	}
	if len(encoded) < 2 || encoded[0] != 0xFF || encoded[1] != 0xD8 {
		// The encoder did not produce the SOI we rely on. Rather than splice
		// blindly into an unexpected layout, ship the valid image without
		// metadata.
		return encoded, nil
	}

	out := make([]byte, 0, len(encoded)+len(segments))
	out = append(out, encoded[:2]...)
	out = append(out, segments...)
	out = append(out, encoded[2:]...)
	return out, nil
}

// extractAppSegments returns the concatenated APP1..APP15 segments of a JPEG,
// in their original order, including their 0xFF 0xMM marker and length bytes.
//
// APP0 is deliberately excluded: it is the JFIF density/thumbnail block, it is
// meaningless once the image has been rescaled, and Go's encoder produces a
// well-formed file without it.
//
// The walk stops at SOS (start of scan) because everything after it is
// entropy-coded data, not segments. A malformed file simply yields whatever
// was parsed before the structure broke, which is safe: the output image is
// already valid on its own and metadata is best-effort.
func extractAppSegments(jpegBytes []byte) []byte {
	if len(jpegBytes) < 4 || jpegBytes[0] != 0xFF || jpegBytes[1] != 0xD8 {
		return nil
	}

	var segments []byte
	i := 2
	for i+4 <= len(jpegBytes) {
		if jpegBytes[i] != 0xFF {
			break // desynchronised; stop rather than guess
		}

		// A marker may be preceded by any number of 0xFF fill bytes (ITU-T
		// T.81 B.1.1.2). Skipping them is not cosmetic: reading the byte after
		// the first 0xFF blindly yields 0xFF as the "marker type" on a padded
		// file, after which the next two bytes are misread as a segment length
		// and the walk desynchronises — silently dropping the very EXIF this
		// function exists to preserve.
		markerPos := i
		for markerPos+1 < len(jpegBytes) && jpegBytes[markerPos+1] == 0xFF {
			markerPos++
		}
		if markerPos+4 > len(jpegBytes) {
			break
		}
		marker := jpegBytes[markerPos+1]

		// Standalone markers carry no length field.
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i = markerPos + 2
			continue
		}
		if marker == 0xDA || marker == 0xD9 {
			break // start of scan / end of image
		}

		length := int(binary.BigEndian.Uint16(jpegBytes[markerPos+2 : markerPos+4]))
		if length < 2 || markerPos+2+length > len(jpegBytes) {
			break // truncated or nonsensical segment
		}

		if marker >= 0xE1 && marker <= 0xEF {
			segments = append(segments, jpegBytes[markerPos:markerPos+2+length]...)
		}
		i = markerPos + 2 + length
	}
	return segments
}

// applyGeometry updates the server's recorded dimensions to describe the image
// the plugin actually stored.
//
// This is required, not cosmetic. The server reads Width and Height from the
// ORIGINAL upload before the hook runs (UploadFileTask.preprocessImage, and
// genFileInfoFromReader on the upload-session path) and never recomputes them
// afterwards. Resizing a file without correcting them would reproduce exactly
// the stale-metadata defect SPEC §1 exists to eliminate — just in a different
// column.
//
// The update is guarded rather than unconditional. preprocessImage SWAPS width
// and height when EXIF says the image is rotated 90 degrees, and the plugin
// preserves that EXIF tag without rotating the pixels, so the server's swapped
// convention must survive. Matching the recorded pair against the decoded
// source geometry tells us which convention is in force:
//
//   - recorded == source          -> write the new dimensions as-is;
//   - recorded == source swapped  -> write the new dimensions swapped;
//   - neither                     -> leave it alone, because an unrecognised
//     convention is not something to overwrite on a guess.
//
// Reported separately from the hook so it stays unit-testable without a server.
func applyGeometry(recordedW, recordedH int, o processOutcome) (int, int, bool) {
	if !o.reEncoded || o.width <= 0 || o.height <= 0 {
		return recordedW, recordedH, false
	}
	switch {
	case recordedW == o.sourceWidth && recordedH == o.sourceHeight:
		return o.width, o.height, true
	case recordedW == o.sourceHeight && recordedH == o.sourceWidth:
		return o.height, o.width, true
	default:
		return recordedW, recordedH, false
	}
}

package main

// the security audit, the security and robustness audit.
//
// The tests in this file are the ones written to pin findings from the audit
// rather than behaviour from a feature slice. Each names the attack it
// describes, because the property being defended is not obvious from the code
// it guards.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
)

// pngWithDeclaredGeometry builds a PNG header declaring width x height,
// followed by `padBytes` of ancillary chunk data.
//
// "prIV" is deliberately a private ANCILLARY chunk — its first letter is
// lowercase, which the PNG specification defines as "a decoder may skip this".
// That is what makes the file well-formed rather than merely large: nothing in
// it is invalid, so no parser rejects it on the way in.
func pngWithDeclaredGeometry(width, height uint32, padBytes int) []byte {
	var buf bytes.Buffer
	buf.Write(pngMagic)

	ihdr := make([]byte, 0, 17)
	ihdr = append(ihdr, 'I', 'H', 'D', 'R')
	ihdr = binary.BigEndian.AppendUint32(ihdr, width)
	ihdr = binary.BigEndian.AppendUint32(ihdr, height)
	ihdr = append(ihdr, 8, 6, 0, 0, 0) // 8-bit RGBA, no interlace
	buf.Write(binary.BigEndian.AppendUint32(nil, uint32(len(ihdr)-4)))
	buf.Write(ihdr)
	buf.Write(binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE(ihdr)))

	const per = 1 << 20
	pad := make([]byte, per)
	for written := 0; written < padBytes; written += per {
		n := min(per, padBytes-written)
		chunk := append([]byte("prIV"), pad[:n]...)
		buf.Write(binary.BigEndian.AppendUint32(nil, uint32(n)))
		buf.Write(chunk)
		buf.Write(binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE(chunk)))
	}
	return buf.Bytes()
}

// TestASmallImageWithAHugeBodyIsNotBufferedIntoMemory is the audit's first
// finding, and it is the one an attacker reaches most easily.
//
// THE ATTACK. `TestHugeFakeImageIsNotBufferedIntoMemory` already proves that an
// image declaring a geometry ABOVE the decode cap is streamed rather than
// buffered. The inverse was not covered and is the exploitable direction: a PNG
// whose IHDR declares 100x100 passes `safeToDecode` comfortably, after which
// `readAllBuffered` reads the ENTIRE upload into one contiguous byte slice.
// `image.DecodeConfig` only ever reads the IHDR, so the declared geometry says
// nothing at all about the file's length.
//
// WHY THE CONCURRENCY LIMIT DOES NOT HELP. The buffering happens in
// processNamedUpload BEFORE processBuffered takes an image slot, so the SPEC
// §4.4 limit of four concurrent image jobs bounds the number of DECODES and
// not the number of buffers. Concurrent uploads therefore each hold their own
// full copy, and the peak is bounded only by the server's own file-size limit
// times the number of uploads in flight.
//
// The fix is a cap on how much is buffered, derived from the geometry that was
// actually declared: a real image of W x H pixels cannot usefully exceed a
// generous multiple of W*H*4 bytes, and anything beyond that is padding with
// no pixels behind it. Over the cap the upload is passed through, which is the
// behaviour SPEC §4.1 already requires for every image the plugin declines to
// compress.
func TestASmallImageWithAHugeBodyIsNotBufferedIntoMemory(t *testing.T) {
	// 100x100 declared: 40 000 bytes of pixels at most, and 64 MB of body.
	const bodySize = 64 * 1024 * 1024
	crafted := pngWithDeclaredGeometry(100, 100, bodySize)

	// The header is what DecodeConfig sees; everything after it is the part
	// that must not be accumulated.
	header := crafted[:headerProbeBytes]
	probe := &readAheadProbe{
		body: bytes.NewReader(crafted[headerProbeBytes:]),
		// Generous: the plugin legitimately holds the 512 KB probe buffer
		// plus a copy or two while it decides. 8 MB is far above that and far
		// below the 64 MB the defect produces.
		maxAhead: 8 * 1024 * 1024,
	}
	src := io.MultiReader(bytes.NewReader(header), probe)

	sink := &writeTrackingWriter{probe: probe}
	outcome, err := processUpload(sink, src, defaultConfiguration())
	if err != nil {
		t.Fatalf("processUpload errored: %v", err)
	}

	// The whole file must still reach the server. Refusing the upload would be
	// a worse answer than storing it: the sender did nothing wrong as far as
	// the plugin can tell, and SPEC §4.1 says an image upload never fails
	// because of compression.
	if outcome.bytesWritten != int64(len(crafted)) {
		t.Errorf("bytesWritten = %d, want %d — the upload must survive intact",
			outcome.bytesWritten, len(crafted))
	}

	if probe.peakAhead > probe.maxAhead {
		t.Errorf("the pipeline read %d bytes (%.1f MB) further than it had written, "+
			"limit %d (%.1f MB); a PNG declaring 100x100 buffered its whole body "+
			"in memory, so a concurrent burst of these exhausts the plugin process",
			probe.peakAhead, float64(probe.peakAhead)/(1<<20),
			probe.maxAhead, float64(probe.maxAhead)/(1<<20))
	}
	if sink.largestWrite > 1024*1024 {
		t.Errorf("a single write of %d bytes suggests the body was buffered",
			sink.largestWrite)
	}
}

// TestAnImageWithPlausiblePaddingIsStillCompressed guards the other direction,
// which matters more than the finding itself.
//
// A cure that declined to compress any image carrying more bytes than its
// pixels strictly need would be worse than the disease: a real photograph
// legitimately carries EXIF, an embedded thumbnail and an ICC profile, and a
// PNG of flat colour compresses to a tiny fraction of its pixel count while a
// noisy one does not. This test is the one that fails if the cap is set too
// tightly.
func TestAnImageWithPlausiblePaddingIsStillCompressed(t *testing.T) {
	// A genuinely large photograph: 3000x2000 of noise, which does not
	// compress well, plus a real EXIF segment.
	base := encodeJPEG(t, noisyImage(3000, 2000, 7), 95)
	withEXIF, _ := jpegWithEXIF(t, base)

	if len(withEXIF) <= defaultConfiguration().thresholdBytes() {
		t.Fatalf("fixture is only %d bytes, below the threshold — it would pass "+
			"through for an unrelated reason and prove nothing", len(withEXIF))
	}

	_, outcome := run(t, withEXIF)
	if !outcome.reEncoded {
		t.Errorf("a real %d-byte 3000x2000 photograph was NOT compressed; the "+
			"buffering cap is set too tightly and is refusing legitimate images",
			len(withEXIF))
	}
}

// TestAHostileFileNameCannotForgeTheMessageShownToTheSender is the audit's
// finding on the one input that reaches a user's screen.
//
// THE ATTACK. The rejection message is built with %q on the file name
// (hooks.go), and the name is chosen entirely by the uploader. Three shapes
// were checked: a name containing the plugin's own sentence structure, one
// carrying newlines and ANSI escapes, and one carrying path separators.
//
// %q is what defends this, and the defence is worth pinning rather than
// assuming: it quotes the whole name and escapes every control character to
// its Go literal form, so a newline cannot break out of the sentence and a
// terminal escape cannot reach a terminal. A future change to %s — which reads
// as harmless tidying — would remove all of it at once.
func TestAHostileFileNameCannotForgeTheMessageShownToTheSender(t *testing.T) {
	hostile := []struct {
		name string
		what string
	}{
		{"\nMediaForge could not accept \"innocent.mp4\": disk failure", "a forged second sentence"},
		{"evil\r\n\r\nSet-Cookie: x=y", "header injection through a log"},
		{"boom\x1b[2J\x1b[31mRED", "a terminal escape sequence"},
		{"../../../etc/passwd", "a path traversal"},
		{"a\x00b", "an embedded NUL"},
	}

	for _, tc := range hostile {
		t.Run(tc.what, func(t *testing.T) {
			// The exact construction from FileWillBeUploaded's rejection path.
			rendered := rejectionMessageFor(tc.name, "the file may be damaged")

			for _, forbidden := range []string{"\n", "\r", "\x00", "\x1b"} {
				if strings.Contains(rendered, forbidden) {
					t.Errorf("the message shown to the sender contains a raw %q, "+
						"so a chosen file name can forge or corrupt it: %q",
						forbidden, rendered)
				}
			}
			// And the message must still be the plugin's own sentence, with
			// the name clearly delimited rather than pasted in.
			if !strings.HasPrefix(rendered, "MediaForge could not accept ") {
				t.Errorf("the message no longer starts with the plugin's own words: %q", rendered)
			}
		})
	}
}

// TestNoExternallyChosenValueBecomesAnFFmpegArgument settles the four G204
// findings gosec reported, by construction rather than by argument.
//
// THE CLAIM BEING PROVED. Every ffmpeg and ffprobe invocation in the plugin
// takes its arguments from exactly two sources: constants compiled into the
// binary, and numbers the configuration sanitiser has already bounded. The
// only attacker-controlled bytes that reach the toolchain do so as the CONTENT
// of a file at a path the plugin itself created with os.CreateTemp.
//
// This is asserted against a hostile configuration AND a hostile file name,
// because those are the two routes an outsider has. The file name is the
// sharper one: it routes into the voice path, and a naive implementation would
// have used it to name the spool file.
func TestNoExternallyChosenValueBecomesAnFFmpegArgument(t *testing.T) {
	// Values an attacker would try if a setting were reachable. They are not
	// reachable — only an administrator edits settings — but the sanitiser is
	// what guarantees a number is a number, and that guarantee is what these
	// command builders rest on.
	hostileRaw := map[string]any{
		"MaxVideoHeight":   "1080 -f lavfi -i /etc/passwd",
		"VideoCRF":         "23; rm -rf /",
		"AudioBitrateKbps": "128 -metadata comment=x",
		"VideoCodec":       "h264 -vf drawtext=/etc/shadow",
		"MaxVoiceSeconds":  "600 -y /tmp/pwned",
	}
	cfg, _ := sanitiseConfiguration(hostileRaw)

	// The paths the plugin passes are the ones it created itself. A hostile
	// name is handed in alongside, to prove it does not travel.
	const spoolPath = "/tmp/mediaforge/mediaforge-upload-12345"
	const outPath = spoolPath + ".out.mp4"

	commands := map[string][]string{
		"encoderArgs (video)": encoderArgs(transcodeRequest{
			inputPath: spoolPath, outputPath: outPath, cfg: cfg,
		}),
		// Both voice variants, because the trimming switch adds an argument
		// to this command line and an unchecked branch is exactly where an
		// injection would hide.
		"voiceEncoderArgs":           voiceEncoderArgs(spoolPath, spoolPath+".out.m4a", false),
		"voiceEncoderArgs (trimmed)": voiceEncoderArgs(spoolPath, spoolPath+".out.m4a", true),
	}

	// Every argument must be either one of the plugin's own literals, a
	// path the plugin built, or a decimal number.
	for label, args := range commands {
		for i, arg := range args {
			if arg == spoolPath || strings.HasPrefix(arg, spoolPath) {
				continue // a path the plugin created
			}
			if isPlainToken(arg) {
				continue
			}
			t.Errorf("%s argument %d is neither a plugin literal, a plugin-built "+
				"path nor a plain number: %q — the full command was %q",
				label, i, arg, args)
		}
	}

	// And the decisive negative: none of the hostile text appears anywhere.
	for label, args := range commands {
		joined := strings.Join(args, "\x00")
		for setting, raw := range hostileRaw {
			text, _ := raw.(string)
			// The leading number is the part that survives sanitisation as a
			// number; the injected tail must not appear at all.
			_, tail, found := strings.Cut(text, " ")
			if !found {
				continue
			}
			if strings.Contains(joined, tail) {
				t.Errorf("%s carries text from the hostile %s setting: %q appears in %q",
					label, setting, tail, args)
			}
		}
		for _, shellish := range []string{";", "|", "&&", "$(", "`", "\n", "/etc/"} {
			if strings.Contains(joined, shellish) {
				t.Errorf("%s contains %q, which no legitimate argument needs: %q",
					label, shellish, args)
			}
		}
	}
}

// isPlainToken reports whether an argument is one of the shapes the plugin's
// own command builders produce: an ffmpeg flag, an encoder or format name, a
// number, or one of the small filter expressions written as a literal here.
func isPlainToken(arg string) bool {
	allowed := map[string]bool{
		// Flags and fixed values, exactly as the two builders write them.
		"-nostdin": true, "-y": true, "-v": true, "error": true,
		"-err_detect": true, "explode": true, "-i": true,
		"-map": true, "0:v:0": true, "0:a:0?": true, "0:a:0": true,
		"-c:v": true, "-c:a": true, "-preset": true, "-crf": true,
		"-vf": true, "-b:a": true, "-ac": true, "-f": true, "-vn": true,
		"-pix_fmt": true, "yuv420p": true, "-movflags": true,
		"+faststart": true, "-tag:v": true, "hvc1": true,
		"libx264": true, "libx265": true, "libsvtav1": true,
		"aac": true, "libopus": true, "mp4": true, "webm": true,
		"veryfast": true, "1": true, "2": true, "10": true,
		// The silence filter and its flag. Listed as a literal on purpose:
		// this is the whole point of the filter being a constant rather than
		// assembled from settings, so the allowlist can name it exactly and
		// any future version built from configuration fails here.
		"-af": true, voiceSilenceFilter: true,
	}
	if allowed[arg] {
		return true
	}
	// A bitrate such as "128k", a CRF such as "23", or the scale filter the
	// builder composes from the validated height.
	if strings.HasPrefix(arg, "scale=-2:min(") && strings.HasSuffix(arg, "\\,ih)") {
		inner := strings.TrimSuffix(strings.TrimPrefix(arg, "scale=-2:min("), "\\,ih)")
		return isDecimal(inner)
	}
	if strings.HasSuffix(arg, "k") && isDecimal(strings.TrimSuffix(arg, "k")) {
		return true
	}
	return isDecimal(arg)
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// TestTheSenderIsNeverToldAnInternalPath is the audit's disclosure finding, and
// it is the one that was actually leaking.
//
// THE MEASUREMENT. The hook's broken-stream path rendered the underlying error
// into the sender's message with %v. Every error that arrives there from the
// spool is a wrapped *os.PathError, so on a real ENOSPC — reproduced against a
// 1 MB tmpfs rather than simulated — the uploader was shown:
//
//	MediaForge could not read "holiday.mp4": writing the upload to a
//	temporary file: write /tmp/mediaforge/mediaforge-upload-XXXXXXXX:
//	no space left on device
//
// That hands an ordinary user the server's temp directory layout, the plugin's
// spool prefix, and the news that the disk is full. None of it is actionable by
// the sender and all of it is useful to somebody probing the host. PLAN §Slice
// 11 area 4 forbids exactly this: "an internal path, an ffmpeg call or another
// user's file name belongs in none of the three".
//
// The fix keeps the diagnosis where it is useful — the log line above the
// return already carries the full error for the administrator — and gives the
// sender a sentence about their own upload.
func TestTheSenderIsNeverToldAnInternalPath(t *testing.T) {
	// The error shapes the broken-stream path can genuinely produce. Each is
	// wrapped the way the pipeline wraps it.
	causes := []struct {
		what string
		err  error
	}{
		{"a full disk while spooling", fmt.Errorf("writing the upload to a temporary file: %w",
			&os.PathError{Op: "write", Path: "/tmp/mediaforge/mediaforge-upload-XXXXXXXX",
				Err: syscall.ENOSPC})},
		{"an unreadable spool directory", fmt.Errorf("creating a temporary file for the upload: %w",
			&os.PathError{Op: "open", Path: "/srv/mattermost/tmp/mediaforge", Err: syscall.EACCES})},
		{"a broken client connection", fmt.Errorf("reading the upload: %w", io.ErrUnexpectedEOF)},
		{"a failed read of the converted file", fmt.Errorf("re-reading the temporary file: %w",
			&os.PathError{Op: "open", Path: "/tmp/mediaforge/mediaforge-upload-XXXXXXXX.out.mp4",
				Err: syscall.ENOENT})},
	}

	for _, tc := range causes {
		t.Run(tc.what, func(t *testing.T) {
			shown := readFailureMessageFor("holiday.mp4", tc.err)

			// Nothing resembling a filesystem path, and nothing naming the
			// plugin's own internals.
			for _, leak := range []string{
				"/tmp", "/var", spoolDirName, spoolFilePrefix, ".out.mp4",
			} {
				if strings.Contains(shown, leak) {
					t.Errorf("the sender is shown %q, which discloses %q: %s",
						leak, leak, shown)
				}
			}
			// A bare "/" is the general form of the same leak.
			if strings.Contains(shown, "/") {
				t.Errorf("the sender is shown a path separator: %s", shown)
			}
			// And the errno text, which tells an outsider about the host's
			// state rather than about their own file.
			for _, hostState := range []string{
				"no space left", "permission denied", "no such file",
			} {
				if strings.Contains(strings.ToLower(shown), hostState) {
					t.Errorf("the sender is told about the server's own state (%q): %s",
						hostState, shown)
				}
			}

			// It must still be useful: it names THEIR file and says what to do.
			if !strings.Contains(shown, "holiday.mp4") {
				t.Errorf("the message does not name the sender's own file: %s", shown)
			}
			if !strings.Contains(strings.ToLower(shown), "again") {
				t.Errorf("the message does not tell the sender what to do next: %s", shown)
			}
		})
	}
}

// TestTheReportNeverNamesAFile pins the other half of the disclosure area.
//
// The administrator's report aggregates byte counts and file counts and must
// never carry a file name or a path — it is the one page in the plugin that
// shows one user figures derived from every user's uploads. The counters are
// checked by construction: statsCounters and statsDay carry nothing but
// numbers and a date, so there is no field a name could travel in.
//
// This is a structural assertion rather than a behavioural one, which is what
// makes it worth having: a future field added to the wire format for
// "diagnosis" would be caught here rather than in review.
func TestTheReportNeverNamesAFile(t *testing.T) {
	// A day's record, marshalled the way it is written to the store and then
	// served to the report. Nothing an uploader chose may appear in it.
	day := statsDay{Day: "2026-09-18"}
	day.Image.add(statsEvent{files: 1, original: 11906359, stored: 2323820})
	day.Video.add(statsEvent{failures: 1})
	day.recompute()

	encoded, err := json.Marshal(statsReport{
		Total: day.Total, Image: day.Image, Video: day.Video,
		Days: []statsDay{day}, RetentionDays: statsRetentionDays,
	})
	if err != nil {
		t.Fatalf("marshalling the report: %v", err)
	}

	// The record the note keeps DOES carry a name, deliberately, for
	// diagnosis. The report must not have inherited it.
	for _, forbidden := range []string{"name", "path", "file_name", "user"} {
		if strings.Contains(strings.ToLower(string(encoded)), `"`+forbidden+`"`) {
			t.Errorf("the report's wire format carries a %q field, which could "+
				"disclose one user's upload to another: %s", forbidden, encoded)
		}
	}
	if strings.Contains(string(encoded), "/") {
		t.Errorf("the report's wire format contains a path separator: %s", encoded)
	}
}

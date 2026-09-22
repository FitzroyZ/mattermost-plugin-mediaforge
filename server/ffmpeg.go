package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// spoolDirName is the subdirectory of the system temp directory that holds
// uploads while they are being converted.
//
// Everything lives under one known directory so leftovers are findable — by an
// administrator wondering where the disk went, and by the zero-state check at
// the end of the test suite.
const spoolDirName = "mediaforge"

// spoolFilePrefix is the filename prefix of every spooled upload.
//
// It doubles as the marker the test suite matches against when it looks for
// leaked processes: the prefix appears in the ffmpeg command line, so a
// transcode started by this plugin can be told apart from an unrelated ffmpeg
// the administrator or another service is running. Killing by program name
// alone would be reckless.
const spoolFilePrefix = "mediaforge-upload-"

// probeTimeout bounds the ffprobe call that inspects an upload.
//
// ffprobe only reads the container header, so it finishes in milliseconds on
// anything well-formed. The budget exists for the pathological case: a file
// crafted so the demuxer keeps searching for a stream it will never find.
const probeTimeout = 20 * time.Second

// mediaTools records where the external programs are, or why they are missing.
//
// Availability is resolved once when the configuration is loaded, not per
// upload. SPEC §5 requires the plugin to notice the absence of ffmpeg and
// disable the feature with an admin-visible message rather than failing upload
// after upload, and a per-upload PATH lookup would be exactly that second
// behaviour.
type mediaTools struct {
	// FFmpegPath and FFprobePath are absolute paths, empty when not found.
	FFmpegPath  string
	FFprobePath string
	// Reason explains, in words an administrator can act on, why video
	// processing is unavailable. Empty when everything was found.
	Reason string
}

// available reports whether both programs were found.
func (t mediaTools) available() bool {
	return t.FFmpegPath != "" && t.FFprobePath != ""
}

// detectMediaTools looks up ffmpeg and ffprobe.
//
// The lookup function is injected so the "not installed" behaviour can be
// tested on a machine that does have ffmpeg. Testing it by uninstalling ffmpeg
// is not a test anyone would run twice.
func detectMediaTools(lookPath func(string) (string, error)) mediaTools {
	var tools mediaTools
	var missing []string

	if path, err := lookPath("ffmpeg"); err == nil {
		tools.FFmpegPath = path
	} else {
		missing = append(missing, "ffmpeg")
	}
	if path, err := lookPath("ffprobe"); err == nil {
		tools.FFprobePath = path
	} else {
		missing = append(missing, "ffprobe")
	}

	if len(missing) > 0 {
		tools.Reason = fmt.Sprintf(
			"%s not found in the server's PATH, so video compression is switched off "+
				"and videos are stored exactly as they were sent",
			strings.Join(missing, " and "))
	}
	return tools
}

// uploadRejection is an error that is meant to be shown to the person who
// uploaded the file.
//
// The distinction matters. Most errors in this plugin are internal and must
// never fail an upload — compression is an optimisation. A transcode failure is
// the one deliberate exception (SPEC §4.2), and it carries a sentence written
// for the sender, not a copy of ffmpeg's diagnostics.
type uploadRejection struct {
	// message is what the user sees.
	message string
	// cause is what goes in the server log.
	cause error
}

func (r *uploadRejection) Error() string {
	if r.cause != nil {
		return r.message + ": " + r.cause.Error()
	}
	return r.message
}

func (r *uploadRejection) Unwrap() error { return r.cause }

// rejectUpload builds a rejection carrying a user-facing sentence.
func rejectUpload(message string, cause error) error {
	return &uploadRejection{message: message, cause: cause}
}

// asRejection reports whether err is (or wraps) an uploadRejection.
func asRejection(err error, target **uploadRejection) bool {
	return errors.As(err, target)
}

// spoolFile is an upload written to disk so ffmpeg can seek in it.
//
// The hook receives an io.Reader, and MP4 is not a streamable format for
// reading: the moov atom that indexes the file may sit at the very end, which
// is where every phone and every camera puts it. ffmpeg would have to buffer
// the whole thing itself, and it cannot do that reliably from a pipe. Writing
// the upload out once, in bounded chunks, is the honest solution.
type spoolFile struct {
	// path is the file on disk.
	path string
	// size is the number of bytes written.
	size int64
	// truncated is true when the upload exceeded the configured cap and the
	// spool was abandoned. The caller must then pass the upload through: a
	// resource limit is not a reason to lose someone's video.
	truncated bool
}

// remove deletes the spool file. It is safe to call more than once.
func (s *spoolFile) remove() {
	if s == nil || s.path == "" {
		return
	}
	_ = os.Remove(s.path)
	s.path = ""
}

// ensureSpoolDir creates the directory spool files live in.
func ensureSpoolDir(dir string) (string, error) {
	if dir == "" {
		dir = filepath.Join(os.TempDir(), spoolDirName)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating the temporary directory %s: %w", dir, err)
	}
	return dir, nil
}

// transcodeRequest is one conversion job.
type transcodeRequest struct {
	// inputPath is the spooled upload.
	inputPath string
	// outputPath is where ffmpeg writes the result.
	outputPath string
	// cfg carries the codec, quality and resolution settings.
	cfg configuration
}

// videoProfile describes one selectable output format.
//
// Keeping the three profiles in one table is what keeps the System Console text
// and the encoder arguments from drifting apart: the help text promises "plays
// on iPhone", and the arguments are right next to that promise.
type videoProfile struct {
	// videoCodec and audioCodec are ffmpeg encoder names.
	videoCodec string
	audioCodec string
	// format is the ffmpeg muxer name.
	format string
	// preset is the encoder's speed/efficiency knob.
	//
	// It belongs to the profile rather than being one shared value, because
	// the encoders do not agree on what a preset is: x264 and x265 take names
	// like "veryfast", while SVT-AV1 takes a NUMBER from 0 to 13. Passing
	// x264's vocabulary to SVT-AV1 makes ffmpeg refuse to start, which turns
	// every AV1 upload into a rejection that looks like a damaged file.
	preset string
	// extraArgs are format-specific flags.
	extraArgs []string
}

var videoProfiles = map[string]videoProfile{
	videoCodecH264: {
		videoCodec: "libx264",
		audioCodec: "aac",
		format:     "mp4",
		preset:     "veryfast",
		// yuv420p is what makes the file playable on hardware decoders;
		// without it a 4:4:4 source produces a file many devices refuse.
		extraArgs: []string{"-pix_fmt", "yuv420p", "-movflags", "+faststart"},
	},
	videoCodecH265: {
		videoCodec: "libx265",
		audioCodec: "aac",
		format:     "mp4",
		preset:     "veryfast",
		// hvc1 rather than the default hev1 tag: Apple's players reject hev1
		// in MP4, and "plays on iPhone" is the whole point of offering a
		// labelled choice.
		extraArgs: []string{"-pix_fmt", "yuv420p", "-tag:v", "hvc1", "-movflags", "+faststart"},
	},
	videoCodecAV1: {
		videoCodec: "libsvtav1",
		audioCodec: "libopus",
		format:     "webm",
		// SVT-AV1 presets are numeric, 0 (slowest) to 13 (fastest). 10 is the
		// fast end of usable and is chosen for the same reason as x264's
		// "veryfast": the sender is waiting and the server's write timeout is
		// running. Even so AV1 is much slower than H.264, which is why the
		// System Console text warns about it.
		preset:    "10",
		extraArgs: []string{"-pix_fmt", "yuv420p"},
	},
}

// encoderArgs builds the ffmpeg command line for one request.
//
// Two decisions are worth naming:
//
//   - The scale filter never enlarges. `min(<limit>,ih)` caps the height at the
//     configured value and leaves a smaller video alone, and `-2` on the width
//     keeps the aspect ratio while rounding to an even number, which every
//     4:2:0 encoder requires.
//   - The preset is fixed at the fast end. This is a synchronous transcode with
//     the sender waiting and the server's own write timeout running; a slower
//     preset buys a few per cent of file size for a multiple of the wall time.
//     The measurement behind that is recorded in PLAN §video compression.
func encoderArgs(req transcodeRequest) []string {
	profile, ok := videoProfiles[req.cfg.VideoCodec]
	if !ok {
		profile = videoProfiles[videoCodecH264]
	}

	args := []string{
		// No terminal is attached, and ffmpeg must never wait for one.
		"-nostdin",
		"-y",
		"-v", "error",
		// Refuse to start on a file the demuxer already distrusts, instead of
		// producing a partial video and reporting success.
		"-err_detect", "explode",
		"-i", req.inputPath,
		// Video and audio only. Subtitle and data streams may be unmappable
		// into the target container, and a rejected upload over an unsupported
		// subtitle track would be absurd.
		"-map", "0:v:0",
		"-map", "0:a:0?",
		"-c:v", profile.videoCodec,
		"-preset", profile.preset,
		"-crf", strconv.Itoa(req.cfg.VideoCRF),
		"-vf", fmt.Sprintf("scale=-2:min(%d\\,ih)", req.cfg.MaxVideoHeight),
		"-c:a", profile.audioCodec,
		"-b:a", strconv.Itoa(req.cfg.AudioBitrateKbps) + "k",
		"-ac", "2",
	}
	args = append(args, profile.extraArgs...)
	args = append(args, "-f", profile.format, req.outputPath)
	return args
}

// runTranscode converts one spooled file, killing ffmpeg if it overruns.
//
// The process group handling is the part that earns its keep. ffmpeg spawns
// worker threads, and killing only the parent PID can leave the group running
// with the output file still open — the orphan this slice is required to rule
// out. Starting the child in its own process group and signalling the whole
// group is what makes the kill complete. cmd.Wait then reaps it, so no zombie
// is left either.
func runTranscode(req transcodeRequest, tools mediaTools, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.Command(tools.FFmpegPath, encoderArgs(req)...)
	isolateProcessGroup(cmd)
	// ffmpeg reads stdin when it is a terminal; give it a closed one so it can
	// never block waiting for input that is not coming.
	cmd.Stdin = nil

	var stderr strings.Builder
	cmd.Stderr = limitedWriter{w: &stderr, limit: 4096}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting ffmpeg: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("ffmpeg failed: %w (%s)", err, strings.TrimSpace(stderr.String()))
		}
		return nil

	case <-ctx.Done():
		// Ask the whole process group to stop, then insist.
		//
		// SIGTERM first so ffmpeg can close its output file; SIGKILL after a
		// short grace period, because a wedged decoder will not answer either
		// way and the upload is already lost.
		signalProcessGroup(cmd, terminateSignal)

		select {
		case <-done:
		case <-time.After(terminateGracePeriod):
			signalProcessGroup(cmd, killSignal)

			// Every wait here is bounded, including the one after SIGKILL.
			//
			// SIGKILL cannot be ignored, so the process itself is certainly
			// gone — but cmd.Wait() does not only reap the process. Because
			// cmd.Stderr is not an *os.File, os/exec copies stderr through a
			// pipe on a goroutine, and Wait also waits for THAT. The goroutine
			// finishes when the pipe reaches EOF, which needs every holder of
			// the write end to be gone. A grandchild that called setsid has
			// escaped the process group, survives the kill, and holds the pipe
			// open indefinitely.
			//
			// Reproduced before this guard existed: Wait had not returned 8 s
			// after the group was killed. Unbounded, that would hang the upload
			// forever and hold a server request open — strictly worse than the
			// timeout it is meant to enforce. The process is reaped if it can
			// be; if not, giving up is the correct answer, because the caller
			// is about to reject the upload either way.
			select {
			case <-done:
			case <-time.After(reapGracePeriod):
			}
		}
		return errTranscodeTimeout
	}
}

// terminateGracePeriod is how long ffmpeg is given to shut down cleanly after
// SIGTERM before it is killed outright.
const terminateGracePeriod = 2 * time.Second

// reapGracePeriod bounds the wait after SIGKILL. See runTranscode for why a
// bound is needed at all when the signal cannot be ignored.
const reapGracePeriod = 5 * time.Second

// errTranscodeTimeout marks a conversion that ran out of its budget.
var errTranscodeTimeout = errors.New("the transcode exceeded its time budget")

// limitedWriter keeps at most limit bytes.
//
// ffmpeg can emit a diagnostic per damaged frame, which on a badly corrupted
// file means megabytes of near-identical lines. The first few kilobytes say
// everything a log reader needs.
type limitedWriter struct {
	w     *strings.Builder
	limit int
}

func (l limitedWriter) Write(p []byte) (int, error) {
	if remaining := l.limit - l.w.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		l.w.Write(p)
	}
	// Report the full length: ffmpeg must not see a short write and stop.
	return len(p), nil
}

// probeResult is what ffprobe could tell us about an upload.
type probeResult struct {
	// hasVideo is true when the file carries at least one video stream that is
	// not a still image.
	hasVideo bool
	// frameRate is the stream's average frame rate in frames per second, 0
	// when the container did not state one.
	frameRate float64
	// width, height and durationSeconds describe that stream. Zero when
	// unknown — a container may legitimately omit the duration.
	width           int
	height          int
	durationSeconds float64
}

// bitsPerPixel is how many bits the source spends on each pixel of each frame.
//
// It is the one number that says whether re-encoding a video can pay off, and
// it is readable from the header alone. A phone or a screen recorder spends
// lavishly; anything that has already been through an encoder at sensible
// settings spends little, and re-encoding it makes it BIGGER while costing the
// same time. See videoIsAlreadyEfficient.
func (p probeResult) bitsPerPixel(sizeBytes int64, frameRate float64) float64 {
	pixels := float64(p.width) * float64(p.height)
	if pixels <= 0 || p.durationSeconds <= 0 || frameRate <= 0 {
		return 0
	}
	bitsPerSecond := float64(sizeBytes) * 8 / p.durationSeconds
	return bitsPerSecond / (pixels * frameRate)
}

// probeVideo asks ffprobe what the spooled file actually is.
//
// The magic bytes cannot answer this. An `.m4a` voice memo, a still image in an
// MP4 wrapper and a phone recording all share the same ftyp header, and only
// one of them is a video. Guessing from the extension would be worse: the hook
// receives whatever the user named the file.
func probeVideo(path string, tools mediaTools) (probeResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, tools.FFprobePath,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_name,codec_type,width,height,nb_frames,avg_frame_rate",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1",
		path)
	isolateProcessGroup(cmd)

	out, err := cmd.Output()
	if err != nil {
		return probeResult{}, fmt.Errorf("ffprobe could not read the file: %w", err)
	}

	var res probeResult
	var codecName, frameRate string
	var frames int
	for line := range strings.SplitSeq(string(out), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "codec_type":
			res.hasVideo = res.hasVideo || value == "video"
		case "codec_name":
			codecName = value
		case "width":
			res.width, _ = strconv.Atoi(value)
		case "height":
			res.height, _ = strconv.Atoi(value)
		case "nb_frames":
			frames, _ = strconv.Atoi(value)
		case "avg_frame_rate":
			frameRate = value
		case "duration":
			res.durationSeconds, _ = strconv.ParseFloat(value, 64)
		}
	}

	// A JPEG or PNG is reported as a one-frame "video stream" by ffprobe,
	// because that is how its decoders are organised. Those belong to the image
	// pipeline and must not be run through an encoder. A frame rate of 0/0 is
	// the other half of the same signal.
	if isStillImageCodec(codecName) || frames == 1 || frameRate == "0/0" {
		res.hasVideo = false
	}
	res.frameRate = parseFrameRate(frameRate)

	return res, nil
}

// parseFrameRate turns ffprobe's "30000/1001" rational into frames per second.
func parseFrameRate(value string) float64 {
	numerator, denominator, found := strings.Cut(value, "/")
	if !found {
		fps, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return 0
		}
		return fps
	}
	num, numErr := strconv.ParseFloat(strings.TrimSpace(numerator), 64)
	den, denErr := strconv.ParseFloat(strings.TrimSpace(denominator), 64)
	if numErr != nil || denErr != nil || den == 0 {
		return 0
	}
	return num / den
}

// efficientBitsPerPixel is the threshold below which re-encoding a video is
// expected to make it larger rather than smaller.
//
// The number is measured on the reference container, not chosen (PLAN §video compression).
// At the default CRF 23 the encoder itself produces roughly 0.10 bits per pixel
// per frame on ordinary material. A source already below that has nothing to
// give: measured, a 1.7 Mbit/s 1080p30 clip (0.028 bpp) came back at 155 % of
// its size, and a 0.5 Mbit/s 480p25 clip at 139 %. The threshold sits at half
// the encoder's own output so that only clearly-frugal sources are skipped and
// borderline ones are still attempted — the size comparison afterwards remains
// the final word either way.
const efficientBitsPerPixel = 0.05

// videoIsAlreadyEfficient reports whether transcoding this source is expected
// to be a waste.
//
// Why this check exists at all: without it, a long, already-compressed video is
// the worst case in the whole slice. It occupies every core for minutes, it
// produces a LARGER file that is then thrown away, and if it runs past the
// budget the upload is rejected outright — a file that never needed converting
// would fail to upload. Deciding from the header costs one ffprobe call.
//
// The check deliberately does not apply when the video will be scaled down:
// halving the height quarters the pixel count, so even a frugal source shrinks
// substantially and the transcode is worth doing.
func videoIsAlreadyEfficient(probed probeResult, sizeBytes int64, cfg configuration) bool {
	if probed.height > cfg.MaxVideoHeight {
		return false
	}
	bpp := probed.bitsPerPixel(sizeBytes, probed.frameRate)
	if bpp <= 0 {
		// The container did not say enough to judge. Attempting the transcode
		// is the safe answer: the size comparison afterwards still protects
		// against storing a larger file.
		return false
	}
	return bpp < efficientBitsPerPixel
}

// isStillImageCodec lists the "video" codecs that are really picture formats.
func isStillImageCodec(name string) bool {
	switch name {
	case "mjpeg", "png", "bmp", "gif", "tiff", "webp", "apng":
		return true
	default:
		return false
	}
}

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
)

// ---------------------------------------------------------------- fixtures

// ffmpegOrSkip returns the tools detected on this machine, skipping the test
// when ffmpeg is not installed.
//
// The "ffmpeg is missing" behaviour has its own test and does NOT depend on the
// machine actually lacking ffmpeg — it injects a failing lookup instead. This
// helper is only for the tests that need a real transcode.
func ffmpegOrSkip(t *testing.T) mediaTools {
	t.Helper()
	tools := detectMediaTools(exec.LookPath)
	if !tools.available() {
		t.Skipf("ffmpeg/ffprobe not installed on this machine: %s", tools.Reason)
	}
	return tools
}

// videoConfig is the default configuration with video enabled and the real
// tools attached.
func videoConfig(t *testing.T) configuration {
	t.Helper()
	cfg := defaultConfiguration()
	cfg.tools = ffmpegOrSkip(t)
	return cfg
}

// makeVideo renders a short test clip with ffmpeg and returns its bytes.
//
// The clip is encoded deliberately badly — ultrafast at a very low CRF — so it
// is far larger than it needs to be. That is what gives the transcode
// something real to improve, and it is also how a phone recording behaves: high
// bitrate, little effort spent on compression.
func makeVideo(t *testing.T, tools mediaTools, seconds int, size string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.mp4")
	cmd := exec.Command(tools.FFmpegPath,
		"-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%s:rate=25:duration=%d", size, seconds),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "10",
		"-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "128k",
		"-movflags", "+faststart", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating the fixture video failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the fixture video failed: %v", err)
	}
	return data
}

// corruptVideo damages the payload of a valid MP4 while leaving the container
// structure intact.
//
// This is the interesting kind of broken file. A completely scrambled file is
// not recognisable as a video at all, and the plugin is supposed to pass those
// through untouched rather than reject them. This one still announces an H.264
// stream, so the plugin genuinely commits to converting it — and then cannot.
// That is the case where rejection is the correct answer, and it is the case
// the slice promises to handle.
func corruptVideo(original []byte) []byte {
	damaged := append([]byte(nil), original...)
	from, to := len(damaged)*3/10, len(damaged)*7/10
	for i := from; i < to; i++ {
		damaged[i] = 0
	}
	return damaged
}

// probe reports the container duration and the first video stream's geometry.
func probe(t *testing.T, tools mediaTools, path string) (width, height int, duration float64) {
	t.Helper()
	run := func(args ...string) string {
		out, err := exec.Command(tools.FFprobePath, args...).Output()
		if err != nil {
			t.Fatalf("ffprobe %v failed: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	fields := strings.Split(run("-v", "error",
		"-select_streams", "v:0", "-show_entries", "stream=width,height",
		"-of", "csv=p=0", path), ",")
	if len(fields) < 2 {
		t.Fatalf("ffprobe reported no video stream for %s", path)
	}
	width, _ = strconv.Atoi(strings.TrimSpace(fields[0]))
	height, _ = strconv.Atoi(strings.TrimSpace(fields[1]))
	duration, _ = strconv.ParseFloat(run("-v", "error",
		"-show_entries", "format=duration", "-of", "csv=p=0", path), 64)
	return width, height, duration
}

// writeTemp puts bytes on disk so ffprobe can look at them.
func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing %s failed: %v", path, err)
	}
	return path
}

// ------------------------------------------------------------ the behaviour

// TestVideoIsTranscodedSmallerAndStillPlays is the slice's core promise.
//
// "Smaller" alone would be satisfied by truncating the file, so the check that
// matters is the one after it: ffprobe must read the stored bytes back as a
// video of the same length, and the byte count must match what the hook
// reported as written.
func TestVideoIsTranscodedSmallerAndStillPlays(t *testing.T) {
	cfg := videoConfig(t)
	original := makeVideo(t, cfg.tools, 3, "640x480")

	var stored bytes.Buffer
	outcome, err := processUpload(&stored, bytes.NewReader(original), cfg)
	if err != nil {
		t.Fatalf("the upload was rejected: %v", err)
	}

	if !outcome.reEncoded {
		t.Fatalf("the video was passed through instead of being transcoded (%d bytes in, %d out)",
			len(original), stored.Len())
	}
	if stored.Len() >= len(original) {
		t.Errorf("the stored video is not smaller: %d bytes in, %d bytes stored",
			len(original), stored.Len())
	}
	if outcome.bytesWritten != int64(stored.Len()) {
		t.Errorf("bytesWritten %d does not match the %d bytes actually written",
			outcome.bytesWritten, stored.Len())
	}

	// Playability, not file size: decode the stored bytes again.
	storedPath := writeTemp(t, "stored.mp4", stored.Bytes())
	w, h, duration := probe(t, cfg.tools, storedPath)
	if w <= 0 || h <= 0 {
		t.Errorf("the stored video reports a nonsensical geometry %dx%d", w, h)
	}
	if duration < 2.5 || duration > 3.5 {
		t.Errorf("the stored video is %.2f s long, but the source was 3 s — content was lost", duration)
	}
	if outcome.width != w || outcome.height != h {
		t.Errorf("the reported geometry %dx%d does not describe the stored file %dx%d",
			outcome.width, outcome.height, w, h)
	}
}

// TestOversizedVideoIsScaledDownToTheConfiguredHeight proves the resolution
// limit is applied and that the aspect ratio survives it.
func TestOversizedVideoIsScaledDownToTheConfiguredHeight(t *testing.T) {
	cfg := videoConfig(t)
	cfg.MaxVideoHeight = 240
	original := makeVideo(t, cfg.tools, 2, "640x480")

	var stored bytes.Buffer
	outcome, err := processUpload(&stored, bytes.NewReader(original), cfg)
	if err != nil {
		t.Fatalf("the upload was rejected: %v", err)
	}
	if !outcome.reEncoded {
		t.Fatalf("the video was passed through instead of being transcoded")
	}

	w, h, _ := probe(t, cfg.tools, writeTemp(t, "stored.mp4", stored.Bytes()))
	if h != 240 {
		t.Errorf("the stored video is %d px high, want exactly 240", h)
	}
	if w != 320 {
		t.Errorf("the stored video is %dx%d, want 320x240 — the aspect ratio was not kept", w, h)
	}
}

// TestSmallVideoIsNotScaledUp — the image pipeline refuses to enlarge, and
// video must behave the same way. Upscaling would waste space to invent
// detail that is not there.
func TestSmallVideoIsNotScaledUp(t *testing.T) {
	cfg := videoConfig(t)
	cfg.MaxVideoHeight = 1080
	original := makeVideo(t, cfg.tools, 2, "320x240")

	var stored bytes.Buffer
	if _, err := processUpload(&stored, bytes.NewReader(original), cfg); err != nil {
		t.Fatalf("the upload was rejected: %v", err)
	}

	w, h, _ := probe(t, cfg.tools, writeTemp(t, "stored.mp4", stored.Bytes()))
	if w != 320 || h != 240 {
		t.Errorf("a 320x240 video was stored as %dx%d — it must not be enlarged", w, h)
	}
}

// TestCorruptVideoIsRejectedAndNothingIsStored is the failure contract from
// SPEC §4.2: a video the plugin cannot convert fails the upload rather than
// being stored broken.
func TestCorruptVideoIsRejectedAndNothingIsStored(t *testing.T) {
	cfg := videoConfig(t)
	damaged := corruptVideo(makeVideo(t, cfg.tools, 3, "640x480"))

	var stored bytes.Buffer
	_, err := processUpload(&stored, bytes.NewReader(damaged), cfg)
	if err == nil {
		t.Fatalf("a corrupt video was accepted; %d bytes were written", stored.Len())
	}

	var rejection *uploadRejection
	if !asRejection(err, &rejection) {
		t.Fatalf("the failure is not a user-facing rejection: %v", err)
	}
	if rejection.message == "" {
		t.Error("the rejection carries no message for the user")
	}
	// The hook writes into a pipe the server drains, so bytes can reach the
	// output before the failure is known. What must be guaranteed is that the
	// upload is REJECTED — the server then discards everything it received.
	// This is asserted through the hook in TestHookRejectsACorruptVideo.
}

// TestHookRejectsACorruptVideo is the same failure seen the way the server
// sees it: through FileWillBeUploaded, with the server's own storage rules
// applied.
func TestHookRejectsACorruptVideo(t *testing.T) {
	cfg := videoConfig(t)
	damaged := corruptVideo(makeVideo(t, cfg.tools, 3, "640x480"))

	p := &MediaForgePlugin{}
	p.configuration.store(cfg)

	res := simulateServerUpload(t, p, damaged)
	if res.rejection == "" {
		t.Fatalf("the server would have stored a corrupt video (%d bytes)", len(res.storedBytes))
	}
	if res.storedBytes != nil {
		t.Errorf("a rejected upload must store nothing, but %d bytes were kept", len(res.storedBytes))
	}
	if !strings.Contains(res.rejection, "fixture.bin") {
		t.Errorf("the rejection does not name the file, so the user cannot tell which upload failed: %q",
			res.rejection)
	}
	for _, jargon := range []string{"exit status", "ffmpeg", "stderr", "errno"} {
		if strings.Contains(strings.ToLower(res.rejection), jargon) {
			t.Errorf("the message shown to the user contains developer jargon %q: %q", jargon, res.rejection)
		}
	}
}

// TestMissingFFmpegDisablesVideoAndLetsUploadsThrough is the third promise:
// without ffmpeg the feature switches itself off instead of failing uploads.
func TestMissingFFmpegDisablesVideoAndLetsUploadsThrough(t *testing.T) {
	real := ffmpegOrSkip(t) // only to build a fixture
	original := makeVideo(t, real, 2, "320x240")

	tools := detectMediaTools(func(string) (string, error) {
		return "", exec.ErrNotFound
	})
	if tools.available() {
		t.Fatalf("tools report available although the lookup always fails")
	}
	if tools.Reason == "" {
		t.Error("an unavailable toolchain must explain itself to the administrator")
	}
	if !strings.Contains(strings.ToLower(tools.Reason), "ffmpeg") {
		t.Errorf("the reason does not name the missing program: %q", tools.Reason)
	}

	cfg := defaultConfiguration()
	cfg.tools = tools
	if cfg.videoEnabled() {
		t.Error("video processing is reported as enabled although ffmpeg is missing")
	}

	var stored bytes.Buffer
	outcome, err := processUpload(&stored, bytes.NewReader(original), cfg)
	if err != nil {
		t.Fatalf("an upload failed although the feature is supposed to be off: %v", err)
	}
	if outcome.reEncoded {
		t.Error("the video was reported as transcoded without ffmpeg")
	}
	if !bytes.Equal(stored.Bytes(), original) {
		t.Errorf("the stored video is not byte-identical: %d bytes in, %d bytes stored",
			len(original), stored.Len())
	}
}

// TestMissingFFmpegIsReportedToTheAdministrator — "disables itself" is only
// half the promise; the administrator has to be able to find out why.
func TestMissingFFmpegIsReportedToTheAdministrator(t *testing.T) {
	var logged []string
	api := &plugintest.API{}
	api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
		dest := args.Get(0).(*map[string]any)
		*dest = map[string]any{}
	}).Return(nil)
	api.On("LogWarn", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		logged = append(logged, fmt.Sprint(args...))
	}).Return()
	api.On("LogError", mock.Anything, mock.Anything).Maybe().Run(func(args mock.Arguments) {
		logged = append(logged, fmt.Sprint(args...))
	}).Return()
	api.On("LogInfo", mock.Anything, mock.Anything).Maybe().Return()
	api.On("LogDebug", mock.Anything, mock.Anything).Maybe().Return()

	p := &MediaForgePlugin{}
	p.SetAPI(api)
	// Pretend nothing is installed, whatever this machine actually has.
	p.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }

	if err := p.OnConfigurationChange(); err != nil {
		t.Fatalf("OnConfigurationChange failed: %v", err)
	}

	found := ""
	for _, line := range logged {
		if strings.Contains(strings.ToLower(line), "ffmpeg") {
			found = line
		}
	}
	if found == "" {
		t.Fatalf("nothing in the log tells the administrator that video is off; logged: %v", logged)
	}
	if !strings.Contains(strings.ToLower(found), "video") {
		t.Errorf("the message does not say which feature is affected: %q", found)
	}
}

// TestTimeoutRejectsTheUploadAndCleansUp covers the fourth promise. It is the
// reason this slice needs a zero-state check at all: a killed transcode must
// leave neither a temporary file nor a running process behind.
func TestTimeoutRejectsTheUploadAndCleansUp(t *testing.T) {
	cfg := videoConfig(t)
	// Long enough that a 1-second budget cannot possibly finish it, short
	// enough that generating it does not slow the suite down.
	original := makeVideo(t, cfg.tools, 20, "1280x720")
	cfg.VideoTimeoutSeconds = 1

	spool := t.TempDir()
	cfg.spoolDir = spool

	before := runningFFmpegProcesses(t)

	var stored bytes.Buffer
	start := time.Now()
	_, err := processUpload(&stored, bytes.NewReader(original), cfg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("a transcode that ran out of time was accepted anyway")
	}
	var rejection *uploadRejection
	if !asRejection(err, &rejection) {
		t.Fatalf("a timeout must reject the upload, but produced: %v", err)
	}
	if !strings.Contains(strings.ToLower(rejection.message), "long") &&
		!strings.Contains(strings.ToLower(rejection.message), "time") {
		t.Errorf("the message does not tell the user it took too long: %q", rejection.message)
	}
	// The budget is one second; anything close to the fixture's own encode
	// time would mean the deadline was not enforced.
	if elapsed > 30*time.Second {
		t.Errorf("the transcode ran for %s despite a 1 s budget", elapsed.Round(time.Second))
	}

	leftovers, err := os.ReadDir(spool)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("inspecting the spool directory failed: %v", err)
	}
	if len(leftovers) != 0 {
		names := make([]string, 0, len(leftovers))
		for _, e := range leftovers {
			names = append(names, e.Name())
		}
		t.Errorf("the timeout left %d temporary file(s) behind: %v", len(names), names)
	}

	// The process must be gone, not merely abandoned. Give the kill a moment
	// to be reaped, then compare against the state before the run.
	deadline := time.Now().Add(10 * time.Second)
	var after []string
	for time.Now().Before(deadline) {
		after = runningFFmpegProcesses(t)
		if len(after) <= len(before) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(after) > len(before) {
		t.Errorf("the timeout left an ffmpeg process running: before=%v after=%v", before, after)
	}
}

// TestSpooledFilesAreRemovedOnEveryPath — the success and pass-through paths
// create the same temporary files as the failure path, so they get the same
// scrutiny.
func TestSpooledFilesAreRemovedOnEveryPath(t *testing.T) {
	base := videoConfig(t)
	good := makeVideo(t, base.tools, 2, "320x240")
	damaged := corruptVideo(makeVideo(t, base.tools, 2, "320x240"))
	notAVideo := bytes.Repeat([]byte("not a video at all\n"), 4096)

	cases := []struct {
		name  string
		input []byte
	}{
		{"successful transcode", good},
		{"rejected transcode", damaged},
		{"file that is not a video", notAVideo},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.spoolDir = t.TempDir()

			var stored bytes.Buffer
			_, _ = processUpload(&stored, bytes.NewReader(tc.input), cfg)

			entries, err := os.ReadDir(cfg.spoolDir)
			if err != nil {
				t.Fatalf("inspecting the spool directory failed: %v", err)
			}
			if len(entries) != 0 {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("%d temporary file(s) were left behind: %v", len(entries), names)
			}
		})
	}
}

// TestVideoLargerThanTheSpoolLimitIsPassedThrough proves the size cap does not
// turn into a failed upload. Refusing to buffer a huge file is a resource
// decision, not a reason to lose the user's video.
func TestVideoLargerThanTheSpoolLimitIsPassedThrough(t *testing.T) {
	cfg := videoConfig(t)
	original := makeVideo(t, cfg.tools, 3, "640x480")
	cfg.MaxVideoSizeMB = 1
	if len(original) <= 1024*1024 {
		t.Fatalf("the fixture is only %d bytes, so it does not exceed the 1 MB cap", len(original))
	}
	cfg.spoolDir = t.TempDir()

	var stored bytes.Buffer
	outcome, err := processUpload(&stored, bytes.NewReader(original), cfg)
	if err != nil {
		t.Fatalf("an oversized video was rejected instead of being stored: %v", err)
	}
	if outcome.reEncoded {
		t.Error("a video above the spool limit was reported as transcoded")
	}
	if !bytes.Equal(stored.Bytes(), original) {
		t.Errorf("the stored video is not byte-identical: %d bytes in, %d stored",
			len(original), stored.Len())
	}
	entries, _ := os.ReadDir(cfg.spoolDir)
	if len(entries) != 0 {
		t.Errorf("the abandoned spool file was not removed: %d entries left", len(entries))
	}
}

// TestAlreadyEfficientVideoIsNotTranscodedAtAll is the measured case, not a
// theoretical one.
//
// A video that is already encoded frugally gets BIGGER when it is re-encoded —
// measured on the reference container: a 60 s 1080p clip at 1.7 Mbit/s came out
// at 155 % of its original size, and a 30-minute 480p clip at 0.5 Mbit/s at
// 139 %. Those same files are the long ones, so they are also the ones that run
// into the time limit. Detecting them from the header, before any encoding
// starts, is what keeps both problems from happening: no wasted cores, no
// rejection of a file that never needed converting.
//
// The check must be a decision made BEFORE the transcode, which is why this
// test measures the elapsed time as well as the bytes: passing the file through
// after a full transcode would produce the same file and still burn the time.
func TestAlreadyEfficientVideoIsNotTranscodedAtAll(t *testing.T) {
	cfg := videoConfig(t)

	// Encode the fixture the way a streaming service would: slow preset, high
	// CRF, so there is nothing left for the plugin to win.
	path := filepath.Join(t.TempDir(), "efficient.mp4")
	cmd := exec.Command(cfg.tools.FFmpegPath, "-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=40",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=40",
		"-c:v", "libx264", "-preset", "slow", "-crf", "34",
		"-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "64k",
		"-movflags", "+faststart", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating the efficient fixture failed: %v\n%s", err, out)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the efficient fixture failed: %v", err)
	}

	var stored bytes.Buffer
	start := time.Now()
	outcome, err := processUpload(&stored, bytes.NewReader(original), cfg)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("an already-efficient video was rejected: %v", err)
	}
	if outcome.reEncoded {
		t.Errorf("the video was re-encoded although it was already efficient: %d -> %d bytes",
			len(original), stored.Len())
	}
	if !bytes.Equal(stored.Bytes(), original) {
		t.Errorf("the stored video is not byte-identical: %d bytes in, %d stored",
			len(original), stored.Len())
	}
	// Probing reads the header only and finishes in milliseconds. A full
	// transcode of forty seconds of 720p takes several seconds on this
	// hardware, so this bound distinguishes "decided from the header" from
	// "encoded and then discarded". Both produce the same stored file; only
	// one of them wastes the server's cores and the sender's time.
	//
	// The margin is measured against the same transcode running in
	// TestHighBitrateVideoIsStillTranscoded, so it is not an arbitrary number.
	if elapsed > 2*time.Second {
		t.Errorf("the decision took %s, which means the file was transcoded before "+
			"being discarded — the point is to not encode it at all", elapsed.Round(time.Millisecond))
	}
}

// TestHighBitrateVideoIsStillTranscoded is the counterpart: the skip must not
// swallow the files the feature exists for.
func TestHighBitrateVideoIsStillTranscoded(t *testing.T) {
	cfg := videoConfig(t)
	// makeVideo encodes at ultrafast/CRF 10, which is what a phone or a screen
	// recorder produces: a lot of bits for the picture on offer.
	original := makeVideo(t, cfg.tools, 3, "1280x720")

	var stored bytes.Buffer
	outcome, err := processUpload(&stored, bytes.NewReader(original), cfg)
	if err != nil {
		t.Fatalf("the upload was rejected: %v", err)
	}
	if !outcome.reEncoded {
		t.Fatalf("a wastefully encoded video was passed through: %d bytes", len(original))
	}
	if stored.Len() >= len(original) {
		t.Errorf("the stored video is not smaller: %d -> %d", len(original), stored.Len())
	}
}

// TestVideoCompressionCanBeSwitchedOff — the kill switch has to reach the
// video path, not just the image path.
func TestVideoCompressionCanBeSwitchedOff(t *testing.T) {
	cfg := videoConfig(t)
	cfg.EnableVideoCompression = false
	original := makeVideo(t, cfg.tools, 2, "320x240")

	var stored bytes.Buffer
	outcome, err := processUpload(&stored, bytes.NewReader(original), cfg)
	if err != nil {
		t.Fatalf("the upload failed although video compression is off: %v", err)
	}
	if outcome.reEncoded {
		t.Error("the video was transcoded although the feature is switched off")
	}
	if !bytes.Equal(stored.Bytes(), original) {
		t.Error("the stored video is not byte-identical although the feature is switched off")
	}
}

// TestNonVideoFilesAreUntouchedByTheVideoPath is the regression guard for
// earlier work: adding video must not change what happens to anything else.
func TestNonVideoFilesAreUntouchedByTheVideoPath(t *testing.T) {
	cfg := videoConfig(t)

	t.Run("large JPEG is still compressed", func(t *testing.T) {
		original := encodeJPEG(t, noisyImage(4000, 3000, 77), 95)
		var stored bytes.Buffer
		outcome, err := processUpload(&stored, bytes.NewReader(original), cfg)
		if err != nil {
			t.Fatalf("a JPEG upload failed: %v", err)
		}
		if !outcome.reEncoded {
			t.Fatal("the image pipeline stopped compressing once video was added")
		}
		if stored.Len() >= len(original) {
			t.Errorf("the stored image is not smaller: %d -> %d", len(original), stored.Len())
		}
	})

	t.Run("arbitrary binary is byte-identical", func(t *testing.T) {
		original := randomBytes(t, 3*1024*1024)
		var stored bytes.Buffer
		if _, err := processUpload(&stored, bytes.NewReader(original), cfg); err != nil {
			t.Fatalf("a binary upload failed: %v", err)
		}
		if !bytes.Equal(stored.Bytes(), original) {
			t.Error("a non-media file was modified")
		}
	})

	t.Run("audio-only MP4 is passed through", func(t *testing.T) {
		// An .m4a carries the same ftyp header as a video. It must not be
		// mistaken for one — voice recording owns audio, this slice does not.
		path := filepath.Join(t.TempDir(), "voice.m4a")
		cmd := exec.Command(cfg.tools.FFmpegPath, "-nostdin", "-y", "-v", "error",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
			"-c:a", "aac", "-b:a", "128k", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generating the audio fixture failed: %v\n%s", err, out)
		}
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the audio fixture failed: %v", err)
		}

		var stored bytes.Buffer
		outcome, err := processUpload(&stored, bytes.NewReader(original), cfg)
		if err != nil {
			t.Fatalf("an audio upload was rejected: %v", err)
		}
		if outcome.reEncoded {
			t.Error("an audio file was transcoded by the video path")
		}
		if !bytes.Equal(stored.Bytes(), original) {
			t.Error("an audio file was modified")
		}
	})
}

// TestBrokenStreamDuringAVideoUploadIsStillRejected keeps the the first hook version
// contract alive on the new code path: a stream that dies mid-upload must
// never be stored as a short but complete file.
func TestBrokenStreamDuringAVideoUploadIsStillRejected(t *testing.T) {
	cfg := videoConfig(t)
	original := makeVideo(t, cfg.tools, 3, "640x480")
	cfg.spoolDir = t.TempDir()

	// The stream delivers the first half of a real video and then dies. That
	// is the shape the first hook version rejects, and adding a spool file in front of the
	// transcoder must not turn it into a stored, truncated video.
	broken := io.MultiReader(
		bytes.NewReader(original[:len(original)/2]),
		&erroringReader{err: errors.New("backing store went away")},
	)

	var stored bytes.Buffer
	if _, err := processUpload(&stored, broken, cfg); err == nil {
		t.Fatalf("a broken upload stream was accepted, storing %d bytes", stored.Len())
	}

	entries, _ := os.ReadDir(cfg.spoolDir)
	if len(entries) != 0 {
		t.Errorf("the aborted upload left %d temporary file(s) behind", len(entries))
	}
}

// TestCodecSelectionProducesTheSelectedCodec proves the codec setting is wired
// through to the stored file rather than merely accepted by the validator.
func TestCodecSelectionProducesTheSelectedCodec(t *testing.T) {
	base := videoConfig(t)
	original := makeVideo(t, base.tools, 2, "320x240")

	// Every codec the System Console offers is exercised. Listing only two of
	// them was how a broken AV1 path reached the test instance: SVT-AV1 takes
	// numeric presets, not x264's names, so ffmpeg refused to start and every
	// AV1 upload was rejected as "damaged". A selectable option that cannot
	// work is worse than no option.
	cases := []struct {
		setting   string
		codecName string
	}{
		{videoCodecH264, "h264"},
		{videoCodecH265, "hevc"},
		{videoCodecAV1, "av1"},
	}

	for _, tc := range cases {
		t.Run(tc.setting, func(t *testing.T) {
			cfg := base
			cfg.VideoCodec = tc.setting

			var stored bytes.Buffer
			outcome, err := processUpload(&stored, bytes.NewReader(original), cfg)
			if err != nil {
				t.Fatalf("the upload was rejected: %v", err)
			}
			if !outcome.reEncoded {
				t.Fatalf("the video was not transcoded at all")
			}

			path := writeTemp(t, "stored.bin", stored.Bytes())
			out, err := exec.Command(base.tools.FFprobePath, "-v", "error",
				"-select_streams", "v:0", "-show_entries", "stream=codec_name",
				"-of", "csv=p=0", path).Output()
			if err != nil {
				t.Fatalf("the stored file could not be probed: %v", err)
			}
			got := strings.TrimSpace(string(out))
			if got != tc.codecName {
				t.Errorf("setting %q produced codec %q, want %q", tc.setting, got, tc.codecName)
			}
		})
	}
}

// TestLowerQualityProducesASmallerFile proves the CRF setting reaches the
// encoder. Two runs of the same fixture, one setting apart.
func TestLowerQualityProducesASmallerFile(t *testing.T) {
	base := videoConfig(t)
	original := makeVideo(t, base.tools, 3, "640x480")

	size := func(crf int) int {
		cfg := base
		cfg.VideoCRF = crf
		var stored bytes.Buffer
		if _, err := processUpload(&stored, bytes.NewReader(original), cfg); err != nil {
			t.Fatalf("CRF %d was rejected: %v", crf, err)
		}
		return stored.Len()
	}

	high := size(20)
	low := size(40)
	if low >= high {
		t.Errorf("CRF 40 produced %d bytes and CRF 20 produced %d — the setting has no effect",
			low, high)
	}
}

// TestTimeoutIsNotDefeatedByAnEscapedGrandchild is a regression test for an
// independent review finding, reproduced before it was believed.
//
// The claim was that the wait after SIGKILL can block forever. SIGKILL cannot
// be ignored, so the first reading of that is wrong — but the conclusion is
// right for a different reason, and the difference matters for the fix.
//
// cmd.Wait() does not only reap the process. When cmd.Stderr is anything other
// than an *os.File, os/exec inserts a pipe and a goroutine copying from it, and
// Wait also waits for that goroutine. The goroutine ends when the pipe reaches
// EOF, which happens only once EVERY holder of the write end is gone. A
// grandchild that called setsid escapes the process group, survives the kill,
// and keeps the pipe open — so the process is dead, reaped, and Wait still has
// not returned.
//
// Measured with a standalone probe before the fix: Wait had not returned 8 s
// after the group was killed. In the plugin that would have hung the upload
// forever, holding a server request open — a worse outcome than the timeout it
// was supposed to enforce.
//
// The fixture below is that exact shape: a script that spawns a detached
// grandchild and then sleeps far past the budget.
func TestTimeoutIsNotDefeatedByAnEscapedGrandchild(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is not available, so the escape cannot be staged")
	}

	dir := t.TempDir()
	fake := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\nsetsid sleep 120 &\nsleep 120\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatalf("writing the fake encoder failed: %v", err)
	}

	cfg := defaultConfiguration()
	cfg.tools = mediaTools{FFmpegPath: fake, FFprobePath: fake}
	cfg.VideoTimeoutSeconds = 1

	req := transcodeRequest{
		inputPath:  filepath.Join(dir, "in.mp4"),
		outputPath: filepath.Join(dir, "out.mp4"),
		cfg:        cfg,
	}

	finished := make(chan error, 1)
	go func() {
		finished <- runTranscode(req, cfg.tools, time.Duration(cfg.VideoTimeoutSeconds)*time.Second)
	}()

	select {
	case err := <-finished:
		if !errors.Is(err, errTranscodeTimeout) {
			t.Errorf("want a timeout, got: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runTranscode never returned: the timeout was defeated by a process " +
			"that outlived the kill, so the upload would hang indefinitely")
	}

	// Clean up the escaped grandchild ourselves — it is deliberately outside
	// the plugin's process group, so the plugin cannot and must not kill it.
	_ = exec.Command("pkill", "-f", "sleep 120").Run()
}

// ------------------------------------------------------------- zero state

// TestMain runs the suite and then checks that this package left nothing
// running and nothing on disk.
//
// This is mandatory for this slice: it is the first one that starts foreign
// processes. A green test run says the behaviour is right; it says nothing
// about whether a killed transcode was actually reaped or whether a spool file
// survived. Those are exactly the defects that only show up in production, as
// a slowly filling disk or a machine full of orphaned encoders.
func TestMain(m *testing.M) {
	before := len(findProcesses(spoolFilePrefix))

	code := m.Run()

	// Give a just-killed process a moment to be reaped before judging.
	var leaked []string
	for i := 0; i < 25; i++ {
		leaked = findProcesses(spoolFilePrefix)
		if len(leaked) <= before {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(leaked) > before {
		fmt.Fprintf(os.Stderr,
			"\nZERO-STATE CHECK FAILED: %d ffmpeg process(es) survived the test run:\n",
			len(leaked)-before)
		for _, l := range leaked {
			fmt.Fprintf(os.Stderr, "  %s\n", l)
		}
		code = 1
	}

	spool := filepath.Join(os.TempDir(), spoolDirName)
	entries, err := os.ReadDir(spool)
	if err == nil && len(entries) > 0 {
		fmt.Fprintf(os.Stderr,
			"\nZERO-STATE CHECK FAILED: %d temporary file(s) left in %s:\n", len(entries), spool)
		for _, e := range entries {
			fmt.Fprintf(os.Stderr, "  %s\n", e.Name())
		}
		code = 1
	}
	if code == 0 {
		fmt.Fprintf(os.Stderr,
			"zero-state check: no leftover ffmpeg processes, no leftover temporary files\n")
	}

	os.Exit(code)
}

// runningFFmpegProcesses is the per-test view of the same check.
func runningFFmpegProcesses(t *testing.T) []string {
	t.Helper()
	return findProcesses(spoolFilePrefix)
}

// findProcesses lists live processes whose command line mentions marker.
//
// Matching on the plugin's own spool-file prefix rather than on "ffmpeg" is
// deliberate: an unrelated ffmpeg started by the developer, or by the
// Mattermost test instance in the next terminal, must not be reported as a
// leak — and must not be killed by anyone acting on this report.
func findProcesses(marker string) []string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := strconv.Itoa(os.Getpid())
	var found []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == self {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		cmdline := strings.ReplaceAll(string(raw), "\x00", " ")
		if strings.Contains(cmdline, marker) {
			found = append(found, e.Name()+": "+strings.TrimSpace(cmdline))
		}
	}
	return found
}

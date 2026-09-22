package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
)

// ---------------------------------------------------------------- fixtures

// makeRecording renders what a browser's MediaRecorder produces: an audio-only
// WebM stream carrying Opus, with no video track at all.
//
// The format is measured rather than assumed. On the test instance a headless
// Chromium recorded 3 s of tone and produced exactly this — `audio/webm;
// codecs=opus`, an EBML header, one Opus stream at 48 kHz — while its
// `audio/mp4` offer negotiated to `audio/mp4;codecs=opus` and delivered zero
// bytes. The fixture therefore reproduces the only thing the browser really
// hands over.
func makeRecording(t *testing.T, tools mediaTools, seconds int) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recording.webm")
	cmd := exec.Command(tools.FFmpegPath,
		"-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", seconds),
		"-c:a", "libopus", "-b:a", "64k", "-ac", "1",
		"-f", "webm", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating the recording fixture failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the recording fixture failed: %v", err)
	}
	return data
}

// makeAacRecording renders a recording that is ALREADY AAC in MP4, which is
// what Safari's MediaRecorder can produce. There is nothing to convert.
func makeAacRecording(t *testing.T, tools mediaTools, seconds int) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recording.m4a")
	cmd := exec.Command(tools.FFmpegPath,
		"-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=660:duration=%d", seconds),
		"-c:a", "aac", "-b:a", "64k", "-ac", "1",
		"-movflags", "+faststart", "-f", "mp4", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating the AAC recording fixture failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the AAC recording fixture failed: %v", err)
	}
	return data
}

// audioFacts is what an independent tool says about a stored file.
type audioFacts struct {
	formatName  string
	audioCodec  string
	videoCodec  string
	channels    int
	duration    float64
	decodesFull bool
}

// inspectAudio reads a stored file back the way the proof script does: ffprobe
// for the container and stream facts, and then a FULL decode with -xerror,
// because ffprobe reads the header and will happily describe a file whose
// payload is unusable.
func inspectAudio(t *testing.T, tools mediaTools, data []byte) audioFacts {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stored.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing the stored bytes failed: %v", err)
	}

	out, err := exec.Command(tools.FFprobePath,
		"-v", "error",
		"-show_entries", "stream=codec_name,codec_type,channels",
		"-show_entries", "format=format_name,duration",
		"-of", "default=noprint_wrappers=1",
		path).Output()
	if err != nil {
		t.Fatalf("ffprobe could not read the stored file: %v", err)
	}

	var facts audioFacts
	var pendingCodec string
	for _, line := range strings.Split(string(out), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "codec_name":
			pendingCodec = value
		case "codec_type":
			if value == "audio" {
				facts.audioCodec = pendingCodec
			}
			if value == "video" {
				facts.videoCodec = pendingCodec
			}
		case "channels":
			facts.channels, _ = strconv.Atoi(value)
		case "format_name":
			facts.formatName = value
		case "duration":
			facts.duration, _ = strconv.ParseFloat(value, 64)
		}
	}

	// The decisive check. -xerror turns the first decoding error into a
	// non-zero exit, so a file that merely looks right in its header cannot
	// pass.
	decode := exec.Command(tools.FFmpegPath,
		"-nostdin", "-v", "error", "-xerror", "-i", path, "-f", "null", "-")
	facts.decodesFull = decode.Run() == nil
	return facts
}

// isMP4Container reports whether ffprobe named the MOV/MP4/M4A family.
func (f audioFacts) isMP4Container() bool {
	return strings.Contains(f.formatName, "mp4") || strings.Contains(f.formatName, "m4a")
}

// voiceConfig is the default configuration with the real tools attached.
func voiceConfig(t *testing.T) configuration {
	t.Helper()
	cfg := defaultConfiguration()
	cfg.tools = ffmpegOrSkip(t)
	cfg.spoolDir = t.TempDir()
	return cfg
}

// getVoiceSettings asks the plugin's own route the question the webapp asks
// before it draws the button, over real HTTP.
func getVoiceSettings(t *testing.T, tp *testPlugin, userID string) (body string, status int) {
	t.Helper()
	return getVoiceSettingsQuery(t, tp, userID, "")
}

// getVoiceSettingsQuery is the same question with a query string attached, so
// a test can ask it for one channel.
func getVoiceSettingsQuery(t *testing.T, tp *testPlugin, userID, query string) (body string, status int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, voiceSettingsPath+query, nil)
	if userID != "" {
		req.Header.Set("Mattermost-User-Id", userID)
	}
	rec := httptest.NewRecorder()
	tp.ServeHTTP(&plugin.Context{}, rec, req)
	return rec.Body.String(), rec.Code
}

// recordingName is the name the webapp gives an upload it recorded.
func recordingName(ext string) string {
	return voiceRecordingPrefix + "20260918-101500." + ext
}

// ------------------------------------------------------- the core promise

// TestAVoiceRecordingIsStoredAsAPlayableM4A is the slice's Done criterion, in
// the form this container can actually check.
//
// The acceptance criterion in the plan — "plays natively in the Android and
// iOS apps" — needs a phone, and there is none here. What is provable is the
// chain the native playback rests on, and it is proved end to end: the browser
// hands over WebM/Opus, and what reaches the disk is an MP4/M4A container with
// an AAC track that decodes in full and carries a duration.
func TestAVoiceRecordingIsStoredAsAPlayableM4A(t *testing.T) {
	cfg := voiceConfig(t)
	recording := makeRecording(t, cfg.tools, 3)

	// What the browser sent is deliberately NOT already playable everywhere.
	sent := inspectAudio(t, cfg.tools, recording)
	if sent.audioCodec != "opus" {
		t.Fatalf("the fixture is not the Opus recording this test is about: %q", sent.audioCodec)
	}

	var stored bytes.Buffer
	outcome, err := processNamedUpload(&stored, bytes.NewReader(recording), cfg, recordingName("webm"))
	if err != nil {
		t.Fatalf("a voice recording was rejected: %v", err)
	}
	if !outcome.isVoice {
		t.Fatal("the recording was not recognised as a voice message")
	}
	if !outcome.voiceConverted {
		t.Fatal("the recording was stored without being converted")
	}

	got := inspectAudio(t, cfg.tools, stored.Bytes())
	if !got.isMP4Container() {
		t.Errorf("the stored recording is not an MP4/M4A container: %q", got.formatName)
	}
	if got.audioCodec != "aac" {
		t.Errorf("the stored recording carries %q, want an AAC track", got.audioCodec)
	}
	if got.videoCodec != "" {
		t.Errorf("the stored recording carries a video track (%q); a voice message must not",
			got.videoCodec)
	}
	if !got.decodesFull {
		t.Error("the stored recording does not decode in full")
	}
	if got.duration < 2.5 || got.duration > 4.5 {
		t.Errorf("the stored recording claims %.3f s, want about 3 s", got.duration)
	}
}

// TestTheStoredRecordingIsAnnouncedAsAudio pins the half of the promise that
// lives in the database rather than in the bytes.
//
// A correct M4A that the server still calls "recording.webm" with a video MIME
// type is exactly the broken-voice-plugin failure SPEC §4.3 exists to avoid: a
// phone decides how to offer a file from what the server announces, not by
// sniffing it. The hook must therefore hand back a corrected *model.FileInfo,
// which is the only channel it has for this.
func TestTheStoredRecordingIsAnnouncedAsAudio(t *testing.T) {
	tools := ffmpegOrSkip(t)
	tp := newTestPluginWithSettings(t, map[string]any{})
	allowAnyLogging(tp.api)
	// The hook now records a playing time for a converted recording, so a
	// key-value store has to be present or the write panics.
	attachKV(tp.api)
	recording := makeRecording(t, tools, 2)

	info := &model.FileInfo{
		Id:        model.NewId(),
		Name:      recordingName("webm"),
		Extension: "webm",
		MimeType:  "video/webm",
		Size:      int64(len(recording)),
		Path:      "20260918/teams/t/channels/c/users/u/" + recordingName("webm"),
	}

	var out writeCounter
	returned, rejection := tp.FileWillBeUploaded(
		&plugin.Context{}, info, bytesReaderOf(recording), &out)
	if rejection != "" {
		t.Fatalf("a voice recording was rejected: %s", rejection)
	}
	if returned == nil {
		t.Fatal("the hook returned no FileInfo, so the stored M4A keeps a WebM name and MIME type")
	}

	if !strings.HasSuffix(returned.Name, ".m4a") {
		t.Errorf("the stored recording is named %q, want an .m4a name", returned.Name)
	}
	if returned.Extension != "m4a" {
		t.Errorf("the stored recording has extension %q, want \"m4a\"", returned.Extension)
	}
	if returned.MimeType != voiceMimeType {
		t.Errorf("the stored recording is announced as %q, want %q",
			returned.MimeType, voiceMimeType)
	}
	// The server overwrites Size itself, so the hook must not pre-empt it.
	if returned.Size != info.Size {
		t.Errorf("the hook changed FileInfo.Size to %d; the server sets it from its own byte count",
			returned.Size)
	}
	if returned.Width != 0 || returned.Height != 0 {
		t.Errorf("the hook recorded geometry %dx%d on an audio file",
			returned.Width, returned.Height)
	}
}

// TestTheHookDoesNotMutateTheServersFileInfoInPlaceForAVoiceMessage repeats
// the image compression discipline on the new path: the server's own struct must be left
// alone, because a rejection later in the chain would otherwise leave a
// renamed file that was never stored.
func TestTheHookDoesNotMutateTheServersFileInfoInPlaceForAVoiceMessage(t *testing.T) {
	tools := ffmpegOrSkip(t)
	tp := newTestPluginWithSettings(t, map[string]any{})
	allowAnyLogging(tp.api)
	recording := makeRecording(t, tools, 2)

	name := recordingName("webm")
	info := &model.FileInfo{
		Id: model.NewId(), Name: name, Extension: "webm", MimeType: "video/webm",
		Size: int64(len(recording)),
		Path: "20260918/teams/t/channels/c/users/u/" + name,
	}

	var out writeCounter
	returned, _ := tp.FileWillBeUploaded(&plugin.Context{}, info, bytesReaderOf(recording), &out)

	if info.Name != name || info.Extension != "webm" || info.MimeType != "video/webm" {
		t.Errorf("the hook rewrote the server's own FileInfo: %s / %s / %s",
			info.Name, info.Extension, info.MimeType)
	}
	if returned == info {
		t.Error("the hook returned the server's own pointer instead of a corrected copy")
	}
}

// -------------------------------------------- what must NOT change (video compression)

// TestOrdinaryAudioUploadsAreStillPassedThroughUntouched is the interaction
// the plan warns about, pinned.
//
// A voice recording is an audio track in a video container, and video compression
// deliberately passes exactly those through unchanged. This slice must only
// take the files the plugin's own recorder produced — everything else a user
// attaches stays byte-identical, whatever its container.
func TestOrdinaryAudioUploadsAreStillPassedThroughUntouched(t *testing.T) {
	cfg := voiceConfig(t)

	cases := []struct {
		name  string
		bytes func() []byte
	}{
		{"voice-memo.m4a", func() []byte { return makeAacRecording(t, cfg.tools, 2) }},
		{"song.webm", func() []byte { return makeRecording(t, cfg.tools, 2) }},
		{"notes.opus", func() []byte { return makeRecording(t, cfg.tools, 2) }},
		// A user who names an ordinary attachment the way the recorder does
		// still keeps their file: the container has to agree as well.
		{recordingName("mp4"), func() []byte { return makeVideo(t, cfg.tools, 2, "320x240") }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := tc.bytes()
			var stored bytes.Buffer
			outcome, err := processNamedUpload(&stored, bytes.NewReader(original), cfg, tc.name)
			if err != nil {
				t.Fatalf("the upload was rejected: %v", err)
			}
			if outcome.voiceConverted {
				t.Error("an ordinary attachment was converted by the voice path")
			}
			if !bytes.Equal(stored.Bytes(), original) {
				t.Errorf("an ordinary attachment was modified: %d -> %d bytes",
					len(original), stored.Len())
			}
		})
	}
}

// TestTheVoicePathNeverTakesAFileWithAVideoTrack guards the boundary from the
// other side: a screen recording the browser produced as WebM still belongs to
// the video pipeline, even if it somehow carries the recorder's name.
func TestTheVoicePathNeverTakesAFileWithAVideoTrack(t *testing.T) {
	cfg := voiceConfig(t)
	path := filepath.Join(t.TempDir(), "screen.webm")
	cmd := exec.Command(cfg.tools.FFmpegPath, "-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libvpx-vp9", "-b:v", "200k", "-c:a", "libopus", "-b:a", "64k",
		"-f", "webm", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this machine cannot build a WebM video fixture: %v\n%s", err, out)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the fixture failed: %v", err)
	}

	var stored bytes.Buffer
	outcome, err := processNamedUpload(&stored, bytes.NewReader(original), cfg, recordingName("webm"))
	if err != nil {
		t.Fatalf("the upload was rejected: %v", err)
	}
	if outcome.voiceConverted {
		t.Fatal("a file with a video track was converted into a voice message, " +
			"which would silently throw the picture away")
	}
	got := inspectAudio(t, cfg.tools, stored.Bytes())
	if got.videoCodec == "" {
		t.Error("the stored file lost its video track")
	}
}

// TestARecordingThatIsAlreadyAacIsStoredAsSent covers the browser that records
// MP4/AAC directly. Re-encoding it would cost quality and time for nothing.
func TestARecordingThatIsAlreadyAacIsStoredAsSent(t *testing.T) {
	cfg := voiceConfig(t)
	original := makeAacRecording(t, cfg.tools, 2)

	var stored bytes.Buffer
	outcome, err := processNamedUpload(&stored, bytes.NewReader(original), cfg, recordingName("m4a"))
	if err != nil {
		t.Fatalf("an AAC recording was rejected: %v", err)
	}
	if outcome.voiceConverted {
		t.Error("a recording that was already AAC in MP4 was re-encoded")
	}
	if !bytes.Equal(stored.Bytes(), original) {
		t.Error("a recording that was already AAC in MP4 was modified")
	}
}

// ------------------------------------------------------------- failure paths

// TestABrokenRecordingIsRejectedRatherThanSilentlyShortened is the sharpest
// test in this slice, and it exists because the obvious implementation passes
// everything else while being badly wrong.
//
// A recording damaged in the middle does NOT make ffmpeg fail. The demuxer
// stops at the damage, the encoder writes what it got, the exit status is 0,
// and the result decodes perfectly — it is simply shorter. So a test that only
// asks "was it rejected or does it decode" cannot see the defect at all: the
// sender speaks for three seconds, the recipient hears one, and nothing
// anywhere reports a problem.
//
// The test therefore measures the DURATION and requires either a rejection or
// a recording of the right length. Silently storing a shortened one fails.
func TestABrokenRecordingIsRejectedRatherThanSilentlyShortened(t *testing.T) {
	cfg := voiceConfig(t)
	recording := makeRecording(t, cfg.tools, 3)

	// Four kinds of damage, all of which leave the file recognisable as an
	// Opus recording and all of which ffmpeg converts without complaint.
	damage := map[string]func([]byte) []byte{
		"zeroed in the middle": func(b []byte) []byte {
			d := append([]byte(nil), b...)
			for i := len(d) * 3 / 10; i < len(d)*7/10; i++ {
				d[i] = 0
			}
			return d
		},
		"zeroed to the end": func(b []byte) []byte {
			d := append([]byte(nil), b...)
			for i := len(d) / 4; i < len(d); i++ {
				d[i] = 0
			}
			return d
		},
		"truncated": func(b []byte) []byte {
			return append([]byte(nil), b[:len(b)*4/10]...)
		},
		"scrambled payload": func(b []byte) []byte {
			d := append([]byte(nil), b...)
			for i := len(d) / 3; i < len(d); i++ {
				d[i] = byte(int(d[i])*7 + 13)
			}
			return d
		},
	}

	for label, damageIt := range damage {
		t.Run(label, func(t *testing.T) {
			var stored bytes.Buffer
			_, err := processNamedUpload(
				&stored, bytes.NewReader(damageIt(recording)), cfg, recordingName("webm"))

			if err != nil {
				var rejection *uploadRejection
				if !asRejection(err, &rejection) {
					t.Fatalf("a damaged recording produced %v, want a rejection "+
						"carrying a sentence for the sender", err)
				}
				if rejection.message == "" {
					t.Error("the rejection carries no message for the sender")
				}
				if strings.Contains(strings.ToLower(rejection.message), "ffmpeg") {
					t.Errorf("the rejection shows the sender an internal diagnostic: %q",
						rejection.message)
				}
				return
			}

			// It was accepted. That is only acceptable if the recording is
			// actually complete — which is the case this test exists to rule
			// out. The expected length is MEASURED from the fixture rather
			// than written in: the fixture generator could change, and a
			// hard-coded bound would then be comparing against a number that
			// is no longer true.
			got := inspectAudio(t, cfg.tools, stored.Bytes())
			intact := inspectAudio(t, cfg.tools, recording)
			if got.duration < intact.duration-0.25 {
				t.Fatalf("a damaged recording was accepted and silently shortened "+
					"to %.3f s of the %.3f s that were spoken — the sender is told "+
					"nothing and the recipient hears a fragment",
					got.duration, intact.duration)
			}
		})
	}
}

// TestAHealthyRecordingIsNeverMistakenForADamagedOne is the other half of the
// rule above, and it is the one that keeps the cure from being worse than the
// disease.
//
// The damage is recognised by ffmpeg saying something at error level, so
// anything that makes a HEALTHY file produce a diagnostic would start refusing
// perfectly good recordings — and the sender would lose their words to a bug
// that reads as "voice messages do not work". Every input the plugin is
// expected to meet is therefore checked to convert silently.
func TestAHealthyRecordingIsNeverMistakenForADamagedOne(t *testing.T) {
	cfg := voiceConfig(t)

	for i := 0; i < 3; i++ {
		var stored bytes.Buffer
		recording := makeRecording(t, cfg.tools, 2+i)
		outcome, err := processNamedUpload(
			&stored, bytes.NewReader(recording), cfg, recordingName("webm"))
		if err != nil {
			t.Fatalf("a healthy %d s recording was rejected: %v", 2+i, err)
		}
		if !outcome.voiceConverted {
			t.Fatalf("a healthy %d s recording was not converted", 2+i)
		}
		got := inspectAudio(t, cfg.tools, stored.Bytes())
		if got.duration < float64(2+i)-0.5 {
			t.Errorf("a healthy %d s recording came back as %.3f s", 2+i, got.duration)
		}
	}
}

// TestABrokenStreamDuringAVoiceUploadIsStillRejected keeps the the first hook version
// contract alive on the new path: a stream that dies mid-upload must never be
// stored as a short but complete file.
func TestABrokenStreamDuringAVoiceUploadIsStillRejected(t *testing.T) {
	cfg := voiceConfig(t)
	recording := makeRecording(t, cfg.tools, 3)

	broken := io.MultiReader(
		bytes.NewReader(recording[:len(recording)/2]),
		&erroringReader{err: errors.New("connection reset by peer")},
	)

	var stored bytes.Buffer
	if _, err := processNamedUpload(&stored, broken, cfg, recordingName("webm")); err == nil {
		t.Fatal("a stream that broke mid-upload was accepted")
	}
}

// TestVoiceConversionCanBeSwitchedOff.
//
// With the feature off the plugin must be indistinguishable from one that does
// not have it: the recording is stored exactly as the browser sent it.
func TestVoiceConversionCanBeSwitchedOff(t *testing.T) {
	cfg := voiceConfig(t)
	cfg.EnableVoiceMessages = false
	original := makeRecording(t, cfg.tools, 2)

	var stored bytes.Buffer
	outcome, err := processNamedUpload(&stored, bytes.NewReader(original), cfg, recordingName("webm"))
	if err != nil {
		t.Fatalf("the upload was rejected: %v", err)
	}
	if outcome.voiceConverted {
		t.Error("a recording was converted although voice messages are switched off")
	}
	if !bytes.Equal(stored.Bytes(), original) {
		t.Error("a recording was modified although voice messages are switched off")
	}
}

// TestWithoutFfmpegARecordingIsStoredAsSent. SPEC §5: a missing toolchain
// disables the feature quietly, it never fails uploads.
func TestWithoutFfmpegARecordingIsStoredAsSent(t *testing.T) {
	cfg := voiceConfig(t)
	original := makeRecording(t, cfg.tools, 2)
	cfg.tools = mediaTools{Reason: "ffmpeg not found in the server's PATH"}

	var stored bytes.Buffer
	outcome, err := processNamedUpload(&stored, bytes.NewReader(original), cfg, recordingName("webm"))
	if err != nil {
		t.Fatalf("an upload failed because ffmpeg is missing: %v", err)
	}
	if outcome.voiceConverted {
		t.Error("a recording was reported as converted without ffmpeg")
	}
	if !bytes.Equal(stored.Bytes(), original) {
		t.Error("a recording was modified without ffmpeg")
	}
}

// TestARefusedVoiceMessageIsNotCountedAsAFailedVideo pins a defect that every
// other test in this file was blind to.
//
// The report has exactly two kinds, images and videos. A refused upload is
// counted under "video" unless the outcome says it was a voice message — and
// the rejection paths returned an EMPTY outcome, which reads as "not a voice
// message". So every refused recording would have been filed as a video that
// could not be converted, inflating the one failure figure an administrator
// acts on with something that is not a video at all.
//
// It is invisible from the outside: the upload is refused either way and the
// sender sees the same sentence. Only the report is wrong, and only quietly.
func TestARefusedVoiceMessageIsNotCountedAsAFailedVideo(t *testing.T) {
	tools := ffmpegOrSkip(t)
	sp := newStatsPlugin(t, map[string]any{})

	recording := makeRecording(t, tools, 3)
	damaged := append([]byte(nil), recording...)
	for i := len(damaged) * 3 / 10; i < len(damaged)*7/10; i++ {
		damaged[i] = 0
	}

	before := sp.report(t)
	_, rejection := sp.uploadThroughHook(t, recordingName("webm"), damaged)
	if rejection == "" {
		t.Fatal("a damaged recording was accepted, so this test proves nothing")
	}
	after := sp.report(t)

	if after.Total.Failures != before.Total.Failures {
		t.Errorf("a refused voice message was counted as a failure: %d -> %d "+
			"(the report only has images and videos, and this is neither)",
			before.Total.Failures, after.Total.Failures)
	}
	if got := after.byKind(statsKindVideo).Failures; got != before.byKind(statsKindVideo).Failures {
		t.Errorf("a refused voice message was counted as a failed VIDEO: %d -> %d",
			before.byKind(statsKindVideo).Failures, got)
	}
}

// TestAMissingToolchainIsReportedForVoiceMessagesToo.
//
// SPEC §5 requires a missing ffmpeg to disable a feature with an
// admin-visible message rather than fail uploads. For voice messages the
// silence is worse than for video: the button is simply absent, because the
// webapp asks the server first and is told "not available". An administrator
// who switched the feature on has no other way to learn why nothing happened.
func TestAMissingToolchainIsReportedForVoiceMessagesToo(t *testing.T) {
	tp := &testPlugin{MediaForgePlugin: &MediaForgePlugin{}, settings: map[string]any{}}
	tp.MediaForgePlugin.lookPath = func(string) (string, error) {
		return "", errors.New("not found")
	}

	var warnings []string
	api := &plugintest.API{}
	api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
		dest := args.Get(0).(*map[string]any)
		*dest = map[string]any{}
	}).Return(nil)
	api.On("LogWarn", mock.Anything).Run(func(args mock.Arguments) {
		warnings = append(warnings, args.String(0))
	}).Maybe().Return()
	api.On("LogWarn", mock.Anything, mock.Anything).Maybe().Return()
	api.On("LogError", mock.Anything).Maybe().Return()
	api.On("LogDebug", mock.Anything).Maybe().Return()
	tp.api = api
	tp.SetAPI(api)

	if err := tp.OnConfigurationChange(); err != nil {
		t.Fatalf("OnConfigurationChange returned %v", err)
	}

	var found string
	for _, w := range warnings {
		if strings.Contains(strings.ToLower(w), "voice") {
			found = w
		}
	}
	if found == "" {
		t.Fatalf("no warning names voice messages; the administrator is told "+
			"nothing about why the record button is missing. Warnings were: %v",
			warnings)
	}
	if !strings.Contains(found, "ffmpeg") {
		t.Errorf("the warning does not say what is missing: %q", found)
	}
	// Written for an administrator, so it has to say what to DO.
	if !strings.Contains(strings.ToLower(found), "install") {
		t.Errorf("the warning does not say how to fix it: %q", found)
	}
}

// ------------------------------------------ the the size note and the statistics report contracts

// TestAConvertedRecordingCarriesNoSizeNoteAndNoFigures is the interaction with
// the two slices that measure savings, and it is a decision rather than an
// omission.
//
// Converting Opus to AAC is a format change, not a compression: at the same
// bitrate the file usually comes out slightly LARGER. Counting it as processed
// would put a negative saving into the administrator's report — the one number
// the statistics report exists to make trustworthy — and would attach a note reading "0 %
// smaller" to a message where nothing was made smaller. The note contract of
// SPEC §4.4 says the absence of a note means "stored as the original"; for a
// voice message that is close enough to the truth to keep the contract honest,
// and a note claiming a saving would not be.
func TestAConvertedRecordingCarriesNoSizeNoteAndNoFigures(t *testing.T) {
	tools := ffmpegOrSkip(t)
	sp := newStatsPlugin(t, map[string]any{})
	recording := makeRecording(t, tools, 2)

	before := sp.report(t)
	stored, rejection := sp.uploadThroughHook(t, recordingName("webm"), recording)
	if rejection != "" {
		t.Fatalf("a voice recording was rejected: %s", rejection)
	}
	if len(stored) == 0 {
		t.Fatal("nothing was stored")
	}

	after := sp.report(t)
	if after.Total.Files != before.Total.Files {
		t.Errorf("a voice message was counted as a processed file: %d -> %d",
			before.Total.Files, after.Total.Files)
	}
	if after.Total.BytesSaved != before.Total.BytesSaved {
		t.Errorf("a voice message changed the reported saving by %d byte(s)",
			after.Total.BytesSaved-before.Total.BytesSaved)
	}
	if after.Total.Failures != before.Total.Failures {
		t.Errorf("a voice message was counted as a failure: %d -> %d",
			before.Total.Failures, after.Total.Failures)
	}

	// A recording DOES now leave a record behind, and the contract is that it
	// carries a playing time and NOTHING ELSE.
	//
	// This assertion used to be "no record at all", which was the right rule
	// while the record could only mean a saving. The length has to travel
	// somewhere to reach a phone, and this is the channel that already
	// reaches one — so the rule became narrower rather than weaker: no sizes,
	// therefore no saving can ever be claimed for a conversion that usually
	// makes the file bigger. The three report assertions above are the other
	// half of that promise and are unchanged.
	keys := sp.kv.keys(noteKeyPrefix)
	if len(keys) != 1 {
		t.Fatalf("a converted recording wrote %d note record(s), want exactly 1 "+
			"carrying its playing time: %v", len(keys), keys)
	}
	raw, ok := sp.kv.get(keys[0])
	if !ok {
		t.Fatalf("the record %s cannot be read back", keys[0])
	}
	var record noteRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("the stored record does not parse: %v", err)
	}
	if record.Original != 0 || record.Stored != 0 {
		t.Errorf("a voice message recorded sizes %d -> %d; a conversion is not a "+
			"saving and must never be reported as one", record.Original, record.Stored)
	}
	if record.Seconds <= 0 {
		t.Errorf("the record carries no playing time (%v), which is the only "+
			"reason it exists", record.Seconds)
	}
}

// ------------------------------------------------------------ the recogniser

// TestOnlyTheRecordersOwnNamesReachTheVoicePath pins the narrow rule that
// keeps this slice away from every other upload.
func TestOnlyTheRecordersOwnNamesReachTheVoicePath(t *testing.T) {
	cases := map[string]bool{
		voiceRecordingPrefix + "20260918-101500.webm": true,
		voiceRecordingPrefix + "20260918-101500.mp4":  true,
		voiceRecordingPrefix + "20260918-101500.m4a":  true,
		voiceRecordingPrefix + "20260918-101500.ogg":  true,
		"voice-message.webm":                          false,
		"my-voice-message-2026.webm":                  false,
		"holiday.webm":                                false,
		"":                                            false,
		voiceRecordingPrefix + "note.txt":             false,
		voiceRecordingPrefix + "clip.mkv":             false,
	}
	for name, want := range cases {
		if got := looksLikeVoiceRecording(name); got != want {
			t.Errorf("looksLikeVoiceRecording(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestTheUnnamedEntryPointStillBehavesLikeSliceSeven guards every existing
// caller: processUpload has no file name and must therefore never take the
// voice path, whatever the bytes look like.
func TestTheUnnamedEntryPointStillBehavesLikeSliceSeven(t *testing.T) {
	cfg := voiceConfig(t)
	original := makeRecording(t, cfg.tools, 2)

	var stored bytes.Buffer
	outcome, err := processUpload(&stored, bytes.NewReader(original), cfg)
	if err != nil {
		t.Fatalf("the upload was rejected: %v", err)
	}
	if outcome.voiceConverted {
		t.Error("an unnamed upload was taken by the voice path")
	}
	if !bytes.Equal(stored.Bytes(), original) {
		t.Error("an unnamed upload was modified")
	}
}

// ----------------------------------------------------------- the webapp gate

// TestTheWebappIsToldWhetherRecordingIsAvailable.
//
// The button must not offer something the server will not do. If an
// administrator switches voice messages off, or the server has no ffmpeg, a
// recording would be stored as WebM — playable on a desktop browser and silent
// on a phone, which is precisely the failure SPEC §4.3 is designed to rule
// out. The webapp therefore asks before it shows the entry.
func TestTheWebappIsToldWhetherRecordingIsAvailable(t *testing.T) {
	tools := ffmpegOrSkip(t)

	t.Run("available when the feature is on and ffmpeg is there", func(t *testing.T) {
		tp := newTestPluginWithSettings(t, map[string]any{})
		allowAnyLogging(tp.api)
		body, status := getVoiceSettings(t, tp, "someuserid")
		if status != 200 {
			t.Fatalf("status %d, want 200", status)
		}
		if !strings.Contains(body, `"enabled":true`) {
			t.Errorf("the webapp was told %q, want enabled:true", body)
		}
		_ = tools
	})

	t.Run("unavailable when the administrator switched it off", func(t *testing.T) {
		tp := newTestPluginWithSettings(t, map[string]any{"EnableVoiceMessages": false})
		allowAnyLogging(tp.api)
		body, status := getVoiceSettings(t, tp, "someuserid")
		if status != 200 {
			t.Fatalf("status %d, want 200", status)
		}
		if !strings.Contains(body, `"enabled":false`) {
			t.Errorf("the webapp was told %q, want enabled:false", body)
		}
	})

	t.Run("refused without a session", func(t *testing.T) {
		tp := newTestPluginWithSettings(t, map[string]any{})
		allowAnyLogging(tp.api)
		_, status := getVoiceSettings(t, tp, "")
		if status != 401 {
			t.Errorf("status %d without a session, want 401", status)
		}
	})
}

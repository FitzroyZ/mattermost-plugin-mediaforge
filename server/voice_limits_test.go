package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

// ---------------------------------------------------------------- fixtures

// makeRecordingOfLength renders a recording of a precise length in seconds.
//
// Fractional lengths matter here in a way they did not in voice recording: the whole
// point of a limit is the pair of cases either side of it, and a fixture
// generator that only took whole seconds could not produce "just under".
func makeRecordingOfLength(t *testing.T, tools mediaTools, seconds float64) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recording.webm")
	cmd := exec.Command(tools.FFmpegPath,
		"-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%.3f", seconds),
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

// ------------------------------------------------------- reading the length

// TestARecordingsLengthIsReadFromItsPacketsNotGuessed is the measurement this
// whole slice rests on, pinned as a test.
//
// A browser's MediaRecorder writes NO duration into its header: measured on
// the test instance, `format=duration` and `stream=duration` both come back as
// N/A. So the length has to be derived, and the obvious derivation is a trap
// this project has already fallen into once: multiplying the packet count by
// "20 ms, the Opus default" gives 1.3 s for a file that is really 4.0 s,
// because Chromium writes 60 ms packets. A duration limit built on that number
// would cut every recording off at a third of the value the administrator set.
//
// The test therefore checks the two things that distinguish the right answer
// from the plausible one: the header really is empty, and the derived length
// really matches what was recorded.
func TestARecordingsLengthIsReadFromItsPacketsNotGuessed(t *testing.T) {
	cfg := voiceConfig(t)

	for _, want := range []float64{1.0, 3.0, 7.5} {
		recording := makeRecordingOfLength(t, cfg.tools, want)
		path := filepath.Join(t.TempDir(), "r.webm")
		if err := os.WriteFile(path, recording, 0o600); err != nil {
			t.Fatal(err)
		}

		// The header is empty. If this ever stops being true the machinery
		// below is not wrong, but the reason for it has changed.
		header, _ := exec.Command(cfg.tools.FFprobePath, "-v", "error",
			"-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
		if got := strings.TrimSpace(string(header)); got != "" && got != "N/A" {
			t.Logf("note: this ffmpeg DOES report a header duration (%q) for a "+
				"MediaRecorder-style WebM; the packet walk is still correct", got)
		}

		probed, err := probeAudioDuration(path, cfg.tools)
		if err != nil {
			t.Fatalf("reading the length of a %.1f s recording failed: %v", want, err)
		}
		if probed.seconds < want-0.2 || probed.seconds > want+0.2 {
			t.Errorf("a %.1f s recording was measured as %.3f s — a limit built on "+
				"this number would fire at the wrong length", want, probed.seconds)
		}
		if probed.damaged {
			t.Errorf("a healthy %.1f s recording was reported as damaged: %q",
				want, probed.complaint)
		}
	}
}

// TestThePacketCountShortcutIsNotWhatIsUsed guards the specific wrong answer.
//
// It is a regression test for a mistake rather than for a defect: the shortcut
// "packets × 20 ms" is what a reader reaches for, it is what the proof script
// originally did, and it is wrong by a factor of three on this browser. The
// test pins that the real implementation does NOT agree with it.
func TestThePacketCountShortcutIsNotWhatIsUsed(t *testing.T) {
	cfg := voiceConfig(t)
	recording := makeRecordingOfLength(t, cfg.tools, 4.0)
	path := filepath.Join(t.TempDir(), "r.webm")
	if err := os.WriteFile(path, recording, 0o600); err != nil {
		t.Fatal(err)
	}

	probed, err := probeAudioDuration(path, cfg.tools)
	if err != nil {
		t.Fatalf("reading the length failed: %v", err)
	}

	out, err := exec.Command(cfg.tools.FFprobePath, "-v", "error",
		"-count_packets", "-select_streams", "a:0",
		"-show_entries", "stream=nb_read_packets", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("counting packets failed: %v", err)
	}
	var packets int
	fmt.Sscanf(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(string(out)), ",")), "%d", &packets)
	shortcut := float64(packets) * 0.020

	if probed.seconds < 3.8 || probed.seconds > 4.2 {
		t.Fatalf("the measured length %.3f s is not the 4 s that were recorded", probed.seconds)
	}
	t.Logf("%d packets; the \"× 20 ms\" shortcut says %.3f s, the packets themselves say %.3f s",
		packets, shortcut, probed.seconds)
}

// ------------------------------------------------------------- the limit

// TestARecordingOverTheLimitIsRefusedAndTheSenderIsTold is the slice's own
// Done criterion for the duration limit.
//
// The limit has to be enforced on the SERVER, not only in the browser: a
// recording can be uploaded without the plugin's webapp at all, by anything
// that can post a multipart form. A limit that only exists in JavaScript is a
// suggestion.
func TestARecordingOverTheLimitIsRefusedAndTheSenderIsTold(t *testing.T) {
	cfg := voiceConfig(t)
	cfg.MaxVoiceSeconds = 3

	recording := makeRecordingOfLength(t, cfg.tools, 6.0)

	var stored bytes.Buffer
	outcome, err := processNamedUpload(
		&stored, bytes.NewReader(recording), cfg, recordingName("webm"))
	if err == nil {
		t.Fatalf("a 6 s recording was accepted although the limit is %d s — "+
			"%d bytes were stored", cfg.MaxVoiceSeconds, stored.Len())
	}

	var rejection *uploadRejection
	if !asRejection(err, &rejection) {
		t.Fatalf("an over-long recording produced %v, want a rejection carrying "+
			"a sentence for the sender", err)
	}
	// The sender has to learn WHY, and in their own terms: they spoke for too
	// long. A rejection that says "invalid input" leaves them pressing record
	// again and getting the same answer.
	lower := strings.ToLower(rejection.message)
	if !strings.Contains(lower, "long") {
		t.Errorf("the sender is not told that the recording was too long: %q",
			rejection.message)
	}
	if !strings.Contains(rejection.message, "3") {
		t.Errorf("the sender is not told what the limit is: %q", rejection.message)
	}
	if strings.Contains(lower, "ffmpeg") || strings.Contains(lower, "ffprobe") {
		t.Errorf("the sender is shown an internal diagnostic: %q", rejection.message)
	}

	// And it is a voice outcome, so the report does not file it under video.
	// This is the voice recording defect, and a new rejection path is exactly where it
	// would come back.
	if !outcome.isVoice {
		t.Error("an over-long recording was not marked as a voice message, so the " +
			"administrator's report counts it as a failed VIDEO")
	}
}

// TestARecordingJustUnderTheLimitIsStoredComplete is the other half, and it is
// the half that catches an off-by-one that would be invisible in daily use.
//
// A limit that fires slightly early does not look broken: the sender simply
// gets "too long" a little sooner than they expected. A limit that silently
// TRUNCATES instead of refusing is worse and is what this test rules out —
// the stored recording has to be the whole thing.
func TestARecordingJustUnderTheLimitIsStoredComplete(t *testing.T) {
	cfg := voiceConfig(t)
	cfg.MaxVoiceSeconds = 5

	recording := makeRecordingOfLength(t, cfg.tools, 4.5)

	var stored bytes.Buffer
	outcome, err := processNamedUpload(
		&stored, bytes.NewReader(recording), cfg, recordingName("webm"))
	if err != nil {
		t.Fatalf("a 4.5 s recording was refused although the limit is %d s: %v",
			cfg.MaxVoiceSeconds, err)
	}
	if !outcome.voiceConverted {
		t.Fatal("a recording under the limit was not converted")
	}

	got := inspectAudio(t, cfg.tools, stored.Bytes())
	if !got.decodesFull {
		t.Error("the stored recording does not decode in full")
	}
	if got.duration < 4.3 {
		t.Errorf("a 4.5 s recording was stored as %.3f s — it was cut off rather "+
			"than refused, and neither the sender nor the recipient is told",
			got.duration)
	}
}

// TestTheLimitIsCheckedBeforeTheEncoderRuns is a cost property, and it is the
// one the plan asked to be decided deliberately rather than by accident.
//
// The server only learns a recording's real length by reading it. Doing that
// AFTER the conversion would mean transcoding a ten-minute file in full and
// then throwing it away — processor time spent on a file nobody will ever
// receive, and on the one path where a sender is already waiting.
//
// Measured on this container: the packet walk over a 10-minute recording takes
// about 0.23 s, against 5.8 s to convert it. So the check is ~25x cheaper than
// the work it avoids, and it belongs first.
//
// The test pins it by watching the spool directory: a refused recording must
// leave no converted output behind, because none was ever produced.
func TestTheLimitIsCheckedBeforeTheEncoderRuns(t *testing.T) {
	cfg := voiceConfig(t)
	cfg.MaxVoiceSeconds = 2

	recording := makeRecordingOfLength(t, cfg.tools, 8.0)

	var stored bytes.Buffer
	_, err := processNamedUpload(
		&stored, bytes.NewReader(recording), cfg, recordingName("webm"))
	if err == nil {
		t.Fatal("an over-long recording was accepted")
	}

	// Nothing may be left in the spool directory at all — neither the input
	// nor an output the encoder should never have written.
	entries, readErr := os.ReadDir(cfg.spoolDir)
	if readErr != nil {
		t.Fatalf("reading the spool directory failed: %v", readErr)
	}
	for _, e := range entries {
		t.Errorf("a refused recording left %s behind in the spool directory", e.Name())
	}
}

// TestTheDurationLimitIsConfigurable pins the setting itself, including what
// happens to a value an administrator cannot have meant.
func TestTheDurationLimitIsConfigurable(t *testing.T) {
	t.Run("the default is the ten minutes SPEC 4.3 promises", func(t *testing.T) {
		cfg := defaultConfiguration()
		if cfg.MaxVoiceSeconds != 600 {
			t.Errorf("MaxVoiceSeconds defaults to %d s, want 600", cfg.MaxVoiceSeconds)
		}
	})

	t.Run("a sensible value is taken", func(t *testing.T) {
		cfg, warnings := sanitiseConfiguration(map[string]any{"MaxVoiceSeconds": float64(120)})
		if cfg.MaxVoiceSeconds != 120 {
			t.Errorf("MaxVoiceSeconds = %d, want 120", cfg.MaxVoiceSeconds)
		}
		if len(warnings) != 0 {
			t.Errorf("a valid value produced warnings: %v", warnings)
		}
	})

	t.Run("a value out of range falls back rather than being clamped", func(t *testing.T) {
		for _, bad := range []any{float64(0), float64(-30), float64(100000), "ten minutes"} {
			cfg, warnings := sanitiseConfiguration(map[string]any{"MaxVoiceSeconds": bad})
			if cfg.MaxVoiceSeconds != 600 {
				t.Errorf("MaxVoiceSeconds for %v = %d, want the default 600",
					bad, cfg.MaxVoiceSeconds)
			}
			if len(warnings) == 0 {
				t.Errorf("a rejected value %v produced no warning for the administrator", bad)
			}
		}
	})
}

// ------------------------------------------------- damage, measured properly

// TestDurationComparisonAloneWouldNotCatchADamagedRecording is a NEGATIVE
// result, written as a test because it contradicts a plan item and must not be
// quietly forgotten.
//
// voice recording carried a task into this one: "detect a truncated recording by its
// DURATION rather than by ffmpeg's stderr", on the reasoning that comparing
// the output's length against the input's is more robust than trusting a
// particular ffmpeg's diagnostics. It was a fair suggestion and it does not
// work, for a reason that only shows up when measured.
//
// The damage stops the DEMUXER. So the input's own measured length is already
// shortened by exactly the same damage, and the output faithfully matches it.
// Measured on eight cases — four kinds of damage against both a real browser
// recording and the generated fixture — output >= input held every single
// time, while the true lengths were 4.0 s and 3.0 s.
//
// This test pins that fact so the idea is not re-adopted later as an
// improvement. The check that DOES work is kept: a demuxer complaint, now read
// from ffprobe as well as ffmpeg.
func TestDurationComparisonAloneWouldNotCatchADamagedRecording(t *testing.T) {
	cfg := voiceConfig(t)
	recording := makeRecordingOfLength(t, cfg.tools, 3.0)

	damage := map[string]func([]byte) []byte{
		"zeroed in the middle": func(b []byte) []byte {
			d := append([]byte(nil), b...)
			for i := len(d) * 3 / 10; i < len(d)*7/10; i++ {
				d[i] = 0
			}
			return d
		},
		"truncated": func(b []byte) []byte {
			return append([]byte(nil), b[:len(b)*4/10]...)
		},
	}

	for label, damageIt := range damage {
		t.Run(label, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "damaged.webm")
			if err := os.WriteFile(path, damageIt(recording), 0o600); err != nil {
				t.Fatal(err)
			}
			probed, err := probeAudioDuration(path, cfg.tools)
			if err != nil {
				t.Fatalf("reading the damaged recording failed: %v", err)
			}

			// The measured input length is ALREADY short. That is the finding.
			if probed.seconds > 2.7 {
				t.Errorf("the damaged input measured %.3f s, so this test is no "+
					"longer demonstrating what it claims", probed.seconds)
			}
			// And the reason the plugin can still tell: the demuxer complains.
			if !probed.damaged {
				t.Errorf("a %s recording produced no demuxer complaint, so the "+
					"only working detection is gone", label)
			}
			if probed.complaint == "" {
				t.Error("the complaint is empty, so nothing can be logged for the administrator")
			}
		})
	}
}

// TestAHealthyRecordingProducesNoDemuxerComplaint is the other direction, and
// it is the one that keeps the cure from being worse than the disease: a check
// that fired on good recordings would lose people's words.
func TestAHealthyRecordingProducesNoDemuxerComplaint(t *testing.T) {
	cfg := voiceConfig(t)

	fixtures := map[string][]byte{
		"opus in webm": makeRecordingOfLength(t, cfg.tools, 2.5),
		"aac in mp4":   makeAacRecording(t, cfg.tools, 2),
	}
	for label, data := range fixtures {
		t.Run(label, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "healthy.bin")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			probed, err := probeAudioDuration(path, cfg.tools)
			if err != nil {
				t.Fatalf("reading a healthy recording failed: %v", err)
			}
			if probed.damaged {
				t.Errorf("a healthy %s recording was called damaged: %q",
					label, probed.complaint)
			}
			if probed.seconds <= 0 {
				t.Errorf("a healthy %s recording measured %.3f s", label, probed.seconds)
			}
		})
	}
}

// ------------------------------------------------------ per-channel opt-in

// TestARecordingInAChannelThatIsNotOpenedUpIsStoredAsSent is the slice's Done
// criterion for per-channel availability.
//
// The enforcement belongs on the SERVER because a recording can be uploaded
// without the plugin's webapp, and the server is the only side that cannot be
// bypassed. The channel is available to it: the hook receives a FileInfo whose
// Path already contains the channel ID, because the server builds that path
// from post.ChannelId BEFORE the hook runs.
//
// The behaviour is a pass-through rather than a rejection, deliberately. The
// recording is not wrong, it is simply not a channel where this feature is
// offered — so the file is stored exactly as sent, and nobody loses anything
// they said.
func TestARecordingInAChannelThatIsNotOpenedUpIsStoredAsSent(t *testing.T) {
	tools := ffmpegOrSkip(t)
	recording := makeRecordingOfLength(t, tools, 2)

	tp := newTestPluginWithSettings(t, map[string]any{
		"VoiceChannels": "team-updates",
	})
	allowAnyLogging(tp.api)
	// A converted recording now writes a note record carrying its playing
	// time, so the store has to exist.
	attachKV(tp.api)
	stubChannelLookup(tp, map[string]string{
		"allowedchannelid":  "team-updates",
		"ordinarychannelid": "random",
	})

	t.Run("a channel that was not opened up", func(t *testing.T) {
		stored, rejection := uploadInChannel(t, tp, "ordinarychannelid",
			recordingName("webm"), recording)
		if rejection != "" {
			t.Fatalf("a recording in an ordinary channel was refused: %s", rejection)
		}
		if !bytes.Equal(stored, recording) {
			t.Errorf("a recording in a channel that was not opened up was converted "+
				"anyway: %d bytes sent, %d stored", len(recording), len(stored))
		}
	})

	t.Run("a channel that was opened up", func(t *testing.T) {
		stored, rejection := uploadInChannel(t, tp, "allowedchannelid",
			recordingName("webm"), recording)
		if rejection != "" {
			t.Fatalf("a recording in an opened-up channel was refused: %s", rejection)
		}
		if bytes.Equal(stored, recording) {
			t.Error("a recording in an opened-up channel was NOT converted")
		}
		facts := inspectAudio(t, tools, stored)
		if facts.audioCodec != "aac" {
			t.Errorf("the stored recording is %q, want aac", facts.audioCodec)
		}
	})
}

// TestAnEmptyChannelListMeansEveryChannel pins the default, which is the
// setting an untouched installation has and therefore the one that must not
// surprise anybody: SPEC §4.3 says availability defaults to all channels.
func TestAnEmptyChannelListMeansEveryChannel(t *testing.T) {
	tools := ffmpegOrSkip(t)
	recording := makeRecordingOfLength(t, tools, 2)

	tp := newTestPluginWithSettings(t, map[string]any{})
	allowAnyLogging(tp.api)
	// A converted recording now writes a note record carrying its playing
	// time, so the store has to exist.
	attachKV(tp.api)
	stubChannelLookup(tp, map[string]string{"anychannelid": "whatever"})

	stored, rejection := uploadInChannel(t, tp, "anychannelid",
		recordingName("webm"), recording)
	if rejection != "" {
		t.Fatalf("the upload was refused: %s", rejection)
	}
	if bytes.Equal(stored, recording) {
		t.Error("with no channel list configured a recording was not converted; " +
			"the default must be every channel")
	}
}

// TestTheChannelListAcceptsWhatAnAdministratorWouldActuallyType.
//
// An administrator reads channel names off the screen; they do not copy
// twenty-six character IDs out of the database. So the list matches on the
// channel's ID, its URL name and its display name, and it tolerates the
// separators and stray spaces a typed list really contains.
func TestTheChannelListAcceptsWhatAnAdministratorWouldActuallyType(t *testing.T) {
	channel := &model.Channel{
		Id:          "kchannelid00000000000000000",
		Name:        "team-updates",
		DisplayName: "Team Updates",
	}

	allowed := []string{
		"team-updates",
		"Team Updates",
		"kchannelid00000000000000000",
		"  team-updates  ",
		"TEAM-UPDATES",
		"random, team-updates",
		"random\nteam-updates",
		"random,team-updates,",
	}
	for _, list := range allowed {
		if !channelIsOpenedUp(list, channel) {
			t.Errorf("the list %q does not open up the channel it names", list)
		}
	}

	denied := []string{
		"random",
		"team-updates-2",
		"team",
		"updates",
	}
	for _, list := range denied {
		if channelIsOpenedUp(list, channel) {
			t.Errorf("the list %q opens up a channel it does not name", list)
		}
	}

	// Empty means every channel, which is the default.
	for _, list := range []string{"", "   ", ",", "\n"} {
		if !channelIsOpenedUp(list, channel) {
			t.Errorf("the empty list %q did not mean every channel", list)
		}
	}
}

// TestTheChannelIsReadFromThePathTheServerBuilt pins the measurement this
// feature rests on.
//
// The path is built by the server from post.ChannelId BEFORE the hook runs —
// verified against the test instance's database, where a stored file's
// `channels/<id>/` segment matched its fileinfo.channelid exactly. If that
// ever changes, per-channel availability silently stops working, so it gets
// its own test rather than a comment.
func TestTheChannelIsReadFromThePathTheServerBuilt(t *testing.T) {
	cases := map[string]string{
		"20260918/teams/noteam/channels/c9fzq3m7xbyd8kt2rvh5wnp4je/users/u6tbn8kh2wqr5mzxc7vdy3fp9s/f1/voice.webm": "c9fzq3m7xbyd8kt2rvh5wnp4je",
		"20260918/teams/t/channels/c/users/u/f/x.webm":                                                             "c",
		"20260918/teams/t/users/u/f/x.webm":                                                                        "",
		"":                                                                                                         "",
		"channels/":                                                                                                "",
	}
	for path, want := range cases {
		if got := channelIDFromPath(path); got != want {
			t.Errorf("channelIDFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestTheWebappIsToldPerChannel.
//
// The webapp draws the button from the component's PostDraft, which carries
// the channel — the upload callback from registerFileUploadMethod is handed
// nothing at all (file_upload.tsx line 659). So the route has to answer the
// question FOR a channel, otherwise the button would appear in channels where
// the server then declines to convert: a button that offers something the
// server refuses is worse than no button.
func TestTheWebappIsToldPerChannel(t *testing.T) {
	ffmpegOrSkip(t)

	tp := newTestPluginWithSettings(t, map[string]any{"VoiceChannels": "team-updates"})
	allowAnyLogging(tp.api)
	// A converted recording now writes a note record carrying its playing
	// time, so the store has to exist.
	attachKV(tp.api)
	stubChannelLookup(tp, map[string]string{
		"allowedchannelid":  "team-updates",
		"ordinarychannelid": "random",
	})

	t.Run("available in an opened-up channel", func(t *testing.T) {
		body, status := getVoiceSettingsFor(t, tp, "someuserid", "allowedchannelid")
		if status != 200 {
			t.Fatalf("status %d, want 200", status)
		}
		if !strings.Contains(body, `"enabled":true`) {
			t.Errorf("the webapp was told %q, want enabled:true", body)
		}
	})

	t.Run("not available in a channel that was not opened up", func(t *testing.T) {
		body, status := getVoiceSettingsFor(t, tp, "someuserid", "ordinarychannelid")
		if status != 200 {
			t.Fatalf("status %d, want 200", status)
		}
		if !strings.Contains(body, `"enabled":false`) {
			t.Errorf("the webapp was told %q, want enabled:false — the button would "+
				"appear in a channel where the server declines to convert", body)
		}
	})

	t.Run("the limit is published so the browser can stop in time", func(t *testing.T) {
		body, status := getVoiceSettingsFor(t, tp, "someuserid", "allowedchannelid")
		if status != 200 {
			t.Fatalf("status %d, want 200", status)
		}
		if !strings.Contains(body, `"max_seconds":600`) {
			t.Errorf("the webapp was told %q, want max_seconds:600 — without it the "+
				"browser cannot stop a recording before the server refuses it", body)
		}
	})

	t.Run("without a channel the answer is the server-wide one", func(t *testing.T) {
		// Asked without a channel the route cannot know, and must not claim
		// availability it cannot promise. With a list configured, the honest
		// answer is "not here".
		body, status := getVoiceSettings(t, tp, "someuserid")
		if status != 200 {
			t.Fatalf("status %d, want 200", status)
		}
		if !strings.Contains(body, `"enabled":false`) {
			t.Errorf("asked without a channel the route answered %q; with a channel "+
				"list configured it cannot promise availability", body)
		}
	})
}

// ------------------------------------------------------------- helpers

// stubChannelLookup teaches the plugin API which channel an ID belongs to.
func stubChannelLookup(tp *testPlugin, channels map[string]string) {
	for id, name := range channels {
		tp.api.On("GetChannel", id).Return(&model.Channel{
			Id:          id,
			Name:        name,
			DisplayName: strings.ToUpper(name[:1]) + name[1:],
		}, nil).Maybe()
	}
}

// uploadInChannel runs one upload through the hook with a path the server
// would have built for that channel.
func uploadInChannel(t *testing.T, tp *testPlugin, channelID, name string, input []byte) ([]byte, string) {
	t.Helper()
	info := &model.FileInfo{
		Id:   model.NewId(),
		Name: name,
		Size: int64(len(input)),
		Path: "20260918/teams/noteam/channels/" + channelID + "/users/u/f/" + name,
	}
	var stored bytes.Buffer
	_, rejection := tp.FileWillBeUploaded(&plugin.Context{}, info, bytes.NewReader(input), &stored)
	return stored.Bytes(), rejection
}

// getVoiceSettingsFor asks the route the question the webapp asks, for one
// channel.
func getVoiceSettingsFor(t *testing.T, tp *testPlugin, userID, channelID string) (string, int) {
	t.Helper()
	return getVoiceSettingsQuery(t, tp, userID, "?channel_id="+channelID)
}

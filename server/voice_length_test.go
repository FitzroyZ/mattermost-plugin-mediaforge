package main

// Tests for the spoken length shown on the post.
//
// The complaint this answers is "I don't know what I'm letting myself in for".
// A voice attachment gives no hint of its length until it is playing, so a
// reader cannot decide whether to listen now or later. The length is already
// measured -- probeAudioDuration reads it to enforce MaxVoiceSeconds -- and
// was then thrown away.
//
// WHY THE SERVER AND NOT THE WEBAPP. The number has to reach a phone, and the
// mobile apps load no plugin webapp code. Putting it on the post itself is the
// same mechanism the size note already uses for exactly that reason.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
)

// TestTheSpokenLengthIsRenderedForAReader pins the wording a person reads.
//
// It is written against examples rather than a format string, because the
// thing that can regress here is legibility, not arithmetic.
func TestTheSpokenLengthIsRenderedForAReader(t *testing.T) {
	cases := map[float64]string{
		0:    "",     // not a recording, or not measured: say nothing
		0.4:  "0:01", // never "0:00" -- something was said
		1:    "0:01",
		9:    "0:09",
		59:   "0:59",
		60:   "1:00",
		61:   "1:01",
		125:  "2:05",
		600:  "10:00",
		3600: "60:00", // an hour is not special enough for a third field
		3661: "61:01",
	}
	for seconds, want := range cases {
		if got := clockLength(seconds); got != want {
			t.Errorf("clockLength(%v) = %q, want %q", seconds, got, want)
		}
	}
}

// TestTheLengthNeverRoundsDownToNothing is its own test because the rounding
// direction is the part that produces an absurd result.
//
// A recording of 0.4 s rounds to zero by the obvious arithmetic, and "0:00"
// next to an attachment that plainly contains speech reads like a defect.
func TestTheLengthNeverRoundsDownToNothing(t *testing.T) {
	for _, seconds := range []float64{0.1, 0.4, 0.5, 0.9} {
		if got := clockLength(seconds); got != "0:01" {
			t.Errorf("clockLength(%v) = %q, want %q -- a short recording "+
				"must not be reported as no recording", seconds, got, "0:01")
		}
	}
}

// TestALengthIsShownOnTheVoiceNote checks the note a reader actually sees.
func TestALengthIsShownOnTheVoiceNote(t *testing.T) {
	records := []noteRecord{{
		Name:     "mediaforge-voice-20260920-101500.m4a",
		Original: 300000,
		Stored:   120000,
		Seconds:  125,
	}}

	for _, style := range []string{noteStyleLine, noteStyleCompact} {
		note := messageNote(style, records)
		if !strings.Contains(note, "2:05") {
			t.Errorf("style %q: the length is missing from %q", style, note)
		}
	}
	for _, style := range []string{noteStyleBlock, noteStyleFooter} {
		attachment := attachmentNote(style, records)
		if attachment == nil {
			t.Fatalf("style %q produced no attachment", style)
		}
		if !strings.Contains(attachment.Text, "2:05") {
			t.Errorf("style %q: the length is missing from %q", style, attachment.Text)
		}
	}
}

// TestAFileThatIsNotARecordingCarriesNoLength is the other half of the
// contract, and the one that breaks quietly.
//
// Every compressed photo goes through the same renderer. A zero Seconds field
// must produce no length at all rather than "0:00", or every image in the
// server grows a meaningless clock.
func TestAFileThatIsNotARecordingCarriesNoLength(t *testing.T) {
	records := []noteRecord{{Name: "holiday.jpg", Original: 11906359, Stored: 2323820}}

	for _, style := range []string{noteStyleLine, noteStyleCompact} {
		note := messageNote(style, records)
		if strings.Contains(note, ":") && strings.Contains(note, "0:0") {
			t.Errorf("style %q: a photograph was given a playing time: %q", style, note)
		}
	}
	for _, style := range []string{noteStyleBlock, noteStyleFooter} {
		attachment := attachmentNote(style, records)
		if attachment == nil {
			t.Fatalf("style %q produced no attachment", style)
		}
		if strings.Contains(attachment.Text, "0:0") {
			t.Errorf("style %q: a photograph was given a playing time: %q",
				style, attachment.Text)
		}
	}
}

// TestTheLengthSurvivesTheRoundTripThroughTheStoredRecord is the test that
// would have caught the field being added to the struct and never persisted.
//
// The record is written at upload time under the file's ID and read back in a
// different hook, as JSON. A field without a tag, or one dropped on the way,
// leaves the length correct in memory and absent on screen -- a failure that
// no unit test of the renderer can see.
func TestTheLengthSurvivesTheRoundTripThroughTheStoredRecord(t *testing.T) {
	original := []noteRecord{{
		Name: "mediaforge-voice-20260920-101500.m4a", Original: 300000,
		Stored: 120000, Seconds: 125,
	}}

	encoded := encodeRecords(original)
	if encoded == "" {
		t.Fatal("the records did not encode at all")
	}
	if !strings.Contains(encoded, "125") {
		t.Errorf("the length is not in the encoded record: %s", encoded)
	}
}

// TestAVoiceRecordingGetsARecordAtAll is the test that found the real gap,
// and it is the one worth having.
//
// Everything above renders a length that is already in a noteRecord. This asks
// the question one layer down: does a voice recording ever GET a record?
//
// It did not. recordSaving returns early unless outcome.reEncoded is set, and
// a voice conversion deliberately does not set it — Opus in, AAC out usually
// makes the file LARGER, and reporting that as a compression would put a
// negative saving into the administrator's report. Entirely correct for the
// saving, and it meant the length had nowhere to travel.
//
// So the record is now also written for a converted recording, carrying the
// length and nothing else. The saving figures stay out of it: Original and
// Stored are left zero so no renderer can claim a compression that did not
// happen, which is the property the early return was protecting.
func TestAVoiceRecordingGetsARecordAtAll(t *testing.T) {
	api := &plugintest.API{}
	allowAnyLogging(api)
	store := attachKV(api)
	p := &MediaForgePlugin{}
	p.SetAPI(api)

	info := &model.FileInfo{Id: "voicefileid1234567890abcde", Name: "mediaforge-voice-1.m4a"}
	p.recordSaving(info, processOutcome{
		isVoice:        true,
		voiceConverted: true,
		bytesWritten:   120000,
		originalSize:   100000, // a conversion that grew the file, as they do
		spokenSeconds:  125,
	})

	raw, ok := store.get(noteKeyPrefix + info.Id)
	if !ok {
		t.Fatal("a converted voice recording stored no record, so its length " +
			"can never reach the post")
	}
	var got noteRecord
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the stored record does not parse: %v", err)
	}
	if got.Seconds != 125 {
		t.Errorf("the stored length is %v, want 125", got.Seconds)
	}
}

// TestAVoiceRecordingNeverClaimsASaving guards the property the early return
// used to provide for free.
//
// A voice conversion usually makes the file bigger. If the record carried
// Original and Stored, every note would announce a negative or nonsense
// saving, and the administrator's report — which the statistics slice exists
// to make trustworthy — would be fed a growth as if it were a compression.
func TestAVoiceRecordingNeverClaimsASaving(t *testing.T) {
	api := &plugintest.API{}
	allowAnyLogging(api)
	store := attachKV(api)
	p := &MediaForgePlugin{}
	p.SetAPI(api)

	info := &model.FileInfo{Id: "voicefileid1234567890abcdf", Name: "mediaforge-voice-2.m4a"}
	p.recordSaving(info, processOutcome{
		isVoice:        true,
		voiceConverted: true,
		bytesWritten:   140000,
		originalSize:   100000,
		spokenSeconds:  30,
	})

	raw, ok := store.get(noteKeyPrefix + info.Id)
	if !ok {
		t.Fatal("the converted recording stored no record")
	}
	var got noteRecord
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the stored record does not parse: %v", err)
	}
	if got.Original != 0 || got.Stored != 0 {
		t.Errorf("a voice recording recorded sizes %d -> %d; it must record none, "+
			"because the conversion usually grows the file", got.Original, got.Stored)
	}

	note := attachmentNote(noteStyleFooter, []noteRecord{got})
	if note == nil {
		t.Fatal("the recording produced no note at all")
	}
	if strings.Contains(note.Text, "smaller") || strings.Contains(note.Text, "saved") {
		t.Errorf("the note claims a saving for a voice recording: %q", note.Text)
	}
	if !strings.Contains(note.Text, "0:30") {
		t.Errorf("the note does not carry the length: %q", note.Text)
	}
}

// TestAVoiceRecordingThatWasNotConvertedRecordsNothing keeps the note contract
// intact.
//
// A recording stored exactly as it was sent — no ffmpeg, a channel without the
// feature — must carry NO note, because absence of a note is the promise that
// the stored file is the original.
func TestAVoiceRecordingThatWasNotConvertedRecordsNothing(t *testing.T) {
	api := &plugintest.API{}
	allowAnyLogging(api)
	store := attachKV(api)
	p := &MediaForgePlugin{}
	p.SetAPI(api)

	info := &model.FileInfo{Id: "voicefileid1234567890abcdg", Name: "mediaforge-voice-3.webm"}
	p.recordSaving(info, processOutcome{isVoice: true, bytesWritten: 100000})

	if _, ok := store.get(noteKeyPrefix + info.Id); ok {
		t.Error("a recording stored as sent was given a note, breaking the promise " +
			"that no note means the file is untouched")
	}
}

// TestTheLengthReachesTheRecordThroughTheREALHook is the test that the first
// attempt at this feature was missing, and the one that would have saved a
// round on the live instance.
//
// Everything else in this file tests recordSaving and the renderers directly.
// All of it passed while nothing whatsoever appeared on a real server,
// because FileWillBeUploaded returns EARLY for a converted recording — it is
// the one outcome that is neither "compressed" nor "stored as sent" — and the
// recordSaving call sat several branches below that return.
//
// A unit test of a function cannot see a caller that never calls it. This
// drives the hook itself, with real ffmpeg, and asks the key-value store what
// arrived.
func TestTheLengthReachesTheRecordThroughTheREALHook(t *testing.T) {
	tools := ffmpegOrSkip(t)
	tp := newTestPluginWithSettings(t, map[string]any{})
	allowAnyLogging(tp.api)
	store := attachKV(tp.api)

	const spokenSeconds = 3
	recording := makeRecording(t, tools, spokenSeconds)

	name := recordingName("webm")
	info := &model.FileInfo{
		Id: model.NewId(), Name: name, Extension: "webm", MimeType: "video/webm",
		Size: int64(len(recording)),
		Path: "20260918/teams/t/channels/c/users/u/" + name,
	}

	var out writeCounter
	if _, rejection := tp.FileWillBeUploaded(
		&plugin.Context{}, info, bytesReaderOf(recording), &out,
	); rejection != "" {
		t.Fatalf("the recording was refused: %s", rejection)
	}

	raw, ok := store.get(noteKeyPrefix + info.Id)
	if !ok {
		t.Fatal("the hook stored no record for a converted recording, so its " +
			"playing time can never reach the post — this is the defect the " +
			"unit tests could not see")
	}

	var got noteRecord
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the stored record does not parse: %v", err)
	}
	if got.Seconds < spokenSeconds-1 || got.Seconds > spokenSeconds+1 {
		t.Errorf("the recorded length is %v, want about %d", got.Seconds, spokenSeconds)
	}
	if got.Original != 0 || got.Stored != 0 {
		t.Errorf("the hook recorded sizes %d -> %d for a recording; a conversion "+
			"is not a saving", got.Original, got.Stored)
	}

	// And the note a reader would actually see.
	note := attachmentNote(noteStyleFooter, []noteRecord{got})
	if note == nil || !strings.Contains(note.Text, ":") {
		t.Errorf("the note carries no playing time: %+v", note)
	}
}

// TestATrimmedRecordingReportsItsSTOREDLengthThroughTheHook reproduces what
// the operator saw in daily use, through the real hook.
//
// He sent a 23-second recording with pause-shortening on. The post said 0:23;
// the stored file was 0:15. Both numbers were true and the wrong one was
// shown, because the duration probe runs before the encode — where it has to,
// so MaxVoiceSeconds refuses a recording for what the sender spoke rather than
// for what survives a setting they cannot see.
//
// This is deliberately the hook rather than the pipeline function: the same
// distance between "the value is correct" and "the value reaches the post"
// already hid one defect in this feature.
func TestATrimmedRecordingReportsItsSTOREDLengthThroughTheHook(t *testing.T) {
	tools := ffmpegOrSkip(t)
	tp := newTestPluginWithSettings(t, map[string]any{"TrimVoiceSilence": true})
	allowAnyLogging(tp.api)
	store := attachKV(tp.api)

	// Nine seconds in, with a three-second pause in the middle that the
	// trimming takes most of out.
	recording := makeRecordingWithPause(t, tools)

	name := recordingName("webm")
	info := &model.FileInfo{
		Id: model.NewId(), Name: name, Extension: "webm", MimeType: "video/webm",
		Size: int64(len(recording)),
		Path: "20260918/teams/t/channels/c/users/u/" + name,
	}

	var out writeCounter
	if _, rejection := tp.FileWillBeUploaded(
		&plugin.Context{}, info, bytesReaderOf(recording), &out,
	); rejection != "" {
		t.Fatalf("the recording was refused: %s", rejection)
	}

	raw, ok := store.get(noteKeyPrefix + info.Id)
	if !ok {
		t.Fatal("no record was stored for the recording")
	}
	var got noteRecord
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the stored record does not parse: %v", err)
	}

	// The fixture is 9 s spoken. Trimmed it is around 6 s. The defect showed
	// the spoken figure, so anything at or above 8 s is the bug returning.
	if got.Seconds >= 8 {
		t.Errorf("the note reports %.2fs, which is the length BEFORE the pauses "+
			"were shortened; a reader would see a number the stored file does "+
			"not have", got.Seconds)
	}
	if got.Seconds <= 0 {
		t.Errorf("the note reports %.2fs, so no length reaches the post at all",
			got.Seconds)
	}

	// And it must still be a plausible length rather than a collapsed one.
	if got.Seconds < 4 {
		t.Errorf("the note reports %.2fs for a fixture containing 6 s of tone; "+
			"the measurement is reading the wrong file", got.Seconds)
	}
}

// makeRecordingWithPause builds a recording whose middle is silent, so that
// shortening the pause changes its length measurably.
func makeRecordingWithPause(t *testing.T, tools mediaTools) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "withpause.webm")
	cmd := exec.Command(tools.FFmpegPath,
		"-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", "sine=frequency=220:duration=3",
		"-f", "lavfi", "-i", "anullsrc=r=48000:cl=mono:d=3",
		"-f", "lavfi", "-i", "sine=frequency=300:duration=3",
		"-filter_complex", "[0][1][2]concat=n=3:v=0:a=1",
		"-c:a", "libopus", "-b:a", "64k", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("could not build the fixture: %v\n%s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read the fixture: %v", err)
	}
	return data
}

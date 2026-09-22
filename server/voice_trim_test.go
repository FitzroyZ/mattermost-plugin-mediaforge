package main

// Tests for trimming long pauses out of a recording.
//
// The complaint is that voice messages take too long to listen to. Most of the
// obvious answers work on the LISTENER after the fact — play faster, skim a
// transcript. This one removes the part nobody wanted in the first place:
// measured on a recording with realistic pauses, 12.50 s became 9.25 s, at a
// cost of 7 seconds of processing on a ten-minute file.
//
// It is OFF by default, and that is not timidity. Removing silence alters what
// somebody said, irreversibly, and a hesitation can itself be information. An
// administrator has to choose it.

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSilenceTrimmingIsOffUnlessAskedFor pins the default.
//
// The plugin's other defaults are all "do the useful thing": compress images,
// compress videos, convert recordings. This one is the exception, because it
// is the only one that removes content rather than re-encoding it.
func TestSilenceTrimmingIsOffUnlessAskedFor(t *testing.T) {
	cfg := defaultConfiguration()
	if cfg.TrimVoiceSilence {
		t.Error("silence trimming defaults to ON; it alters what somebody said " +
			"and must be an administrator's deliberate choice")
	}

	args := voiceEncoderArgs("/tmp/in.webm", "/tmp/out.m4a", false)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "silenceremove") {
		t.Errorf("the encoder trims silence without being asked: %s", joined)
	}
	if strings.Contains(joined, "-af") {
		t.Errorf("an audio filter was added when none was wanted: %s", joined)
	}
}

// TestSilenceTrimmingIsAppliedWhenAskedFor is the other half.
func TestSilenceTrimmingIsAppliedWhenAskedFor(t *testing.T) {
	args := voiceEncoderArgs("/tmp/in.webm", "/tmp/out.m4a", true)
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "silenceremove") {
		t.Fatalf("trimming was requested and not applied: %s", joined)
	}
	if !strings.Contains(joined, "-af") {
		t.Errorf("the filter is not passed as an audio filter: %s", joined)
	}

	// Everything the unfiltered command does must survive. A filter added by
	// replacing an argument rather than adding one would silently drop the
	// mono flag or the faststart index.
	for _, required := range []string{"-vn", "-map 0:a:0", "-c:a aac", "-ac 1",
		"-movflags +faststart", "-f mp4", "-err_detect explode"} {
		if !strings.Contains(joined, required) {
			t.Errorf("trimming dropped %q from the command: %s", required, joined)
		}
	}
}

// TestTheSilenceFilterShortensPausesRatherThanRemovingThem is the decision
// this slice makes, expressed as a test.
//
// `stop_duration` is what separates "shortened" from "deleted". Set to zero,
// every pause collapses completely and the speaker sounds like they are
// gasping. A pause carries meaning — hesitation, a breath, the end of a
// thought — so the filter leaves some of it standing.
func TestTheSilenceFilterShortensPausesRatherThanRemovingThem(t *testing.T) {
	args := voiceEncoderArgs("/tmp/in.webm", "/tmp/out.m4a", true)

	var filter string
	for i, a := range args {
		if a == "-af" && i+1 < len(args) {
			filter = args[i+1]
		}
	}
	if filter == "" {
		t.Fatal("no audio filter was produced")
	}

	if !strings.Contains(filter, "stop_duration=") {
		t.Errorf("the filter removes pauses entirely instead of shortening them: %s", filter)
	}
	// Matched with the delimiter, not as a prefix: "stop_duration=0.4" starts
	// with "stop_duration=0" and a naive substring check calls a correct
	// filter broken. The failure that matters is a duration of exactly zero.
	if strings.Contains(filter, "stop_duration=0:") || strings.HasSuffix(filter, "stop_duration=0") {
		t.Errorf("the filter collapses every pause, which makes speech sound "+
			"breathless: %s", filter)
	}
	// Only trailing/intermediate silence, never the whole signal: a
	// `start_periods` trim would clip the first word of a recording that
	// begins quietly.
	if strings.Contains(filter, "start_periods") {
		t.Errorf("the filter trims the START of a recording, which clips the "+
			"first word when somebody begins softly: %s", filter)
	}
}

// TestTheSilenceFilterIsNotBuiltFromConfiguration is a security property, and
// it is the reason this is a constant rather than a setting with numbers.
//
// The audit established that no externally chosen value may reach an ffmpeg
// argument. A threshold or duration exposed in the System Console would be a
// new way for text to arrive on that command line. The switch is a boolean;
// the filter is a literal.
func TestTheSilenceFilterIsNotBuiltFromConfiguration(t *testing.T) {
	on := voiceEncoderArgs("/tmp/in.webm", "/tmp/out.m4a", true)
	alsoOn := voiceEncoderArgs("/tmp/other.webm", "/tmp/other.m4a", true)

	var first, second string
	for i, a := range on {
		if a == "-af" {
			first = on[i+1]
		}
	}
	for i, a := range alsoOn {
		if a == "-af" {
			second = alsoOn[i+1]
		}
	}
	if first != second {
		t.Errorf("the filter differs between calls (%q vs %q); it must be a "+
			"literal, so no external value can reach ffmpeg", first, second)
	}

	for _, forbidden := range []string{";", "|", "&&", "$(", "`", "\n", "/etc/"} {
		if strings.Contains(first, forbidden) {
			t.Errorf("the filter contains %q, which has no business on a "+
				"command line: %s", forbidden, first)
		}
	}
}

// TestTheReportedLengthIsTheSTOREDLengthNotTheRecordedOne is the defect the operator
// found in daily use, pinned.
//
// He sent a 23-second recording with trimming on, and the post said 0:23 while
// the stored file was 0:15. Both numbers were true and the wrong one was
// shown: the duration probe runs BEFORE the conversion, because that is where
// MaxVoiceSeconds has to be enforced — a recording must be refused for what
// the sender spoke, not for what is left after the pauses come out.
//
// So the length is now read again AFTER the encode, from the file that was
// actually stored, and that is the figure the post carries. A reader deciding
// whether to listen cares about the file in front of them.
//
// Cheap, and measured rather than assumed: the converted MP4 carries its
// duration in the header, so this is a header read (0.078 s on a ten-minute
// file) and not the packet walk the input needs.
func TestTheReportedLengthIsTheSTOREDLengthNotTheRecordedOne(t *testing.T) {
	tools := ffmpegOrSkip(t)

	// A recording with pauses long enough that trimming changes the length
	// measurably, built the way the proof script builds one.
	spool := filepath.Join(t.TempDir(), "recording.webm")
	build := exec.Command(tools.FFmpegPath,
		"-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", "sine=frequency=220:duration=3",
		"-f", "lavfi", "-i", "anullsrc=r=48000:cl=mono:d=3",
		"-f", "lavfi", "-i", "sine=frequency=300:duration=3",
		"-filter_complex", "[0][1][2]concat=n=3:v=0:a=1",
		"-c:a", "libopus", "-b:a", "64k", spool)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("could not build the fixture: %v\n%s", err, out)
	}

	out := filepath.Join(t.TempDir(), "out.m4a")
	cmd := exec.Command(tools.FFmpegPath, voiceEncoderArgs(spool, out, true)...)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the trimmed encode failed: %v\n%s", err, combined)
	}

	stored, err := probeStoredSeconds(out, tools)
	if err != nil {
		t.Fatalf("the stored length could not be read: %v", err)
	}

	// The input is 9 s (3 + 3 + 3). Trimming takes most of the middle pause
	// out, so the stored file must be clearly shorter than what was spoken.
	if stored <= 0 {
		t.Fatalf("the stored length came back as %v", stored)
	}
	if stored > 8 {
		t.Errorf("the stored length is %.2fs, which is the length BEFORE trimming; "+
			"the post would show a number the file does not have", stored)
	}
	if stored < 5 {
		t.Errorf("the stored length is %.2fs, which is less speech than the "+
			"fixture contains — the measurement is reading the wrong thing", stored)
	}
}

// TestAnUntrimmedRecordingReportsTheSameLengthEitherWay guards the other
// direction.
//
// With trimming off, the stored length and the spoken length are the same
// thing, so switching the measurement to the output must not move the number.
// If it did, every voice message on every installation that never touches the
// new setting would start showing a different figure than before.
func TestAnUntrimmedRecordingReportsTheSameLengthEitherWay(t *testing.T) {
	tools := ffmpegOrSkip(t)

	spool := filepath.Join(t.TempDir(), "recording.webm")
	build := exec.Command(tools.FFmpegPath,
		"-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", "sine=frequency=220:duration=4",
		"-c:a", "libopus", "-b:a", "64k", spool)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("could not build the fixture: %v\n%s", err, out)
	}

	spoken, err := probeAudioDuration(spool, tools)
	if err != nil {
		t.Fatalf("the spoken length could not be read: %v", err)
	}

	out := filepath.Join(t.TempDir(), "out.m4a")
	cmd := exec.Command(tools.FFmpegPath, voiceEncoderArgs(spool, out, false)...)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the encode failed: %v\n%s", err, combined)
	}

	stored, err := probeStoredSeconds(out, tools)
	if err != nil {
		t.Fatalf("the stored length could not be read: %v", err)
	}

	if diff := stored - spoken.seconds; diff > 0.5 || diff < -0.5 {
		t.Errorf("an untrimmed recording reports %.2fs stored against %.2fs spoken; "+
			"the two must agree when nothing was removed", stored, spoken.seconds)
	}
}

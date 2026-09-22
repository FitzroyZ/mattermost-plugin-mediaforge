package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// audioDuration is what the plugin could learn about a recording's length by
// reading it, plus whether the demuxer had anything to complain about on the
// way.
//
// The two travel together because they come from the same pass over the file.
// A separate probe for each would double the cost of the one operation this
// slice performs on every single recording.
type audioDuration struct {
	// seconds is the recording's real length, derived from the packets.
	seconds float64
	// damaged is true when the demuxer complained while reading — which is
	// what a recording that cannot be read in full looks like. See
	// probeAudioDuration for why this is not the same question as "is it
	// shorter than expected".
	damaged bool
	// complaint is what the demuxer said, for the server log. It is never
	// shown to a sender: it names byte offsets and EBML elements.
	complaint string
}

// probeAudioDuration reads how long a recording really is.
//
// # THE MEASUREMENT THIS RESTS ON, AND THE SHORTCUT THAT IS A TRAP
//
// A browser's MediaRecorder writes NO duration into its header. Measured on
// the test instance against a real Chromium recording: `format=duration` and
// `stream=duration` both come back as N/A. So the length cannot be read, it
// has to be derived — and the derivation everyone reaches for first is wrong.
//
// "Packet count × 20 ms, the Opus default" gives 1.3 s for a file that is
// really 3.959 s, because Chromium writes 60 ms packets, not 20 ms ones. A
// duration limit built on that number would refuse recordings at a third of
// the length the administrator configured, and the symptom — "voice messages
// cut off early" — would point at the encoder rather than at the arithmetic.
// This project already made that mistake once, in the voice recording proof script.
//
// What the file actually says is in the packet timestamps: the last packet's
// presentation time plus that packet's own duration. That is read here.
//
// WHY THE DEMUXER'S COMPLAINT IS READ AT THE SAME TIME
//
// voice recording carried a task into this slice: replace the damage check — which
// currently trusts that ffmpeg writes nothing on stderr for a healthy file —
// with a comparison of the output's duration against the input's, on the
// grounds that a duration is a fact and a diagnostic is a habit of one ffmpeg
// build. The idea is sound in principle and it does not work, which was
// measured before anything was written:
//
//	                      true length   input measures   output measures
//	zeroed in the middle       3.959 s          3.959 s           3.959 s
//	zeroed to the end          3.959 s          1.021 s           1.021 s
//	truncated to 40 %          3.959 s          1.564 s           1.564 s
//	scrambled payload          3.959 s          1.323 s           1.322 s
//
// The damage stops the DEMUXER, so the input's own measured length is already
// shortened by exactly the same damage — and the output matches it faithfully.
// Output-versus-input was satisfied in all eight measured cases (four kinds of
// damage against both a real browser recording and the generated fixture)
// while the recording had in fact lost most of itself. The comparison cannot
// work, because there is no undamaged reading of a damaged file to compare
// against.
//
// What DOES survive the damage is the complaint. Both tools produce one:
// ffprobe while walking the packets here, and ffmpeg while converting. Reading
// it here is a genuine improvement over voice recording all the same — it is now a
// second, independent tool saying so, on a pass the plugin has to make anyway,
// and it fires BEFORE the encoder rather than after it.
func probeAudioDuration(path string, tools mediaTools) (audioDuration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, tools.FFprobePath,
		"-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "packet=pts_time,duration_time",
		"-of", "csv=p=0",
		path)
	isolateProcessGroup(cmd)

	var stderr strings.Builder
	cmd.Stderr = limitedWriter{w: &stderr, limit: 4096}

	out, err := cmd.Output()
	if err != nil {
		return audioDuration{}, fmt.Errorf("ffprobe could not read the recording: %w", err)
	}

	var last float64
	for _, line := range strings.Split(string(out), "\n") {
		startText, durationText, found := strings.Cut(strings.TrimSpace(line), ",")
		if !found {
			continue
		}
		start, startErr := strconv.ParseFloat(strings.TrimSpace(startText), 64)
		if startErr != nil {
			// "N/A" and blank lines both land here. A packet without a
			// timestamp says nothing about the length, so it is skipped
			// rather than counted as zero — counting it would drag the
			// measurement down and refuse good recordings.
			continue
		}
		// A packet without its own duration still fixes a lower bound: the
		// recording is at least as long as that packet's start.
		end := start
		if d, durErr := strconv.ParseFloat(strings.TrimSpace(durationText), 64); durErr == nil {
			end += d
		}
		if end > last {
			last = end
		}
	}

	complaint := strings.TrimSpace(stderr.String())
	return audioDuration{
		seconds:   last,
		damaged:   complaint != "",
		complaint: complaint,
	}, nil
}

// probeStoredSeconds reads how long the file that was actually STORED plays.
//
// # WHY THIS EXISTS ALONGSIDE probeAudioDuration, WHICH ALSO MEASURES A LENGTH
//
// They answer different questions about different files, and conflating them
// produced a defect found in daily use: a 23-second recording was sent with
// pause-shortening on, and the post said 0:23 while the stored file was 0:15.
// Both numbers were true; the wrong one was shown.
//
//   - probeAudioDuration measures the INPUT, before the encode, because that
//     is where MaxVoiceSeconds has to be enforced. A recording must be refused
//     for what the sender spoke — refusing it for what survives after the
//     pauses come out would mean the limit moves with a setting the sender
//     cannot see.
//   - this measures the OUTPUT, after the encode, because that is the file the
//     reader is looking at. Somebody deciding whether to listen now or later
//     cares about the attachment in front of them, not about what went into
//     the encoder.
//
// It is a HEADER read, not the packet walk the input needs. A browser's
// MediaRecorder writes no duration at all, which is why the input costs a full
// pass; the MP4 this plugin writes carries its duration in the header, and
// `-movflags +faststart` puts that header at the front. Measured on a
// ten-minute file: 0.078 s, against 0.23 s for the packet walk.
//
// A failure is not fatal and must not be. The file is already converted and
// about to be stored; losing it because a second probe failed would be absurd,
// so the caller keeps the length it already has.
func probeStoredSeconds(path string, tools mediaTools) (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, tools.FFprobePath,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "csv=p=0",
		path)
	isolateProcessGroup(cmd)

	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe could not read the stored recording: %w", err)
	}

	text := strings.TrimSpace(string(out))
	seconds, parseErr := strconv.ParseFloat(text, 64)
	if parseErr != nil || seconds <= 0 {
		// "N/A" is the answer for a container that carries no duration. It
		// should not happen for what this plugin writes, and it is reported
		// rather than guessed at.
		return 0, fmt.Errorf("the stored recording reports no usable duration (%q)", text)
	}
	return seconds, nil
}

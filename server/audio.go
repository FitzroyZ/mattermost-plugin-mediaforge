package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

// voiceRecordingPrefix is the file name the plugin's own recorder gives every
// upload it makes.
//
// It is the ONLY thing that routes a file into this path, and that narrowness
// is deliberate. A voice message is an audio track in a container that also
// carries video, so recognising one by its contents would mean claiming every
// `.m4a` voice memo and every audio-only WebM a user attaches — and video compression
// deliberately stores exactly those byte-identical. Converting them would
// break a proved path and would also be presumptuous: the plugin has no
// business re-encoding a file it did not create.
//
// The prefix is part of the contract between the webapp and the server. Both
// sides carry it, and the recogniser below is what pins it.
const voiceRecordingPrefix = "mediaforge-voice-"

// voiceMimeType is what the stored recording is announced as.
//
// This is half of what makes native playback work and it is easy to overlook.
// A phone decides how to offer a file from what the SERVER says it is, not by
// sniffing the bytes: Mattermost hands the client `FileInfo.MimeType`
// verbatim. A perfectly good M4A announced as `video/webm` is offered as a
// video, which is the silent failure SPEC §4.3 exists to avoid.
//
// `audio/mp4` rather than `audio/x-m4a`: it is the registered type for AAC in
// an MP4 container (RFC 4337) and is what Go's own mime package returns for
// `.m4a`, so the plugin agrees with the server's own table.
const voiceMimeType = "audio/mp4"

// voiceExtension is the extension of a stored recording.
const voiceExtension = "m4a"

// voiceBitrateKbps is the AAC bitrate one recording is encoded at.
//
// Deliberately not a setting. Speech is mono and narrow-band, and the whole
// range an administrator could sensibly choose sounds the same: 64 kbit/s mono
// AAC is transparent for a voice, while the settings page already carries
// fifteen entries. A sixteenth that cannot improve anything is a worse trade
// than a fixed number with a reason written next to it.
const voiceBitrateKbps = 64

// voiceContainerExtensions are the containers a browser's MediaRecorder
// actually produces, measured rather than assumed.
//
// On the test instance a headless Chromium offered `audio/webm;codecs=opus`
// and delivered exactly that, while its `audio/mp4` offer negotiated to
// `audio/mp4;codecs=opus` and produced zero bytes. Safari records MP4/AAC, and
// `.ogg` is what Firefox's Opus output can land in.
//
// `m4a` is in the list for the file that comes BACK, not for one the browser
// emits: a recording the plugin already converted keeps the prefix, so a
// re-upload of it must still be recognised rather than treated as a stranger.
var voiceContainerExtensions = map[string]bool{
	"webm": true,
	"mp4":  true,
	"m4a":  true,
	"ogg":  true,
}

// looksLikeVoiceRecording reports whether a file name is one the plugin's own
// recorder produced.
//
// Both halves are required. The prefix says who made it; the extension keeps a
// mistyped or hostile name from reaching the encoder with something that is
// not a recording at all. `strings.HasPrefix` is not enough on its own —
// `mediaforge-voice-notes.txt` would pass it.
func looksLikeVoiceRecording(name string) bool {
	if !strings.HasPrefix(name, voiceRecordingPrefix) {
		return false
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	return voiceContainerExtensions[ext]
}

// voiceEnabled reports whether a recording may be converted.
//
// Both halves matter, the same way videoEnabled's do: the administrator's
// switch, and whether ffmpeg is actually installed. SPEC §5 requires a missing
// toolchain to disable the feature quietly rather than fail upload after
// upload.
func (c configuration) voiceEnabled() bool {
	return c.EnableVoiceMessages && c.tools.available()
}

// storedVoiceName is the name the recording gets once it is AAC in MP4.
func storedVoiceName(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name)) + "." + voiceExtension
}

// processVoiceUpload converts one recording and writes the result.
//
// The shape follows processVideoUpload, for the same reasons and with one
// deliberate difference at the end:
//
//  1. Spool the upload, because ffmpeg needs to seek in an MP4 and the hook
//     only offers a stream. Over the cap the file is passed through — a
//     resource limit is the plugin's problem, not the sender's.
//  2. Ask ffprobe what it really is. A file carrying a VIDEO track is not a
//     voice message however it is named, and is handed back to the caller
//     untouched: converting it would silently throw the picture away.
//  3. A recording that is already AAC in MP4 is stored as sent. There is
//     nothing to gain from re-encoding it and a generation of quality to lose.
//  4. Otherwise convert to AAC in MP4. From here a failure REJECTS the upload,
//     because the plugin has committed to delivering something a phone can
//     play, and storing the unplayable original instead would break that
//     promise without telling anybody.
//
// The one difference from video: the result is stored even when it is LARGER
// than the input. Opus is a better codec than AAC, so a converted recording
// usually grows by a few per cent — and the point of this slice is not a
// smaller file, it is a file that plays on a phone. Keeping the smaller
// original would defeat the entire design.
func processVoiceUpload(output io.Writer, head []byte, rest io.Reader, cfg configuration) (processOutcome, error) {
	dir, err := ensureSpoolDir(cfg.spoolDir)
	if err != nil {
		// Without a writable temp directory nothing can be converted, but
		// nothing is broken either: store what the user sent.
		return passThrough(output, io.MultiReader(newByteReader(head), rest))
	}

	spool, err := spoolUpload(dir, head, rest, cfg.maxVideoBytes())
	if spool != nil {
		defer spool.remove()
	}
	if err != nil {
		// The upload stream itself broke. This is the the first hook version contract and it
		// outranks everything here: a truncated file must never be stored.
		return processOutcome{}, err
	}

	if spool.truncated {
		return passThroughSpooled(output, spool, rest)
	}

	probed, probeErr := probeRecording(spool.path, cfg.tools)
	switch {
	case probeErr != nil || !probed.hasAudio:
		// Not something ffprobe can make sense of as audio. Nothing has been
		// promised about it yet, so pass it through rather than reject.
		return passThroughSpooled(output, spool, nil)
	case probed.hasVideo:
		// A picture is not this slice's business, whatever the name says.
		return passThroughSpooled(output, spool, nil)
	}

	// How long the recording really is, read BEFORE anything is encoded.
	//
	// The order is the decision this slice had to make deliberately, and it is
	// the same question video compression answered for video with "decide from the
	// header before the encoder starts". The server cannot know a recording's
	// length without reading it, and reading it after the conversion would
	// mean transcoding a file in full and then throwing it away — processor
	// time spent on something nobody will ever receive, on the one path where
	// a sender is already waiting for an answer.
	//
	// Measured on the reference container: walking the packets of a
	// ten-minute recording takes 0.23 s against 5.8 s to convert it, so the
	// check costs about a twenty-fifth of the work it avoids.
	length, lengthErr := probeAudioDuration(spool.path, cfg.tools)
	if lengthErr != nil {
		// The length could not be read at all. Nothing has been promised yet,
		// so the recording is stored as sent rather than refused: losing
		// somebody's words because a probe failed would be the worse of the
		// two mistakes.
		return passThroughSpooled(output, spool, nil)
	}

	if length.damaged {
		// The demuxer could not read the recording in full.
		//
		// This is the voice recording defect, caught one step earlier and by a second
		// tool. A damaged recording does NOT make ffmpeg fail: the demuxer
		// stops at the damage, the encoder writes what it got, the exit status
		// is 0 and the result decodes perfectly — it is simply shorter. The
		// sender speaks for four seconds and the recipient hears one, with no
		// error anywhere for either of them to notice.
		//
		// voice recording detected it from ffmpeg's stderr after the conversion. It is
		// detected here from ffprobe's stderr before it, which costs nothing
		// extra — this pass has to happen anyway for the duration — and no
		// longer spends an encode on a file that will be refused. The ffmpeg
		// check stays in place as well: two independent tools reporting the
		// same thing is what makes the rule robust rather than a property of
		// one build's diagnostics.
		return processOutcome{isVoice: true}, rejectUpload(
			"this voice message arrived damaged and was not saved. Please record it again.",
			fmt.Errorf("the recording could not be read in full: %s", length.complaint))
	}

	if maxSeconds := cfg.MaxVoiceSeconds; maxSeconds > 0 && length.seconds > float64(maxSeconds) {
		// Over the limit. Refused rather than cut short, deliberately: a
		// recording that was silently shortened is exactly the failure the
		// paragraph above exists to prevent, and doing it on purpose would be
		// no better. The sender is told in their own terms — how long they
		// spoke and how long they may — so the next attempt can succeed.
		return processOutcome{isVoice: true}, rejectUpload(
			fmt.Sprintf(
				"this voice message is too long. It is %s and the limit is %s. "+
					"Please record a shorter one.",
				humanSeconds(length.seconds), humanSeconds(float64(maxSeconds))),
			fmt.Errorf("the recording is %.1f s, over the %d s limit",
				length.seconds, maxSeconds))
	}

	if probed.isAlreadyVoiceReady() {
		// Already AAC in an MP4 container: exactly what would be produced.
		res, passErr := passThroughSpooled(output, spool, nil)
		res.isVoice = true
		return res, passErr
	}

	// A slot is taken only now, and this is the earliest point at which one
	// could be needed: everything above cost a header read. The image gate is
	// the right one — an audio encode is measured in seconds (10 minutes of
	// speech in 5.8 s on the reference container), so putting it behind the
	// single video slot would make a two-second voice message wait out a
	// four-minute transcode.
	release, admitted := acquireImageSlot(cfg)
	if !admitted {
		// Every slot busy. The recording is stored exactly as the browser sent
		// it — intact and playable in the webapp, though a phone may not offer
		// it. That is the the concurrency limit answer applied here: failing an upload
		// because the SERVER was busy is something the sender can neither
		// understand nor act on, and it would lose their words outright. The
		// warning in the log names the file for the administrator.
		res, passErr := passThroughSpooled(output, spool, nil)
		res.isVoice = true
		res.queueFull = true
		return res, passErr
	}
	defer release()

	outPath := spool.path + ".out.m4a"
	defer os.Remove(outPath)

	start := time.Now()
	err = runVoiceTranscode(spool.path, outPath, cfg)
	elapsed := time.Since(start)
	release()

	if err != nil {
		// isVoice is carried on the REJECTION too, and that is not tidiness.
		// The hook reads it to decide whether to count a failure, and the
		// report has only two kinds — images and videos. An empty outcome
		// here reads as "not a voice message", so every refused recording
		// would be filed under "videos that could not be converted" and the
		// one figure an administrator acts on would describe something else
		// than it says.
		return processOutcome{isVoice: true}, rejectUpload(
			"this voice message could not be prepared for sending and was not saved. "+
				"Please record it again.", err)
	}

	converted, statErr := os.Stat(outPath)
	if statErr != nil || converted.Size() == 0 {
		return processOutcome{isVoice: true}, rejectUpload(
			"this voice message could not be prepared for sending and was not saved. "+
				"Please record it again.",
			fmt.Errorf("ffmpeg reported success but produced no usable output: %w", statErr))
	}

	written, err := copyFileTo(output, outPath)
	if err != nil {
		// Whatever reached the output is a partial file, so rejecting is
		// required — the the first hook version contract again.
		return processOutcome{bytesWritten: written, isVoice: true}, err
	}

	// The length shown to a READER is measured on the stored file, not on the
	// recording that went in.
	//
	// These differ whenever pauses were shortened, and the difference was a
	// defect found in daily use: a 23-second recording showed 0:23 on the post
	// while the stored file was 0:15. Both figures were true and the wrong one
	// was displayed.
	//
	// `length.seconds` above cannot be reused, and must not be changed to
	// measure the output, because it enforces MaxVoiceSeconds: a recording has
	// to be refused for what the sender spoke. Refusing it for what survives
	// the trimming would make the limit move with a setting the sender cannot
	// see.
	//
	// It falls back to the spoken length rather than failing. The file is
	// converted and about to be stored; losing it because a second probe
	// failed would be absurd, and a slightly stale number is a far smaller
	// fault than a lost recording.
	//
	// The failure is carried back on the outcome rather than logged here.
	// This function deliberately takes only a configuration and has no
	// logger: it is the part that can run inside a gate, and threading an
	// API handle into it to report a non-event would be the wrong trade. The
	// hook logs it, where every other diagnosis about this upload is written.
	spoken := length.seconds
	storedSeconds, storedErr := probeStoredSeconds(outPath, cfg.tools)
	if storedErr == nil {
		spoken = storedSeconds
	}

	return processOutcome{
		bytesWritten:      written,
		originalSize:      spool.size,
		isVoice:           true,
		voiceConverted:    true,
		duration:          elapsed,
		spokenSeconds:     spoken,
		storedLengthError: storedErr,
	}, nil
}

// humanSeconds renders a length the way a sender would say it.
//
// The sentence this feeds is read by somebody who has just lost a recording,
// so it has to be immediately obvious rather than precise: "3 minutes 20
// seconds", not "200.4 s". Anything under a minute stays in seconds, because
// "0 minutes 12 seconds" is how a machine talks.
func humanSeconds(seconds float64) string {
	whole := int(seconds + 0.5)
	if whole < 1 {
		whole = 1
	}
	if whole < 60 {
		return fmt.Sprintf("%d seconds", whole)
	}

	minutes := whole / 60
	rest := whole % 60
	minuteWord := "minutes"
	if minutes == 1 {
		minuteWord = "minute"
	}
	if rest == 0 {
		return fmt.Sprintf("%d %s", minutes, minuteWord)
	}
	return fmt.Sprintf("%d %s %d seconds", minutes, minuteWord, rest)
}

// voiceTimeout bounds one recording's conversion.
//
// Measured on the reference container: ten minutes of speech converts in 5.8 s,
// so sixty seconds is roughly a tenfold margin over the longest recording SPEC
// §4.3 allows. It is a constant rather than a setting because there is no
// value an administrator could usefully choose: anything shorter would refuse
// legitimate recordings, and anything longer would keep encoding after the
// server's own write window had closed.
const voiceTimeout = 60 * time.Second

// runVoiceTranscode converts one spooled recording to AAC in MP4.
//
// The process-group handling is shared with runTranscode for the same reason
// it exists there: killing only the parent PID can leave workers running with
// the output file open.
func runVoiceTranscode(inputPath, outputPath string, cfg configuration) error {
	ctx, cancel := context.WithTimeout(context.Background(), voiceTimeout)
	defer cancel()

	cmd := exec.Command(cfg.tools.FFmpegPath,
		voiceEncoderArgs(inputPath, outputPath, cfg.TrimVoiceSilence)...)
	isolateProcessGroup(cmd)
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
		// A clean exit is NOT enough, and this is the one thing about the
		// voice path that had to be measured before it could be written.
		//
		// A recording whose payload is damaged in the middle does not make
		// ffmpeg fail. The demuxer stops where the damage starts, ffmpeg
		// encodes what it got, reports success, and produces a file that
		// decodes perfectly — it is simply SHORTER. Measured on four kinds of
		// damage: a 3.0 s recording came back as 0.9 s, 0.7 s, 1.2 s and
		// 1.0 s, every one of them with exit status 0, and every one of them
		// passing a full `ffmpeg -xerror` decode afterwards. Neither
		// `-err_detect explode` nor `-xerror` changes that: they govern
		// decoding, and nothing here failed to decode.
		//
		// That is the worst outcome this slice could produce. The sender
		// speaks for three seconds, sees the message posted, and the
		// recipient hears the first second — with no error anywhere for
		// either of them to notice.
		//
		// The demuxer does say so, though, on stderr. At `-v error` a healthy
		// file produces EXACTLY NOTHING: measured at zero bytes for the
		// browser's own recording, for a generated Opus file and for an
		// already-AAC file, against 56 to 120 bytes of EBML complaints for
		// each of the four damaged ones. So a diagnostic at error level is
		// treated as what it is — a recording that could not be read in full.
		if diagnostics := strings.TrimSpace(stderr.String()); diagnostics != "" {
			return fmt.Errorf("the recording could not be read in full: %s", diagnostics)
		}
		return nil

	case <-ctx.Done():
		signalProcessGroup(cmd, terminateSignal)
		select {
		case <-done:
		case <-time.After(terminateGracePeriod):
			signalProcessGroup(cmd, killSignal)
			// Bounded even after SIGKILL: cmd.Wait also waits for the stderr
			// pipe to reach EOF, which a grandchild that escaped the process
			// group can hold open indefinitely. See runTranscode.
			select {
			case <-done:
			case <-time.After(reapGracePeriod):
			}
		}
		return errTranscodeTimeout
	}
}

// voiceEncoderArgs builds the ffmpeg command line for one recording.
//
// Four of these arguments are load-bearing rather than tidy:
//
//   - `-vn` and `-map 0:a:0` take the audio and nothing else. A browser
//     sometimes writes a cover-art or thumbnail stream into a recording, and
//     an MP4 carrying a still image is offered as a video by some clients.
//   - `-ac 1` stores speech in mono. A microphone delivers one signal, so a
//     stereo file would carry the same samples twice for double the bytes.
//   - `-movflags +faststart` moves the index to the front. Without it a player
//     has to fetch the end of the file before it can begin, which on a phone
//     over mobile data is the difference between instant playback and a wait.
//   - `-err_detect explode` refuses to start on a file the demuxer already
//     distrusts, instead of producing half a recording and reporting success.
func voiceEncoderArgs(inputPath, outputPath string, trimSilence bool) []string {
	args := []string{
		"-nostdin",
		"-y",
		"-v", "error",
		"-err_detect", "explode",
		"-i", inputPath,
		"-vn",
		"-map", "0:a:0",
	}
	if trimSilence {
		args = append(args, "-af", voiceSilenceFilter)
	}
	return append(args,
		"-c:a", "aac",
		"-b:a", strconv.Itoa(voiceBitrateKbps)+"k",
		"-ac", "1",
		"-movflags", "+faststart",
		"-f", "mp4",
		outputPath,
	)
}

// voiceSilenceFilter shortens long pauses inside a recording.
//
// A LITERAL, not built from configuration, and that is a security property
// rather than an omission. The audit established that no externally chosen
// value reaches an ffmpeg argument; exposing a threshold or a duration in the
// System Console would open exactly that door again for the sake of two
// numbers nobody would tune. The setting is therefore a switch, and this is a
// constant.
//
// What the parts do, and why each is where it is:
//
//   - `stop_periods=-1` applies to every pause in the file rather than only
//     the last one. Without it the filter trims the tail and leaves the
//     middle, which is where the waiting actually happens.
//   - `stop_duration=0.4` is what makes this SHORTEN rather than DELETE.
//     Collapsing a pause to nothing runs sentences together and makes a
//     speaker sound breathless; four tenths of a second is still audibly a
//     pause, and it is the difference between editing somebody's words and
//     tightening their rhythm.
//   - `stop_threshold=-40dB` is quiet enough that room tone and breathing
//     count as silence, loud enough that a softly spoken word does not.
//   - there is deliberately NO `start_periods`. Trimming the beginning would
//     clip the first word of anybody who starts quietly, and the first word
//     is the one that carries the most.
const voiceSilenceFilter = "silenceremove=stop_periods=-1:stop_duration=0.4:stop_threshold=-40dB"

// recordingProbe is what ffprobe could tell us about an upload's streams.
type recordingProbe struct {
	// hasAudio is true when there is at least one audio stream.
	hasAudio bool
	// hasVideo is true when there is a video stream that is not cover art.
	hasVideo bool
	// audioCodec is the first audio stream's codec, empty when unknown.
	audioCodec string
	// formatName is ffprobe's container family, e.g. "mov,mp4,m4a,3gp,3g2,mj2".
	formatName string
}

// isAlreadyVoiceReady reports whether the file is already what this path would
// produce: an AAC track in an MP4-family container.
func (p recordingProbe) isAlreadyVoiceReady() bool {
	return p.audioCodec == "aac" && strings.Contains(p.formatName, "mp4")
}

// probeRecording asks ffprobe what streams the spooled file really carries.
//
// It reads ALL streams rather than selecting one, because the decisive
// question here is a negative: does this file contain a video track? Selecting
// `a:0` the way probeVideo selects `v:0` would answer "there is audio" while
// staying silent about the picture that must not be thrown away.
func probeRecording(path string, tools mediaTools) (recordingProbe, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, tools.FFprobePath,
		"-v", "error",
		"-show_entries", "stream=codec_name,codec_type,disposition=attached_pic",
		"-show_entries", "format=format_name",
		"-of", "default=noprint_wrappers=1",
		path)
	isolateProcessGroup(cmd)

	out, err := cmd.Output()
	if err != nil {
		return recordingProbe{}, fmt.Errorf("ffprobe could not read the file: %w", err)
	}

	var res recordingProbe
	var codecName string
	var attachedPic bool
	for _, line := range strings.Split(string(out), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "codec_name":
			codecName = value
		case "DISPOSITION:attached_pic":
			attachedPic = value == "1"
		case "codec_type":
			switch value {
			case "audio":
				if !res.hasAudio {
					res.audioCodec = codecName
				}
				res.hasAudio = true
			case "video":
				// Cover art is a still picture attached to an audio file, not
				// a video. Treating it as one would pass every recording with
				// an embedded thumbnail straight through unconverted.
				if !attachedPic && !isStillImageCodec(codecName) {
					res.hasVideo = true
				}
			}
			attachedPic = false
		case "format_name":
			res.formatName = value
		}
	}
	return res, nil
}

// applyVoiceNaming returns the FileInfo the server must store for a converted
// recording, or nil when nothing needs correcting.
//
// Returning a COPY is required rather than tidy. The server passes its own
// struct into the hook and keeps using it afterwards, so writing through the
// pointer would rename a file on a path that may still reject the upload. The
// copy is what runPluginsHook assigns over its local pointer, and gob decodes
// it back into the caller's struct field by field — which is what makes the
// new name and MIME type reach the database at all.
//
// Size is deliberately not touched: the server overwrites it from its own byte
// count immediately after the hook returns.
//
// Path is deliberately not touched either, and that is the one asymmetry worth
// naming. The stored file keeps its original `.webm` suffix ON DISK while the
// database record says `.m4a` — so a file-store listing shows a `.webm` name
// holding MP4 bytes. It looks wrong and it was checked rather than waved away:
//
//   - It does not affect playback anywhere. Mattermost builds Content-Type and
//     Content-Disposition from MimeType and Name, never from Path, so a phone
//     is offered `…m4a` as `audio/mp4` exactly as intended. Verified on the
//     live instance: the API served the recording as `audio/mp4`.
//   - Rewriting Path here would be WORSE than the inconsistency. The server
//     wrote the original bytes to the old path before the hook ran and moves
//     its temporary file onto `info.Path` afterwards (runPluginsHook, upload.go
//     lines 96-121). Changing the field moves the destination but not the file
//     already sitting at the old one, so every recording would leave an orphan
//     behind — a storage leak traded for a tidier name.
//
// Fixing it properly means deleting the original path after the move, which is
// a change to code this slice does not own. It is a cosmetic wart with a real
// trap behind it, so it is written down rather than patched in passing.
func applyVoiceNaming(info *model.FileInfo) *model.FileInfo {
	if info == nil {
		return nil
	}
	updated := *info
	updated.Name = storedVoiceName(info.Name)
	updated.Extension = voiceExtension
	updated.MimeType = voiceMimeType
	// A voice message has no picture. Stale geometry from the original
	// container would otherwise make a client reserve space for one.
	updated.Width = 0
	updated.Height = 0
	return &updated
}

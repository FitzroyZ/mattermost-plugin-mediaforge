package main

import (
	"fmt"
	"io"
	"runtime/debug"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

// namedResult assembles the FileInfo the hook returns for a file it compressed.
//
// It exists because the naming rule (the file-naming work.2) has to apply to EVERY
// compressed file, while the geometry correction applies only to some — the
// image path returns nil when the dimensions were already right, and the video
// path returns nil when ffprobe reported none. Threading the rename through
// those branches by hand is how one of them ends up forgotten, which is
// precisely the mistake the voice send fix made three times in the webapp.
//
// So the branches say what they know about geometry, and this one function
// decides what the hook returns. A nil result still means "accept unchanged".
//
// FileInfo.Path is deliberately untouched: the security audit found that rewriting it
// orphans the file, because the bytes are already on disk under the old path.
// The rename is a display concern only.
func namedResult(
	info *model.FileInfo,
	cfg configuration,
	width, height int,
	geometryChanged bool,
	at time.Time,
) *model.FileInfo {
	if info == nil {
		return nil
	}
	newName := compressedName(info.Name, info.MimeType, at, cfg.MarkCompressedFilenames)
	renamed := newName != info.Name

	if !renamed && !geometryChanged {
		return nil
	}

	updated := *info
	if renamed {
		updated.Name = newName
	}
	if geometryChanged {
		updated.Width = width
		updated.Height = height
	}
	return &updated
}

// FileWillBeUploaded is invoked by the server for every uploaded file, before
// the file is committed to the backing store.
//
// Contract (server/public/plugin/hooks.go):
//   - returning a non-empty string rejects the upload and shows that text;
//   - writing to output replaces the file, and the server then sets
//     FileInfo.Size from the bytes it actually received;
//   - returning a nil *model.FileInfo with an empty string accepts the file.
//
// image compression routes every upload through processUpload, which compresses JPEG and
// PNG over the size threshold and streams everything else through unchanged.
// The surrounding contract is untouched: the hook still always writes its
// bytes to output, so the server still always takes its "written > 0" branch
// and recomputes FileInfo.Size from what it received. That is the the first hook version
// invariant, and it is what makes the compressed size correct for free.
//
// Width and Height are a different matter and must be fixed up explicitly —
// see applyGeometry. The server samples them from the ORIGINAL bytes before
// the hook runs and never looks again, so a resize without this correction
// would leave stale geometry in the database.
//
// Compression never fails an upload. processUpload falls back to the original
// bytes whenever it cannot improve a file, so the only rejection path remains
// the one from the first hook version: a broken input stream.
func (p *MediaForgePlugin) FileWillBeUploaded(
	_ *plugin.Context,
	info *model.FileInfo,
	file io.Reader,
	output io.Writer,
) (returnedInfo *model.FileInfo, rejection string) {
	// A panic below this line must not leave the hook.
	//
	// THIS WAS A DECISION, NOT A REFLEX, and the measurement behind it is in
	// PLAN §the security audit. The question the audit had to answer was whether a
	// recovery handler here is the right answer or whether it hides real
	// defects. Both halves were measured on the test instance by killing the
	// plugin process outright, which is what an escaping panic does:
	//
	//   - the Mattermost SERVER survives it (ping kept answering);
	//   - the upload in flight still succeeds and is stored intact, but
	//     UNCOMPRESSED;
	//   - the server restarts the plugin by itself within seconds.
	//
	// So an escaping panic is not a server outage. It is worse in a quieter
	// way: every upload being processed at that moment loses its compression,
	// the process restarts with its statistics shard and both gates reset, and
	// the only trace is a plugin restart nobody is watching for.
	//
	// Recovering is therefore the better answer, and it is deliberately
	// FAIL-CLOSED rather than fail-open. The output writer may already hold a
	// partially written file when the panic happens, and accepting that would
	// store a truncated file as the user's own — the one outcome the the first hook version
	// contract forbids above everything else. Refusing costs the sender one
	// retry; accepting costs them their data silently.
	//
	// It does NOT hide the defect, which was the argument against it. The
	// recovery logs at ERROR level with the panic value and the full stack,
	// which is strictly louder than the current behaviour: today the process
	// dies and its stack goes to a plugin stderr nobody reads. The sender is
	// shown the plugin's own sentence and never the panic value, which would
	// leak internals exactly as the spool path did.
	defer func() {
		if r := recover(); r != nil {
			returnedInfo, rejection = p.recoverHookPanic(info, r)
		}
	}()

	cfg := p.getConfiguration()

	// Per-channel availability is decided here, before the pipeline runs, and
	// it is decided on the SERVER because that is the only side that cannot be
	// bypassed: a recording can be uploaded by anything that can post a
	// multipart form, with or without this plugin's webapp.
	//
	// The channel is knowable at this point even though FileInfo.ChannelId is
	// still empty, because the server built FileInfo.Path from post.ChannelId
	// strictly before calling this hook — see channelIDFromPath, which is
	// verified against the test instance's own database.
	//
	// A channel that was not opened up turns the feature OFF for this one
	// upload rather than refusing it. The recording is then stored exactly as
	// sent, which is the same thing that happens when an administrator
	// switches voice messages off altogether: nobody loses what they said,
	// and the file is simply not converted.
	if cfg.voiceEnabled() && looksLikeVoiceRecording(info.Name) &&
		!p.voiceAllowedInChannel(cfg, info) {
		p.logDebug(
			"MediaForge: storing a voice recording as sent because voice messages "+
				"are not switched on for this channel",
			"file_id", info.Id,
			"file_name", info.Name,
			"channel_id", channelIDFromPath(info.Path),
		)
		cfg.EnableVoiceMessages = false
	}

	// The file name is handed to the pipeline because it is the only thing
	// that identifies a recording the plugin's own message-box button made
	// (SPEC §4.3). Nothing else is decided by it.
	outcome, err := processNamedUpload(output, file, cfg, info.Name)

	// A transcode that failed is the one deliberate case where MediaForge
	// fails an upload (SPEC §4.2). It is separated from the stream-broke case
	// below because the sentence shown to the user is different and was
	// written for them: storing a video the plugin could not convert would
	// silently break the promise that stored videos are the converted ones.
	var refusal *uploadRejection
	if err != nil && asRejection(err, &refusal) {
		p.logError(
			"MediaForge: rejecting an upload because the file could not be converted",
			"file_id", info.Id,
			"file_name", info.Name,
			"error", err.Error(),
		)
		// Counted for the administrator's report. A refused upload is the one
		// failure the sender sees and the administrator does not, so it is
		// also the one most likely to arrive as a support request without any
		// figure to put it in context.
		//
		// A failed VOICE conversion is deliberately left out of it. The report
		// counts images and videos, and a voice message is neither: filing it
		// under "videos that could not be converted" would make the one figure
		// an administrator can act on describe something else than it says.
		// Giving it a third kind belongs to the slice that owns the report, not
		// to this one. The log line above still names the file.
		if !outcome.isVoice {
			p.recordUploadStats(outcome, true)
		}
		// The file name is added here rather than inside the pipeline: this is
		// the only layer that knows it, and a user with several uploads in
		// flight needs to be told which one failed.
		return nil, rejectionMessageFor(info.Name, refusal.message)
	}

	if err != nil {
		// The stream broke midway, so anything already written is a
		// truncated file. Rejecting is the only safe answer, and it is
		// sufficient: App.runPluginsHook checks the rejection error and
		// returns — removing both info.Path and the .tmp file — strictly
		// before it reaches the "written > 0 { MoveFile }" branch. A
		// partial copy therefore can never overwrite the stored file.
		//
		// Deliberately NOT counted as a failure in the administrator's
		// report. The upload broke on its way to the server — a dropped
		// connection, a client that gave up — and MediaForge neither caused
		// it nor could have prevented it. Counting it would put a number on
		// the report page that an administrator cannot act on, mixed in with
		// the transcodes that genuinely did not work, and would make the one
		// figure they CAN act on harder to read.
		p.logError(
			"MediaForge: failed to process uploaded file, rejecting upload",
			"file_id", info.Id,
			"file_name", info.Name,
			"bytes_written", outcome.bytesWritten,
			"error", err.Error(),
		)
		return nil, readFailureMessageFor(info.Name, err)
	}

	// A converted recording is handled before the compression rules, because
	// it is the one outcome that is neither "made smaller" nor "stored as
	// sent". It saved nothing and must therefore contribute nothing to the
	// size note or to the report — but the stored bytes are no longer what the
	// server thinks they are, and that has to be corrected.
	if outcome.voiceConverted {
		p.logDebug(
			"MediaForge: converted a voice message so every device can play it",
			"file_id", info.Id,
			"file_name", info.Name,
			"stored_name", storedVoiceName(info.Name),
			"original_size", info.Size,
			"stored_size", outcome.bytesWritten,
			"seconds", outcome.duration.Seconds(),
		)
		if outcome.storedLengthError != nil {
			// The recording is fine and stored; only the figure on the post is
			// the one from before the conversion. Worth a line so nobody has
			// to wonder why a trimmed recording shows its untrimmed length.
			p.logWarn(
				"MediaForge: stored a voice message but could not measure the stored "+
					"file's length, so the note shows the length as recorded rather "+
					"than as stored. The recording itself is unaffected.",
				"file_id", info.Id,
				"file_name", info.Name,
				"error", outcome.storedLengthError.Error(),
			)
		}
		// The playing time still has to reach the post, and this early return
		// is why it did not on the first attempt: every unit test of the note
		// passed while nothing appeared on a real server, because the call
		// below is several branches further down and a recording never gets
		// there.
		//
		// recordSaving writes a length-only record here — no sizes at all —
		// so the report and the note still see nothing that looks like a
		// saving, which is exactly what the comment above requires.
		p.recordSaving(info, outcome)
		return applyVoiceNaming(info), ""
	}

	if !outcome.reEncoded {
		if outcome.queueFull {
			// The upload succeeded and the file is intact; only the
			// compression was skipped. It is logged as a warning because it
			// is the one thing an administrator can act on: the limits are
			// set below what this instance is being asked to do.
			//
			// For a voice message it is worth more than that: the recording
			// is stored exactly as the browser made it, which plays in the
			// webapp but may not be offered by a phone. The sender is not
			// told, because there is nothing they could do about it.
			p.logWarn(
				"MediaForge: stored a file without converting it because the server was "+
					"already busy with as many files as its limits allow. Nothing was lost, "+
					"but raising the limits in the System Console would let more be converted.",
				"file_id", info.Id,
				"file_name", info.Name,
				"is_voice_message", outcome.isVoice,
			)
		}
		// nil FileInfo plus an empty string means "accept unchanged". The
		// server derives the size from the bytes it received, so the plugin
		// must not set it itself.
		//
		// Note on empty files: processUpload reports 0 bytes written, so the
		// server takes its "written == 0" branch, discards the temporary file
		// and keeps the original upload with its original size. A zero-byte
		// file is therefore always a genuine no-op, whatever this hook does —
		// the hook API offers no way to replace a file with an empty one.
		return nil, ""
	}

	// The file was replaced, so the recorded geometry now describes bytes that
	// no longer exist. Returning a corrected *model.FileInfo is the only way
	// to fix it: runPluginsHook assigns the returned pointer over its local
	// info, and that same pointer is what UploadFileTask later saves.
	//
	// Size is deliberately NOT set here. The server overwrites it from its own
	// byte count immediately after the hook returns, so writing it would be
	// both pointless and misleading.
	newWidth, newHeight, corrected := applyGeometry(info.Width, info.Height, outcome)

	// Record what this file saved, under the ID the post will reference. This
	// is the only moment both sizes are known: the server is about to
	// overwrite fileinfo.Size with the compressed byte count, after which the
	// original size is gone. Recorded only for a file that really was
	// re-encoded, which is what makes the absence of a note mean "stored as
	// the original" (SPEC §4.4).
	p.recordSaving(info, outcome)

	// And add it to the administrator's report, from the SAME measurement.
	//
	// processOutcome is the only trustworthy source for the original size, and
	// it is deliberately the source both the note and the report read: two
	// separate measurements of the same saving drift apart over time, and then
	// nobody can say which of the two figures is the right one.
	p.recordUploadStats(outcome, false)

	if outcome.isVideo {
		// Video geometry is never subject to the EXIF-rotation convention that
		// applyGeometry guards against — the server does not sample dimensions
		// from video at all, so both values are typically zero and there is
		// nothing to match against. Write what the stored file actually is.
		p.logDebug(
			"MediaForge: transcoded video",
			"file_id", info.Id,
			"file_name", info.Name,
			"original_size", info.Size,
			"stored_size", outcome.bytesWritten,
			"source_dimensions", fmt.Sprintf("%dx%d", outcome.sourceWidth, outcome.sourceHeight),
			"stored_dimensions", fmt.Sprintf("%dx%d", outcome.width, outcome.height),
			"codec", cfg.VideoCodec,
			"seconds", outcome.duration.Seconds(),
		)
		// The rename applies even when ffprobe gave no usable geometry, which
		// is why this goes through namedResult rather than returning nil here.
		return namedResult(
			info, cfg, outcome.width, outcome.height,
			outcome.width > 0 && outcome.height > 0, time.Now(),
		), ""
	}

	p.logDebug(
		"MediaForge: compressed image",
		"file_id", info.Id,
		"file_name", info.Name,
		"original_size", info.Size,
		"stored_size", outcome.bytesWritten,
		"source_dimensions", fmt.Sprintf("%dx%d", outcome.sourceWidth, outcome.sourceHeight),
		"stored_dimensions", fmt.Sprintf("%dx%d", outcome.width, outcome.height),
		"geometry_corrected", corrected,
	)

	// Same as the video branch: the rename applies whether or not the geometry
	// needed correcting, so both cases go through one place.
	return namedResult(info, cfg, newWidth, newHeight, corrected, time.Now()), ""
}

// readFailureMessageFor builds the sentence the sender sees when their upload
// could not be read to the end.
//
// It deliberately carries NO diagnosis, and that is the audit's disclosure
// finding rather than a stylistic choice. This message used to render the
// underlying error with %v, and every error that reaches this path from the
// spool is a wrapped *os.PathError — so on a real out-of-space (reproduced
// against a 1 MB tmpfs, not simulated) an ordinary uploader was shown:
//
//	MediaForge could not read "holiday.mp4": writing the upload to a temporary
//	file: write /tmp/mediaforge/mediaforge-upload-XXXXXXXX: no space left on
//	device
//
// That is the server's temp directory layout, the plugin's own spool prefix,
// and the news that the host's disk is full, handed to anybody who can upload a
// file. None of it is actionable by the sender, and all of it is useful to
// somebody mapping the host.
//
// The diagnosis is not lost: the LogError call immediately above this return
// carries the full error, the file id and the byte count for the administrator,
// who is the only person who can act on it. The split is the same one
// uploadRejection exists to enforce — a sentence for the sender, the cause for
// the log.
//
// The causes are deliberately not distinguished for the sender either. A broken
// connection, a full disk and an unreadable directory all mean the same thing
// from their side of it ("this did not arrive, try again"), and telling them
// apart would only disclose which one it was.
func readFailureMessageFor(fileName string, _ error) string {
	return fmt.Sprintf(
		"MediaForge could not read %q all the way to the end, so it was not saved. "+
			"Please try sending it again.", fileName)
}

// rejectionMessageFor builds the sentence the sender sees when MediaForge
// refuses their upload.
//
// The file name is chosen entirely by the uploader, and this is the one place
// it is shown back to a person. %q rather than %s is what makes that safe, and
// it is load-bearing rather than stylistic: %q wraps the name in quotes and
// escapes every control character to its Go literal form, so a name carrying a
// newline cannot append a second, forged sentence to the plugin's own, an
// embedded terminal escape cannot reach a terminal that is tailing the log, and
// a NUL cannot truncate the string for a C consumer downstream.
//
// Changing it to %s reads as harmless tidying and would remove all of that at
// once, which is why TestAHostileFileNameCannotForgeTheMessageShownToTheSender
// pins it against the five shapes an attacker would actually send.
//
// The rejection text itself is always one of the plugin's own literals — it
// never carries ffmpeg's diagnostics, which name internal paths and byte
// offsets. That separation is the uploadRejection type's whole purpose.
func rejectionMessageFor(fileName, reason string) string {
	return fmt.Sprintf("MediaForge could not accept %q: %s", fileName, reason)
}

// recoverHookPanic turns a panic inside the upload pipeline into a refused
// upload, and says so loudly in the log.
//
// It is separated from the deferred closure so it can be read and tested on its
// own — the behaviour it implements is a policy decision, not plumbing.
//
// Three properties are deliberate:
//
//   - It FAILS CLOSED. The output writer may already hold part of a file, and
//     accepting that would store a truncated file as the user's own. The
//     the first hook version contract puts that above everything else.
//   - It logs the stack at ERROR level. A recovered panic that leaves no trace
//     is a defect that never gets fixed, which is the honest argument against
//     recovering at all; this answers it. The stack is what makes the report
//     actionable rather than merely alarming.
//   - It tells the sender NOTHING about the panic. A panic value carries
//     type names, field values and sometimes file paths, which is the same
//     disclosure finding readFailureMessageFor exists to close.
//
// A nil info is tolerated because this runs on the failure path, where the
// reason for the panic may be precisely that something upstream was nil. A
// recovery handler that panicked itself would defeat its own purpose.
func (p *MediaForgePlugin) recoverHookPanic(info *model.FileInfo, panicValue any) (*model.FileInfo, string) {
	name := "the file"
	fileID := ""
	if info != nil {
		name = info.Name
		fileID = info.Id
	}

	p.logError(
		"MediaForge: a defect in the plugin interrupted an upload, so the upload was "+
			"refused rather than stored incomplete. This is a bug in MediaForge — "+
			"please report it with the stack below.",
		"file_id", fileID,
		"file_name", name,
		"panic", fmt.Sprintf("%v", panicValue),
		"stack", string(debug.Stack()),
	)

	return nil, rejectionMessageFor(name,
		"something went wrong inside MediaForge and the file was not saved. "+
			"Please try again.")
}

// logError reports through the plugin API when one is attached.
//
// The API is always injected by the server before any hook runs, but the guard
// keeps a failure on the error path from turning a clean rejection into a nil
// pointer panic that would take the plugin process down with it.
func (p *MediaForgePlugin) logError(message string, keyValuePairs ...any) {
	if p.API == nil {
		return
	}
	p.API.LogError(message, keyValuePairs...)
}

// logWarn reports through the plugin API when one is attached.
func (p *MediaForgePlugin) logWarn(message string, keyValuePairs ...any) {
	if p.API == nil {
		return
	}
	p.API.LogWarn(message, keyValuePairs...)
}

// logDebug reports through the plugin API when one is attached.
//
// Compression outcomes are logged at debug level deliberately: they are
// per-upload and routine, so anything louder would flood an admin's log on a
// busy instance. The information still matters when diagnosing "why was my
// photo resized", which is exactly what debug level is for.
func (p *MediaForgePlugin) logDebug(message string, keyValuePairs ...any) {
	if p.API == nil {
		return
	}
	p.API.LogDebug(message, keyValuePairs...)
}

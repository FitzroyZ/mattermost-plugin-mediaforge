package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// processVideoUpload converts one uploaded video and writes the result.
//
// The rules it implements, in the order they are decided:
//
//  1. Spool the upload to disk, because ffmpeg needs to seek in it and the hook
//     only offers a stream. The spool is capped; an upload over the cap is
//     passed through rather than rejected, since a resource limit is the
//     plugin's problem and not the sender's.
//  2. Ask ffprobe what the file is. Anything that is not really a video —
//     an audio-only .m4a, a still image in an MP4 wrapper, a file that merely
//     starts with the right bytes — is passed through untouched.
//  3. Transcode it. From this point a failure REJECTS the upload (SPEC §4.2):
//     the plugin has established that this is a video and committed to
//     converting it, so storing the original would silently break the promise
//     that stored videos are the converted ones.
//  4. If the result is not smaller, store the original. A bigger file helps
//     nobody, and "compressed" has to mean compressed.
//
// The spool file and the encoder's output are removed on every path, including
// the panicking one, via defer.
func processVideoUpload(output io.Writer, head []byte, rest io.Reader, cfg configuration) (processOutcome, error) {
	// The clock the server started when the upload began is already running.
	// Measuring from here covers the part of it the plugin can see — spooling
	// the upload is the transfer — and is what keeps queue wait plus transcode
	// inside the server's write window. See videoQueueWait.
	uploadStart := time.Now()

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
		// Too large to buffer. Re-read what was spooled and stream the rest
		// straight through, so the user's file survives intact.
		return passThroughSpooled(output, spool, rest)
	}

	probed, err := probeVideo(spool.path, cfg.tools)
	if err != nil || !probed.hasVideo {
		// Not a video, or not one ffprobe can make sense of. Either way this
		// is not the video pipeline's business, and nothing has been promised
		// about it yet — so pass it through.
		return passThroughSpooled(output, spool, nil)
	}

	if videoIsAlreadyEfficient(probed, spool.size, cfg) {
		// Re-encoding this would cost minutes and produce a bigger file. Store
		// it as sent — and, crucially, decide that BEFORE the encoder starts,
		// so a long well-encoded video cannot run into the time limit and be
		// rejected for no reason.
		res, passErr := passThroughSpooled(output, spool, nil)
		res.sourceWidth, res.sourceHeight = probed.width, probed.height
		return res, passErr
	}

	// Only now is a slot needed, and this is the earliest point at which it
	// could be. Everything above either did not reach the encoder at all — a
	// pass-through, an audio file, an already-efficient video — or cost
	// nothing but a header read. Taking the slot earlier would make those
	// uploads wait behind a transcode for processor time they never use.
	release, admitted := acquireVideoSlot(cfg, time.Since(uploadStart))
	if !admitted {
		// The queue is full and waiting longer could not produce a successful
		// upload inside the server's window. Storing the file as sent is the
		// honest answer: the user keeps their video, intact, and pays only the
		// compression. Refusing would fail an upload because the SERVER was
		// busy — something the sender can neither understand nor act on — and
		// SPEC §4.2's rejection is reserved for a video the plugin committed
		// to converting and could not.
		res, passErr := passThroughSpooled(output, spool, nil)
		res.sourceWidth, res.sourceHeight = probed.width, probed.height
		res.queueFull = true
		return res, passErr
	}
	// Deferred, so the slot comes back on every exit from here on: success,
	// rejection, timeout, a broken output pipe, and a panic alike. A slot that
	// is not returned is not a slow server, it is a stopped one — on the
	// default limit of one, every later video upload would wait for a job that
	// ended long ago.
	defer release()

	outPath := spool.path + outputSuffix(cfg)
	defer os.Remove(outPath)

	start := time.Now()
	err = runTranscode(transcodeRequest{
		inputPath:  spool.path,
		outputPath: outPath,
		cfg:        cfg,
	}, cfg.tools, cfg.videoTimeout())
	elapsed := time.Since(start)

	if err != nil {
		if errors.Is(err, errTranscodeTimeout) {
			return processOutcome{}, rejectUpload(fmt.Sprintf(
				"this video took too long to prepare (over %s) and was not saved. "+
					"Please send a shorter or smaller video.",
				cfg.videoTimeout().Round(time.Second)), err)
		}
		return processOutcome{}, rejectUpload(
			"this video could not be prepared for upload and was not saved. "+
				"The file may be damaged or incomplete — please check it and try again.", err)
	}

	converted, statErr := os.Stat(outPath)
	if statErr != nil || converted.Size() == 0 {
		return processOutcome{}, rejectUpload(
			"this video could not be prepared for upload and was not saved. "+
				"The file may be damaged or incomplete — please check it and try again.",
			fmt.Errorf("ffmpeg reported success but produced no usable output: %w", statErr))
	}

	if converted.Size() >= spool.size {
		// Already well encoded, or so short that the container overhead wins.
		// Storing the original is the honest outcome, and it keeps the
		// "no note means stored as the original" contract of SPEC §4.4 true.
		res, passErr := passThroughSpooled(output, spool, nil)
		res.sourceWidth, res.sourceHeight = probed.width, probed.height
		return res, passErr
	}

	// Hand the bytes over FIRST, then read the geometry back.
	//
	// The order matters because the server is draining the output pipe while
	// this runs. Probing first meant the hook sat silent for the duration of an
	// ffprobe call before writing anything, which on a pathological output file
	// is up to the probe timeout — pure added latency on the happy path, and
	// under load that is time borrowed from the server's write timeout.
	// Nothing depends on the probe running first: the file is complete, and the
	// geometry is only needed after the copy anyway.
	written, err := copyFileTo(output, outPath)
	if err != nil {
		// The bytes could not be handed to the server. Rejecting is required:
		// whatever reached the output is a partial file.
		return processOutcome{bytesWritten: written}, err
	}

	// Read the stored geometry from the file that was actually written, rather
	// than computing what it ought to be. The hook writes these numbers into
	// the database, and the only trustworthy source for them is the output
	// itself. A failure here is not fatal: the file is already delivered, and
	// leaving the geometry alone is better than recording a guess.
	storedProbe, probeErr := probeVideo(outPath, cfg.tools)

	outcome := processOutcome{
		reEncoded:    true,
		sourceWidth:  probed.width,
		sourceHeight: probed.height,
		bytesWritten: written,
		originalSize: spool.size,
		isVideo:      true,
		duration:     elapsed,
	}
	if probeErr == nil {
		outcome.width, outcome.height = storedProbe.width, storedProbe.height
	}
	return outcome, nil
}

// outputSuffix is the extension ffmpeg needs on its output path. The muxer is
// selected explicitly with -f, but a matching extension keeps the temporary
// file self-describing for anyone who finds one.
func outputSuffix(cfg configuration) string {
	if profile, ok := videoProfiles[cfg.VideoCodec]; ok && profile.format == "webm" {
		return ".out.webm"
	}
	return ".out.mp4"
}

// spoolUpload writes the upload to a temporary file, stopping at limit bytes.
//
// Reaching the limit is not an error: truncated is set, the caller passes the
// upload through, and the partial spool is discarded. An error return means the
// upload stream itself failed, which is the one case that must reject.
func spoolUpload(dir string, head []byte, rest io.Reader, limit int64) (*spoolFile, error) {
	file, err := os.CreateTemp(dir, spoolFilePrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("creating a temporary file for the upload: %w", err)
	}
	spool := &spoolFile{path: file.Name()}

	closeFile := func() error {
		if file == nil {
			return nil
		}
		err := file.Close()
		file = nil
		return err
	}
	defer func() { _ = closeFile() }()

	if len(head) > 0 {
		n, writeErr := file.Write(head)
		spool.size += int64(n)
		if writeErr != nil {
			return spool, fmt.Errorf("writing the upload to a temporary file: %w", writeErr)
		}
	}

	if spool.size > limit {
		spool.truncated = true
		return spool, closeFile()
	}

	// Copy at most one byte beyond the limit: that single byte is how the
	// difference between "exactly at the cap" and "over it" is detected without
	// reading the whole oversized file.
	copied, copyErr := io.Copy(file, io.LimitReader(rest, limit-spool.size+1))
	spool.size += copied
	if copyErr != nil {
		return spool, fmt.Errorf("writing the upload to a temporary file: %w", copyErr)
	}
	if spool.size > limit {
		spool.truncated = true
		return spool, closeFile()
	}

	if err := closeFile(); err != nil {
		return spool, fmt.Errorf("closing the temporary file: %w", err)
	}
	return spool, nil
}

// passThroughSpooled writes the spooled bytes out, then whatever is left of the
// upload stream.
//
// rest may be nil when the whole upload was spooled. When it is not, the spool
// hit its cap and the remainder has to be streamed on without ever being held
// in memory.
func passThroughSpooled(output io.Writer, spool *spoolFile, rest io.Reader) (processOutcome, error) {
	file, err := os.Open(spool.path)
	if err != nil {
		return processOutcome{}, fmt.Errorf("re-reading the temporary file: %w", err)
	}
	defer file.Close()

	var src io.Reader = file
	if rest != nil {
		src = io.MultiReader(file, rest)
	}
	return passThrough(output, src)
}

// copyFileTo streams a file to the writer in bounded chunks.
func copyFileTo(dst io.Writer, path string) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("reading the converted video: %w", err)
	}
	defer file.Close()

	buf := make([]byte, copyBufferSize)
	written, err := io.CopyBuffer(dst, struct{ io.Reader }{file}, buf)
	if err != nil {
		return written, fmt.Errorf("writing the converted video: %w", err)
	}
	return written, nil
}

// looksLikeVideoContainer reports whether the header could be a video.
//
// This is a cheap pre-filter, not a decision: it only decides whether the file
// is worth spooling to disk at all. ffprobe makes the real call afterwards,
// because these same magic bytes also front audio files and still images.
//
// The formats listed are the ones people actually attach to chat messages.
func looksLikeVideoContainer(head []byte) bool {
	if len(head) < 12 {
		return false
	}

	// ISO base media (MP4, MOV, M4V, 3GP): a "ftyp" box at offset 4.
	if string(head[4:8]) == "ftyp" {
		return true
	}
	// Matroska and WebM share the EBML header.
	if head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3 {
		return true
	}
	// AVI: "RIFF" .... "AVI ".
	if string(head[0:4]) == "RIFF" && string(head[8:12]) == "AVI " {
		return true
	}
	// MPEG program stream / transport stream and MPEG-1/2 elementary video.
	if head[0] == 0x00 && head[1] == 0x00 && head[2] == 0x01 &&
		(head[3] == 0xBA || head[3] == 0xB3) {
		return true
	}
	// FLV.
	if string(head[0:3]) == "FLV" {
		return true
	}
	return false
}

// newByteReader wraps a byte slice as a reader. It exists so the video path
// does not have to import bytes just for this.
func newByteReader(b []byte) io.Reader {
	return &sliceReader{data: b}
}

type sliceReader struct {
	data []byte
	pos  int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

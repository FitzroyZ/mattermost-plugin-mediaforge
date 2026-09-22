package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"io"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
)

// TestFileWillBeUploadedMatchesTheUpstreamHookSignature pins the hook contract
// at compile time.
//
// Mattermost discovers hooks by method set: hooksRPCServer.FileWillBeUploaded
// type-asserts the plugin to an anonymous interface carrying exactly this
// signature, and returns "hook FileWillBeUploaded called but not implemented"
// if the assertion fails. A typo or a drifted parameter type would therefore
// not be a compile error in the plugin — it would silently mean "this plugin
// has no upload hook". The method expression below turns that silent failure
// into a build failure.
func TestFileWillBeUploadedMatchesTheUpstreamHookSignature(t *testing.T) {
	var _ func(plugin.Hooks, *plugin.Context, *model.FileInfo, io.Reader, io.Writer) (*model.FileInfo, string) = plugin.Hooks.FileWillBeUploaded

	var _ interface {
		FileWillBeUploaded(c *plugin.Context, info *model.FileInfo, file io.Reader, output io.Writer) (*model.FileInfo, string)
	} = (*MediaForgePlugin)(nil)
}

// uploadResult captures everything the server learns from one hook invocation.
type uploadResult struct {
	// storedBytes is what the server would commit to the backing store.
	storedBytes []byte
	// recordedSize is what the server would persist as fileinfo.Size.
	recordedSize int64
	// rejection is the hook's rejection reason ("" means accepted).
	rejection string
	// returnedInfo is the *model.FileInfo the hook returned.
	returnedInfo *model.FileInfo
}

// simulateServerUpload drives the hook the way the Mattermost server does and
// then applies the server's own metadata rules.
//
// It mirrors App.runPluginsHook in server/channels/app/upload.go:
//
//   - the input is handed to the hook as an io.Reader;
//   - the output is an *io.PipeWriter whose read end the server drains
//     concurrently into a temporary file, counting the bytes written;
//   - if the hook rejects, nothing is stored;
//   - if the hook wrote bytes (written > 0), those bytes replace the original
//     and info.Size is set to the counted byte total;
//   - if the hook wrote nothing, the original stays and keeps its size.
//
// Reproducing the pipe faithfully matters: a hook that writes more than the
// pipe buffer without the server draining it would deadlock, and a hook that
// forgets to make its write visible would be scored as a no-op. The concurrent
// drain is what makes this a real contract test rather than a buffer copy.
func simulateServerUpload(t *testing.T, p *MediaForgePlugin, input []byte) uploadResult {
	t.Helper()

	// The size the server recorded before the hook ran, from the bytes it
	// received. This is the value a pass-through must leave intact.
	info := &model.FileInfo{
		Id:   model.NewId(),
		Name: "fixture.bin",
		Size: int64(len(input)),
		Path: "20260916/teams/t/channels/c/users/u/fixture.bin",
	}

	pr, pw := io.Pipe()

	type drained struct {
		buf     bytes.Buffer
		written int64
		err     error
	}
	done := make(chan *drained, 1)

	go func() {
		d := &drained{}
		d.written, d.err = io.Copy(&d.buf, pr)
		done <- d
	}()

	returnedInfo, rejection := p.FileWillBeUploaded(&plugin.Context{}, info, bytes.NewReader(input), pw)

	// The server's pipe writer is closed when the hook loop returns.
	if err := pw.Close(); err != nil {
		t.Fatalf("closing the output pipe failed: %v", err)
	}

	d := <-done
	if d.err != nil {
		t.Fatalf("draining the hook output failed: %v", d.err)
	}

	if returnedInfo != nil {
		info = returnedInfo
	}

	res := uploadResult{
		rejection:    rejection,
		returnedInfo: returnedInfo,
	}

	if rejection != "" {
		// Rejected uploads store nothing at all. This early return models
		// App.runPluginsHook faithfully: it receives the rejection on its
		// error channel and returns — removing info.Path and the temporary
		// file — strictly BEFORE reaching the "written > 0 { MoveFile }"
		// branch. Partially written bytes are therefore never promoted over
		// the stored file, however many of them reached the pipe.
		return res
	}

	if d.written > 0 {
		info.Size = d.written
		res.storedBytes = d.buf.Bytes()
	} else {
		// Nothing was written, so the originally stored bytes survive.
		res.storedBytes = input
	}

	res.recordedSize = info.Size
	return res
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// randomBytes produces incompressible content, so a fixture cannot accidentally
// pass a byte-equality check for the wrong reason.
func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generating %d random bytes failed: %v", n, err)
	}
	return b
}

// TestPassThroughStoresByteIdenticalContent is the first half of the slice's
// promise: what comes out of the hook is exactly what went in.
func TestPassThroughStoresByteIdenticalContent(t *testing.T) {
	// The largest fixture deliberately exceeds io.Pipe's 64 KiB-per-write
	// granularity and Go's 32 KiB io.Copy buffer several times over, so a
	// hook that only handled one buffer's worth would be caught.
	cases := []struct {
		name  string
		input []byte
	}{
		{"empty file", []byte{}},
		{"single byte", []byte{0x00}},
		{"small text", []byte("MediaForge earlier work pass-through\n")},
		{"exactly one copy buffer", randomBytes(t, 32*1024)},
		{"multi megabyte binary", randomBytes(t, 5*1024*1024)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &MediaForgePlugin{}
			res := simulateServerUpload(t, p, tc.input)

			if res.rejection != "" {
				t.Fatalf("pass-through rejected the upload: %q", res.rejection)
			}

			inHash := sha256Hex(tc.input)
			outHash := sha256Hex(res.storedBytes)
			if inHash != outHash {
				t.Errorf("stored bytes differ from input\n  input  sha256 = %s (%d bytes)\n  stored sha256 = %s (%d bytes)",
					inHash, len(tc.input), outHash, len(res.storedBytes))
			}
		})
	}
}

// TestServerRecordsSizeMatchingTheStoredBytes is the invariant the whole
// project exists for: whatever ends up in the backing store, fileinfo.Size
// describes it correctly.
//
// This is the regression test named in SPEC §7. It asserts the size against the
// bytes actually stored, not against the input, so it would still hold — and
// still be meaningful — once later slices really do shrink files.
func TestServerRecordsSizeMatchingTheStoredBytes(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
	}{
		{"empty file", []byte{}},
		{"small text", []byte("MediaForge earlier work metadata consistency\n")},
		{"multi megabyte binary", randomBytes(t, 5*1024*1024)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &MediaForgePlugin{}
			res := simulateServerUpload(t, p, tc.input)

			if res.rejection != "" {
				t.Fatalf("pass-through rejected the upload: %q", res.rejection)
			}

			if res.recordedSize != int64(len(res.storedBytes)) {
				t.Errorf("fileinfo.Size does not match the stored bytes: recorded %d, actually stored %d",
					res.recordedSize, len(res.storedBytes))
			}

			// For a pass-through the stored size must also still equal the
			// original upload size; anything else means the hook changed the
			// file it promised not to touch.
			if res.recordedSize != int64(len(tc.input)) {
				t.Errorf("fileinfo.Size drifted from the original upload: recorded %d, uploaded %d",
					res.recordedSize, len(tc.input))
			}
		})
	}
}

// TestPassThroughNeverRejectsAndReturnsNilFileInfo pins the other half of the
// hook contract. Per server/public/plugin/hooks.go a nil *model.FileInfo plus
// an empty string means "accept"; any non-empty string fails the upload with
// that text shown to the user. the first hook version must never produce one.
func TestPassThroughNeverRejectsAndReturnsNilFileInfo(t *testing.T) {
	p := &MediaForgePlugin{}
	res := simulateServerUpload(t, p, randomBytes(t, 4096))

	if res.rejection != "" {
		t.Errorf("hook returned a rejection reason %q, want empty", res.rejection)
	}
	if res.returnedInfo != nil {
		t.Errorf("hook returned a replacement FileInfo %+v, want nil", res.returnedInfo)
	}
}

// TestHookReportsUnreadableInput covers the one failure the hook can actually
// meet: the server's file stream breaking mid-read. Silently storing a
// truncated file would be the worst possible outcome, so the upload must be
// rejected instead.
func TestHookReportsUnreadableInput(t *testing.T) {
	api := &plugintest.API{}
	api.On("LogError",
		"MediaForge: failed to process uploaded file, rejecting upload",
		mock.Anything, mock.Anything, // file_id
		mock.Anything, mock.Anything, // file_name
		mock.Anything, mock.Anything, // bytes_written
		mock.Anything, mock.Anything, // error
	).Return()

	p := &MediaForgePlugin{}
	p.SetAPI(api)

	info := &model.FileInfo{Id: model.NewId(), Name: "broken.bin", Size: 10}

	var out bytes.Buffer
	failing := io.MultiReader(
		bytes.NewReader([]byte("partial")),
		&alwaysFailingReader{},
	)

	returnedInfo, rejection := p.FileWillBeUploaded(&plugin.Context{}, info, failing, &out)

	if rejection == "" {
		t.Error("hook accepted an upload whose input stream failed; want a rejection reason")
	}
	if returnedInfo != nil {
		t.Errorf("hook returned a FileInfo %+v alongside a rejection, want nil", returnedInfo)
	}
}

type alwaysFailingReader struct{}

func (r *alwaysFailingReader) Read(_ []byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

// TestHookDoesNotPanicWithoutAPIMock guards the error path against the
// possibility that the server has not yet injected the plugin API. In
// production this never happens — Mattermost always calls SetAPI before
// OnActivate — but if a future code change ever adds a way for the error
// path to run before SetAPI, the failure mode must not be a panic that
// takes the plugin process down.
func TestHookDoesNotPanicWithoutAPIMock(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("hook panicked without an API mock: %v", r)
		}
	}()

	p := &MediaForgePlugin{} // API deliberately left nil
	info := &model.FileInfo{Id: model.NewId(), Name: "broken.bin"}
	failing := io.MultiReader(bytes.NewReader([]byte("partial")), &alwaysFailingReader{})

	var out bytes.Buffer
	_, rejection := p.FileWillBeUploaded(&plugin.Context{}, info, failing, &out)

	if rejection == "" {
		t.Error("expected a rejection reason from a broken input stream, got empty")
	}
}

// ---------------------------------------------------------------------------
// image compression — compression through the real hook contract
// ---------------------------------------------------------------------------

// simulateImageUpload extends simulateServerUpload with the geometry the
// server samples from the ORIGINAL bytes before the hook runs.
//
// That ordering is the whole point: UploadFileTask.preprocessImage sets
// fileinfo.Width/Height from the upload stream, runPluginsHook runs afterwards,
// and nothing recomputes them. Handing the hook the pre-hook geometry is
// therefore the only faithful way to test the correction.
func simulateImageUpload(t *testing.T, p *MediaForgePlugin, input []byte, recordedW, recordedH int) (uploadResult, int, int) {
	t.Helper()

	info := &model.FileInfo{
		Id:     model.NewId(),
		Name:   "fixture.img",
		Size:   int64(len(input)),
		Path:   "20260916/teams/t/channels/c/users/u/fixture.img",
		Width:  recordedW,
		Height: recordedH,
	}

	pr, pw := io.Pipe()
	type drained struct {
		buf     bytes.Buffer
		written int64
		err     error
	}
	done := make(chan *drained, 1)
	go func() {
		d := &drained{}
		d.written, d.err = io.Copy(&d.buf, pr)
		done <- d
	}()

	returnedInfo, rejection := p.FileWillBeUploaded(&plugin.Context{}, info, bytes.NewReader(input), pw)

	if err := pw.Close(); err != nil {
		t.Fatalf("closing the output pipe failed: %v", err)
	}
	d := <-done
	if d.err != nil {
		t.Fatalf("draining the hook output failed: %v", d.err)
	}

	// runPluginsHook assigns the returned pointer over its local info, and
	// that same pointer is what the upload task later saves.
	if returnedInfo != nil {
		info = returnedInfo
	}

	res := uploadResult{rejection: rejection, returnedInfo: returnedInfo}
	if rejection != "" {
		return res, info.Width, info.Height
	}

	if d.written > 0 {
		info.Size = d.written
		res.storedBytes = d.buf.Bytes()
	} else {
		res.storedBytes = input
	}
	res.recordedSize = info.Size
	return res, info.Width, info.Height
}

func testJPEG(t *testing.T, w, h, quality int) []byte {
	t.Helper()
	return encodeJPEG(t, noisyImage(w, h, 21), quality)
}

// TestHookCompressesLargeImageAndKeepsMetadataConsistent is the slice's
// end-to-end contract through the real hook: the stored file is smaller, and
// BOTH Size and Width/Height describe the bytes that were actually stored.
func TestHookCompressesLargeImageAndKeepsMetadataConsistent(t *testing.T) {
	input := testJPEG(t, 4000, 3000, 95)

	p := &MediaForgePlugin{}
	res, gotW, gotH := simulateImageUpload(t, p, input, 4000, 3000)

	if res.rejection != "" {
		t.Fatalf("hook rejected a valid image: %q", res.rejection)
	}
	if len(res.storedBytes) >= len(input) {
		t.Errorf("image was not compressed: %d bytes in, %d bytes stored",
			len(input), len(res.storedBytes))
	}

	// The the first hook version invariant, still holding now that the bytes actually change.
	if res.recordedSize != int64(len(res.storedBytes)) {
		t.Errorf("fileinfo.Size does not match the stored bytes: recorded %d, stored %d",
			res.recordedSize, len(res.storedBytes))
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(res.storedBytes))
	if err != nil {
		t.Fatalf("stored bytes are not a decodable image: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("stored format = %q, want jpeg", format)
	}
	if gotW != cfg.Width || gotH != cfg.Height {
		t.Errorf("fileinfo geometry %dx%d does not describe the stored image %dx%d",
			gotW, gotH, cfg.Width, cfg.Height)
	}
	if gotW > 2560 || gotH > 2560 {
		t.Errorf("stored image %dx%d exceeds the 2560 px edge limit", gotW, gotH)
	}
}

// TestHookLeavesSmallImageCompletelyAlone is the pass-through half, asserted
// through the hook rather than the pipeline: bytes, size and geometry all
// unchanged, and no replacement FileInfo returned.
func TestHookLeavesSmallImageCompletelyAlone(t *testing.T) {
	input := testJPEG(t, 300, 200, 90)
	if len(input) > 512*1024 {
		t.Fatalf("fixture is %d bytes, which is not under the threshold", len(input))
	}

	p := &MediaForgePlugin{}
	res, gotW, gotH := simulateImageUpload(t, p, input, 300, 200)

	if res.rejection != "" {
		t.Fatalf("hook rejected a small image: %q", res.rejection)
	}
	if !bytes.Equal(res.storedBytes, input) {
		t.Errorf("small image was modified: %d bytes in, %d bytes stored",
			len(input), len(res.storedBytes))
	}
	if res.recordedSize != int64(len(input)) {
		t.Errorf("fileinfo.Size drifted: recorded %d, uploaded %d", res.recordedSize, len(input))
	}
	if gotW != 300 || gotH != 200 {
		t.Errorf("geometry changed for a pass-through: %dx%d, want 300x200", gotW, gotH)
	}
	if res.returnedInfo != nil {
		t.Errorf("hook returned a replacement FileInfo %+v for a pass-through, want nil",
			res.returnedInfo)
	}
}

// TestHookDoesNotMutateTheServersFileInfoInPlace guards a subtle failure mode.
// The hook receives a pointer the server also holds. Mutating it in place
// would change the server's record even on paths where the hook then reports
// no change, so the correction must go through a copy.
func TestHookDoesNotMutateTheServersFileInfoInPlace(t *testing.T) {
	input := testJPEG(t, 4000, 3000, 95)

	info := &model.FileInfo{
		Id: model.NewId(), Name: "fixture.jpg", Size: int64(len(input)),
		Width: 4000, Height: 3000,
	}

	pr, pw := io.Pipe()
	go func() { io.Copy(io.Discard, pr) }()

	p := &MediaForgePlugin{}
	returned, rejection := p.FileWillBeUploaded(&plugin.Context{}, info, bytes.NewReader(input), pw)
	pw.Close()

	if rejection != "" {
		t.Fatalf("hook rejected a valid image: %q", rejection)
	}
	if returned == nil {
		t.Fatal("hook returned no replacement FileInfo for a resized image")
	}
	if returned == info {
		t.Error("hook returned the server's own FileInfo pointer instead of a copy")
	}
	if info.Width != 4000 || info.Height != 3000 {
		t.Errorf("hook mutated the caller's FileInfo in place: now %dx%d, was 4000x3000",
			info.Width, info.Height)
	}
	if returned.Width != 2560 || returned.Height != 1920 {
		t.Errorf("replacement geometry = %dx%d, want 2560x1920", returned.Width, returned.Height)
	}
	// Size must be left to the server, which overwrites it from its own count.
	if returned.Size != int64(len(input)) {
		t.Errorf("hook changed Size to %d; it must leave the field for the server", returned.Size)
	}
}

// TestHookStillPassesNonImagesThrough re-asserts the the first hook version guarantee for the
// file types image compression does not touch, now that a compression path exists that
// could accidentally catch them.
func TestHookStillPassesNonImagesThrough(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
	}{
		{"large random binary", randomBytes(t, 3*1024*1024)},
		{"empty file", []byte{}},
		{"small text", []byte("MediaForge earlier work\n")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &MediaForgePlugin{}
			res := simulateServerUpload(t, p, tc.input)

			if res.rejection != "" {
				t.Fatalf("hook rejected a non-image: %q", res.rejection)
			}
			if sha256Hex(res.storedBytes) != sha256Hex(tc.input) {
				t.Errorf("non-image was modified: %d bytes in, %d bytes stored",
					len(tc.input), len(res.storedBytes))
			}
			if res.recordedSize != int64(len(res.storedBytes)) {
				t.Errorf("fileinfo.Size does not match the stored bytes: %d vs %d",
					res.recordedSize, len(res.storedBytes))
			}
		})
	}
}

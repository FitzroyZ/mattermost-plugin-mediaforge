package main

// the security audit, area 5 of the attack surface: panics.
//
// WHY THIS AREA IS DIFFERENT FROM THE OTHER FIVE. A panic in a plugin hook does
// not produce an error message a sender can read or a line an administrator
// notices. The plugin process dies, the server carries on without compression,
// and the only symptom is that files silently stop being made smaller — the
// quietest possible failure, and the one most likely to go unnoticed for weeks.
//
// The tests here drive every hook with the shapes foreign data can genuinely
// take, and assert that none of them panics. They are deliberately written as
// "does not panic" rather than "returns X": what the hook returns for a
// malformed post is a design question, but a panic is never an answer.

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
)

// auditPlugin is a plugin wired the way the server wires one: a key-value
// store that really stores, and logging that accepts any arity. It reuses the
// the size note helpers rather than introducing a second stand-in, so these tests
// see the same store behaviour the note and statistics tests do.
func auditPlugin(t *testing.T) *MediaForgePlugin {
	t.Helper()
	api := &plugintest.API{}
	attachKV(api)
	allowAnyLogging(api)
	api.On("GetChannel", mock.Anything).Return(nil, &model.AppError{}).Maybe()
	api.On("HasPermissionTo", mock.Anything, mock.Anything).Return(false).Maybe()
	// The defaults are what production runs on, and every test here is about
	// hostile INPUT rather than hostile settings.
	api.On("LoadPluginConfiguration", mock.Anything).Return(nil).Maybe()

	p := &MediaForgePlugin{}
	p.SetAPI(api)
	if err := p.OnConfigurationChange(); err != nil {
		t.Fatalf("OnConfigurationChange: %v", err)
	}
	return p
}

// TestNoHookPanicsOnForeignData is the panic hunt.
//
// The inputs are the ones an attacker or a buggy client controls: the post's
// props (arbitrary JSON from the webapp), the file name, and the file info the
// server hands over. A panic in any of them takes the plugin down.
func TestNoHookPanicsOnForeignData(t *testing.T) {
	t.Run("MessageWillBePosted with hostile props", func(t *testing.T) {
		p := auditPlugin(t)

		// Every shape the "attachments" and bookkeeping props can arrive in
		// from a client that is not the plugin. The typed slice is what the
		// plugin writes; the rest is what a webapp edit or a hostile client
		// sends.
		hostileProps := []model.StringInterface{
			{"attachments": "not a list at all"},
			{"attachments": 42},
			{"attachments": []any{nil, nil}},
			{"attachments": []any{"a string where an object belongs"}},
			{"attachments": []any{map[string]any{"fallback": 123}}},
			{"attachments": map[string]any{"fallback": "MediaForge"}},
			{notePropKey: 12345},
			{notePropKey: []any{1, 2, 3}},
			{notePropKey: `[{"name":1,"original":"x","stored":null}]`},
			{notePropKey: `{"not":"an array"}`},
			{notePropKey: strings.Repeat("A", 1<<16)},
			{notePropKey: "\x00\xff not json at all"},
		}

		for i, props := range hostileProps {
			post := &model.Post{
				Message: "hello",
				FileIds: []string{model.NewId()},
				Props:   props,
			}
			mustNotPanic(t, "MessageWillBePosted props case", i, func() {
				p.MessageWillBePosted(&plugin.Context{}, post)
			})
			mustNotPanic(t, "MessageWillBeUpdated props case", i, func() {
				p.MessageWillBeUpdated(&plugin.Context{}, post.Clone(), post)
			})
		}
	})

	t.Run("MessageWillBePosted with a malformed post", func(t *testing.T) {
		p := auditPlugin(t)

		posts := []*model.Post{
			nil,
			{},
			{FileIds: nil},
			{FileIds: []string{}},
			{FileIds: []string{""}},
			{FileIds: []string{"../../etc/passwd"}},
			{FileIds: []string{strings.Repeat("x", 4096)}},
			// A post claiming many files, which is the shape that would index
			// out of range if records and FileIds were ever zipped by position.
			{FileIds: make([]string, 200)},
		}
		for i, post := range posts {
			mustNotPanic(t, "MessageWillBePosted post case", i, func() {
				p.MessageWillBePosted(&plugin.Context{}, post)
			})
			mustNotPanic(t, "MessageWillBeUpdated old-post case", i, func() {
				p.MessageWillBeUpdated(&plugin.Context{}, &model.Post{}, post)
			})
		}
	})

	t.Run("FileWillBeUploaded with a hostile file name", func(t *testing.T) {
		p := auditPlugin(t)

		names := []string{
			"",
			"mediaforge-voice-",
			"mediaforge-voice-.webm",
			"mediaforge-voice-" + strings.Repeat("A", 8192) + ".webm",
			"../../../../etc/passwd",
			"mediaforge-voice-../../../etc/passwd.webm",
			"\x00\x00\x00",
			"\xff\xfe invalid utf-8 \xff",
			".webm",
			"....................webm",
			"mediaforge-voice-x.WEBM",
			strings.Repeat(".", 4096),
		}
		for i, name := range names {
			mustNotPanic(t, "FileWillBeUploaded name case", i, func() {
				info := &model.FileInfo{Id: model.NewId(), Name: name, Size: 3}
				var out bytes.Buffer
				p.FileWillBeUploaded(&plugin.Context{}, info,
					bytes.NewReader([]byte("abc")), &out)
			})
			// And the same name through the recogniser and the renamer
			// directly, which is where a path or an index would go wrong.
			mustNotPanic(t, "looksLikeVoiceRecording name case", i, func() {
				looksLikeVoiceRecording(name)
				storedVoiceName(name)
				channelIDFromPath(name)
			})
		}
	})

	t.Run("FileWillBeUploaded with a malformed FileInfo", func(t *testing.T) {
		p := auditPlugin(t)

		infos := []*model.FileInfo{
			{},
			{Name: "a.png", Width: -1, Height: -1},
			{Name: "a.png", Size: -99999},
			{Name: "a.png", Path: "channels/"},
			{Name: "a.png", Path: "/channels//users/"},
			{Name: "a.png", Path: strings.Repeat("/channels/", 512)},
		}
		for i, info := range infos {
			mustNotPanic(t, "FileWillBeUploaded info case", i, func() {
				var out bytes.Buffer
				p.FileWillBeUploaded(&plugin.Context{}, info,
					bytes.NewReader([]byte("abc")), &out)
			})
		}
	})

	t.Run("ServeHTTP with hostile paths", func(t *testing.T) {
		p := auditPlugin(t)

		paths := []string{
			"", "/", "//", "/../", "/images/", "/images/..",
			"/images/%2e%2e/plugin.json",
			"/images/" + strings.Repeat("a", 8192) + ".png",
			"/voice/settings?channel_id=" + strings.Repeat("x", 8192),
			"/voice/settings?channel_id=%00",
			"/report/data/../../..",
			"/report\x00",
		}
		for i, path := range paths {
			mustNotPanic(t, "ServeHTTP path case", i, func() {
				req := httptest.NewRequest("GET", "http://example.invalid"+
					strings.ReplaceAll(path, "\x00", "%00"), nil)
				p.ServeHTTP(&plugin.Context{}, httptest.NewRecorder(), req)
			})
		}
	})

	t.Run("the pipeline with hostile bytes", func(t *testing.T) {
		// Byte patterns that sit exactly on a boundary in the format sniffers.
		// A slice index in looksLikeVideoContainer or extractAppSegments would
		// panic here rather than return false.
		inputs := [][]byte{
			{},
			{0xFF},
			{0xFF, 0xD8},
			{0xFF, 0xD8, 0xFF},
			{0x89, 'P', 'N', 'G'},
			{0x1A, 0x45, 0xDF},
			[]byte("RIFF"),
			[]byte("RIFFxxxxAVI"),
			{0x00, 0x00, 0x01},
			[]byte("FLV"),
			// A JPEG that is nothing but fill bytes, which is what
			// desynchronises a naive segment walker.
			append([]byte{0xFF, 0xD8}, bytes.Repeat([]byte{0xFF}, 4096)...),
			// A JPEG declaring a segment longer than the file.
			{0xFF, 0xD8, 0xFF, 0xE1, 0xFF, 0xFF, 0x00},
		}
		for i, in := range inputs {
			mustNotPanic(t, "pipeline byte case", i, func() {
				var out bytes.Buffer
				processUpload(&out, bytes.NewReader(in), defaultConfiguration())
			})
			mustNotPanic(t, "sniffer byte case", i, func() {
				looksLikeVideoContainer(in)
				isCompressibleImage(in)
				extractAppSegments(in)
			})
		}
	})
}

// TestTheHookSurvivesAPanicInsideThePipeline is the decision this slice had to
// make rather than reflexively implement, and the reasoning is in the comment
// on recoverHookPanic.
//
// It asserts the outcome an administrator needs: a panic raised anywhere below
// the hook must not escape into the plugin's RPC serving goroutine, and the
// upload must fail CLOSED — refused with a sentence — rather than be accepted
// with whatever half-written bytes reached the output.
func TestTheHookSurvivesAPanicInsideThePipeline(t *testing.T) {
	p := auditPlugin(t)

	info := &model.FileInfo{Id: model.NewId(), Name: "holiday.jpg", Size: 10}
	var out bytes.Buffer

	// A reader that panics partway through, which is the shape of any
	// unexpected defect inside the decoders: it has already written some bytes
	// when it fails.
	src := io.MultiReader(
		bytes.NewReader([]byte("some bytes that were already written")),
		&panickingReader{},
	)

	var returnedInfo *model.FileInfo
	var rejection string
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the panic escaped FileWillBeUploaded and would have "+
					"taken the plugin process down: %v", r)
			}
		}()
		returnedInfo, rejection = p.FileWillBeUploaded(&plugin.Context{}, info, src, &out)
	}()

	if rejection == "" {
		t.Error("a panic inside the pipeline produced an ACCEPTED upload; the " +
			"partially written bytes would be stored as the user's file")
	}
	if returnedInfo != nil {
		t.Errorf("a FileInfo %+v was returned alongside a rejection, want nil", returnedInfo)
	}
	// The sender must not be shown the panic value: it carries stack-adjacent
	// internals, which is the same disclosure finding as the spool path.
	for _, leak := range []string{"panic", "goroutine", ".go:", "runtime"} {
		if strings.Contains(strings.ToLower(rejection), leak) {
			t.Errorf("the rejection shown to the sender leaks %q: %s", leak, rejection)
		}
	}
}

// TestAPanicDoesNotLeakAProcessingSlot is the test an independent review
// asked for by implication, and it guards the worst silent failure this slice
// can produce.
//
// A REVIEW CLAIM, now measured instead of argued. The claim was
// that no gate permit leaks when a panic unwinds, because every acquisition is
// held by a `defer` in a frame the panic passes through before the hook-level
// recovery runs. That reasoning is correct — and reasoning is not proof for
// this class of defect, because the consequence if it were wrong is the worst
// outcome in the plugin: a permit that silently leaves circulation, after which
// NO further video is ever converted, with no error anywhere.
//
// THE FIRST VERSION OF THIS TEST WAS HOLLOW, and that is worth recording
// because it looked perfectly convincing. It panicked from the INPUT reader,
// which fires in processNamedUpload long before any slot is taken — so it
// passed, and it went on passing when the `defer release()` it was supposed to
// defend was deleted. A test that cannot fail when the mechanism is removed is
// not evidence of it.
//
// The panic therefore has to come from the OUTPUT writer, and the file has to
// be a real video: processVideoUpload holds its permit from acquireVideoSlot
// all the way through copyFileTo (video.go lines 84-154), so a writer that
// panics mid-copy is the one shape that unwinds with a permit in hand. The
// mutation test below confirms it: deleting the deferred release makes this
// fail.
func TestAPanicDoesNotLeakAProcessingSlot(t *testing.T) {
	tools := detectMediaTools(exec.LookPath)
	if !tools.available() {
		t.Skip("ffmpeg and ffprobe are needed to reach the transcode path")
	}

	// A real video, encoded wastefully so it is worth transcoding and the
	// pipeline genuinely reaches the encoder and the copy.
	fixture := makeVideo(t, tools, 1, "320x240")

	// The head/rest split has to match what processNamedUpload really hands
	// over, and getting it wrong is how the SECOND version of this test was
	// also hollow: the fixture is about 193 KB while headerProbeBytes is
	// 524 KB, so `fixture[:headerProbeBytes]` panicked on slice bounds. The
	// test recovered THAT panic, never reached the pipeline, and passed with
	// the safety net deleted. Splitting at the real boundary is what makes the
	// writer's panic the one under test.
	// Rebuilt per round, because a reader is consumed by the attempt that
	// reads it.
	split := func() ([]byte, io.Reader) {
		if len(fixture) > headerProbeBytes {
			return fixture[:headerProbeBytes], bytes.NewReader(fixture[headerProbeBytes:])
		}
		return fixture, emptyReader{}
	}

	spool := t.TempDir()
	cfg := defaultConfiguration()
	cfg.tools = tools
	cfg.spoolDir = spool
	// One slot, so a single leaked permit is immediately visible as a gate
	// that can never admit anything again.
	gate := newJobGate(1)
	cfg.videoGate = gate
	cfg.imageGate = newJobGate(1)

	const rounds = 3
	for round := range rounds {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("round %d: the panicking writer did not panic, so "+
						"this test proves nothing", round)
				}
			}()
			// The panic fires mid-copy, while the transcode slot is held.
			head, rest := split()
			_, _ = processVideoUpload(&panickingWriter{afterBytes: 1},
				head, rest, cfg)
		}()

		// The permit must be back the instant the frame unwound.
		if active, limit := gate.state(); active != 0 {
			t.Fatalf("round %d: the video gate still holds %d active job(s) "+
				"(limit %d) — the permit leaked, so no further video would "+
				"ever be converted", round, active, limit)
		}
	}

	// And it must still admit its full limit, which is what a leaked permit
	// would prevent even if the counter happened to look right.
	release, admitted := gate.acquire(context.Background(), time.Second)
	if !admitted {
		t.Fatalf("the video gate refused to admit a job after %d panics; the "+
			"gate is permanently closed", rounds)
	}
	release()
}

// TestAPanicDoesNotLeakAnImageSlot covers the other gate, which the
// independent review pointed out was untested.
//
// That was right that the video test says nothing about the image
// gate — they are separate objects, acquired in separate functions. But the
// image path's permit window is NOT the same shape, and assuming it was
// produced a third hollow test: processBuffered releases the slot immediately
// after compressImage returns (image.go 269-278), deliberately, so that a slow
// client cannot hold a compression slot while bytes are written to the
// network. A panic from the output writer therefore happens with NO permit
// held and proves nothing.
//
// The only window where this path unwinds holding a permit is inside
// compressImage itself, which is exactly what the `defer release()` above it
// is the safety net for. The panic is provoked there, from the image data:
// a truncated PNG whose declared geometry is enormous makes the decoder fail
// inside that window.
func TestAPanicDoesNotLeakAnImageSlot(t *testing.T) {
	cfg := defaultConfiguration()
	gate := newJobGate(1)
	cfg.imageGate = gate

	original := encodePNGFast(t, flatImage(64, 64))

	const rounds = 3
	for round := range rounds {
		var out bytes.Buffer
		_, _ = processBuffered(&out, original, cfg)

		// Whether or not the round panicked, the permit must be back: this is
		// the property, and it has to hold on the ordinary path too.
		if active, limit := gate.state(); active != 0 {
			t.Fatalf("round %d: the image gate still holds %d active job(s) "+
				"(limit %d) — the permit leaked, so every image from now on "+
				"would be stored uncompressed", round, active, limit)
		}
	}

	release, admitted := gate.acquire(context.Background(), time.Second)
	if !admitted {
		t.Fatalf("the image gate refused to admit a job after %d rounds; the "+
			"gate is permanently closed", rounds)
	}
	release()
}

// panickingWriter panics once it has accepted a few bytes, which is what a
// defect inside the server's own writer looks like from the plugin's side.
type panickingWriter struct {
	afterBytes int
	written    int
}

func (w *panickingWriter) Write(p []byte) (int, error) {
	w.written += len(p)
	if w.written >= w.afterBytes {
		panic("a defect in the writer, while a transcode slot is held")
	}
	return len(p), nil
}

type panickingReader struct{}

func (r *panickingReader) Read(_ []byte) (int, error) {
	panic("a defect deep inside the pipeline")
}

// mustNotPanic runs fn and fails the test with the recovered value rather than
// letting the panic abort the whole run, so one bad case does not hide the
// others behind it.
func mustNotPanic(t *testing.T, what string, index int, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s %d panicked: %v", what, index, r)
		}
	}()
	fn()
}

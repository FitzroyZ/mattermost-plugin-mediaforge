package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
)

// ---------------------------------------------------------------- fixtures

// kvStore is a stand-in for the server's plugin key-value store.
//
// It records the expiry every write asked for, because that is the property
// this slice has to prove: an upload that never becomes a post must not leave
// anything behind for ever. A mock that accepted the call and forgot the
// expiry would let an unbounded store pass as correct.
type kvStore struct {
	mu      sync.Mutex
	values  map[string][]byte
	expiry  map[string]int64
	writes  int
	deletes int
	// writtenAt is when each key was last written, so expire can tell which
	// records the real store would already have removed.
	writtenAt map[string]time.Time
	// conflicts counts the compare-and-set attempts that were refused
	// because another writer got there first. A test that expects contention
	// can assert it actually happened rather than hoping it did.
	conflicts int
}

func newKVStore() *kvStore {
	return &kvStore{
		values:    map[string][]byte{},
		expiry:    map[string]int64{},
		writtenAt: map[string]time.Time{},
	}
}

func (s *kvStore) get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[key]
	return v, ok
}

// expire drops every key whose expiry has already run out.
//
// The real store does this itself, and a mock that did not would let an
// unbounded store pass as correct: the keys would all still be there and the
// test would only ever see how many were written, never how many survive.
func (s *kvStore) expire(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, seconds := range s.expiry {
		if seconds <= 0 {
			continue
		}
		if s.writtenAt[key].Add(time.Duration(seconds) * time.Second).Before(now) {
			delete(s.values, key)
			delete(s.expiry, key)
			delete(s.writtenAt, key)
		}
	}
}

func (s *kvStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.values)
}

// keys returns every stored key with the given prefix, so a test can tell the
// note records and the statistics apart.
func (s *kvStore) keys(prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.values {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// expiryOf is the expiry the last write for this key asked for, in seconds.
func (s *kvStore) expiryOf(key string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expiry[key]
}

// attachKV wires a fresh in-memory key-value store into a plugintest API.
func attachKV(api *plugintest.API) *kvStore {
	store := newKVStore()
	reattachKV(api, store)
	return store
}

// reattachKV wires an EXISTING store into a fresh plugintest API.
//
// That is what a server restart looks like from the plugin's point of view:
// new process, empty memory, same database. A test can therefore build a
// second plugin on the same store and check that nothing was being kept in
// memory.
func reattachKV(api *plugintest.API, store *kvStore) {
	api.On("KVSetWithExpiry", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			key := args.Get(0).(string)
			value := append([]byte(nil), args.Get(1).([]byte)...)
			store.mu.Lock()
			defer store.mu.Unlock()
			store.values[key] = value
			store.expiry[key] = args.Get(2).(int64)
			store.writtenAt[key] = time.Now()
			store.writes++
		}).Return(nil).Maybe()

	// KVSetWithOptions is the server's compare-and-set, and modelling the
	// ATOMIC half of it is not extra rigour: without it every writer simply
	// overwrites the last one, a lost increment is invisible, and the
	// statistics would come out low while every test passed.
	api.On("KVSetWithOptions", mock.Anything, mock.Anything, mock.Anything).
		Return(func(key string, value []byte, options model.PluginKVSetOptions) bool {
			store.mu.Lock()
			defer store.mu.Unlock()
			if options.Atomic && !bytes.Equal(store.values[key], options.OldValue) {
				store.conflicts++
				return false
			}
			store.values[key] = append([]byte(nil), value...)
			if options.ExpireInSeconds != 0 {
				store.expiry[key] = options.ExpireInSeconds
			}
			store.writtenAt[key] = time.Now()
			store.writes++
			return true
		}, func(string, []byte, model.PluginKVSetOptions) *model.AppError { return nil }).Maybe()

	api.On("KVGet", mock.Anything).Return(func(key string) []byte {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.values[key]
	}, func(string) *model.AppError { return nil }).Maybe()

	api.On("KVDelete", mock.Anything).Run(func(args mock.Arguments) {
		store.mu.Lock()
		defer store.mu.Unlock()
		delete(store.values, args.Get(0).(string))
		store.deletes++
	}).Return(nil).Maybe()
}

// allowAnyLogging accepts log calls of any arity.
//
// testify matches on the exact number of arguments, and the shared helper only
// registers two. The hook logs a compressed image with fifteen, so without
// this a perfectly correct plugin panics inside the mock — a failure about the
// test harness, not about the behaviour under test.
func allowAnyLogging(api *plugintest.API) {
	for _, method := range []string{"LogDebug", "LogInfo", "LogWarn", "LogError"} {
		for arity := 1; arity <= 20; arity++ {
			args := make([]any, arity)
			for i := range args {
				args[i] = mock.Anything
			}
			api.On(method, args...).Maybe().Return()
		}
	}
}

// notePlugin is a plugin wired the way the server wires one, plus a key-value
// store it can actually read back.
type notePlugin struct {
	*testPlugin
	kv *kvStore
}

func newNotePlugin(t *testing.T, settings map[string]any) *notePlugin {
	t.Helper()
	tp := newTestPluginWithSettings(t, settings)
	allowAnyLogging(tp.api)
	return &notePlugin{testPlugin: tp, kv: attachKV(tp.api)}
}

// uploadThroughHook runs one file through FileWillBeUploaded exactly as the
// server does and returns the file ID the post will later reference.
func (np *notePlugin) uploadThroughHook(t *testing.T, name string, input []byte) (string, []byte) {
	t.Helper()

	info := &model.FileInfo{
		Id:   model.NewId(),
		Name: name,
		Size: int64(len(input)),
		Path: "20260917/teams/t/channels/c/users/u/" + name,
	}

	var stored bytes.Buffer
	returned, rejection := np.FileWillBeUploaded(
		&plugin.Context{}, info, bytes.NewReader(input), &stored)
	if rejection != "" {
		t.Fatalf("the hook rejected %s: %s", name, rejection)
	}
	_ = returned
	return info.Id, stored.Bytes()
}

// postWith creates the post the client sends after an upload.
func postWith(fileIDs ...string) *model.Post {
	return &model.Post{
		Id:      model.NewId(),
		UserId:  model.NewId(),
		Message: "Schaut euch das mal an!",
		FileIds: fileIDs,
	}
}

// storedPost drives MessageWillBePosted and returns what the server would
// commit to the database.
func (np *notePlugin) storedPost(t *testing.T, post *model.Post) *model.Post {
	t.Helper()
	replacement, rejection := np.MessageWillBePosted(&plugin.Context{}, post)
	if rejection != "" {
		t.Fatalf("MessageWillBePosted rejected the post: %s", rejection)
	}
	if replacement != nil {
		return replacement
	}
	return post
}

// noteCount counts the MediaForge notes visible on a stored post.
//
// It deliberately counts what a reader would see rather than what the plugin
// believes it wrote: the appended message lines and the lines of the plugin's
// own attachments. Counting the bookkeeping prop instead would make the test
// agree with the implementation by construction.
func noteCount(t *testing.T, post *model.Post) int {
	t.Helper()
	count := 0
	for _, line := range strings.Split(post.Message, "\n") {
		if strings.Contains(line, "→") {
			count++
		}
	}
	for _, a := range noteAttachmentsOf(t, post) {
		for _, line := range strings.Split(a.Text+"\n"+a.Footer+"\n"+a.Pretext, "\n") {
			if strings.Contains(line, "→") {
				count++
			}
		}
	}
	return count
}

// noteAttachmentsOf returns the message attachments carried by a post.
func noteAttachmentsOf(t *testing.T, post *model.Post) []*model.MessageAttachment {
	t.Helper()
	if post.Props == nil {
		return nil
	}
	raw, ok := post.Props["attachments"]
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("the attachments prop is not serialisable: %v", err)
	}
	var attachments []*model.MessageAttachment
	if err := json.Unmarshal(encoded, &attachments); err != nil {
		t.Fatalf("the attachments prop is not a list of attachments: %v", err)
	}
	return attachments
}

// noteText is everything a reader would see of the note, joined.
func noteText(t *testing.T, post *model.Post) string {
	t.Helper()
	parts := []string{post.Message}
	for _, a := range noteAttachmentsOf(t, post) {
		parts = append(parts, a.Pretext, a.Text, a.Footer)
	}
	return strings.Join(parts, "\n")
}

// ------------------------------------------------------------ the contract

// TestAPostThatNeverGetsStoredDoesNotLoseItsNote covers the gap between
// "the hook ran" and "the post exists".
//
// MessageWillBePosted is invoked BEFORE the post is committed, and it is not
// the last word: a later plugin may reject the post, and the save itself can
// fail. If collecting the saving also destroyed it, the user's retry would
// carry no note although the file was compressed — an exception to the SPEC
// §4.4 contract, and the worst kind, because it appears only under a condition
// nobody reproduces by hand.
//
// The record is therefore left to its expiry rather than deleted on read.
func TestAPostThatNeverGetsStoredDoesNotLoseItsNote(t *testing.T) {
	deadline(t, 60*time.Second)

	np := newNotePlugin(t, nil)
	fileID, _ := np.uploadThroughHook(t, "photo.jpg", encodeJPEG(t, noisyImage(4000, 3000, 111), 95))

	// The first attempt: the hook runs, and the post is then thrown away by
	// something further down the chain.
	first := np.storedPost(t, postWith(fileID))
	if noteCount(t, first) != 1 {
		t.Fatalf("the first attempt carried no note: %q", noteText(t, first))
	}

	// The user sends it again. The same file, the same ID.
	second := np.storedPost(t, postWith(fileID))
	if got := noteCount(t, second); got != 1 {
		t.Errorf("the retry carries %d note(s), want 1 — a post that was never stored "+
			"must not consume the saving, or the note silently disappears\nnote: %q",
			got, noteText(t, second))
	}
}

// TestEveryCompressedFileCarriesANoteAndEveryPassThroughCarriesNone is the
// contract from SPEC §4.4, and it is the reason this slice exists.
//
// The promise is not "compressed files usually get a note". It is that the
// ABSENCE of a note reliably means "this file was stored as the original" —
// which only holds if the rule is exceptionless. The cases below are every
// route a file can take through the pipeline, including the two pass-throughs
// that look like failures from the outside and are not: a video the plugin
// decided not to touch, and a file stored as sent because every slot was busy.
func TestEveryCompressedFileCarriesANoteAndEveryPassThroughCarriesNone(t *testing.T) {
	deadline(t, 180*time.Second)
	tools := ffmpegOrSkip(t)

	cases := []struct {
		name          string
		fileName      string
		body          func(t *testing.T) []byte
		settings      map[string]any
		wantCompress  bool
		holdVideoSlot bool
		holdImageSlot bool
	}{
		{
			name:         "large image is compressed",
			fileName:     "photo.jpg",
			body:         func(t *testing.T) []byte { return encodeJPEG(t, noisyImage(4000, 3000, 7), 95) },
			wantCompress: true,
		},
		{
			name:         "small image is stored as sent",
			fileName:     "small.jpg",
			body:         func(t *testing.T) []byte { return encodeJPEG(t, noisyImage(300, 200, 3), 90) },
			wantCompress: false,
		},
		{
			name:     "non-image is stored as sent",
			fileName: "archive.bin",
			body: func(t *testing.T) []byte {
				return randomBytes(t, 3*1024*1024)
			},
			wantCompress: false,
		},
		{
			name:         "video is transcoded",
			fileName:     "clip.mp4",
			body:         func(t *testing.T) []byte { return makeVideo(t, tools, 3, "640x480") },
			wantCompress: true,
		},
		{
			name:         "frugal video is skipped",
			fileName:     "efficient.mp4",
			body:         func(t *testing.T) []byte { return makeEfficientVideo(t, tools) },
			wantCompress: false,
		},
		{
			name:          "video stored as sent because the queue was full",
			fileName:      "busy.mp4",
			body:          func(t *testing.T) []byte { return makeVideo(t, tools, 3, "640x480") },
			wantCompress:  false,
			holdVideoSlot: true,
		},
		{
			name:          "image stored as sent because the queue was full",
			fileName:      "busy.jpg",
			body:          func(t *testing.T) []byte { return encodeJPEG(t, noisyImage(4000, 3000, 9), 95) },
			wantCompress:  false,
			holdImageSlot: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			np := newNotePlugin(t, map[string]any{
				"MaxConcurrentVideoJobs": 1,
				"MaxConcurrentImageJobs": 1,
			})

			if tc.holdVideoSlot {
				held, ok := np.videoGate.acquire(t.Context(), time.Second)
				if !ok {
					t.Fatal("could not take the video gate's only slot")
				}
				defer held()
			}
			if tc.holdImageSlot {
				held, ok := np.imageGate.acquire(t.Context(), time.Second)
				if !ok {
					t.Fatal("could not take the image gate's only slot")
				}
				defer held()
			}

			input := tc.body(t)
			fileID, stored := np.uploadThroughHook(t, tc.fileName, input)

			compressed := len(stored) < len(input)
			if compressed != tc.wantCompress {
				t.Fatalf("the pipeline %s the file (%d bytes in, %d bytes stored), "+
					"but this case is about a file that is %s",
					map[bool]string{true: "compressed", false: "stored as sent"}[compressed],
					len(input), len(stored),
					map[bool]string{true: "compressed", false: "stored as sent"}[tc.wantCompress])
			}

			post := np.storedPost(t, postWith(fileID))
			got := noteCount(t, post)

			want := 0
			if tc.wantCompress {
				want = 1
			}
			if got != want {
				t.Errorf("the stored post carries %d note(s), want %d — %s\npost message: %q\nnote text: %q",
					got, want,
					map[bool]string{
						true:  "a compressed file must always say so",
						false: "a file stored as the original must carry no note, whatever the reason",
					}[tc.wantCompress],
					post.Message, noteText(t, post))
			}
		})
	}
}

// TestTheNoteNamesBothSizes checks the numbers themselves. A note that says a
// file was compressed without saying from what to what is decoration.
func TestTheNoteNamesBothSizes(t *testing.T) {
	deadline(t, 60*time.Second)

	np := newNotePlugin(t, nil)
	input := encodeJPEG(t, noisyImage(4000, 3000, 11), 95)
	fileID, stored := np.uploadThroughHook(t, "photo.jpg", input)

	post := np.storedPost(t, postWith(fileID))
	text := noteText(t, post)

	for _, want := range []string{formatFileSize(int64(len(input))), formatFileSize(int64(len(stored)))} {
		if !strings.Contains(text, want) {
			t.Errorf("the note does not mention %q\nnote: %q", want, text)
		}
	}
}

// TestSeveralAttachmentsEachGetTheirOwnNote covers a post carrying more than
// one file. The contract is per FILE, not per post: two compressed files must
// produce two notes, and a pass-through alongside them must still produce
// none.
//
// The two compressed fixtures deliberately have DIFFERENT sizes. Using the
// same file twice would have made the test pass while the code mixed the two
// records up, because both notes would read identically — a hole found by
// reordering the records on purpose and watching nothing fail.
func TestSeveralAttachmentsEachGetTheirOwnNote(t *testing.T) {
	deadline(t, 120*time.Second)

	np := newNotePlugin(t, nil)

	inputA := encodeJPEG(t, noisyImage(4000, 3000, 21), 95)
	inputB := encodeJPEG(t, noisyImage(2800, 2100, 22), 95)

	bigA, storedA := np.uploadThroughHook(t, "a.jpg", inputA)
	bigB, storedB := np.uploadThroughHook(t, "b.jpg", inputB)
	small, _ := np.uploadThroughHook(t, "c.jpg", encodeJPEG(t, noisyImage(300, 200, 23), 90))

	post := np.storedPost(t, postWith(bigA, bigB, small))
	text := noteText(t, post)

	if got := noteCount(t, post); got != 2 {
		t.Errorf("a post with two compressed files and one pass-through carries %d note(s), want 2\nnote: %q",
			got, text)
	}

	// Both files' own numbers must appear, and the two must be distinguishable
	// — otherwise a mix-up between the records would be invisible.
	for name, pair := range map[string][2]int{
		"a.jpg": {len(inputA), len(storedA)},
		"b.jpg": {len(inputB), len(storedB)},
	} {
		original, stored := formatFileSize(int64(pair[0])), formatFileSize(int64(pair[1]))
		if !strings.Contains(text, original) || !strings.Contains(text, stored) {
			t.Errorf("the note does not name %s's sizes %q → %q\nnote: %q",
				name, original, stored, text)
		}
	}
	if formatFileSize(int64(len(storedA))) == formatFileSize(int64(len(storedB))) {
		t.Fatalf("both fixtures compressed to the same displayed size (%s), so this test "+
			"cannot tell a correct pairing from a mixed-up one",
			formatFileSize(int64(len(storedA))))
	}

	// The note lines carry the sizes in the order the post lists its files.
	// That ordering is the only thing linking a line to an attachment, since
	// the note deliberately does not repeat the file name.
	lines := []string{}
	for _, a := range noteAttachmentsOf(t, post) {
		for _, line := range strings.Split(a.Text, "\n") {
			if strings.Contains(line, "→") {
				lines = append(lines, line)
			}
		}
	}
	if len(lines) != 2 {
		t.Fatalf("expected two note lines, got %d: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], formatFileSize(int64(len(storedA)))) {
		t.Errorf("the first note line is %q, but the post's first file compressed to %s "+
			"— the lines must follow the order of post.FileIds, or a reader cannot tell "+
			"which attachment a line belongs to",
			lines[0], formatFileSize(int64(len(storedA))))
	}
	if !strings.Contains(lines[1], formatFileSize(int64(len(storedB)))) {
		t.Errorf("the second note line is %q, but the post's second file compressed to %s",
			lines[1], formatFileSize(int64(len(storedB))))
	}
}

// TestAPostWithoutAttachmentsIsLeftAlone guards the common case. Most posts
// carry no file at all, and the plugin must be invisible for them.
func TestAPostWithoutAttachmentsIsLeftAlone(t *testing.T) {
	deadline(t, 30*time.Second)

	np := newNotePlugin(t, nil)

	original := &model.Post{Id: model.NewId(), Message: "Nur Text."}
	beforeMessage := original.Message

	replacement, rejection := np.MessageWillBePosted(&plugin.Context{}, original)
	if rejection != "" {
		t.Fatalf("a plain text post was rejected: %s", rejection)
	}
	if replacement != nil {
		t.Errorf("a post without attachments was replaced: %+v", replacement)
	}
	if original.Message != beforeMessage {
		t.Errorf("the message was changed from %q to %q", beforeMessage, original.Message)
	}
	if len(original.Props) != 0 {
		t.Errorf("props were added to a post without attachments: %v", original.Props)
	}
}

// TestEditingAPostNeitherDuplicatesNorLosesTheNote covers the case that makes
// a note on someone else's message risky.
//
// An edit re-submits the stored message, note included. Appending again would
// double it; ignoring the edit would drop it. Both are visible to the user and
// both would make the note untrustworthy.
func TestEditingAPostNeitherDuplicatesNorLosesTheNote(t *testing.T) {
	deadline(t, 120*time.Second)

	for _, style := range []string{noteStyleLine, noteStyleCompact, noteStyleBlock, noteStyleFooter} {
		t.Run(style, func(t *testing.T) {
			np := newNotePlugin(t, map[string]any{"SizeNoteStyle": style})

			fileID, _ := np.uploadThroughHook(t, "photo.jpg",
				encodeJPEG(t, noisyImage(4000, 3000, 31), 95))
			posted := np.storedPost(t, postWith(fileID))

			if noteCount(t, posted) != 1 {
				t.Fatalf("the posted message does not carry exactly one note: %q", noteText(t, posted))
			}

			// The client sends back the stored message with the user's edit
			// applied. The note travels with it, because that is what the
			// edit box contains.
			edited := posted.Clone()
			edited.Message = strings.Replace(posted.Message,
				"Schaut euch das mal an!", "Schaut euch das bitte an!", 1)

			replacement, rejection := np.MessageWillBeUpdated(&plugin.Context{}, edited, posted)
			if rejection != "" {
				t.Fatalf("the edit was rejected: %s", rejection)
			}
			stored := edited
			if replacement != nil {
				stored = replacement
			}

			if got := noteCount(t, stored); got != 1 {
				t.Errorf("after the edit the post carries %d note(s), want exactly 1\nnote: %q",
					got, noteText(t, stored))
			}
			if !strings.Contains(stored.Message, "Schaut euch das bitte an!") {
				t.Errorf("the user's edit was lost: %q", stored.Message)
			}
		})
	}

	t.Run("a user who deletes the note gets it back", func(t *testing.T) {
		np := newNotePlugin(t, map[string]any{"SizeNoteStyle": noteStyleLine})

		fileID, _ := np.uploadThroughHook(t, "photo.jpg",
			encodeJPEG(t, noisyImage(4000, 3000, 32), 95))
		posted := np.storedPost(t, postWith(fileID))

		edited := posted.Clone()
		edited.Message = "Schaut euch das bitte an!"

		replacement, _ := np.MessageWillBeUpdated(&plugin.Context{}, edited, posted)
		stored := edited
		if replacement != nil {
			stored = replacement
		}

		if got := noteCount(t, stored); got != 1 {
			t.Errorf("the note vanished from an edited post (%d notes): %q",
				got, noteText(t, stored))
		}
	})
}

// TestAnUploadThatNeverBecomesAPostExpires proves the store between the two
// hooks cannot grow without bound.
//
// Drafts are abandoned, uploads are cancelled, and clients crash. Every one of
// those leaves a recorded saving that no post will ever collect, so the record
// must carry an expiry rather than wait for a reader that never comes.
func TestAnUploadThatNeverBecomesAPostExpires(t *testing.T) {
	deadline(t, 120*time.Second)

	np := newNotePlugin(t, nil)

	for i := 0; i < 3; i++ {
		np.uploadThroughHook(t, fmt.Sprintf("photo-%d.jpg", i),
			encodeJPEG(t, noisyImage(4000, 3000, int64(40+i)), 95))
	}

	// Scoped to the note's own records. The store also holds the
	// administrator's statistics now, which are kept for far longer on
	// purpose, so an unscoped loop would compare this slice's expiry against
	// a key that has nothing to do with it.
	notes := np.kv.keys(noteKeyPrefix)
	if len(notes) == 0 {
		t.Fatal("no saving was recorded at all, so a post could never carry a note")
	}
	for _, key := range notes {
		seconds := np.kv.expiryOf(key)
		if seconds <= 0 {
			t.Errorf("the record %q was written with expiry %d, so it would stay for ever",
				key, seconds)
		}
		if seconds > int64(maxNoteRetention/time.Second) {
			t.Errorf("the record %q expires after %d s, which is longer than the %s this slice allows",
				key, seconds, maxNoteRetention)
		}
	}
}

// TestACollectedSavingIsCleanedUpByItsExpiry checks the other half of the
// store's lifetime.
//
// An earlier version deleted the record as soon as a post collected it, which
// looks tidier and is wrong: MessageWillBePosted runs before the post is
// committed, so a record consumed there is lost if the post never gets stored
// — see TestAPostThatNeverGetsStoredDoesNotLoseItsNote. The expiry does the
// cleaning instead, and this test pins that the record therefore survives the
// post AND still carries a bounded lifetime.
func TestACollectedSavingIsCleanedUpByItsExpiry(t *testing.T) {
	deadline(t, 60*time.Second)

	np := newNotePlugin(t, nil)
	fileID, _ := np.uploadThroughHook(t, "photo.jpg", encodeJPEG(t, noisyImage(4000, 3000, 51), 95))

	// Counted by prefix rather than over the whole store: the administrator's
	// statistics live there too now, and they are not note records.
	if got := len(np.kv.keys(noteKeyPrefix)); got != 1 {
		t.Fatalf("the upload recorded %d savings, want 1", got)
	}

	np.storedPost(t, postWith(fileID))

	if got := len(np.kv.keys(noteKeyPrefix)); got != 1 {
		t.Errorf("the saving was consumed by the post (%d left); a post that is never "+
			"stored would then lose its note for good", got)
	}

	for _, key := range np.kv.keys(noteKeyPrefix) {
		seconds := np.kv.expiryOf(key)
		if seconds <= 0 || seconds > int64(maxNoteRetention/time.Second) {
			t.Errorf("the surviving record %q has the expiry %d s, which is not inside "+
				"the %s this slice allows — without it nothing would ever remove it",
				key, seconds, maxNoteRetention)
		}
	}
}

// TestConcurrentUploadsNeverBorrowAnotherFilesNumbers is the one that matters
// for the store's correctness.
//
// The two hooks are far apart in time and the store between them is written by
// every upload on the server at once. Labelling a post with another file's
// numbers would be worse than no note at all: it would be a confident,
// specific, wrong statement about someone's file.
func TestConcurrentUploadsNeverBorrowAnotherFilesNumbers(t *testing.T) {
	deadline(t, 180*time.Second)

	np := newNotePlugin(t, nil)

	const uploads = 8
	type upload struct {
		id       string
		original int
		stored   int
	}
	results := make([]upload, uploads)

	var wg sync.WaitGroup
	for i := 0; i < uploads; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each fixture has its own geometry, so its compressed size is
			// its own and a mix-up cannot pass unnoticed.
			input := encodeJPEG(t, noisyImage(3000+i*120, 2200+i*90, int64(60+i)), 95)
			id, stored := np.uploadThroughHook(t, fmt.Sprintf("photo-%d.jpg", i), input)
			results[i] = upload{id: id, original: len(input), stored: len(stored)}
		}(i)
	}
	wg.Wait()

	compressedCount := 0
	for i, up := range results {
		post := np.storedPost(t, postWith(up.id))
		text := noteText(t, post)

		// Not every upload is necessarily compressed: eight at once against
		// the default limit of four means some legitimately find the queue
		// full and are stored as sent — correct behaviour, and more likely
		// under -race, which slows the pipeline down several times over.
		// Asserting "all eight carry a note" would therefore fail for a
		// reason that has nothing to do with the property under test.
		//
		// The property under test is that no post is EVER labelled with
		// another file's numbers, and it is checked in both directions: a
		// compressed file names its own two sizes, and a file stored as sent
		// names nothing at all.
		if up.stored < up.original {
			wantOriginal := formatFileSize(int64(up.original))
			wantStored := formatFileSize(int64(up.stored))
			if !strings.Contains(text, wantOriginal) || !strings.Contains(text, wantStored) {
				t.Errorf("upload %d (%d → %d bytes) got the note %q, which does not name %q → %q",
					i, up.original, up.stored, text, wantOriginal, wantStored)
			}
			compressedCount++
			continue
		}

		if got := noteCount(t, post); got != 0 {
			t.Errorf("upload %d was stored as sent (%d bytes) but its post carries %d note(s): %q",
				i, up.original, got, text)
		}

		// And the decisive one: a file stored as sent must not have picked up
		// ANY other upload's numbers either.
		for j, other := range results {
			if j == i || other.stored >= other.original {
				continue
			}
			if strings.Contains(text, formatFileSize(int64(other.stored))) {
				t.Errorf("upload %d carries upload %d's stored size %q: %q",
					i, j, formatFileSize(int64(other.stored)), text)
			}
		}
	}

	if compressedCount == 0 {
		t.Fatal("not one of the uploads was compressed, so the test proved nothing " +
			"about which numbers end up on which post")
	}
	t.Logf("%d of %d concurrent uploads were compressed; each post carried its own "+
		"numbers and no other's", compressedCount, uploads)
}

// ------------------------------------------------------------- the setting

// TestTheNoteStyleSettingFollowsTheSliceThreeRules holds the new setting to
// the rules every other setting already obeys: the default is in force
// untouched, a value outside the offered options falls back to the default and
// is named in the log, and a change takes effect without a restart.
func TestTheNoteStyleSettingFollowsTheSliceThreeRules(t *testing.T) {
	deadline(t, 30*time.Second)

	t.Run("the default is the small footer", func(t *testing.T) {
		if got := defaultConfiguration().SizeNoteStyle; got != noteStyleFooter {
			t.Errorf("the default note style is %q, want %q", got, noteStyleFooter)
		}
	})

	t.Run("every offered value is accepted", func(t *testing.T) {
		for _, style := range []string{noteStyleLine, noteStyleCompact, noteStyleBlock, noteStyleFooter, noteStyleNone} {
			cfg, warnings := sanitiseConfiguration(map[string]any{"SizeNoteStyle": style})
			if cfg.SizeNoteStyle != style {
				t.Errorf("the value %q was not accepted, the plugin uses %q", style, cfg.SizeNoteStyle)
			}
			if len(warnings) != 0 {
				t.Errorf("accepting %q produced warnings: %v", style, warnings)
			}
		}
	})

	t.Run("an unknown value falls back and is named", func(t *testing.T) {
		for _, bad := range []any{"tiny", "", 4, true, nil} {
			cfg, warnings := sanitiseConfiguration(map[string]any{"SizeNoteStyle": bad})
			if cfg.SizeNoteStyle != noteStyleFooter {
				t.Errorf("the value %#v produced the style %q, want the default %q",
					bad, cfg.SizeNoteStyle, noteStyleFooter)
			}
			if bad == nil {
				// A cleared field is indistinguishable from "never set" and
				// is not a mistake worth warning about.
				continue
			}
			if len(warnings) != 1 || warnings[0].Setting != "SizeNoteStyle" {
				t.Errorf("the rejected value %#v produced %v, want one warning naming SizeNoteStyle",
					bad, warnings)
			}
		}
	})

	t.Run("a change takes effect without a restart", func(t *testing.T) {
		np := newNotePlugin(t, map[string]any{"SizeNoteStyle": noteStyleFooter})

		fileID, _ := np.uploadThroughHook(t, "photo.jpg",
			encodeJPEG(t, noisyImage(4000, 3000, 71), 95))
		first := np.storedPost(t, postWith(fileID))
		if len(noteAttachmentsOf(t, first)) == 0 {
			t.Fatalf("the footer style produced no attachment: %q", noteText(t, first))
		}

		np.reconfigure(t, map[string]any{"SizeNoteStyle": noteStyleNone})

		otherID, _ := np.uploadThroughHook(t, "photo2.jpg",
			encodeJPEG(t, noisyImage(4000, 3000, 72), 95))
		second := np.storedPost(t, postWith(otherID))
		if got := noteCount(t, second); got != 0 {
			t.Errorf("after switching the note off, a post still carries %d note(s): %q",
				got, noteText(t, second))
		}
	})
}

// TestSwitchingTheNoteOffLeavesThePostExactlyAsSent is the fifth option's
// whole purpose. It replaces a separate on/off switch, so it has to be a true
// off: no note, no attachment, no prop.
func TestSwitchingTheNoteOffLeavesThePostExactlyAsSent(t *testing.T) {
	deadline(t, 60*time.Second)

	np := newNotePlugin(t, map[string]any{"SizeNoteStyle": noteStyleNone})

	fileID, _ := np.uploadThroughHook(t, "photo.jpg", encodeJPEG(t, noisyImage(4000, 3000, 81), 95))
	sent := postWith(fileID)
	before := sent.Message

	stored := np.storedPost(t, sent)

	if stored.Message != before {
		t.Errorf("the message was changed with the note switched off: %q → %q", before, stored.Message)
	}
	if len(noteAttachmentsOf(t, stored)) != 0 {
		t.Errorf("an attachment was added with the note switched off: %v", stored.Props)
	}
}

// TestEachStyleRendersTheApprovedShape pins the four visible variants against
// the design approved on the running instance, including which of them write
// into the user's own message and which do not.
func TestEachStyleRendersTheApprovedShape(t *testing.T) {
	deadline(t, 120*time.Second)

	cases := []struct {
		style           string
		writesToMessage bool
		wantInNote      []string
	}{
		{noteStyleLine, true, []string{"*MediaForge: ", " saved)*"}},
		{noteStyleCompact, true, []string{"`"}},
		{noteStyleBlock, false, []string{"📦", "saved by MediaForge"}},
		{noteStyleFooter, false, []string{"MediaForge · ", "smaller)"}},
	}

	for _, tc := range cases {
		t.Run(tc.style, func(t *testing.T) {
			np := newNotePlugin(t, map[string]any{"SizeNoteStyle": tc.style})

			fileID, _ := np.uploadThroughHook(t, "photo.jpg",
				encodeJPEG(t, noisyImage(4000, 3000, 91), 95))
			sent := postWith(fileID)
			original := sent.Message
			stored := np.storedPost(t, sent)

			text := noteText(t, stored)
			for _, want := range tc.wantInNote {
				if !strings.Contains(text, want) {
					t.Errorf("the %s note does not contain %q: %q", tc.style, want, text)
				}
			}

			changed := stored.Message != original
			if changed != tc.writesToMessage {
				t.Errorf("the %s style %s the user's message, want %s\nmessage: %q",
					tc.style,
					map[bool]string{true: "changed", false: "left"}[changed],
					map[bool]string{true: "changed", false: "left alone"}[tc.writesToMessage],
					stored.Message)
			}
			if tc.writesToMessage && !strings.HasPrefix(stored.Message, original) {
				t.Errorf("the %s style did not keep the user's own words first: %q",
					tc.style, stored.Message)
			}
		})
	}
}

// TestFileSizesAreWrittenTheWayTheApprovedDesignShowsThem pins the number
// format. The previews read "11.9 MB → 2.3 MB", which is decimal units and an
// English decimal point — not the binary units a developer would reach for by
// habit, and, since the documentation pass unified every user-visible text on English, not
// the German decimal comma the first draft of this note used.
func TestFileSizesAreWrittenTheWayTheApprovedDesignShowsThem(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{999, "999 B"},
		{1000, "1 KB"},
		{340_000, "340 KB"},
		{999_499, "999 KB"},
		{1_000_000, "1.0 MB"},
		{2_323_820, "2.3 MB"},
		{11_906_359, "11.9 MB"},
		{999_949_999, "999.9 MB"},
		{1_000_000_000, "1.0 GB"},
		{39_558_822, "39.6 MB"},
	}

	for _, tc := range cases {
		if got := formatFileSize(tc.bytes); got != tc.want {
			t.Errorf("formatFileSize(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}

// TestNoSizeIsWrittenWithGermanNumberPunctuation is the narrow guard for the
// one thing a careless edit brings back.
//
// A decimal comma and a thousands dot are the German spelling of a number, and
// they were what the first draft of the note used. Nothing else in the plugin
// would notice their return: the note would still render, every contract test
// would still pass, and the System Console would still look English. This
// checks the punctuation itself over the whole range the note can produce.
func TestNoSizeIsWrittenWithGermanNumberPunctuation(t *testing.T) {
	for _, bytes := range []int64{
		0, 1, 999, 1000, 1500, 340_000, 999_499, 999_999,
		1_000_000, 2_323_820, 11_906_359, 999_949_999,
		1_000_000_000, 1_500_000_000, 1_000_000_000_000,
	} {
		got := formatFileSize(bytes)
		if strings.Contains(got, ",") {
			t.Errorf("formatFileSize(%d) = %q — a comma is German number punctuation; "+
				"English writes the decimal separator as a point", bytes, got)
		}
	}
}

// TestTheSavedPercentageIsRounded checks the other number in the note.
func TestTheSavedPercentageIsRounded(t *testing.T) {
	cases := []struct {
		original, stored int64
		want             int
	}{
		{11_906_359, 2_323_820, 80},
		{1000, 500, 50},
		{1000, 999, 0},
		{1000, 1000, 0},
		{0, 0, 0},
	}
	for _, tc := range cases {
		if got := savedPercent(tc.original, tc.stored); got != tc.want {
			t.Errorf("savedPercent(%d, %d) = %d, want %d", tc.original, tc.stored, got, tc.want)
		}
	}
}

// TestTheManifestOffersExactlyTheFiveApprovedOptions checks the System Console
// side of the setting.
//
// An option the code does not understand would always fall back to the
// default, so the console would show a choice that quietly does nothing — and
// a missing option would make a code path unreachable. Both are invisible
// without this test.
func TestTheManifestOffersExactlyTheFiveApprovedOptions(t *testing.T) {
	raw, err := os.ReadFile("../plugin.json")
	if err != nil {
		t.Fatalf("reading the manifest failed: %v", err)
	}
	var manifest model.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("the manifest is not valid JSON: %v", err)
	}
	if manifest.SettingsSchema == nil {
		t.Fatal("the manifest declares no settings schema")
	}

	var setting *model.PluginSetting
	for _, s := range allManifestSettings(manifest.SettingsSchema) {
		if s.Key == "SizeNoteStyle" {
			setting = s
		}
	}
	if setting == nil {
		t.Fatal("the manifest has no SizeNoteStyle setting, so the note cannot be configured at all")
	}

	want := []string{noteStyleLine, noteStyleCompact, noteStyleBlock, noteStyleFooter, noteStyleNone}
	got := make([]string, 0, len(setting.Options))
	for _, o := range setting.Options {
		got = append(got, o.Value)
		if strings.TrimSpace(o.DisplayName) == "" {
			t.Errorf("the option %q has no display name", o.Value)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("the setting offers %d options %v, want the five approved ones %v", len(got), got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("option %d is %q, want %q — the numbers in the option texts and in the "+
				"preview images are what pair them, so the order is part of the design",
				i+1, got[i], w)
		}
	}

	// Each of the five preview images must be both referenced and served.
	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("note-%d.png", i)
		if !strings.Contains(setting.HelpText, name) {
			t.Errorf("the help text does not show %s, so option %d has no preview", name, i)
		}
		if _, err := noteImages.ReadFile("public/" + name); err != nil {
			t.Errorf("%s is referenced but not shipped in the binary: %v", name, err)
		}
	}
}

// TestTheNoteSurvivesAServerThatCannotStoreIt makes the failure mode explicit.
//
// If the key-value store is unavailable the saving cannot be recorded, and the
// post will carry no note. That is a silent hole in the SPEC §4.4 contract, so
// it has to be visible in the server log rather than merely absent.
func TestTheNoteSurvivesAServerThatCannotStoreIt(t *testing.T) {
	deadline(t, 60*time.Second)

	tp := newTestPluginWithSettings(t, nil)
	// The whole store is unavailable, not just one method of it. Stubbing
	// only the note's own write would leave the statistics reaching a store
	// that answers, which is not the situation this test is about — and the
	// plugin must survive both.
	tp.api.On("KVSetWithExpiry", mock.Anything, mock.Anything, mock.Anything).
		Return(&model.AppError{Message: "store unavailable"})
	tp.api.On("KVGet", mock.Anything).
		Return([]byte(nil), &model.AppError{Message: "store unavailable"}).Maybe()
	tp.api.On("KVSetWithOptions", mock.Anything, mock.Anything, mock.Anything).
		Return(false, &model.AppError{Message: "store unavailable"}).Maybe()
	tp.api.On("KVDelete", mock.Anything).
		Return(&model.AppError{Message: "store unavailable"}).Maybe()

	// Registered BEFORE the catch-all, because testify serves the first
	// matching expectation and the catch-all would otherwise swallow the call
	// this check depends on.
	logged := false
	tp.api.On("LogWarn", mock.Anything, mock.Anything, mock.Anything, mock.Anything,
		mock.Anything, mock.Anything, mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		if strings.Contains(fmt.Sprint(args.Get(0)), "note") {
			logged = true
		}
	}).Return().Maybe()
	allowAnyLogging(tp.api)

	np := &notePlugin{testPlugin: tp}
	input := encodeJPEG(t, noisyImage(4000, 3000, 101), 95)

	// The upload itself must still succeed. A note is a convenience; storing
	// the user's file is not.
	_, stored := np.uploadThroughHook(t, "photo.jpg", input)
	if len(stored) >= len(input) {
		t.Fatalf("the upload was not compressed: %d → %d bytes", len(input), len(stored))
	}
	if !logged {
		t.Error("a saving that could not be recorded was not reported in the server log")
	}
}

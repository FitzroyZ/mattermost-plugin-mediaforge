package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/stretchr/testify/mock"
)

// ---------------------------------------------------------------- fixtures

// statsPlugin is a plugin wired the way the server wires one, with the shared
// key-value stand-in from the note tests.
//
// That store already models compare-and-set, which is what the counters rely
// on to survive concurrent uploads: a mock that let every writer overwrite the
// last one would make a lost increment invisible, and the totals would simply
// come out low.
type statsPlugin struct {
	*testPlugin
	kv *kvStore
}

// writeCounter is the output writer the server hands to the hook, and it also
// counts what it received — the number the server uses to overwrite
// FileInfo.Size.
type writeCounter struct {
	buf bytes.Buffer
	n   int64
}

func (w *writeCounter) Write(p []byte) (int, error) {
	written, err := w.buf.Write(p)
	w.n += int64(written)
	return written, err
}

func (w *writeCounter) bytes() []byte { return w.buf.Bytes() }

// bytesReaderOf hides the concrete type so the hook sees a plain reader, the
// way it does in production.
func bytesReaderOf(data []byte) io.Reader {
	return struct{ io.Reader }{bytes.NewReader(data)}
}

// readManifestBytes reads the manifest the System Console renders.
func readManifestBytes() ([]byte, error) {
	return os.ReadFile("../plugin.json")
}

// newStatsPlugin builds a plugin with a compare-and-set capable store.
func newStatsPlugin(t *testing.T, settings map[string]any) *statsPlugin {
	t.Helper()
	tp := newTestPluginWithSettings(t, settings)
	allowAnyLogging(tp.api)
	return &statsPlugin{testPlugin: tp, kv: attachKV(tp.api)}
}

// reopen builds a SECOND plugin on the SAME store, which is what a server
// restart looks like from the plugin's point of view: fresh process, fresh
// memory, same database.
func (sp *statsPlugin) reopen(t *testing.T) *statsPlugin {
	t.Helper()
	tp := newTestPluginWithSettings(t, sp.settings)
	allowAnyLogging(tp.api)
	reattachKV(tp.api, sp.kv)
	return &statsPlugin{testPlugin: tp, kv: sp.kv}
}

// uploadThroughHook runs one file through FileWillBeUploaded exactly as the
// server does, then does what the SERVER does immediately afterwards: it
// overwrites FileInfo.Size with the number of bytes it actually received.
//
// That last step is the point. It is the the first hook version invariant, and it is what
// makes any implementation that computes the saving from fileinfo.Size report
// a flat zero. A helper that skipped it would let exactly that bug pass.
func (sp *statsPlugin) uploadThroughHook(t *testing.T, name string, input []byte) (stored []byte, rejection string) {
	t.Helper()

	info := &model.FileInfo{
		Id:   model.NewId(),
		Name: name,
		Size: int64(len(input)),
		Path: "20260918/teams/t/channels/c/users/u/" + name,
	}

	var out writeCounter
	returned, rejection := sp.FileWillBeUploaded(
		&plugin.Context{}, info, bytesReaderOf(input), &out)
	if returned != nil {
		info = returned
	}
	if rejection == "" && out.n > 0 {
		// The server sets the size from the bytes it received, throwing the
		// original size away. Everything after the hook sees only this.
		info.Size = out.n
	}
	return out.bytes(), rejection
}

// report drives the collection the System Console does.
func (sp *statsPlugin) report(t *testing.T) statsReport {
	t.Helper()
	report, err := sp.MediaForgePlugin.statistics()
	if err != nil {
		t.Fatalf("collecting the statistics failed: %v", err)
	}
	return report
}

// ------------------------------------------------------- the control set

// TestTheReportMatchesAHandCountedControlSet is the slice's Done criterion.
//
// A known series of files goes through the hook, the expected totals are
// computed independently from what was actually stored, and the report has to
// agree exactly. Anything less than exact agreement makes the figures useless:
// an administrator decides on their basis, and a number that is roughly right
// is worse than no number at all.
func TestTheReportMatchesAHandCountedControlSet(t *testing.T) {
	tools := ffmpegOrSkip(t)
	sp := newStatsPlugin(t, map[string]any{})

	// Two images that genuinely compress, one that is below the threshold and
	// is stored as sent, one video that transcodes, and one corrupt video
	// that is rejected. Between them they cover every counter the report has.
	bigA := encodeJPEG(t, noisyImage(3000, 2000, 71), 95)
	bigB := encodeJPEG(t, noisyImage(2400, 1800, 72), 95)
	small := encodeJPEG(t, noisyImage(200, 150, 73), 90)
	video := makeVideo(t, tools, 2, "640x480")
	broken := corruptVideo(video)

	type expectation struct {
		name     string
		input    []byte
		kind     string
		rejected bool
	}
	series := []expectation{
		{"a.jpg", bigA, statsKindImage, false},
		{"b.jpg", bigB, statsKindImage, false},
		{"small.jpg", small, "", false},
		{"clip.mp4", video, statsKindVideo, false},
		{"broken.mp4", broken, statsKindVideo, true},
	}

	// The control set, computed here and nowhere else.
	want := map[string]struct{ files, original, stored, failures int64 }{}
	for _, item := range series {
		stored, rejection := sp.uploadThroughHook(t, item.name, item.input)
		entry := want[item.kind]

		switch {
		case item.rejected:
			if rejection == "" {
				t.Fatalf("%s was accepted, but this control set needs it rejected", item.name)
			}
			entry.failures++
		case int64(len(stored)) < int64(len(item.input)):
			entry.files++
			entry.original += int64(len(item.input))
			entry.stored += int64(len(stored))
		default:
			// Stored as sent. It contributes to nothing, and that is the
			// property the third row is here to pin.
			if item.kind != "" {
				t.Fatalf("%s was expected to compress but was stored as sent", item.name)
			}
			continue
		}
		want[item.kind] = entry
	}

	got := sp.report(t)

	var wantFiles, wantOriginal, wantStored, wantFailures int64
	for kind, w := range want {
		if kind == "" {
			continue
		}
		wantFiles += w.files
		wantOriginal += w.original
		wantStored += w.stored
		wantFailures += w.failures

		byKind := got.byKind(kind)
		if byKind.Files != w.files {
			t.Errorf("%s: the report counts %d processed file(s), the control set says %d",
				kind, byKind.Files, w.files)
		}
		if byKind.BytesSaved != w.original-w.stored {
			t.Errorf("%s: the report claims %d byte(s) saved, the control set says %d",
				kind, byKind.BytesSaved, w.original-w.stored)
		}
		if byKind.Failures != w.failures {
			t.Errorf("%s: the report counts %d failure(s), the control set says %d",
				kind, byKind.Failures, w.failures)
		}
	}

	if got.Total.Files != wantFiles {
		t.Errorf("the report counts %d processed file(s) in total, the control set says %d",
			got.Total.Files, wantFiles)
	}
	if got.Total.BytesSaved != wantOriginal-wantStored {
		t.Errorf("the report claims %d byte(s) saved in total, the control set says %d "+
			"(%d original minus %d stored)",
			got.Total.BytesSaved, wantOriginal-wantStored, wantOriginal, wantStored)
	}
	if got.Total.Failures != wantFailures {
		t.Errorf("the report counts %d failure(s) in total, the control set says %d",
			got.Total.Failures, wantFailures)
	}
	if got.Total.BytesSaved <= 0 {
		t.Fatalf("the report claims %d byte(s) saved, so nothing was measured at all — "+
			"this is what computing the saving from fileinfo.Size looks like",
			got.Total.BytesSaved)
	}
}

// TestASavingIsNeverComputedFromTheSizeTheServerOverwrites is the trap the
// plan names explicitly.
//
// The server replaces FileInfo.Size with the compressed byte count the instant
// the hook returns. An implementation that reads the original size from there
// gets the compressed size, computes original minus original, and reports a
// flat zero everywhere — while every test that merely checked "a number came
// back" would still pass.
func TestASavingIsNeverComputedFromTheSizeTheServerOverwrites(t *testing.T) {
	sp := newStatsPlugin(t, map[string]any{})
	input := encodeJPEG(t, noisyImage(3000, 2000, 74), 95)

	stored, rejection := sp.uploadThroughHook(t, "photo.jpg", input)
	if rejection != "" {
		t.Fatalf("the hook rejected the upload: %s", rejection)
	}
	if len(stored) >= len(input) {
		t.Fatalf("the fixture did not compress, so this test proves nothing")
	}

	got := sp.report(t)
	wantSaved := int64(len(input) - len(stored))
	if got.Total.BytesSaved != wantSaved {
		t.Errorf("the report claims %d byte(s) saved, want %d — the original size must come "+
			"from processOutcome.originalSize, not from fileinfo.Size",
			got.Total.BytesSaved, wantSaved)
	}
	if got.Total.BytesOriginal != int64(len(input)) {
		t.Errorf("the report records %d original byte(s), want %d",
			got.Total.BytesOriginal, len(input))
	}
}

// TestTheReportAndTheNoteAgreeOnEveryFile pins the two figures to one
// measurement.
//
// the size note already records the original and stored size per file. Measuring a
// second time for the report is what makes two numbers drift apart until
// nobody can say which is right, so the same source has to feed both.
func TestTheReportAndTheNoteAgreeOnEveryFile(t *testing.T) {
	sp := newStatsPlugin(t, map[string]any{})

	inputs := [][]byte{
		encodeJPEG(t, noisyImage(3000, 2000, 75), 95),
		encodeJPEG(t, noisyImage(2000, 1600, 76), 95),
	}

	var noteOriginal, noteStored int64
	for i, input := range inputs {
		name := fmt.Sprintf("photo-%d.jpg", i)
		if _, rejection := sp.uploadThroughHook(t, name, input); rejection != "" {
			t.Fatalf("the hook rejected %s: %s", name, rejection)
		}
	}

	// Read every note record the upload wrote and add it up independently.
	for _, key := range sp.kv.keys(noteKeyPrefix) {
		raw, _ := sp.kv.get(key)
		var record noteRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatalf("a note record is not readable: %v", err)
		}
		noteOriginal += record.Original
		noteStored += record.Stored
	}
	if noteOriginal == 0 {
		t.Fatal("no note records were written, so there is nothing to compare against")
	}

	got := sp.report(t)
	if got.Total.BytesOriginal != noteOriginal || got.Total.BytesStored != noteStored {
		t.Errorf("the report says %d original / %d stored, the notes say %d / %d — "+
			"the two figures must come from the same measurement",
			got.Total.BytesOriginal, got.Total.BytesStored, noteOriginal, noteStored)
	}
}

// ------------------------------------------------------------ reliability

// TestTheFiguresSurviveAServerRestart.
//
// A counter kept in memory reads as zero after every restart, and an
// administrator comparing this week with last week would see a saving
// collapse that never happened.
func TestTheFiguresSurviveAServerRestart(t *testing.T) {
	sp := newStatsPlugin(t, map[string]any{})
	input := encodeJPEG(t, noisyImage(3000, 2000, 77), 95)
	if _, rejection := sp.uploadThroughHook(t, "photo.jpg", input); rejection != "" {
		t.Fatalf("the hook rejected the upload: %s", rejection)
	}

	before := sp.report(t)
	if before.Total.Files != 1 || before.Total.BytesSaved <= 0 {
		t.Fatalf("nothing was counted before the restart: %+v", before.Total)
	}

	after := sp.reopen(t).report(t)
	if after.Total != before.Total {
		t.Errorf("after a restart the report says %+v, before it said %+v — "+
			"the figures must live in the key-value store, not in memory",
			after.Total, before.Total)
	}
}

// TestTheCountersAreSafeAgainstALostUpdate is the test that actually pins the
// atomic update, and it exists because the upload-level test above does NOT.
//
// That was measured rather than assumed: replacing the compare-and-set with a
// plain read-modify-write left the twelve-upload test passing, and the reason
// is that it never collided once — compressing an image takes milliseconds
// while the two store calls take microseconds, so the windows simply never
// overlap. A test that cannot fail when the mechanism is removed is not
// evidence of it.
//
// This one records straight into the figures from many goroutines at once, the
// way a busy instance does across several server processes, and it asserts
// that contention really happened as well as that the sum came out right.
func TestTheCountersAreSafeAgainstALostUpdate(t *testing.T) {
	sp := newStatsPlugin(t, map[string]any{})

	// Several server processes, each with its own shard, each running its own
	// uploads in parallel. That is the shape the real thing has: the figures
	// are split per writer precisely so that not everyone competes for one
	// value, and a test that drove every goroutine through a single plugin
	// would be measuring an arrangement the plugin does not use.
	//
	// The workers-per-process figure is DERIVED from the limits the plugin
	// enforces rather than chosen, and that is the resolution of the flake
	// PLAN §the security audit carried into this slice. It used to be a flat 8, which is
	// more than SPEC §4.4 permits one process to have in flight (4 images plus
	// 1 video), so the test was about twice as hard as the product allows —
	// and at that level the retry budget occasionally ran out under the race
	// detector, dropping 8 to 13 of 512 contributions.
	//
	// The budget is deliberately NOT raised. Raising it would have silenced the
	// finding rather than answered it, which PLAN §the security audit rules out
	// explicitly. The measurement that settles it is in
	// shard_contention_test.go: with eight processes the busiest shard really
	// does hold two or three of them, so several processes' writers DO meet in
	// one shard — but at the permitted per-process concurrency the existing
	// sixteen attempts absorb it, which
	// TestTheRetryBudgetSurvivesTheWorstRealisticContention pins by forcing
	// three processes into one shard on purpose.
	const processes = 8
	const workers = defaultMaxConcurrentImageJobs + defaultMaxConcurrentVideoJobs
	const each = 13

	plugins := make([]*statsPlugin, processes)
	plugins[0] = sp
	for i := 1; i < processes; i++ {
		// A second plugin on the SAME store is what another server process
		// looks like: fresh memory, shared database.
		plugins[i] = sp.reopen(t)
	}

	var wg sync.WaitGroup
	for _, instance := range plugins {
		for range workers {
			wg.Add(1)
			go func(p *statsPlugin) {
				defer wg.Done()
				for range each {
					if err := p.MediaForgePlugin.recordStats(statsKindImage, statsEvent{
						files: 1, original: 1000, stored: 400,
					}); err != nil {
						t.Errorf("recording failed: %v", err)
						return
					}
				}
			}(instance)
		}
	}
	wg.Wait()

	const want = processes * workers * each
	got := sp.report(t)
	if got.Total.Files != want {
		t.Errorf("the report counts %d file(s) after %d increments — %d were lost",
			got.Total.Files, want, want-got.Total.Files)
	}
	if got.Total.BytesSaved != want*600 {
		t.Errorf("the report claims %d byte(s) saved, want %d",
			got.Total.BytesSaved, want*600)
	}
	if sp.kv.conflicts == 0 {
		t.Error("not a single write collided, so this test would pass without any " +
			"atomic update at all and proves nothing")
	}
	// Contention must be survivable, not merely detectable. An exhausted retry
	// loop drops a file's contribution silently and the totals come out low —
	// the exact failure this mechanism exists to prevent.
	if t.Failed() {
		t.Logf("the store saw %d conflict(s) across %d write(s)",
			sp.kv.conflicts, sp.kv.writes)
	}
}

// TestConcurrentUploadsNeverSwallowACount.
//
// The counters are incremented from every upload at once. This is the
// end-to-end version — it goes through the real hook, so it also pins that a
// file stored as sent is excluded from the sum — while
// TestTheCountersAreSafeAgainstALostUpdate is what actually pins the atomic
// update, because this one does not collide.
func TestConcurrentUploadsNeverSwallowACount(t *testing.T) {
	sp := newStatsPlugin(t, map[string]any{})

	const uploads = 12
	inputs := make([][]byte, uploads)
	for i := range inputs {
		inputs[i] = encodeJPEG(t, noisyImage(1600, 1200, int64(200+i)), 95)
	}

	var wg sync.WaitGroup
	stored := make([]int, uploads)
	skipped := make([]bool, uploads)
	for i := range uploads {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, rejection := sp.uploadThroughHook(t, fmt.Sprintf("p-%d.jpg", i), inputs[i])
			if rejection != "" {
				t.Errorf("upload %d was rejected: %s", i, rejection)
				return
			}
			stored[i] = len(out)
			// the concurrency limit may legitimately store a file as sent when every slot
			// is busy. Such a file is not a processed file and must not be
			// counted as one, so it is excluded from the control sum too.
			skipped[i] = len(out) >= len(inputs[i])
		}(i)
	}
	wg.Wait()

	var wantFiles, wantSaved int64
	for i := range uploads {
		if skipped[i] {
			continue
		}
		wantFiles++
		wantSaved += int64(len(inputs[i]) - stored[i])
	}
	if wantFiles == 0 {
		t.Fatal("every upload was stored as sent, so this test proves nothing")
	}

	got := sp.report(t)
	if got.Total.Files != wantFiles {
		t.Errorf("the report counts %d processed file(s), %d were actually compressed — "+
			"an increment was lost", got.Total.Files, wantFiles)
	}
	if got.Total.BytesSaved != wantSaved {
		t.Errorf("the report claims %d byte(s) saved, the uploads actually saved %d",
			got.Total.BytesSaved, wantSaved)
	}
}

// TestTheStoreDoesNotGrowWithoutBound.
//
// The figures are kept per day so the report can show a trend. Per day means
// one new key every day for as long as the plugin is installed unless
// something bounds it, and a store that grows for ever is a defect that only
// shows up on the instance that has been running longest.
func TestTheStoreDoesNotGrowWithoutBound(t *testing.T) {
	sp := newStatsPlugin(t, map[string]any{})

	// Write one day's figures for far more days than are kept.
	day := time.Now().AddDate(0, 0, -3*statsRetentionDays)
	for i := 0; i < 3*statsRetentionDays; i++ {
		if err := sp.MediaForgePlugin.recordStatsOn(day, statsKindImage, statsEvent{
			files: 1, original: 1000, stored: 400,
		}); err != nil {
			t.Fatalf("recording a day failed: %v", err)
		}
		day = day.AddDate(0, 0, 1)
	}

	keys := sp.kv.keys(statsKeyPrefix)
	if len(keys) != 3*statsRetentionDays {
		t.Fatalf("only %d of %d days were written, so this test proves nothing",
			len(keys), 3*statsRetentionDays)
	}

	// Every key must carry an expiry, and it must be measured from the day the
	// figures belong to rather than from the write — otherwise a day that
	// keeps being written to outlives the window the report claims to show.
	for _, key := range keys {
		seconds := sp.kv.expiryOf(key)
		if seconds <= 0 {
			t.Errorf("the figures under %q are stored without an expiry, so they stay for ever", key)
			continue
		}
		if seconds > int64((statsRetentionDays+1)*24*time.Hour/time.Second) {
			t.Errorf("the figures under %q are kept for %d second(s), which is longer than "+
				"the %d-day window", key, seconds, statsRetentionDays)
		}
	}

	// The real store removes an expired key by itself; the stand-in has to be
	// asked, or this test would only count what was written and never what
	// survives. The clock is nudged two seconds forward because a day that is
	// already past the window gets the shortest expiry the API accepts.
	sp.kv.expire(time.Now().Add(2 * time.Second))
	if kept := sp.kv.keys(statsKeyPrefix); len(kept) > statsRetentionDays+1 {
		t.Errorf("after the expiries ran, the store still holds %d day(s) of figures, "+
			"and at most %d may be kept", len(kept), statsRetentionDays)
	}

	report := sp.report(t)
	if len(report.Days) > statsRetentionDays {
		t.Errorf("the report shows %d day(s), and at most %d are kept",
			len(report.Days), statsRetentionDays)
	}
}

// TestTheDailySeriesAddsUpToTheTotals.
//
// The trend and the totals are read by the same person on the same page. If
// the columns do not add up to the total, one of the two is wrong and there is
// no way to tell which.
func TestTheDailySeriesAddsUpToTheTotals(t *testing.T) {
	sp := newStatsPlugin(t, map[string]any{})

	now := time.Now()
	for i, offset := range []int{0, 1, 5} {
		if err := sp.MediaForgePlugin.recordStatsOn(now.AddDate(0, 0, -offset),
			statsKindImage, statsEvent{files: 1, original: int64(1000 * (i + 1)), stored: 100},
		); err != nil {
			t.Fatalf("recording a day failed: %v", err)
		}
		if err := sp.MediaForgePlugin.recordStatsOn(now.AddDate(0, 0, -offset),
			statsKindVideo, statsEvent{failures: 1},
		); err != nil {
			t.Fatalf("recording a day failed: %v", err)
		}
	}

	report := sp.report(t)
	var files, saved, failures int64
	for _, day := range report.Days {
		files += day.Total.Files
		saved += day.Total.BytesSaved
		failures += day.Total.Failures
	}
	if files != report.Total.Files || saved != report.Total.BytesSaved ||
		failures != report.Total.Failures {
		t.Errorf("the daily rows add up to %d file(s) / %d byte(s) / %d failure(s), "+
			"but the totals say %d / %d / %d",
			files, saved, failures,
			report.Total.Files, report.Total.BytesSaved, report.Total.Failures)
	}
	if len(report.Days) < 2 {
		t.Errorf("the report shows %d day(s); a trend needs one row per day with figures",
			len(report.Days))
	}
}

// TestImagesAndVideosAreCountedApart.
//
// "Split by type" is half the slice. A single pot would hide the case an
// administrator actually wants to see: video transcoding is where the time and
// the failures are, images are where the volume is.
func TestImagesAndVideosAreCountedApart(t *testing.T) {
	tools := ffmpegOrSkip(t)
	sp := newStatsPlugin(t, map[string]any{})

	image := encodeJPEG(t, noisyImage(3000, 2000, 78), 95)
	if _, rejection := sp.uploadThroughHook(t, "photo.jpg", image); rejection != "" {
		t.Fatalf("the image was rejected: %s", rejection)
	}
	video := makeVideo(t, tools, 2, "640x480")
	if _, rejection := sp.uploadThroughHook(t, "clip.mp4", video); rejection != "" {
		t.Fatalf("the video was rejected: %s", rejection)
	}

	report := sp.report(t)
	images := report.byKind(statsKindImage)
	videos := report.byKind(statsKindVideo)

	if images.Files != 1 {
		t.Errorf("the report counts %d image(s), want 1", images.Files)
	}
	if videos.Files != 1 {
		t.Errorf("the report counts %d video(s), want 1", videos.Files)
	}
	if images.BytesSaved <= 0 || videos.BytesSaved <= 0 {
		t.Errorf("images saved %d byte(s) and videos %d; both must be counted",
			images.BytesSaved, videos.BytesSaved)
	}
	if images.BytesSaved == videos.BytesSaved {
		t.Errorf("both types report exactly %d byte(s) saved, which means they share one pot",
			images.BytesSaved)
	}
}

// TestARejectedVideoIsCountedAsAFailure.
//
// A failure an administrator cannot see is the one that costs them a support
// request. SPEC §4.2 makes the rejection deliberate, so it has to be visible
// as one.
func TestARejectedVideoIsCountedAsAFailure(t *testing.T) {
	tools := ffmpegOrSkip(t)
	sp := newStatsPlugin(t, map[string]any{})

	broken := corruptVideo(makeVideo(t, tools, 2, "640x480"))
	_, rejection := sp.uploadThroughHook(t, "broken.mp4", broken)
	if rejection == "" {
		t.Fatal("the corrupt video was accepted, so there is no failure to count")
	}

	report := sp.report(t)
	if report.byKind(statsKindVideo).Failures != 1 {
		t.Errorf("the report counts %d video failure(s), want 1",
			report.byKind(statsKindVideo).Failures)
	}
	if report.Total.Failures != 1 {
		t.Errorf("the report counts %d failure(s) in total, want 1", report.Total.Failures)
	}
	if report.Total.Files != 0 {
		t.Errorf("a rejected upload was counted as %d processed file(s), want 0",
			report.Total.Files)
	}
}

// TestAFileStoredAsSentIsNotCountedAsProcessed.
//
// This is the same distinction the note contract rests on, and the report must
// not blur it: a file the plugin passed through saved nothing, so counting it
// would inflate the number of processed files while the saving stayed put.
func TestAFileStoredAsSentIsNotCountedAsProcessed(t *testing.T) {
	sp := newStatsPlugin(t, map[string]any{})

	small := encodeJPEG(t, noisyImage(200, 150, 79), 90)
	stored, rejection := sp.uploadThroughHook(t, "small.jpg", small)
	if rejection != "" {
		t.Fatalf("the hook rejected the small file: %s", rejection)
	}
	if len(stored) != len(small) {
		t.Fatalf("the small file was changed, so it is not a pass-through")
	}

	report := sp.report(t)
	if report.Total.Files != 0 || report.Total.BytesSaved != 0 || report.Total.Failures != 0 {
		t.Errorf("a pass-through produced %+v, want all zeroes", report.Total)
	}
}

// TestNothingIsRecordedForAnInstanceThatNeverProcessedAnything.
//
// The zero state has to read as zero rather than as a broken page, and it must
// not invent a day row for a day on which nothing happened.
func TestNothingIsRecordedForAnInstanceThatNeverProcessedAnything(t *testing.T) {
	sp := newStatsPlugin(t, map[string]any{})

	report := sp.report(t)
	if report.Total.Files != 0 || report.Total.BytesSaved != 0 || report.Total.Failures != 0 {
		t.Errorf("a fresh instance reports %+v, want all zeroes", report.Total)
	}
	if len(report.Days) != 0 {
		t.Errorf("a fresh instance reports %d day(s) of figures, want none", len(report.Days))
	}
	if keys := sp.kv.keys(statsKeyPrefix); len(keys) != 0 {
		t.Errorf("a fresh instance already wrote %v", keys)
	}
}

// TestADayWhoseFiguresCannotBeReadStartsCountingAgain.
//
// A record in the store can become unreadable — a half-written value, a
// format from a future version, a corrupted row. That day's figures are then
// lost, which is unfortunate but survivable.
//
// What is NOT survivable is the day staying stuck. The update is a
// compare-and-set, and "replace whatever is there" and "insert only if nothing
// is there" are different operations: if the unreadable value is written off
// as "nothing is there", every attempt to replace it is refused, the retries
// are all spent, and that day counts nothing for as long as the record lives —
// up to the full retention window. An administrator would see a day sitting at
// zero while uploads were plainly being compressed.
func TestADayWhoseFiguresCannotBeReadStartsCountingAgain(t *testing.T) {
	sp := newStatsPlugin(t, map[string]any{})

	// Put something unreadable where today's figures belong — in THIS
	// plugin's own shard, because that is the key its uploads write to.
	key := statsShardKeyFor(time.Now(), sp.MediaForgePlugin.statsShard())
	if appErr := sp.API.KVSetWithExpiry(key, []byte("{not json at all"), 3600); appErr != nil {
		t.Fatalf("seeding the broken record failed: %v", appErr)
	}
	if _, ok := sp.kv.get(key); !ok {
		t.Fatal("the broken record was not stored, so this test proves nothing")
	}

	if err := sp.MediaForgePlugin.recordStats(statsKindImage, statsEvent{
		files: 1, original: 1000, stored: 400,
	}); err != nil {
		t.Errorf("recording after an unreadable record failed: %v", err)
	}

	got := sp.report(t)
	if got.Total.Files != 1 {
		t.Errorf("the report counts %d file(s) after the day was started again, want 1 — "+
			"an unreadable record must be replaced, not treated as an empty key",
			got.Total.Files)
	}
	if got.Total.BytesSaved != 600 {
		t.Errorf("the report claims %d byte(s) saved, want 600", got.Total.BytesSaved)
	}

	// And it keeps counting afterwards, rather than the next upload hitting
	// the same wall.
	if err := sp.MediaForgePlugin.recordStats(statsKindImage, statsEvent{
		files: 1, original: 2000, stored: 500,
	}); err != nil {
		t.Errorf("the second recording failed: %v", err)
	}
	if got := sp.report(t); got.Total.Files != 2 {
		t.Errorf("the report counts %d file(s) after a second upload, want 2", got.Total.Files)
	}
}

// TestAStoredTotalIsNeverTrusted.
//
// The daily rows and the report's own totals are two routes to the same
// number, and an administrator reads both on one page. If they could
// disagree, one of them would be wrong with no way to tell which.
//
// They cannot, because the persisted `total` field is discarded and
// recomputed from the two per-kind pots on every read. This test writes a
// record whose stored total is deliberately nonsense — the shape a future
// version, a partial write or a hand-edited row would have — and requires
// the report to ignore it.
func TestAStoredTotalIsNeverTrusted(t *testing.T) {
	sp := newStatsPlugin(t, map[string]any{})

	// A well-formed record with a total that contradicts its own pots.
	record := statsDay{
		Day:   time.Now().UTC().Format(statsDayLayout),
		Image: statsCounters{Files: 2, BytesOriginal: 1000, BytesStored: 400, BytesSaved: 600},
		Video: statsCounters{Files: 1, BytesOriginal: 500, BytesStored: 100, BytesSaved: 400},
		Total: statsCounters{Files: 999, BytesOriginal: 7, BytesStored: 7, BytesSaved: 123456},
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("preparing the record failed: %v", err)
	}
	key := statsShardKeyFor(time.Now(), sp.MediaForgePlugin.statsShard())
	if appErr := sp.API.KVSetWithExpiry(key, encoded, 3600); appErr != nil {
		t.Fatalf("seeding the record failed: %v", appErr)
	}

	got := sp.report(t)
	if got.Total.Files != 3 || got.Total.BytesSaved != 1000 {
		t.Errorf("the report says %d file(s) / %d byte(s) saved, want 3 / 1000 — the stored "+
			"total must be recomputed from the two kinds, never believed",
			got.Total.Files, got.Total.BytesSaved)
	}
	if len(got.Days) != 1 {
		t.Fatalf("the report shows %d day(s), want 1", len(got.Days))
	}
	if got.Days[0].Total != got.Total {
		t.Errorf("the day row says %+v but the report total says %+v — the two must agree",
			got.Days[0].Total, got.Total)
	}
}

// ------------------------------------------------- the earlier work contract

// TestCollectingTheFiguresLeavesTheNoteContractIntact.
//
// The store between the two note hooks is deliberately NOT emptied when a post
// collects it, because MessageWillBePosted runs before the post is stored.
// Reading the same records for the report must not change that: a saving
// consumed by the report would leave a compressed file without its note, which
// is precisely the exception SPEC §4.4 forbids.
func TestCollectingTheFiguresLeavesTheNoteContractIntact(t *testing.T) {
	sp := newStatsPlugin(t, map[string]any{})

	input := encodeJPEG(t, noisyImage(3000, 2000, 80), 95)
	info := &model.FileInfo{Id: model.NewId(), Name: "photo.jpg", Size: int64(len(input))}
	var out writeCounter
	if _, rejection := sp.FileWillBeUploaded(
		&plugin.Context{}, info, bytesReaderOf(input), &out); rejection != "" {
		t.Fatalf("the hook rejected the upload: %s", rejection)
	}

	before := len(sp.kv.keys(noteKeyPrefix))
	if before == 0 {
		t.Fatal("no note record was written, so this test proves nothing")
	}

	sp.report(t)

	if after := len(sp.kv.keys(noteKeyPrefix)); after != before {
		t.Errorf("collecting the figures changed the note store from %d record(s) to %d — "+
			"see TestAPostThatNeverGetsStoredDoesNotLoseItsNote", before, after)
	}

	// And the note itself still arrives on the post.
	post := &model.Post{Id: model.NewId(), Message: "look", FileIds: []string{info.Id}}
	updated, rejection := sp.MessageWillBePosted(&plugin.Context{}, post)
	if rejection != "" {
		t.Fatalf("MessageWillBePosted rejected the post: %s", rejection)
	}
	if updated == nil {
		t.Fatal("the post carries no note although the file was compressed")
	}
}

// ------------------------------------------------------------ the page

// TestTheReportIsRefusedWithoutAdministratorPermission.
//
// The figures say how much traffic an instance carries and when. That is not
// public, and the plugin's HTTP surface is reachable by any logged-in user, so
// the check has to be here and has to fail closed.
func TestTheReportIsRefusedWithoutAdministratorPermission(t *testing.T) {
	sp := newStatsPlugin(t, map[string]any{})
	sp.api.On("HasPermissionTo", mock.Anything, mock.Anything).
		Return(func(userID string, _ *model.Permission) bool {
			return userID == "admin"
		}).Maybe()

	cases := []struct {
		name   string
		userID string
		want   int
	}{
		{"nobody at all", "", http.StatusUnauthorized},
		{"an ordinary user", "someone", http.StatusForbidden},
		{"a system administrator", "admin", http.StatusOK},
	}

	for _, tc := range cases {
		for _, path := range []string{statsPagePath, statsDataPath} {
			t.Run(tc.name+" "+path, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				if tc.userID != "" {
					req.Header.Set("Mattermost-User-Id", tc.userID)
				}
				rec := httptest.NewRecorder()
				sp.ServeHTTP(&plugin.Context{}, rec, req)
				if rec.Code != tc.want {
					t.Errorf("%s got HTTP %d, want %d", path, rec.Code, tc.want)
				}
			})
		}
	}
}

// TestTheReportPageIsWrittenForAdministrators.
//
// The constraint on this slice is explicit: the page is read by someone
// running a server, not by someone reading the source. It has to name the
// practical effect in plain words, and it must not leak the vocabulary of the
// implementation.
func TestTheReportPageIsWrittenForAdministrators(t *testing.T) {
	sp := newStatsPlugin(t, map[string]any{})
	sp.api.On("HasPermissionTo", mock.Anything, mock.Anything).Return(true).Maybe()

	input := encodeJPEG(t, noisyImage(3000, 2000, 81), 95)
	if _, rejection := sp.uploadThroughHook(t, "photo.jpg", input); rejection != "" {
		t.Fatalf("the hook rejected the upload: %s", rejection)
	}

	req := httptest.NewRequest(http.MethodGet, statsPagePath, nil)
	req.Header.Set("Mattermost-User-Id", "admin")
	rec := httptest.NewRecorder()
	sp.ServeHTTP(&plugin.Context{}, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("the report page returned HTTP %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("the report page is served as %q, want HTML", ct)
	}
	body := rec.Body.String()
	lower := strings.ToLower(body)

	for _, want := range []string{"storage space", "images", "videos", "could not"} {
		if !strings.Contains(lower, want) {
			t.Errorf("the page never says %q, so an administrator cannot tell what they "+
				"are looking at", want)
		}
	}
	for _, jargon := range []string{
		"goroutine", "kv store", "key-value", "compare-and-set", "processoutcome",
		"noterecord", "json", "hook", "struct", "nil",
	} {
		if strings.Contains(lower, jargon) {
			t.Errorf("the page contains implementation vocabulary %q", jargon)
		}
	}

	// The page is the plugin's own HTML, so an unescaped file name would be an
	// injection point. Nothing on it may be able to open a tag.
	if strings.Contains(body, "<script") {
		t.Error("the page contains a script tag")
	}
}

// TestTheDataEndpointIsTheNumbersTheConsoleReads.
//
// The page is for reading; the endpoint is what a monitoring system or the
// proof script fetches. They must report the same figures, or a discrepancy
// between the two becomes its own support question.
func TestTheDataEndpointIsTheNumbersTheConsoleReads(t *testing.T) {
	sp := newStatsPlugin(t, map[string]any{})
	sp.api.On("HasPermissionTo", mock.Anything, mock.Anything).Return(true).Maybe()

	input := encodeJPEG(t, noisyImage(3000, 2000, 82), 95)
	if _, rejection := sp.uploadThroughHook(t, "photo.jpg", input); rejection != "" {
		t.Fatalf("the hook rejected the upload: %s", rejection)
	}

	req := httptest.NewRequest(http.MethodGet, statsDataPath, nil)
	req.Header.Set("Mattermost-User-Id", "admin")
	rec := httptest.NewRecorder()
	sp.ServeHTTP(&plugin.Context{}, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("the data endpoint returned HTTP %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("the data endpoint is served as %q, want JSON", ct)
	}

	var got statsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("the data endpoint did not return readable figures: %v", err)
	}
	want := sp.report(t)
	if got.Total != want.Total {
		t.Errorf("the endpoint reports %+v, the collection reports %+v", got.Total, want.Total)
	}
}

// TestTheConsoleTellsTheAdministratorWhereTheReportIs.
//
// A page nobody can find is not a report. The manifest is the only place
// MediaForge can put a signpost, so the link has to be there and has to be the
// route the plugin actually serves.
func TestTheConsoleTellsTheAdministratorWhereTheReportIs(t *testing.T) {
	raw, err := readManifestBytes()
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

	text := manifest.SettingsSchema.Header + "\n" + manifest.SettingsSchema.Footer
	for _, s := range allManifestSettings(manifest.SettingsSchema) {
		text += "\n" + s.HelpText
	}
	route := "/plugins/" + manifest.Id + statsPagePath
	if !strings.Contains(text, route) {
		t.Errorf("no text on the settings page links to %s, so the report is unreachable "+
			"from the System Console", route)
	}
}

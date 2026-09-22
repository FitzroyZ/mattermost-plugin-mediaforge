package main

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

// The two kinds of work the report keeps apart.
//
// One pot would hide the case an administrator actually needs to see: video
// transcoding is where the processor time and the failures are, images are
// where the volume is. Telling them apart is what makes the figures
// actionable rather than merely large.
const (
	statsKindImage = "image"
	statsKindVideo = "video"
)

// statsKeyPrefix namespaces one day's figures in the key-value store.
const statsKeyPrefix = "mfstats_"

// statsRetentionDays is how many days of figures are kept.
//
// The figures are kept per day so the report can show a trend, which means one
// new key per day for as long as the plugin is installed. Without a bound that
// store grows for ever — a defect that only surfaces on the instance that has
// been running longest, which is the one least able to afford it.
//
// Ninety days covers a quarter, which is the span an administrator actually
// compares ("is this worth keeping switched on?"), and it is short enough that
// the whole report is a few dozen small reads.
const statsRetentionDays = 90

// statsDayLayout is the day key, in UTC.
//
// UTC rather than the server's local zone on purpose: a machine whose timezone
// changes — a move, a daylight-saving step, an administrator correcting the
// clock — must not make a day's figures land in two different buckets or
// overwrite yesterday's.
const statsDayLayout = "2006-01-02"

// statsEvent is one file's contribution to the figures.
//
// It is deliberately the same two sizes noteRecord carries, because they come
// from the same measurement. Measuring a second time for the report is how two
// numbers drift apart until nobody can say which one is right.
type statsEvent struct {
	// files is 1 for a file that really was re-encoded, 0 otherwise. A file
	// stored as sent saved nothing and must not inflate the count — the same
	// distinction the note contract of SPEC §4.4 rests on.
	files int64
	// original and stored are the sizes before and after.
	//
	// original comes from processOutcome.originalSize and NEVER from
	// fileinfo.Size: the server overwrites that field with the compressed byte
	// count the instant the hook returns, so a saving computed from it is a
	// flat zero everywhere while looking perfectly plausible in the code.
	original int64
	stored   int64
	// failures is 1 for an upload the plugin refused because it could not
	// convert the file (SPEC §4.2). A failure an administrator cannot see is
	// the one that turns into a support request.
	failures int64
}

// statsCounters is what the report shows for one type over one span.
type statsCounters struct {
	// Files is how many files were actually re-encoded.
	Files int64 `json:"files"`
	// BytesOriginal and BytesStored are the two measured sums. Both are
	// carried rather than only their difference, so the report can show how
	// much was sent as well as how much was kept — and so a reader can check
	// the arithmetic.
	BytesOriginal int64 `json:"bytes_original"`
	BytesStored   int64 `json:"bytes_stored"`
	// BytesSaved is the difference, computed rather than stored. Keeping it
	// separately would allow the three numbers to disagree.
	BytesSaved int64 `json:"bytes_saved"`
	// Failures is how many uploads were refused because the file could not be
	// converted.
	Failures int64 `json:"failures"`
}

// add folds one event into the counters.
func (c *statsCounters) add(e statsEvent) {
	c.Files += e.files
	c.BytesOriginal += e.original
	c.BytesStored += e.stored
	c.Failures += e.failures
	c.BytesSaved = c.BytesOriginal - c.BytesStored
}

// merge folds another set of counters in.
func (c *statsCounters) merge(other statsCounters) {
	c.Files += other.Files
	c.BytesOriginal += other.BytesOriginal
	c.BytesStored += other.BytesStored
	c.Failures += other.Failures
	c.BytesSaved = c.BytesOriginal - c.BytesStored
}

// statsDay is one day's figures, split by type.
//
// This is also the wire format in the key-value store, which is why the field
// names are short and stable: a day written by one version has to stay
// readable by the next.
type statsDay struct {
	// Day is the UTC date these figures belong to.
	Day string `json:"day"`
	// Image and Video are the two pots.
	Image statsCounters `json:"image"`
	Video statsCounters `json:"video"`
	// Total is the sum of the two, computed on read.
	Total statsCounters `json:"total"`
}

// recompute derives the day's total from its two pots.
func (d *statsDay) recompute() {
	d.Total = statsCounters{}
	d.Total.merge(d.Image)
	d.Total.merge(d.Video)
}

// counters returns a pointer to the pot for one kind, or nil for a kind the
// report does not keep.
func (d *statsDay) counters(kind string) *statsCounters {
	switch kind {
	case statsKindImage:
		return &d.Image
	case statsKindVideo:
		return &d.Video
	default:
		return nil
	}
}

// statsReport is the whole answer the System Console reads.
type statsReport struct {
	// Total is every day added together.
	Total statsCounters `json:"total"`
	// Image and Video are the same split, over the whole retained span.
	Image statsCounters `json:"image"`
	Video statsCounters `json:"video"`
	// Days is the trend, newest first, with one row per day that has figures.
	// A day on which nothing happened gets no row: an invented zero row would
	// suggest the plugin was running and saving nothing.
	Days []statsDay `json:"days"`
	// RetentionDays is how far back the figures go, so the page can say so
	// rather than leave a reader guessing what "total" covers.
	RetentionDays int `json:"retention_days"`
}

// byKind returns the counters for one type.
func (r statsReport) byKind(kind string) statsCounters {
	switch kind {
	case statsKindImage:
		return r.Image
	case statsKindVideo:
		return r.Video
	default:
		return statsCounters{}
	}
}

// statsKeyFor is the store prefix shared by every shard of one UTC day.
//
// It is a prefix rather than a key: the figures for a day live in several
// keys, one per writer, and the report adds them up. See recordStatsOn for
// why they are split.
func statsKeyFor(day time.Time) string {
	return statsKeyPrefix + day.UTC().Format(statsDayLayout)
}

// statsShardCount is how many keys one day's figures are spread over.
//
// Small on purpose. The shards exist to stop concurrent uploads competing for
// one value, not to scale without limit, and every shard is one extra read
// when the report is assembled: ninety days times this number is the whole
// cost of opening the page. Eight is comfortably above the four concurrent
// image jobs SPEC §4.4 allows by default, so in normal operation a writer
// rarely meets another in its own shard at all.
const statsShardCount = 8

// statsShardKeyFor is the key one writer uses for one day.
func statsShardKeyFor(day time.Time, shard int) string {
	return fmt.Sprintf("%s_%d", statsKeyFor(day), shard)
}

// statsShard is the shard this plugin instance writes to.
//
// It is chosen once per process and then never changes, which is what makes
// the sharding work: a writer that picked a new shard per upload would be
// back to competing with everyone, just across more keys. One shard per
// process means the only writers who can collide are that process's own
// uploads — already capped by the concurrency limits.
func (p *MediaForgePlugin) statsShard() int {
	p.statsShardOnce.Do(func() {
		p.statsShardID = rand.IntN(statsShardCount)
	})
	return p.statsShardID
}

// statsExpirySecondsFor is how much longer a day's figures may be kept.
//
// It counts from the END of that day, so every day lives exactly the retained
// window and no longer, however often it is written to. A day that is already
// older than the window still gets the shortest expiry the API accepts rather
// than none at all: a zero would mean "keep for ever", which is the one value
// that must never be written here.
func statsExpirySecondsFor(day time.Time) int64 {
	endOfDay := day.UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	remaining := endOfDay.Add(statsRetentionDays * 24 * time.Hour).Sub(time.Now().UTC())
	seconds := int64(remaining / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

// backoffBeforeRetry waits a short, randomised moment before trying again.
//
// It exists to break a lockstep, not to be clever. When several uploads
// collide on one key they all read the same value, all but one lose, and the
// losers retry at exactly the same instant — colliding again, and again, until
// the attempts run out and their contributions are dropped. Spreading them
// over a random interval that widens with each attempt lets them through one
// at a time.
//
// THE CEILING WAS MEASURED in earlier work, and the measurement is the reason
// it is no longer four milliseconds. the documentation pass carried in a flaky test; the security audit
// was required to settle it by measuring how many writers per shard can really
// meet, rather than by raising a constant on a hunch. The measurement said two
// things:
//
//   - Several server processes DO share a shard. Each picks one of eight with
//     rand.IntN(8) and keeps it, so with eight processes the busiest shard
//     holds 2.6 of them on average and all eight land distinct in 0.23 % of
//     runs. the statistics report's "a writer rarely meets another in its own shard" is true
//     of one process and wrong of several.
//   - At that contention the old budget was NOT enough.
//     TestTheRetryBudgetSurvivesTheWorstRealisticContention drives exactly what
//     SPEC §4.4 permits — three processes, five writers each, one shard — and
//     under the race detector it lost 40 of 240 contributions with the retry
//     budget exhausted three times. Without the detector the same test loses
//     nothing, which is why this went unseen for three slices: the detector's
//     slowdown is what makes the writers actually overlap.
//
// So the window is now eight times wider and the attempts doubled. The cost is
// bounded and invisible: worst case about a second of sleeping spread over 32
// attempts, on a request that already spends seconds transcoding, and only on
// the rare upload that collides repeatedly. The alternative — losing an
// increment — is silent and always biases the totals downwards, which is the
// one defect the whole sharding arrangement exists to prevent.
func backoffBeforeRetry(attempt int) {
	const (
		base = 200 * time.Microsecond
		cap_ = 32 * time.Millisecond
	)
	window := base << min(attempt, 8)
	if window > cap_ {
		window = cap_
	}
	// rand/v2's global source is safe for concurrent use, which matters: this
	// runs on every upload goroutine at once.
	time.Sleep(time.Duration(rand.Int64N(int64(window)) + int64(base)))
}

// recordStats adds one file's contribution to today's figures.
func (p *MediaForgePlugin) recordStats(kind string, event statsEvent) error {
	return p.recordStatsOn(time.Now(), kind, event)
}

// recordStatsOn adds one file's contribution to a given day's figures.
//
// The update is a read-modify-write against a store several server processes
// share, so it MUST be atomic. Uploads run in parallel by design (SPEC §4.4
// allows four images at once by default), and a plain get-then-set loses one
// increment whenever two land together. That loss is silent and it always
// biases the same way: the totals come out low, which is indistinguishable
// from "we saved less than we thought".
//
// The figures are therefore split across one key per day PER WRITER rather
// than one key per day, and the report adds the shards up when it reads them.
// A single key makes every upload on the whole instance compete for the same
// value, and compare-and-set does not queue: the losers retry together, in
// lockstep, and under load some of them simply run out of attempts and drop
// their contribution on the floor. That was measured, not feared — 64 writers
// against one key lost 92 of 512 contributions under the race detector, and a
// randomised backoff alone did not fix it. Sharding removes the competition
// instead of managing it: within one shard only that process's own uploads
// collide, and SPEC §4.4 already caps those at four.
func (p *MediaForgePlugin) recordStatsOn(when time.Time, kind string, event statsEvent) error {
	if p.API == nil {
		return nil
	}
	if event == (statsEvent{}) {
		// Nothing happened worth recording. A file stored as sent lands here,
		// and writing a zero for it would only cost a key.
		return nil
	}

	key := statsShardKeyFor(when, p.statsShard())
	day := when.UTC().Format(statsDayLayout)

	// Bounded rather than endless. A retry loop that never gives up would turn
	// a store that is refusing writes into an upload that never finishes, and
	// the figures are worth far less than the upload. The randomised wait
	// between attempts breaks the lockstep that makes concurrent writers
	// collide again the instant they retry.
	//
	// Thirty-two rather than sixteen, and that number is measured rather than
	// chosen — see backoffBeforeRetry for the measurement that raised it.
	const attempts = 32
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			backoffBeforeRetry(attempt)
		}
		existing, appErr := p.API.KVGet(key)
		if appErr != nil {
			lastErr = fmt.Errorf("reading the figures for %s: %s", day, appErr.Error())
			break
		}

		record := statsDay{Day: day}
		if len(existing) > 0 {
			if err := json.Unmarshal(existing, &record); err != nil {
				// A day that cannot be read back can never become a row in
				// the report. Starting it over loses that day rather than
				// every day, and the alternative — refusing to count from
				// here on — would be worse.
				//
				// `existing` is deliberately KEPT as it is. It is what the
				// compare-and-set matches against, and the unreadable value
				// is genuinely still in the store: clearing it here would
				// turn "replace what is there" into "insert only if nothing
				// is there", which this key can never satisfy. Every attempt
				// would then be refused, the retries would be spent, and that
				// day would count nothing for as long as the broken record
				// lived — up to the full retention window, with a log line
				// blaming a concurrent upload that never happened.
				p.logWarn("MediaForge: the saved figures for one day could not be read and "+
					"were started again; that day's totals will be incomplete",
					"day", day, "error", err.Error())
				record = statsDay{Day: day}
			}
			record.Day = day
		}

		pot := record.counters(kind)
		if pot == nil {
			return fmt.Errorf("unknown kind %q", kind)
		}
		pot.add(event)
		record.recompute()

		updated, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("preparing the figures for %s: %w", day, err)
		}

		written, appErr := p.API.KVSetWithOptions(key, updated, model.PluginKVSetOptions{
			Atomic:   true,
			OldValue: existing,
			// The expiry is what bounds the store, and it is measured from
			// the DAY the figures belong to rather than from this write.
			//
			// Measuring from the write would extend a day's life every time
			// another upload landed on it, so a busy day would survive longer
			// than a quiet one and the retained window would not be the
			// window the report claims to show. Day-relative expiry makes
			// "the last ninety days" mean exactly that.
			ExpireInSeconds: statsExpirySecondsFor(when),
		})
		if appErr != nil {
			lastErr = fmt.Errorf("saving the figures for %s: %s", day, appErr.Error())
			break
		}
		if written {
			return nil
		}
		// Another upload got there first. Read again and redo the arithmetic
		// on top of what it wrote. The count names the attempts SPENT, not
		// the conflicts seen so far: a maintainer reading "8 attempts" in the
		// log can compare it against the bound above without arithmetic.
		lastErr = fmt.Errorf("the figures for %s were still being changed by other uploads "+
			"after %d attempt(s)", day, attempt+1)
	}

	if lastErr != nil {
		// A failure here is logged, never propagated to the upload. Storing
		// the user's file is the job; the figures are a convenience on top of
		// it, and losing one file's contribution must not lose the file.
		p.logWarn("MediaForge: one file's contribution to the statistics could not be "+
			"saved, so the report will be short by that file",
			"error", lastErr.Error())
	}
	return lastErr
}

// statistics reads every retained day back and adds it up.
//
// The days are read by name rather than listed, because a name is derivable
// and a listing is not bounded: KVList returns every key the plugin owns,
// including one per pending size note, so on a busy instance the report would
// page through thousands of unrelated keys to find ninety.
func (p *MediaForgePlugin) statistics() (statsReport, error) {
	report := statsReport{RetentionDays: statsRetentionDays}
	if p.API == nil {
		return report, nil
	}

	today := time.Now().UTC()
	for offset := range statsRetentionDays {
		day := today.AddDate(0, 0, -offset)
		// One day's figures live in several keys — one per writer, so that
		// concurrent uploads never compete for the same value. Adding them
		// up here is the other half of that arrangement.
		combined := statsDay{Day: day.Format(statsDayLayout)}
		found := false

		for shard := range statsShardCount {
			raw, appErr := p.API.KVGet(statsShardKeyFor(day, shard))
			if appErr != nil {
				return statsReport{}, fmt.Errorf("reading the figures for %s: %s",
					day.Format(statsDayLayout), appErr.Error())
			}
			if len(raw) == 0 {
				continue
			}
			var part statsDay
			if err := json.Unmarshal(raw, &part); err != nil {
				p.logWarn("MediaForge: some of the saved figures for one day could not be "+
					"read and were left out of the report",
					"day", day.Format(statsDayLayout), "error", err.Error())
				continue
			}
			combined.Image.merge(part.Image)
			combined.Video.merge(part.Video)
			found = true
		}

		if !found {
			// Nothing happened that day. It gets no row: an invented zero row
			// would suggest the plugin ran and saved nothing.
			continue
		}

		// The stored total is never believed. It is recomputed from the two
		// pots, so the day row and the report's own totals can only ever be
		// two views of one number.
		combined.recompute()
		report.Days = append(report.Days, combined)
		report.Image.merge(combined.Image)
		report.Video.merge(combined.Video)
	}

	report.Total.merge(report.Image)
	report.Total.merge(report.Video)

	// Newest first: the question an administrator opens this page with is
	// almost always about now, not about ninety days ago.
	sort.Slice(report.Days, func(i, j int) bool {
		return report.Days[i].Day > report.Days[j].Day
	})
	return report, nil
}

// recordUploadStats is the hook's single call into the figures.
//
// It reads the same measurement the size note uses — processOutcome, filled in
// where the whole file was still in hand — so the report and the note can
// never disagree. Deriving the original size from fileinfo.Size instead would
// silently produce a flat zero saving everywhere: the server overwrites that
// field with the compressed byte count immediately after the hook returns.
func (p *MediaForgePlugin) recordUploadStats(outcome processOutcome, rejected bool) {
	kind := statsKindImage
	if outcome.isVideo {
		kind = statsKindVideo
	}

	switch {
	case rejected:
		// Only the video pipeline rejects, and only a file it committed to
		// converting (SPEC §4.2).
		_ = p.recordStats(statsKindVideo, statsEvent{failures: 1})
	case outcome.reEncoded && outcome.originalSize > 0 && outcome.bytesWritten > 0:
		_ = p.recordStats(kind, statsEvent{
			files:    1,
			original: outcome.originalSize,
			stored:   outcome.bytesWritten,
		})
	default:
		// Stored as sent, whatever the reason — below the threshold, not
		// compressible, already efficient, or every slot busy. It saved
		// nothing, so it contributes nothing.
	}
}

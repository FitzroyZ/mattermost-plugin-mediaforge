package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
)

// ---------------------------------------------------------------- helpers

// deadline fails the test if it is still running when the budget expires.
//
// Every test in this file gets one. A concurrency defect does not usually
// announce itself as a wrong value — it announces itself as a test that never
// finishes, and `go test` would then report a ten-minute package timeout with
// no indication of which test hung. A per-test deadline turns a deadlock into
// a named failure, which is the difference between a diagnosis and a mystery.
func deadline(t *testing.T, budget time.Duration) {
	t.Helper()

	// The race detector instruments every memory access, so the same work
	// takes several times as long. Without this correction a correct
	// implementation trips its own deadline on the race run — a failure about
	// the harness that looks exactly like the deadlock this helper exists to
	// diagnose. See race_on_test.go.
	budget *= raceSlowdown

	done := make(chan struct{})
	t.Cleanup(func() { close(done) })

	go func() {
		select {
		case <-done:
		case <-time.After(budget):
			// Dump every goroutine before failing: in a deadlock the stack
			// trace names the exact wait that never returned.
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			fmt.Fprintf(os.Stderr, "\nDEADLOCK in %s after %s — all goroutines:\n%s\n",
				t.Name(), budget, buf[:n])
			panic("test " + t.Name() + " exceeded its own deadline of " + budget.String())
		}
	}()
}

// runningTranscodes counts the ffmpeg encoder processes this plugin currently
// has running.
//
// Two conditions, both required. The spool-file prefix attributes the process
// to MediaForge rather than to an unrelated ffmpeg on the same machine. The
// "-crf" flag picks out the ENCODER specifically: ffprobe is also invoked on
// the spooled file and so carries the prefix too, and counting probes as
// transcodes would report a concurrency this slice never promised to limit.
// Fixture generation is excluded by the same pair — it uses -crf but writes to
// the test's own directory, not to a spool file.
func runningTranscodes() []string {
	var found []string
	for _, line := range findProcesses(spoolFilePrefix) {
		if strings.Contains(line, "-crf") {
			found = append(found, line)
		}
	}
	return found
}

// concurrencySampler records the highest number of things running at once.
//
// SPEC §4.4 promises a limit on simultaneous work, and the only honest way to
// check a limit is to watch while the work happens. Counting afterwards proves
// nothing: by then the machine is quiet again and every possible
// implementation, including one with no limit at all, looks identical.
type concurrencySampler struct {
	stop chan struct{}
	done chan struct{}
	peak atomic.Int64
}

// sampleEvery starts polling count until stopped, remembering the maximum.
func sampleEvery(interval time.Duration, count func() int) *concurrencySampler {
	s := &concurrencySampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				if n := int64(count()); n > s.peak.Load() {
					s.peak.Store(n)
				}
			}
		}
	}()
	return s
}

func (s *concurrencySampler) highest() int {
	close(s.stop)
	<-s.done
	return int(s.peak.Load())
}

// queueConfig is videoConfig with real gates attached at the given limits.
func queueConfig(t *testing.T, videoLimit, imageLimit int) configuration {
	t.Helper()
	cfg := videoConfig(t)
	cfg.MaxConcurrentVideoJobs = videoLimit
	cfg.MaxConcurrentImageJobs = imageLimit
	cfg.videoGate = newJobGate(videoLimit)
	cfg.imageGate = newJobGate(imageLimit)
	return cfg
}

// ------------------------------------------------- the core promise: a limit

// TestSimultaneousVideoUploadsNeverExceedTheConfiguredLimit is the slice.
//
// N uploads are started at the same instant and the number of encoder
// processes actually running side by side is sampled while they are in
// flight. The observed peak — not an assumption about the code — has to stay
// within the configured limit.
//
// The test is written so that removing the control makes it fail: with the
// gate gone, all N encoders start within milliseconds of each other and the
// sampler sees them together. That was verified by deleting the acquire call
// and watching this test report a peak of 6.
func TestSimultaneousVideoUploadsNeverExceedTheConfiguredLimit(t *testing.T) {
	deadline(t, 4*time.Minute)

	const uploads = 6
	for _, limit := range []int{1, 2} {
		t.Run("limit="+strconv.Itoa(limit), func(t *testing.T) {
			deadline(t, 2*time.Minute)

			cfg := queueConfig(t, limit, 4)
			cfg.spoolDir = t.TempDir()
			// Wastefully encoded, so every upload genuinely reaches the
			// encoder instead of being skipped as already efficient — a
			// skipped file would prove nothing about concurrency.
			original := makeVideo(t, cfg.tools, 8, "640x480")

			sampler := sampleEvery(10*time.Millisecond, func() int {
				return len(runningTranscodes())
			})

			var wg sync.WaitGroup
			errs := make([]error, uploads)
			encoded := make([]bool, uploads)
			start := make(chan struct{})
			for i := range uploads {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					var stored bytes.Buffer
					outcome, err := processUpload(&stored, bytes.NewReader(original), cfg)
					errs[i] = err
					encoded[i] = outcome.reEncoded
				}()
			}
			close(start)
			wg.Wait()

			peak := sampler.highest()

			for i, err := range errs {
				if err != nil {
					t.Errorf("upload %d failed: %v", i, err)
				}
			}
			if peak > limit {
				t.Errorf("%d transcodes ran at the same time, but the limit is %d — "+
					"concurrent uploads can still saturate the server", peak, limit)
			}
			if peak == 0 {
				t.Fatal("no transcode was ever observed running, so this test measured nothing")
			}

			// Every upload must still have been converted. A limit that is
			// honoured by silently dropping work is not a limit, it is a
			// regression.
			for i, ok := range encoded {
				if !ok {
					t.Errorf("upload %d was not transcoded; the queue lost or skipped it", i)
				}
			}
			t.Logf("limit %d: observed peak %d concurrent transcodes across %d uploads",
				limit, peak, uploads)
		})
	}
}

// TestNoUploadIsLostOrProcessedTwiceUnderLoad — a queue that forgets a job or
// runs it twice would show up as a missing or duplicated result, not as an
// error. Each upload therefore carries a distinguishable payload.
func TestNoUploadIsLostOrProcessedTwiceUnderLoad(t *testing.T) {
	deadline(t, 3*time.Minute)

	cfg := queueConfig(t, 1, 4)
	cfg.spoolDir = t.TempDir()

	const uploads = 8
	// Distinguishable fixtures: different durations produce different stored
	// byte counts and different reported source geometry.
	sizes := []string{"320x240", "416x240", "480x270", "512x288"}

	type result struct {
		index  int
		width  int
		height int
		bytes  int
		err    error
	}
	results := make([]result, uploads)
	originals := make([][]byte, uploads)
	for i := range uploads {
		originals[i] = makeVideo(t, cfg.tools, 2, sizes[i%len(sizes)])
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range uploads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var stored bytes.Buffer
			outcome, err := processUpload(&stored, bytes.NewReader(originals[i]), cfg)
			results[i] = result{
				index: i, width: outcome.width, height: outcome.height,
				bytes: stored.Len(), err: err,
			}
		}()
	}
	close(start)
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Errorf("upload %d was lost: %v", i, r.err)
			continue
		}
		if r.bytes == 0 {
			t.Errorf("upload %d stored nothing", i)
		}
		wantSize := sizes[i%len(sizes)]
		got := fmt.Sprintf("%dx%d", r.width, r.height)
		if got != wantSize {
			t.Errorf("upload %d stored %s but was sent %s — a queued job was "+
				"paired with the wrong upload", i, got, wantSize)
		}
	}

	// Every spool file must be gone. A job processed twice, or a job whose
	// slot was released without its cleanup running, shows up here.
	entries, err := os.ReadDir(cfg.spoolDir)
	if err != nil {
		t.Fatalf("inspecting the spool directory failed: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("%d temporary file(s) survived the concurrent run", len(entries))
	}
}

// ------------------------------------------------ slots are always given back

// TestAFailedJobGivesItsSlotBack is the deadlock guard, in the form the
// problem actually takes: not a crash, but a server on which every upload
// after the first failure hangs forever.
func TestAFailedJobGivesItsSlotBack(t *testing.T) {
	deadline(t, 2*time.Minute)

	cases := []struct {
		name    string
		prepare func(t *testing.T, cfg *configuration) (input []byte, reader io.Reader)
	}{
		{
			name: "the transcode fails",
			prepare: func(t *testing.T, cfg *configuration) ([]byte, io.Reader) {
				damaged := corruptVideo(makeVideo(t, cfg.tools, 2, "320x240"))
				return damaged, bytes.NewReader(damaged)
			},
		},
		{
			name: "the transcode runs out of time",
			prepare: func(t *testing.T, cfg *configuration) ([]byte, io.Reader) {
				long := makeVideo(t, cfg.tools, 20, "1280x720")
				cfg.VideoTimeoutSeconds = 1
				return long, bytes.NewReader(long)
			},
		},
		{
			name: "the upload stream breaks",
			prepare: func(t *testing.T, cfg *configuration) ([]byte, io.Reader) {
				whole := makeVideo(t, cfg.tools, 3, "640x480")
				return whole, io.MultiReader(
					bytes.NewReader(whole[:len(whole)/2]),
					&erroringReader{err: errors.New("backing store went away")},
				)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deadline(t, 90*time.Second)

			// A limit of one makes a leaked slot fatal rather than merely
			// wasteful: if the failing job keeps it, nothing else can ever run.
			cfg := queueConfig(t, 1, 4)
			cfg.spoolDir = t.TempDir()

			_, reader := tc.prepare(t, &cfg)
			var discarded bytes.Buffer
			_, _ = processUpload(&discarded, reader, cfg)

			// Whatever happened above, the slot must be free now.
			if active, _ := cfg.videoGate.state(); active != 0 {
				t.Fatalf("%d slot(s) are still held after the job finished — "+
					"every following upload would queue behind a job that is gone", active)
			}

			// And prove it by using it: a healthy upload must go straight
			// through afterwards.
			cfg.VideoTimeoutSeconds = defaultVideoTimeoutSeconds
			good := makeVideo(t, cfg.tools, 2, "320x240")
			var stored bytes.Buffer
			outcome, err := processUpload(&stored, bytes.NewReader(good), cfg)
			if err != nil {
				t.Fatalf("the upload after a failed job was rejected: %v", err)
			}
			if !outcome.reEncoded {
				t.Error("the upload after a failed job was not transcoded, " +
					"which means it never got a slot")
			}
		})
	}
}

// TestARejectedJobGivesItsSlotBack covers the queue's own refusal: a job that
// waited too long is passed through, and must not keep a slot it never got.
func TestARejectedJobGivesItsSlotBack(t *testing.T) {
	deadline(t, 60*time.Second)

	gate := newJobGate(1)
	held, ok := gate.acquire(t.Context(), time.Second)
	if !ok {
		t.Fatal("the first acquire on an empty gate was refused")
	}

	// The gate is full. A second caller with a short budget must give up
	// rather than wait forever, and must not have taken anything.
	start := time.Now()
	release, ok := gate.acquire(t.Context(), 200*time.Millisecond)
	waited := time.Since(start)
	if ok {
		release()
		t.Fatal("the gate admitted a second job although its only slot was taken")
	}
	if waited > 5*time.Second {
		t.Errorf("the refused caller waited %s despite a 200 ms budget", waited.Round(time.Millisecond))
	}
	if active, _ := gate.state(); active != 1 {
		t.Errorf("%d slots are held, but only one job ever started", active)
	}

	held()
	if active, _ := gate.state(); active != 0 {
		t.Errorf("%d slots are still held after the only job released", active)
	}
}

// TestTheGateNeverLosesAPermitUnderTheHandoverRace hammers the one genuinely
// racy moment in the gate: a waiter whose budget expires at the exact instant
// a releasing job hands it the slot.
//
// The independent review reasoned that the accounting in abandon is correct
// here. Reasoning is not proof for this class of defect, and the consequence
// if it is wrong is the worst one this slice can produce: a permit that
// silently leaves circulation, after which — at the default limit of one —
// every later video upload on the server waits forever for a job that ended
// long ago. It would not show up as an error, only as a plugin that stopped.
//
// The budgets are deliberately tiny and jittered so that timeouts and
// handovers collide constantly rather than occasionally. The invariant checked
// at the end is absolute: with every job finished, the gate must be empty and
// must still admit exactly its limit.
func TestTheGateNeverLosesAPermitUnderTheHandoverRace(t *testing.T) {
	deadline(t, 2*time.Minute)

	const limit = 3
	const workers = 60
	const rounds = 40

	gate := newJobGate(limit)

	var wg sync.WaitGroup
	var admitted atomic.Int64
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range rounds {
				// Budgets straddling the handover: short enough that many
				// expire while queued, long enough that many are still
				// waiting when a slot is handed over. That overlap is the
				// window under test, so it has to be hit often rather than
				// occasionally.
				budget := time.Duration(200+(w+r)%11*100) * time.Microsecond
				release, ok := gate.acquire(t.Context(), budget)
				if !ok {
					continue
				}
				admitted.Add(1)
				// Hold it for a moment so handovers actually happen.
				time.Sleep(time.Duration((w%3)+1) * 100 * time.Microsecond)
				release()
			}
		}()
	}
	wg.Wait()

	if admitted.Load() == 0 {
		t.Fatal("no job was ever admitted, so this test exercised nothing")
	}

	active, got := gate.state()
	if active != 0 {
		t.Fatalf("%d permit(s) leaked: every job has finished, but the gate still "+
			"believes that many are running. At the default limit this is a server "+
			"on which no further video is ever converted.", active)
	}
	if got != limit {
		t.Errorf("the limit drifted to %d, want %d", got, limit)
	}

	// Prove the gate is genuinely usable again, not merely reporting zero.
	var holders []func()
	for i := range limit {
		release, ok := gate.acquire(t.Context(), 2*time.Second)
		if !ok {
			t.Fatalf("slot %d of %d could not be taken after the stress run — "+
				"permits were lost", i+1, limit)
		}
		holders = append(holders, release)
	}
	if _, ok := gate.acquire(t.Context(), 50*time.Millisecond); ok {
		t.Error("the gate admitted more jobs than its limit after the stress run")
	}
	for _, release := range holders {
		release()
	}
	if active, _ := gate.state(); active != 0 {
		t.Errorf("%d permit(s) still held after everything was released", active)
	}
	t.Logf("%d admissions survived %d workers x %d rounds with no lost permit",
		admitted.Load(), workers, rounds)
}

// TestTheGateLeaksNoGoroutines — a waiter that gives up must take its
// bookkeeping with it. A per-upload goroutine that survives the upload is the
// kind of leak that only becomes visible weeks later, as a plugin process that
// grew without anyone touching it.
func TestTheGateLeaksNoGoroutines(t *testing.T) {
	deadline(t, 60*time.Second)

	gate := newJobGate(1)
	held, _ := gate.acquire(t.Context(), time.Second)

	settle := func() int {
		for i := 0; i < 50; i++ {
			runtime.GC()
			time.Sleep(20 * time.Millisecond)
			if n := runtime.NumGoroutine(); i > 5 {
				return n
			}
		}
		return runtime.NumGoroutine()
	}
	before := settle()

	// A hundred callers queue up and every one of them times out.
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if release, ok := gate.acquire(t.Context(), 50*time.Millisecond); ok {
				release()
			}
		}()
	}
	wg.Wait()
	held()

	after := settle()
	if after > before+10 {
		t.Errorf("goroutines went from %d to %d after 100 abandoned waits — "+
			"the gate leaves something behind on the give-up path", before, after)
	}
}

// ------------------------------------------- work that costs nothing waits for nothing

// TestFreeWorkDoesNotQueueBehindATranscode.
//
// PLAN §the concurrency limit requires this explicitly: a pass-through and an
// already-efficient video never start an encoder, so they consume no
// processor time and must not be made to wait for one. Making them queue
// would mean a single large transcode delays every unrelated upload on the
// server.
func TestFreeWorkDoesNotQueueBehindATranscode(t *testing.T) {
	deadline(t, 2*time.Minute)

	cfg := queueConfig(t, 1, 4)
	cfg.spoolDir = t.TempDir()

	// Occupy the only transcode slot for the whole test.
	held, ok := cfg.videoGate.acquire(t.Context(), time.Second)
	if !ok {
		t.Fatal("could not take the gate's only slot")
	}
	defer held()

	// An already-efficient video: decided from the header, no encoder.
	efficient := makeEfficientVideo(t, cfg.tools)

	cases := []struct {
		name  string
		input []byte
	}{
		{"a file that is not a video", bytes.Repeat([]byte("not a video at all\n"), 4096)},
		{"an audio-only MP4", makeAudioOnly(t, cfg.tools)},
		{"an already-efficient video", efficient},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deadline(t, 30*time.Second)

			var stored bytes.Buffer
			start := time.Now()
			outcome, err := processUpload(&stored, bytes.NewReader(tc.input), cfg)
			elapsed := time.Since(start)

			if err != nil {
				t.Fatalf("the upload failed while the transcode slot was busy: %v", err)
			}
			if outcome.reEncoded {
				t.Fatalf("%s was transcoded, so this case does not test what it claims", tc.name)
			}
			if !bytes.Equal(stored.Bytes(), tc.input) {
				t.Errorf("the stored bytes differ from what was sent (%d in, %d stored)",
					len(tc.input), stored.Len())
			}
			// Generous: the point is "did not wait for the slot", not a
			// performance measurement. A queued upload would wait for the
			// whole budget, which is tens of seconds.
			if elapsed > 10*time.Second {
				t.Errorf("the upload took %s although it never needed the encoder — "+
					"it was made to queue behind a transcode", elapsed.Round(time.Millisecond))
			}
		})
	}
}

// ------------------------------------------------------------ images

// TestImagesRunInParallelUpToTheirOwnLimit — SPEC §4.4 gives images a separate,
// higher limit because they finish in milliseconds. Serialising them with the
// videos would make the plugin needlessly slow for the common case.
func TestImagesRunInParallelUpToTheirOwnLimit(t *testing.T) {
	deadline(t, 2*time.Minute)

	const limit = 3
	const uploads = 12

	cfg := defaultConfiguration()
	cfg.MaxConcurrentImageJobs = limit
	cfg.imageGate = newJobGate(limit)
	// A wait long enough that the queue-full fallback cannot fire here.
	//
	// This is not padding. Under the race detector everything runs roughly
	// twenty times slower, and with the production budget several of these
	// twelve images legitimately gave up waiting and were stored
	// uncompressed — which is the DESIGNED behaviour and has its own test
	// below. Mixing the two into one test made this one fail for the right
	// reason at the wrong time. This test is about the observed peak; the
	// fallback is TestImagesNeverFailBecauseTheQueueIsBusy's job.
	cfg.imageQueueWait = 5 * time.Minute

	original := encodeJPEG(t, noisyImage(2000, 1500, 42), 95)

	sampler := sampleEvery(time.Millisecond, func() int {
		active, _ := cfg.imageGate.state()
		return active
	})

	var wg sync.WaitGroup
	start := make(chan struct{})
	compressed := make([]bool, uploads)
	for i := range uploads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var stored bytes.Buffer
			outcome, err := processUpload(&stored, bytes.NewReader(original), cfg)
			if err != nil {
				t.Errorf("image upload %d failed: %v", i, err)
				return
			}
			compressed[i] = outcome.reEncoded
		}()
	}
	close(start)
	wg.Wait()

	peak := sampler.highest()
	if peak > limit {
		t.Errorf("%d image compressions ran at once, but the limit is %d", peak, limit)
	}
	if peak < 2 {
		t.Errorf("the observed peak was %d — images are being serialised, "+
			"but SPEC §4.4 says they run in parallel", peak)
	}
	for i, ok := range compressed {
		if !ok {
			t.Errorf("image %d was not compressed", i)
		}
	}
	t.Logf("observed peak %d concurrent image compressions across %d uploads at limit %d",
		peak, uploads, limit)
}

// TestImagesNeverFailBecauseTheQueueIsBusy keeps the image compression promise alive:
// compression is an optimisation, so a busy server stores the original rather
// than refusing it.
func TestImagesNeverFailBecauseTheQueueIsBusy(t *testing.T) {
	deadline(t, 60*time.Second)

	cfg := defaultConfiguration()
	cfg.MaxConcurrentImageJobs = 1
	cfg.imageGate = newJobGate(1)
	// No budget at all: every image finds the queue full.
	cfg.imageQueueWait = time.Millisecond

	held, ok := cfg.imageGate.acquire(t.Context(), time.Second)
	if !ok {
		t.Fatal("could not take the image gate's only slot")
	}
	defer held()

	original := encodeJPEG(t, noisyImage(2000, 1500, 42), 95)
	var stored bytes.Buffer
	outcome, err := processUpload(&stored, bytes.NewReader(original), cfg)
	if err != nil {
		t.Fatalf("an image upload failed because the queue was busy: %v", err)
	}
	if outcome.reEncoded {
		t.Fatal("the image was compressed although no slot was available")
	}
	if !bytes.Equal(stored.Bytes(), original) {
		t.Error("the image stored while the queue was full is not the original")
	}
}

// --------------------------------------------------------- the time budget

// TestQueueWaitAndTranscodeTogetherStayInsideTheServersWriteWindow is the
// decision PLAN §the concurrency limit demands be made, justified and tested.
//
// The two budgets do not compose. video compression's transcode budget starts when
// ffmpeg starts; the server's write timeout started when the upload did. A job
// that waits in the queue and then transcodes can therefore blow the server's
// window while each half looks compliant on its own — and the sender loses an
// upload that the plugin believed it was handling correctly.
//
// The rule: a job is admitted only if the time still left in the server's
// window is enough for a FULL transcode plus the margin needed to deliver a
// rejection. Waiting longer than that is pointless, because a job admitted
// then could not finish in time anyway. That makes the sum an invariant rather
// than a hope.
func TestQueueWaitAndTranscodeTogetherStayInsideTheServersWriteWindow(t *testing.T) {
	deadline(t, 30*time.Second)

	cfg := defaultConfiguration()

	// Nothing has elapsed yet: the largest wait the plugin will ever grant.
	budget := cfg.videoQueueWait(0)
	total := budget + cfg.videoTimeout()
	window := serverWriteWindow

	if total >= window {
		t.Errorf("waiting %s and then transcoding for up to %s totals %s, which does not "+
			"fit the server's %s write window — the sender would lose the upload",
			budget, cfg.videoTimeout(), total, window)
	}
	if window-total < rejectionDeliveryMargin {
		t.Errorf("only %s are left of the server's window after queue wait plus transcode; "+
			"at least %s are needed to deliver the rejection to the sender",
			window-total, rejectionDeliveryMargin)
	}

	// Time already spent on the upload has to come out of the wait, not be
	// added on top of it.
	for _, spent := range []time.Duration{0, 5 * time.Second, 20 * time.Second} {
		got := cfg.videoQueueWait(spent)
		if got+spent+cfg.videoTimeout() >= window {
			t.Errorf("after %s already spent, a wait of %s still overruns the window", spent, got)
		}
	}

	// A budget that cannot be met is zero, never negative: an administrator
	// who raises the transcode budget past the server's window gets "do not
	// queue", not "wait forever".
	greedy := defaultConfiguration()
	greedy.VideoTimeoutSeconds = maxVideoTimeoutSeconds
	if got := greedy.videoQueueWait(0); got != 0 {
		t.Errorf("with a %d s transcode budget the queue wait is %s, want 0",
			greedy.VideoTimeoutSeconds, got)
	}
}

// TestAVideoThatCannotGetASlotInTimeIsStoredNotRefused pins the queue-full
// decision.
//
// The choice is deliberate and is recorded in PLAN §the concurrency limit. Refusing would
// fail an upload because the SERVER was busy — something the sender can
// neither understand nor act on — while SPEC §4.2's rejection is reserved for
// a video the plugin committed to converting and could not. Waiting without a
// bound is worse still: it runs into the server's write timeout and loses the
// upload anyway, after five minutes of the sender watching a progress bar.
// Storing the file as sent keeps the upload, keeps the file intact, and costs
// only the compression.
func TestAVideoThatCannotGetASlotInTimeIsStoredNotRefused(t *testing.T) {
	deadline(t, 90*time.Second)

	cfg := queueConfig(t, 1, 4)
	cfg.spoolDir = t.TempDir()

	held, ok := cfg.videoGate.acquire(t.Context(), time.Second)
	if !ok {
		t.Fatal("could not take the gate's only slot")
	}
	defer held()

	original := makeVideo(t, cfg.tools, 3, "640x480")

	var stored bytes.Buffer
	start := time.Now()
	outcome, err := processUpload(&stored, bytes.NewReader(original), cfg)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("a video was refused because the server was busy: %v", err)
	}
	if outcome.reEncoded {
		t.Fatal("the video was transcoded although no slot was available")
	}
	if !bytes.Equal(stored.Bytes(), original) {
		t.Errorf("the stored video is not the original: %d bytes in, %d stored",
			len(original), stored.Len())
	}
	// It must give up inside its budget rather than hold the request open.
	if elapsed > cfg.videoQueueWait(0)+15*time.Second {
		t.Errorf("the upload waited %s, which is past its queue budget of %s",
			elapsed.Round(time.Second), cfg.videoQueueWait(0))
	}
	entries, _ := os.ReadDir(cfg.spoolDir)
	if len(entries) != 0 {
		t.Errorf("the passed-through upload left %d temporary file(s) behind", len(entries))
	}
}

// ----------------------------------------------------------- configuration

// TestConcurrencyDefaultsMatchTheSpec — SPEC §4.4 states both numbers.
func TestConcurrencyDefaultsMatchTheSpec(t *testing.T) {
	def := defaultConfiguration()
	if def.MaxConcurrentVideoJobs != 1 {
		t.Errorf("concurrent video jobs = %d, SPEC §4.4 says 1", def.MaxConcurrentVideoJobs)
	}
	if def.MaxConcurrentImageJobs != 4 {
		t.Errorf("concurrent image jobs = %d, SPEC §4.4 says 4", def.MaxConcurrentImageJobs)
	}
}

// TestConcurrencySettingsFollowTheSliceThreeRules — the two new settings obey
// the same contract as every other one: a valid value is applied, an invalid
// value falls back to its own default and says so, and nothing else is
// disturbed.
func TestConcurrencySettingsFollowTheSliceThreeRules(t *testing.T) {
	t.Run("valid values are applied", func(t *testing.T) {
		cfg, warnings := sanitiseConfiguration(map[string]any{
			"MaxConcurrentVideoJobs": float64(3),
			"MaxConcurrentImageJobs": float64(8),
		})
		if len(warnings) != 0 {
			t.Errorf("valid values produced warnings: %v", warnings)
		}
		if cfg.MaxConcurrentVideoJobs != 3 || cfg.MaxConcurrentImageJobs != 8 {
			t.Errorf("values were not applied: video=%d image=%d",
				cfg.MaxConcurrentVideoJobs, cfg.MaxConcurrentImageJobs)
		}
	})

	t.Run("invalid values fall back and warn", func(t *testing.T) {
		cases := []struct {
			key   string
			value any
		}{
			{"MaxConcurrentVideoJobs", float64(0)},
			{"MaxConcurrentVideoJobs", float64(-1)},
			{"MaxConcurrentVideoJobs", float64(maxConcurrentJobsLimit + 1)},
			{"MaxConcurrentVideoJobs", "lots"},
			{"MaxConcurrentVideoJobs", float64(2.5)},
			{"MaxConcurrentImageJobs", float64(0)},
			{"MaxConcurrentImageJobs", float64(-4)},
			{"MaxConcurrentImageJobs", float64(maxConcurrentJobsLimit + 1)},
			{"MaxConcurrentImageJobs", true},
		}
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s=%v", tc.key, tc.value), func(t *testing.T) {
				cfg, warnings := sanitiseConfiguration(map[string]any{tc.key: tc.value})
				if cfg != defaultConfiguration() {
					t.Errorf("a rejected value changed the configuration: %+v", cfg)
				}
				if len(warnings) != 1 || warnings[0].Setting != tc.key {
					t.Errorf("the rejection was not reported for %s: %v", tc.key, warnings)
				}
			})
		}
	})

	t.Run("one bad value does not reset the other", func(t *testing.T) {
		cfg, warnings := sanitiseConfiguration(map[string]any{
			"MaxConcurrentVideoJobs": float64(-1),
			"MaxConcurrentImageJobs": float64(9),
		})
		if cfg.MaxConcurrentVideoJobs != defaultMaxConcurrentVideoJobs {
			t.Errorf("the rejected video value did not fall back: %d", cfg.MaxConcurrentVideoJobs)
		}
		if cfg.MaxConcurrentImageJobs != 9 {
			t.Errorf("a valid image value was lost because the video value was bad: %d",
				cfg.MaxConcurrentImageJobs)
		}
		if len(warnings) != 1 {
			t.Errorf("want exactly one warning, got %v", warnings)
		}
	})
}

// TestChangingTheLimitTakesEffectWithoutARestart — the configuration surface's promise, applied
// to a live gate. The gate is shared across configuration changes on purpose:
// building a new one would let jobs from the old gate and the new gate run at
// the same time and briefly exceed both limits.
func TestChangingTheLimitTakesEffectWithoutARestart(t *testing.T) {
	deadline(t, 60*time.Second)

	gate := newJobGate(1)
	first, ok := gate.acquire(t.Context(), time.Second)
	if !ok {
		t.Fatal("the first job was refused by an empty gate")
	}
	if _, ok := gate.acquire(t.Context(), 100*time.Millisecond); ok {
		t.Fatal("a second job got in although the limit is 1")
	}

	gate.resize(3)

	second, ok := gate.acquire(t.Context(), 2*time.Second)
	if !ok {
		t.Fatal("raising the limit did not let a waiting job in")
	}
	third, ok := gate.acquire(t.Context(), 2*time.Second)
	if !ok {
		t.Fatal("the third slot of a limit of 3 was refused")
	}
	if _, ok := gate.acquire(t.Context(), 100*time.Millisecond); ok {
		t.Fatal("a fourth job got in although the limit is 3")
	}

	// Lowering it must not evict running jobs — it applies to the next ones.
	gate.resize(1)
	if active, limit := gate.state(); active != 3 || limit != 1 {
		t.Errorf("after lowering the limit: active=%d limit=%d, want 3 and 1", active, limit)
	}
	first()
	second()
	if _, ok := gate.acquire(t.Context(), 100*time.Millisecond); ok {
		t.Fatal("a job got in while the one remaining job still exceeds the new limit of 1")
	}
	third()
	fourth, ok := gate.acquire(t.Context(), 2*time.Second)
	if !ok {
		t.Fatal("the gate never recovered after the limit was lowered")
	}
	fourth()
}

// TestTheLimitsReachTheGatesFromTheSystemConsole closes the loop: a saved
// setting has to change what the running plugin actually does, not merely what
// the configuration struct says.
func TestTheLimitsReachTheGatesFromTheSystemConsole(t *testing.T) {
	deadline(t, 60*time.Second)

	p := newTestPluginWithSettings(t, map[string]any{
		"MaxConcurrentVideoJobs": float64(2),
		"MaxConcurrentImageJobs": float64(5),
	})

	cfg := p.getConfiguration()
	if cfg.videoGate == nil || cfg.imageGate == nil {
		t.Fatal("the configuration carries no gates, so nothing is limited")
	}
	if _, limit := cfg.videoGate.state(); limit != 2 {
		t.Errorf("the video gate is at %d, but the console says 2", limit)
	}
	if _, limit := cfg.imageGate.state(); limit != 5 {
		t.Errorf("the image gate is at %d, but the console says 5", limit)
	}

	// The same plugin, reconfigured: the gate object must be the one already
	// in use, resized in place.
	before := cfg.videoGate
	p.reconfigure(t, map[string]any{"MaxConcurrentVideoJobs": float64(4)})
	after := p.getConfiguration()
	if after.videoGate != before {
		t.Error("a configuration change replaced the gate; jobs started under the old " +
			"one would run alongside jobs under the new one and exceed both limits")
	}
	if _, limit := after.videoGate.state(); limit != 4 {
		t.Errorf("the gate was not resized: limit=%d, want 4", limit)
	}
}

// ---------------------------------------------------------------- fixtures

// makeEfficientVideo renders a clip that is already encoded frugally, so the
// plugin decides from the header not to transcode it.
func makeEfficientVideo(t *testing.T, tools mediaTools) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "efficient.mp4")
	runFFmpeg(t, tools.FFmpegPath, "-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=40",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=40",
		"-c:v", "libx264", "-preset", "slow", "-crf", "34",
		"-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "64k",
		"-movflags", "+faststart", path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the efficient fixture failed: %v", err)
	}
	return data
}

// runFFmpeg runs a fixture-generating command and fails the test on error.
func runFFmpeg(t *testing.T, program string, args ...string) {
	t.Helper()
	cmd := exec.Command(program, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating a fixture failed: %v\n%s", err, out)
	}
}

// testPlugin is a plugin wired to a settings map, the way the server wires one.
type testPlugin struct {
	*MediaForgePlugin
	api      *plugintest.API
	settings map[string]any
}

// newTestPluginWithSettings builds a plugin whose System Console settings are
// the given map, and runs the configuration hook once.
func newTestPluginWithSettings(t *testing.T, settings map[string]any) *testPlugin {
	t.Helper()

	tp := &testPlugin{MediaForgePlugin: &MediaForgePlugin{}, settings: settings}
	api := &plugintest.API{}
	api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
		dest := args.Get(0).(*map[string]any)
		copied := make(map[string]any, len(tp.settings))
		for k, v := range tp.settings {
			copied[k] = v
		}
		*dest = copied
	}).Return(nil)
	api.On("LogWarn", mock.Anything, mock.Anything).Maybe().Return()
	api.On("LogError", mock.Anything, mock.Anything).Maybe().Return()
	api.On("LogInfo", mock.Anything, mock.Anything).Maybe().Return()
	api.On("LogDebug", mock.Anything, mock.Anything).Maybe().Return()
	tp.api = api
	tp.SetAPI(api)

	if err := tp.OnConfigurationChange(); err != nil {
		t.Fatalf("OnConfigurationChange returned %v", err)
	}
	return tp
}

// reconfigure saves new settings, exactly as an administrator would.
func (tp *testPlugin) reconfigure(t *testing.T, settings map[string]any) {
	t.Helper()
	tp.settings = settings
	if err := tp.OnConfigurationChange(); err != nil {
		t.Fatalf("OnConfigurationChange returned %v", err)
	}
}

// makeAudioOnly renders an .m4a, which carries a video container's header
// without carrying a video.
func makeAudioOnly(t *testing.T, tools mediaTools) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "voice.m4a")
	runFFmpeg(t, tools.FFmpegPath, "-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:a", "aac", "-b:a", "128k", path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the audio fixture failed: %v", err)
	}
	return data
}

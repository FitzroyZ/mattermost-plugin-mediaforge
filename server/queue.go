package main

import (
	"context"
	"sync"
	"time"
)

// The concurrency limits (SPEC §4.4).
const (
	// defaultMaxConcurrentVideoJobs is how many transcodes may run at once.
	//
	// One, and the number is measured rather than chosen. A single 1080p
	// transcode already occupies all four cores of the reference container
	// (PLAN §video compression), so a second one does not halve the total time — it
	// doubles both, and makes Mattermost itself sluggish while it runs.
	defaultMaxConcurrentVideoJobs = 1
	// defaultMaxConcurrentImageJobs is how many image compressions may run at
	// once. Images finish in milliseconds and are bounded only to keep a burst
	// of uploads from allocating several decoded bitmaps at the same moment.
	defaultMaxConcurrentImageJobs = 4
	// maxConcurrentJobsLimit is the largest value either setting accepts.
	// Above this the number is not a capacity decision any more; the machine
	// would be thrashing long before it was reached.
	maxConcurrentJobsLimit = 64
)

// serverWriteWindow is Mattermost's own ServiceSettings.WriteTimeout, the
// clock that starts when the upload starts.
//
// It is a constant here rather than a setting because the plugin cannot read
// the server's configuration through the hook API, and because the value is
// the server's default and the test instance's value alike. It is used only to
// make the plugin's own budgets conservative; a server configured with a LARGER
// window simply gets a plugin that gives up on queueing a little earlier than
// it had to, which costs a compression and never an upload.
const serverWriteWindow = 300 * time.Second

// rejectionDeliveryMargin is the time reserved at the end of the server's
// window for writing a rejection back to the sender.
//
// video compression pinned the same 30 s between the transcode budget and the window;
// this is that number, now shared by both rules so they cannot drift apart.
const rejectionDeliveryMargin = 30 * time.Second

// defaultImageQueueWait is how long an image may wait for a free slot.
//
// Image compression takes milliseconds, so a slot becomes free almost
// immediately and this budget is nearly never reached. It exists so that a
// pathological burst degrades into "stored uncompressed" rather than into
// uploads that sit still: an image must never fail, and must never be the
// reason a request runs out of time.
const defaultImageQueueWait = 10 * time.Second

// jobGate admits a bounded number of jobs at a time.
//
// Why not a plain buffered channel, which is the usual Go answer: the limit is
// an administrator-visible setting that has to take effect without a restart,
// and a channel's capacity is fixed at creation. Rebuilding the channel on
// every configuration change would be worse than the problem — jobs admitted
// by the old channel and jobs admitted by the new one would run side by side,
// briefly exceeding both limits, which is exactly the property this type
// exists to guarantee.
//
// Waiters are served strictly first-come-first-served. That is what makes
// "a waiting upload is not forgotten" true: there is no wake-up race in which
// a job that just arrived takes the slot a queued job has been waiting for,
// and no possibility of a job waiting behind an unbounded stream of newcomers.
type jobGate struct {
	mu      sync.Mutex
	limit   int
	active  int
	waiters []chan struct{}
}

// newJobGate creates a gate admitting limit jobs at a time.
func newJobGate(limit int) *jobGate {
	if limit < 1 {
		limit = 1
	}
	return &jobGate{limit: limit}
}

// acquire takes a slot, waiting at most budget for one.
//
// It returns a release function and true when a slot was taken, or nil and
// false when it gave up. The release function is idempotent and MUST be called
// exactly on the path that received true — every caller does so with defer, so
// a panic in the job still returns the slot.
//
// Giving up rather than waiting forever is a deliberate part of the design.
// The caller is inside a request the server is timing, so an unbounded wait
// does not produce a slow upload, it produces a lost one — after the sender
// has watched a progress bar for five minutes.
func (g *jobGate) acquire(ctx context.Context, budget time.Duration) (func(), bool) {
	g.mu.Lock()
	if g.active < g.limit && len(g.waiters) == 0 {
		g.active++
		g.mu.Unlock()
		return g.releaser(), true
	}

	if budget <= 0 {
		g.mu.Unlock()
		return nil, false
	}

	ch := make(chan struct{})
	g.waiters = append(g.waiters, ch)
	g.mu.Unlock()

	timer := time.NewTimer(budget)
	defer timer.Stop()

	select {
	case <-ch:
		// A releasing job handed its slot over directly; active was already
		// incremented on our behalf, so there is no window in which the slot
		// is free and another arrival could take it.
		return g.releaser(), true

	case <-timer.C:
		return nil, g.abandon(ch)

	case <-ctx.Done():
		return nil, g.abandon(ch)
	}
}

// abandon removes a waiter that gave up.
//
// The return value handles the one genuinely racy moment in the type: the slot
// may have been handed over in the instant between the timer firing and the
// lock being taken. In that case the waiter is no longer in the queue and it
// now holds a slot it never asked for — so the slot is passed straight on to
// the next waiter instead of being lost. Forgetting it here would take one
// slot out of circulation permanently, which on the default limit of one means
// every later video upload waits for a job that ended long ago.
func (g *jobGate) abandon(ch chan struct{}) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	for i, w := range g.waiters {
		if w == ch {
			g.waiters = append(g.waiters[:i], g.waiters[i+1:]...)
			return false
		}
	}

	// Not in the queue any more: the slot was granted. Give it back.
	g.active--
	g.dispatch()
	return false
}

// releaser returns a function that gives one slot back, at most once.
func (g *jobGate) releaser() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.active--
			g.dispatch()
		})
	}
}

// dispatch hands free slots to waiting jobs. The lock must be held.
//
// The limit is re-read on every pass, so lowering it while jobs are running
// simply stops new ones from starting. Evicting a running transcode instead
// would throw away work the sender is waiting for, and the job that was
// killed would have been the one furthest along.
func (g *jobGate) dispatch() {
	for g.active < g.limit && len(g.waiters) > 0 {
		next := g.waiters[0]
		g.waiters = g.waiters[1:]
		g.active++
		close(next)
	}
}

// resize changes the limit. Running jobs are never interrupted; the new limit
// governs what starts from now on.
func (g *jobGate) resize(limit int) {
	if limit < 1 {
		limit = 1
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limit = limit
	g.dispatch()
}

// state reports the number of jobs running and the current limit.
func (g *jobGate) state() (active, limit int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active, g.limit
}

// videoQueueWait is how long an upload may wait for a transcode slot, given
// the time already spent on it.
//
// This is the answer to the problem PLAN §the concurrency limit names as the hard part of
// the slice: the two budgets do not compose. video compression's transcode budget starts
// when ffmpeg starts, while the server's write timeout started when the upload
// did, so queue wait plus transcode can exceed the server's window while each
// half looks perfectly compliant on its own. The sender then loses the upload
// at the very end, after the longest possible wait.
//
// The rule that makes the sum an invariant instead of a hope: only wait for as
// long as still leaves room for a FULL transcode plus the margin needed to
// deliver a rejection. A job admitted later than that could not finish inside
// the server's window anyway, so waiting longer cannot produce a successful
// upload — it can only produce a slower failure.
//
// With the defaults that is 300 − 30 − 240 = 30 seconds. A second video
// arriving while a long transcode is running is therefore stored as sent
// rather than queued behind it, which is the honest outcome: the alternative
// is an upload that waits four minutes and is then refused.
func (c configuration) videoQueueWait(alreadySpent time.Duration) time.Duration {
	wait := serverWriteWindow - rejectionDeliveryMargin - c.videoTimeout() - alreadySpent
	if wait < 0 {
		return 0
	}
	return wait
}

// imageQueueWaitBudget is how long an image may wait for a slot.
func (c configuration) imageQueueWaitBudget() time.Duration {
	if c.imageQueueWait > 0 {
		return c.imageQueueWait
	}
	return defaultImageQueueWait
}

// acquireVideoSlot takes a transcode slot, or reports that the queue is full.
//
// A nil gate means "no limit", which is what the unit tests that predate this
// slice use and what keeps them honest: they exercise the pipeline, not the
// queue. Production always has a gate, attached by OnConfigurationChange.
func acquireVideoSlot(cfg configuration, alreadySpent time.Duration) (func(), bool) {
	if cfg.videoGate == nil {
		return func() {}, true
	}
	return cfg.videoGate.acquire(context.Background(), cfg.videoQueueWait(alreadySpent))
}

// acquireImageSlot takes an image-compression slot, or reports that the queue
// is full.
func acquireImageSlot(cfg configuration) (func(), bool) {
	if cfg.imageGate == nil {
		return func() {}, true
	}
	return cfg.imageGate.acquire(context.Background(), cfg.imageQueueWaitBudget())
}

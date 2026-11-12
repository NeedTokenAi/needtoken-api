package cloudflareprofile

import (
	"context"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

// Status is the readiness document fetched by the Durable Object. It has no
// secret fields. user_requests is the number of user requests admitted by
// the gate. Zero does not mean the supervisor stopped.
type Status struct {
	Ready             bool  `json:"ready"`
	BackgroundRunning bool  `json:"background_running"`
	FenceHeld         bool  `json:"fence_held"`
	WriterEpoch       int64 `json:"writer_epoch"`
	SupervisorBeats   int64 `json:"supervisor_beats"`
	UserRequests      int64 `json:"user_requests"`
	Accepting         bool  `json:"accepting"`
	Draining          bool  `json:"draining"`
}

// Runtime is the in-process writer supervisor. Background beats run whether
// or not any user request has arrived. Losing the fence stops this process
// from remaining the writer; it does not treat an idle HTTP listener as a
// drained writer.
type Runtime struct {
	lease      writerLease
	holder     string
	ttl        time.Duration
	renewEvery time.Duration

	mu           sync.Mutex
	renewMu      sync.Mutex
	epoch        int64
	beats        int64
	userRequests int64
	inflight     int
	writes       bool
	accepting    bool
	draining     bool
	stopped      bool
	idleWait     chan struct{}

	onLost   func()
	lostOnce sync.Once
	stop     chan struct{}
	stopOnce sync.Once
}

// Start acquires the Redis writer fence. A nil client fails closed. This
// does not fall back to an in-memory store.
func Start(ctx context.Context, client *redis.Client) (*Runtime, error) {
	if client == nil {
		return nil, errRedisMissing()
	}
	holder, err := newHolder()
	if err != nil {
		return nil, err
	}
	rt := newRuntime(newRedisLease(client), holder, writerLeaseTTL)
	if err := rt.Acquire(ctx); err != nil {
		return nil, err
	}
	return rt, nil
}

func errRedisMissing() error {
	return errString(errRedisClient)
}

type errString string

func (e errString) Error() string { return string(e) }

func newRuntime(lease writerLease, holder string, ttl time.Duration) *Runtime {
	every := ttl / 3
	if every <= 0 {
		every = time.Second
	}
	return &Runtime{
		lease:      lease,
		holder:     holder,
		ttl:        ttl,
		renewEvery: every,
		stop:       make(chan struct{}),
	}
}

// SetOnLost registers a non-blocking notification. The callback runs without
// the runtime locks and must not call back into StopAndRelease.
func (rt *Runtime) SetOnLost(fn func()) {
	rt.mu.Lock()
	rt.onLost = fn
	rt.mu.Unlock()
}

// Acquire becomes the writer or returns ErrWriterHeld.
func (rt *Runtime) Acquire(ctx context.Context) error {
	epoch, err := rt.lease.Acquire(ctx, rt.holder, rt.ttl)
	if err != nil {
		return err
	}
	rt.mu.Lock()
	rt.epoch = epoch
	rt.writes = true
	rt.accepting = true
	rt.mu.Unlock()
	return nil
}

// Beat renews the fence and counts one supervisor tick. It does not read
// the user-request counter. A lost fence fires the lost callback once.
func (rt *Runtime) Beat(ctx context.Context) error {
	rt.renewMu.Lock()
	defer rt.renewMu.Unlock()

	rt.mu.Lock()
	stopped := rt.stopped
	holder, epoch, ttl := rt.holder, rt.epoch, rt.ttl
	rt.mu.Unlock()
	if stopped || epoch == 0 {
		return ErrWriterLost
	}
	if err := rt.lease.Renew(ctx, holder, epoch, ttl); err != nil {
		rt.mu.Lock()
		rt.writes = false
		rt.mu.Unlock()
		rt.fireLost()
		return err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.stopped {
		return ErrWriterLost
	}
	rt.beats++
	rt.writes = true
	return nil
}

// Loop keeps beating until the context ends or the runtime is stopped.
// No user traffic is required.
func (rt *Runtime) Loop(ctx context.Context) {
	ticker := time.NewTicker(rt.renewEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-rt.stop:
			return
		case <-ticker.C:
			_ = rt.Beat(ctx)
		}
	}
}

// BeginDrain rejects new user traffic and keeps the fence. Idle HTTP is not
// a drain. Background beats continue until StopAndRelease or StopRenewing.
func (rt *Runtime) BeginDrain() {
	rt.mu.Lock()
	rt.draining = true
	rt.accepting = false
	rt.mu.Unlock()
}

// WaitIdle blocks until admitted user requests finish or ctx ends.
func (rt *Runtime) WaitIdle(ctx context.Context) error {
	rt.mu.Lock()
	if rt.inflight == 0 {
		rt.mu.Unlock()
		return nil
	}
	if rt.idleWait == nil {
		rt.idleWait = make(chan struct{})
	}
	wait := rt.idleWait
	rt.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wait:
		return nil
	}
}

// StopAndRelease stops supervisor renewal and deletes the lease only if this
// holder still owns it.
func (rt *Runtime) StopAndRelease(ctx context.Context) error {
	rt.haltRenewal()
	rt.renewMu.Lock()
	defer rt.renewMu.Unlock()
	rt.mu.Lock()
	holder, epoch := rt.holder, rt.epoch
	rt.writes = false
	rt.mu.Unlock()
	if epoch == 0 {
		return nil
	}
	return rt.lease.Release(ctx, holder, epoch)
}

// StopRenewing stops beats without deleting the lease. Call it when drain
// times out so another process cannot become the writer while this one may
// still be exiting.
func (rt *Runtime) StopRenewing() {
	rt.haltRenewal()
	rt.mu.Lock()
	rt.writes = false
	rt.mu.Unlock()
}

func (rt *Runtime) haltRenewal() {
	rt.stopOnce.Do(func() {
		close(rt.stop)
	})
	rt.mu.Lock()
	rt.stopped = true
	rt.mu.Unlock()
}

// Status snapshots the supervisor. FenceHeld follows the local writer bit,
// which Beat clears when renewal loses.
func (rt *Runtime) Status() Status {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.statusLocked()
}

func (rt *Runtime) statusLocked() Status {
	background := rt.writes && rt.beats > 0 && !rt.stopped
	return Status{
		Ready:             background && rt.accepting && !rt.draining,
		BackgroundRunning: background,
		FenceHeld:         rt.writes && rt.epoch > 0,
		WriterEpoch:       rt.epoch,
		SupervisorBeats:   rt.beats,
		UserRequests:      rt.userRequests,
		Accepting:         rt.accepting,
		Draining:          rt.draining,
	}
}

// InFlight is the number of user requests inside the gate.
func (rt *Runtime) InFlight() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.inflight
}

func (rt *Runtime) tryEnter() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.writes || rt.beats == 0 || !rt.accepting || rt.draining || rt.stopped {
		return false
	}
	rt.inflight++
	rt.userRequests++
	return true
}

func (rt *Runtime) leave() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.inflight > 0 {
		rt.inflight--
	}
	if rt.inflight == 0 && rt.idleWait != nil {
		close(rt.idleWait)
		rt.idleWait = nil
	}
}

func (rt *Runtime) fireLost() {
	rt.mu.Lock()
	fn := rt.onLost
	rt.mu.Unlock()
	if fn == nil {
		return
	}
	rt.lostOnce.Do(fn)
}

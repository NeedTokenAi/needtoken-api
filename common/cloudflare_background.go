package common

import "sync"

// cloudflareBackground stops profile-gated loops. It is a no-op when
// CLOUDFLARE_PROFILE is not exactly "1", so the existing process is unchanged.
// Stopping the loops is not a cross-cloud fence: Cloud Run never reads it.
type cloudflareBackground struct {
	mu       sync.Mutex
	stopped  bool
	inflight int
	stop     chan struct{}
	idle     chan struct{}
}

var cfBackground = newCloudflareBackground()

func newCloudflareBackground() cloudflareBackground {
	return cloudflareBackground{
		stop: make(chan struct{}),
		idle: make(chan struct{}),
	}
}

// ResetCloudflareBackgroundForTest restores the gate. Production startup does
// not call it.
func ResetCloudflareBackgroundForTest() {
	cfBackground.mu.Lock()
	cfBackground.stopped = false
	cfBackground.inflight = 0
	cfBackground.stop = make(chan struct{})
	cfBackground.idle = make(chan struct{})
	cfBackground.mu.Unlock()
}

// CloudflareBackgroundStop is closed when the profile drops the writer permit.
// It is nil when the profile is off, so a select on it does not fire.
func CloudflareBackgroundStop() <-chan struct{} {
	if !CloudflareProfileRequested() {
		return nil
	}
	cfBackground.mu.Lock()
	defer cfBackground.mu.Unlock()
	return cfBackground.stop
}

// CloudflareBackgroundWritesAllowed is true for the normal process. Under the
// profile it becomes false before the Redis lease is released.
func CloudflareBackgroundWritesAllowed() bool {
	if !CloudflareProfileRequested() {
		return true
	}
	cfBackground.mu.Lock()
	defer cfBackground.mu.Unlock()
	return !cfBackground.stopped
}

// RunCloudflareBackground runs one iteration. After the profile stops writers,
// it returns false and does not call work. The normal process always runs work.
func RunCloudflareBackground(work func()) bool {
	if !CloudflareProfileRequested() {
		work()
		return true
	}
	cfBackground.mu.Lock()
	if cfBackground.stopped {
		cfBackground.mu.Unlock()
		return false
	}
	cfBackground.inflight++
	cfBackground.mu.Unlock()

	defer func() {
		cfBackground.mu.Lock()
		if cfBackground.inflight > 0 {
			cfBackground.inflight--
		}
		if cfBackground.stopped && cfBackground.inflight == 0 {
			select {
			case <-cfBackground.idle:
			default:
				close(cfBackground.idle)
			}
		}
		cfBackground.mu.Unlock()
	}()
	work()
	return true
}

// StopCloudflareBackgroundWrites rejects new iterations and waits for the
// iteration that is already inside RunCloudflareBackground. Call it before
// flushing and before releasing the lease. It does not stop Cloud Run.
func StopCloudflareBackgroundWrites() {
	if !CloudflareProfileRequested() {
		return
	}
	cfBackground.mu.Lock()
	if !cfBackground.stopped {
		cfBackground.stopped = true
		close(cfBackground.stop)
	}
	if cfBackground.inflight == 0 {
		cfBackground.mu.Unlock()
		return
	}
	idle := cfBackground.idle
	cfBackground.mu.Unlock()
	<-idle
}

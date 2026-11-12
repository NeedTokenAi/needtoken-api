package cloudflareprofile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

const (
	// writerLeaseTTL is deliberately short. A process that cannot renew is
	// no longer the writer. This is not a claim that the container stays up
	// for any particular wall time.
	writerLeaseTTL = 30 * time.Second

	errRedisClient = "cloudflare profile refuses to start without Redis"
)

var (
	// ErrWriterHeld means another live holder owns the fence.
	ErrWriterHeld = errors.New("cloudflare profile writer fence is held")
	// ErrWriterLost means this process is no longer the holder.
	ErrWriterLost = errors.New("cloudflare profile writer fence was lost")
)

// writerLease is the single-writer fence. The production implementation is
// Redis. Tests supply a fake. There is no production memory substitute.
type writerLease interface {
	Acquire(ctx context.Context, holder string, ttl time.Duration) (int64, error)
	Renew(ctx context.Context, holder string, epoch int64, ttl time.Duration) error
	Release(ctx context.Context, holder string, epoch int64) error
}

func newHolder() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

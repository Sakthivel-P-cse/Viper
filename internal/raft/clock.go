package raft

import (
	"sync"
	"time"
)

// Clock supplies time to the deterministic Raft core.
type Clock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// ManualClock is a deterministic clock useful for local runtimes and tests.
// It never advances unless Set or Advance is called.
type ManualClock struct {
	mu  sync.RWMutex
	now time.Time
}

// NewManualClock creates a clock at start. A zero start uses the Unix epoch.
func NewManualClock(start time.Time) *ManualClock {
	return &ManualClock{now: start}
}

// Now returns the current manually controlled time.
func (c *ManualClock) Now() time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

// Set moves the clock to an explicit instant.
func (c *ManualClock) Set(now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

// Advance moves the clock by duration. Negative movement is rejected.
func (c *ManualClock) Advance(duration time.Duration) bool {
	if c == nil || duration < 0 {
		return false
	}
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
	return true
}

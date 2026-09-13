package upstream

import (
	"sync"
	"time"
)

// Status is the derived circuit-breaker state for one upstream.
type Status int

const (
	StatusClosed Status = iota
	StatusHalfOpen
	StatusOpen
)

// CircuitState tracks consecutive-failure-driven circuit breaking for one
// upstream. Status is derived on read, not stored, matching the reference
// design: Closed if never opened or failures reset to zero, HalfOpen once
// the cooldown has elapsed (allowing exactly one probe through), Open
// otherwise.
type CircuitState struct {
	mu               sync.Mutex
	failures         int
	lastFailure      time.Time
	openUntil        time.Time
	halfOpenInFlight bool
}

func (c *CircuitState) Status(now time.Time) Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.openUntil.IsZero() || c.failures == 0 {
		return StatusClosed
	}
	if !now.Before(c.openUntil) {
		return StatusHalfOpen
	}
	return StatusOpen
}

// TryAcquireHalfOpenProbe returns true exactly once while the circuit is
// half-open, gating concurrent requests to a single in-flight probe. The
// caller MUST call ReleaseHalfOpenProbe when the probe's outcome (success or
// failure) is recorded, even on error — otherwise the upstream stays stuck
// half-open-blocked until that happens.
func (c *CircuitState) TryAcquireHalfOpenProbe(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.openUntil.IsZero() || c.failures == 0 || now.Before(c.openUntil) {
		return false
	}
	if c.halfOpenInFlight {
		return false
	}
	c.halfOpenInFlight = true
	return true
}

func (c *CircuitState) ReleaseHalfOpenProbe() {
	c.mu.Lock()
	c.halfOpenInFlight = false
	c.mu.Unlock()
}

// RecordSuccess resets the breaker to fully closed.
func (c *CircuitState) RecordSuccess() {
	c.mu.Lock()
	c.failures = 0
	c.openUntil = time.Time{}
	c.halfOpenInFlight = false
	c.mu.Unlock()
}

// RecordFailure increments the failure count and opens the circuit once the
// threshold is reached.
func (c *CircuitState) RecordFailure(threshold int, cooldown time.Duration, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures++
	c.lastFailure = now
	if c.failures >= threshold {
		c.openUntil = now.Add(cooldown)
	}
}

// HealthState is the independent signal produced by background health
// probes (see health.go), combined with CircuitState only at
// IsAvailable-check time.
type HealthState struct {
	mu                  sync.Mutex
	healthy             bool
	lastCheck           time.Time
	consecutiveFailures int
}

func NewHealthState() *HealthState {
	return &HealthState{healthy: true} // optimistic until the first check runs
}

func (h *HealthState) IsHealthy() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.healthy
}

// RecordCheck applies the "unhealthy only after 3 consecutive failed
// cycles, healthy immediately on first success" rule.
func (h *HealthState) RecordCheck(ok bool, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastCheck = now
	if ok {
		h.healthy = true
		h.consecutiveFailures = 0
		return
	}
	h.consecutiveFailures++
	if h.consecutiveFailures >= 3 {
		h.healthy = false
	}
}

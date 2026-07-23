// Package circuitbreaker implements a state-machine circuit breaker for
// backend instance health tracking. It is designed for single-threaded use
// within the scheduler event-loop; all fields are plain (non-atomic).
//
// State transitions:
//
//	Closed   --[consecutiveFailures >= FailThreshold]--> Open
//	Open     --[OpenDuration elapsed]-------------------> HalfOpen
//	HalfOpen --[consecutiveSuccesses >= SuccessThreshold]--> Closed
//	HalfOpen --[any failure]------------------------------> Open
package circuitbreaker

import "time"

// State represents the circuit breaker's current phase.
type State int8

const (
	// Closed allows all requests through. Failures are counted.
	Closed State = 0
	// Open blocks all requests. After OpenDuration the breaker transitions
	// to HalfOpen on the next CanExecute call.
	Open State = 1
	// HalfOpen allows requests through as probes. Successes move toward
	// Closed; any failure immediately reopens the circuit.
	HalfOpen State = 2
)

// String returns the human-readable name of the state.
func (s State) String() string {
	switch s {
	case Closed:
		return "Closed"
	case Open:
		return "Open"
	case HalfOpen:
		return "HalfOpen"
	default:
		return "Unknown"
	}
}

// Config holds the tunable parameters for a Breaker.
type Config struct {
	// FailThreshold is the number of consecutive failures in Closed state
	// required to trip the breaker to Open. Default: 3.
	FailThreshold int

	// SuccessThreshold is the number of consecutive successes in HalfOpen
	// state required to close the breaker. Default: 2.
	SuccessThreshold int

	// OpenDuration is how long the breaker stays Open before transitioning
	// to HalfOpen on the next CanExecute call. Default: 30s.
	OpenDuration time.Duration
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		FailThreshold:    3,
		SuccessThreshold: 2,
		OpenDuration:     30 * time.Second,
	}
}

// Breaker is a circuit breaker state machine.
//
// NOT safe for concurrent use. All methods must be called from the scheduler
// event-loop goroutine only.
type Breaker struct {
	state                State
	consecutiveFailures  int
	consecutiveSuccesses int
	openedAt             time.Time
	cfg                  Config
}

// New creates a Breaker in the Closed state with the given configuration.
func New(cfg Config) *Breaker {
	return &Breaker{
		state: Closed,
		cfg:   cfg,
	}
}

// CanExecute checks whether a request is allowed to proceed through the
// circuit breaker at the given wall-clock time.
//
//   - Closed: always returns true.
//   - Open: if cfg.OpenDuration has elapsed since the breaker opened,
//     transitions to HalfOpen and returns true; otherwise returns false.
//   - HalfOpen: always returns true (allows probe requests).
func (b *Breaker) CanExecute(now time.Time) bool {
	switch b.state {
	case Closed:
		return true
	case Open:
		if now.Sub(b.openedAt) >= b.cfg.OpenDuration {
			b.state = HalfOpen
			b.consecutiveSuccesses = 0
			b.consecutiveFailures = 0
			return true
		}
		return false
	case HalfOpen:
		return true
	default:
		return false
	}
}

// RecordSuccess records a successful request outcome and returns the
// (possibly new) state and whether a state transition occurred.
//
//   - Closed: resets consecutiveFailures to zero. No state change.
//   - HalfOpen: increments consecutiveSuccesses. If the success threshold
//     is met, transitions to Closed.
//   - Open: no-op (callers should not record outcomes while Open).
func (b *Breaker) RecordSuccess() (State, bool) {
	switch b.state {
	case Closed:
		b.consecutiveFailures = 0
		return Closed, false
	case HalfOpen:
		b.consecutiveSuccesses++
		if b.consecutiveSuccesses >= b.cfg.SuccessThreshold {
			b.state = Closed
			b.consecutiveFailures = 0
			b.consecutiveSuccesses = 0
			return Closed, true
		}
		return HalfOpen, false
	default:
		return b.state, false
	}
}

// RecordFailure records a failed request outcome at the given wall-clock time
// and returns the (possibly new) state and whether a state transition occurred.
//
//   - Closed: increments consecutiveFailures. If the fail threshold is met,
//     transitions to Open.
//   - HalfOpen: immediately transitions back to Open.
//   - Open: no-op.
func (b *Breaker) RecordFailure(now time.Time) (State, bool) {
	switch b.state {
	case Closed:
		b.consecutiveFailures++
		if b.consecutiveFailures >= b.cfg.FailThreshold {
			b.state = Open
			b.openedAt = now
			b.consecutiveSuccesses = 0
			return Open, true
		}
		return Closed, false
	case HalfOpen:
		b.state = Open
		b.openedAt = now
		b.consecutiveFailures = 0
		b.consecutiveSuccesses = 0
		return Open, true
	default:
		return b.state, false
	}
}

// State returns the current circuit breaker state.
func (b *Breaker) State() State {
	return b.state
}

// Reset returns the breaker to the Closed state with all counters zeroed.
func (b *Breaker) Reset() {
	b.state = Closed
	b.consecutiveFailures = 0
	b.consecutiveSuccesses = 0
	b.openedAt = time.Time{}
}

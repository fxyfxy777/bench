package circuitbreaker

import (
	"time"

	"github.com/yzx/rl-router/internal/domain"
)

// Manager holds per-instance circuit breakers.
// Accessed exclusively from the scheduler event-loop (no mutex needed).
type Manager struct {
	breakers map[string]*Breaker
	cfg      Config
}

// NewManager creates a Manager with the given breaker configuration.
func NewManager(cfg Config) *Manager {
	return &Manager{
		breakers: make(map[string]*Breaker),
		cfg:      cfg,
	}
}

// RecordOutcome records a request outcome for the given instance.
// errorCode == "" means success; any non-empty errorCode means failure.
// Lazily creates Breaker on first failure (healthy instances don't allocate).
// Returns (oldState, newState, changed).
func (m *Manager) RecordOutcome(instanceID, errorCode string, now time.Time) (State, State, bool) {
	if errorCode == "" {
		return m.recordSuccess(instanceID)
	}
	return m.recordFailure(instanceID, now)
}

// recordSuccess handles a successful request for the given instance.
func (m *Manager) recordSuccess(instanceID string) (State, State, bool) {
	b, ok := m.breakers[instanceID]
	if !ok {
		// No breaker exists — instance has been healthy. No-op.
		return Closed, Closed, false
	}

	oldState := b.State()
	newState, changed := b.RecordSuccess()

	// GC cleanup: if breaker is back to Closed, remove it entirely so healthy
	// instances don't keep stale entries. RecordSuccess in Closed state zeroes
	// consecutiveFailures, so any Closed breaker after success is fully clean.
	if newState == Closed {
		delete(m.breakers, instanceID)
	}

	return oldState, newState, changed
}

// recordFailure handles a failed request for the given instance.
func (m *Manager) recordFailure(instanceID string, now time.Time) (State, State, bool) {
	b, ok := m.breakers[instanceID]
	if !ok {
		// Lazy creation on first failure.
		b = New(m.cfg)
		m.breakers[instanceID] = b
	}

	oldState := b.State()
	newState, changed := b.RecordFailure(now)
	return oldState, newState, changed
}

// SyncToNodeState updates NodeState.CircuitOpen for all tracked instances.
// Also handles time-based Open->HalfOpen transitions via CanExecute(now).
// Called from the event-loop heartbeat (e.g. every 30s).
func (m *Manager) SyncToNodeState(nodes map[string]*domain.NodeState, now time.Time) {
	for instanceID, b := range m.breakers {
		node, ok := nodes[instanceID]
		if !ok {
			// Instance no longer registered — clean up stale breaker.
			delete(m.breakers, instanceID)
			continue
		}

		// CanExecute triggers Open->HalfOpen if OpenDuration has elapsed.
		_ = b.CanExecute(now)

		// CircuitOpen should be true only when the breaker is in Open state.
		node.StoreCircuitOpen(b.State() == Open)
	}
}

// Reset clears all breakers (called on step transition).
func (m *Manager) Reset() {
	clear(m.breakers)
}

// ResetByInstances clears breaker state only for the provided instances.
// Called when one resource group starts a new step so unrelated groups keep
// their circuit-breaker history.
func (m *Manager) ResetByInstances(instanceIDs []string) {
	for _, id := range instanceIDs {
		delete(m.breakers, id)
	}
}

// Remove deletes breaker state for a deregistered instance.
func (m *Manager) Remove(instanceID string) {
	delete(m.breakers, instanceID)
}

// States returns a snapshot of all breaker states (for /v1/status API).
func (m *Manager) States() map[string]State {
	if len(m.breakers) == 0 {
		return nil
	}
	snap := make(map[string]State, len(m.breakers))
	for id, b := range m.breakers {
		snap[id] = b.State()
	}
	return snap
}

// Package session manages Claude Code session lifecycle.
package session

import (
	"context"
	"fmt"

	"github.com/jaekwon-park/tgcc/internal/store"
)

// Status represents a session state as defined in docs/02_ARCHITECTURE.md §4.
type Status string

const (
	StatusPending  Status = "pending"
	StatusSpawning Status = "spawning"
	StatusActive   Status = "active"
	StatusIdle     Status = "idle"
	StatusCrashed  Status = "crashed"
	StatusResuming Status = "resuming"
	StatusStopping Status = "stopping"
	StatusStopped  Status = "stopped"
	StatusFailed   Status = "failed"
)

// SideEffect is an optional function executed when a state transition occurs.
// It receives the session record and the transition that triggered it.
type SideEffect func(ctx context.Context, session *store.Session) error

// StateMachine validates and executes session state transitions.
// It encodes the state diagram from docs/02_ARCHITECTURE.md §4.1.
type StateMachine struct {
	transitions map[Status]map[Status]bool
	effects     map[string]SideEffect // key: "from->to"
}

// NewStateMachine creates a new StateMachine with all allowed transitions.
func NewStateMachine() *StateMachine {
	sm := &StateMachine{
		transitions: make(map[Status]map[Status]bool),
		effects:     make(map[string]SideEffect),
	}

	// Define all allowed transitions (docs/02_ARCHITECTURE.md §4 state diagram).
	sm.allow(StatusPending, StatusSpawning)
	sm.allow(StatusPending, StatusStopping)

	sm.allow(StatusSpawning, StatusActive)
	sm.allow(StatusSpawning, StatusFailed)

	sm.allow(StatusActive, StatusIdle)
	sm.allow(StatusActive, StatusCrashed)
	sm.allow(StatusActive, StatusStopping)

	sm.allow(StatusIdle, StatusActive)
	sm.allow(StatusIdle, StatusCrashed)
	sm.allow(StatusIdle, StatusStopping)

	sm.allow(StatusCrashed, StatusResuming)
	sm.allow(StatusCrashed, StatusStopping)

	sm.allow(StatusResuming, StatusActive)
	sm.allow(StatusResuming, StatusFailed)

	sm.allow(StatusStopping, StatusStopped)
	sm.allow(StatusStopping, StatusFailed)

	// Terminal states: Stopped, Failed (no outgoing transitions).
	// Final states are reached only via: Stopping→Stopped, Stopping→Failed, Spawning→Failed, Resuming→Failed

	return sm
}

// allow registers a permitted transition.
func (sm *StateMachine) allow(from, to Status) {
	if sm.transitions[from] == nil {
		sm.transitions[from] = make(map[Status]bool)
	}
	sm.transitions[from][to] = true
}

// RegisterEffect attaches a side effect to a specific transition.
// Only one effect per transition; calling again overwrites.
func (sm *StateMachine) RegisterEffect(from, to Status, effect SideEffect) {
	key := sm.effectKey(from, to)
	sm.effects[key] = effect
}

// CanTransition checks whether a transition is allowed.
func (sm *StateMachine) CanTransition(from, to Status) bool {
	if tos, ok := sm.transitions[from]; ok {
		return tos[to]
	}
	return false
}

// Transition validates the transition and executes the registered side effect (if any).
// Returns an error if the transition is not allowed.
func (sm *StateMachine) Transition(ctx context.Context, session *store.Session, to Status) error {
	current := Status(session.Status)
	if !sm.CanTransition(current, to) {
		return fmt.Errorf("invalid state transition: %s → %s", current, to)
	}

	key := sm.effectKey(current, to)
	if effect, ok := sm.effects[key]; ok {
		if err := effect(ctx, session); err != nil {
			return fmt.Errorf("side effect for %s→%s: %w", current, to, err)
		}
	}
	return nil
}

// IsTerminal returns true if the status is a terminal state (no further transitions possible).
func (sm *StateMachine) IsTerminal(s Status) bool {
	_, hasOutgoing := sm.transitions[s]
	return !hasOutgoing
}

// IsActive returns true if the session is in a running state that should be monitored.
func (sm *StateMachine) IsActive(s Status) bool {
	switch s {
	case StatusActive, StatusIdle, StatusPending, StatusSpawning, StatusResuming:
		return true
	default:
		return false
	}
}

// effectKey builds the internal key for effect lookup.
func (sm *StateMachine) effectKey(from, to Status) string {
	return string(from) + "->" + string(to)
}

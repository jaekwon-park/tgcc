// Package session manages Claude Code session lifecycle.
package session

// ValidStatuses enumerates all valid session statuses.
var ValidStatuses = map[string]bool{
	"pending":  true,
	"spawning": true,
	"active":   true,
	"idle":     true,
	"crashed":  true,
	"resuming": true,
	"stopping": true,
	"stopped":  true,
	"failed":   true,
}

// ValidTransitions defines allowed state transitions.
// Each key is a source status, and the value is a set of allowed target statuses.
var ValidTransitions = map[string]map[string]bool{
	"pending":  {"spawning": true, "failed": true},
	"spawning": {"active": true, "failed": true},
	"active":   {"idle": true, "crashed": true, "stopping": true},
	"idle":     {"active": true, "crashed": true, "stopping": true},
	"crashed":  {"resuming": true},
	"resuming": {"active": true, "failed": true},
	"stopping": {"stopped": true, "failed": true},
	"failed":   {}, // terminal
	"stopped":  {}, // terminal
}

// IsActiveStatus returns true if the status indicates a running session.
func IsActiveStatus(status string) bool {
	return status == "active" || status == "idle" || status == "resuming"
}

// IsHealthyStatus returns true if the session is actively running.
func IsHealthyStatus(status string) bool {
	return status == "active" || status == "idle"
}

// CanTransition checks if a state transition is allowed.
func CanTransition(from, to string) bool {
	if targets, ok := ValidTransitions[from]; ok {
		return targets[to]
	}
	return false
}

// IsTerminalStatus returns true if the status is a terminal (end) state.
func IsTerminalStatus(status string) bool {
	return status == "failed" || status == "stopped"
}

// ReconcilableStatuses lists statuses that the reconciler should check at boot.
var ReconcilableStatuses = []string{"active", "idle", "resuming"}

// SupervisableStatuses lists statuses that the supervisor should monitor.
var SupervisableStatuses = []string{"active", "idle"}

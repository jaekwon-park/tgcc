package session

// Supervisor monitors session health and triggers restart on crash.
// Stub — full implementation in M3.
type Supervisor struct{}

// NewSupervisor creates a new Supervisor.
func NewSupervisor() *Supervisor {
	return &Supervisor{}
}

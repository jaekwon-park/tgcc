package store

import (
	"database/sql"
	"time"
)

// SessionRow represents a row from the sessions table.
type SessionRow struct {
	ID              string
	TopicID         int
	TmuxSession     string
	TmuxWindow      string
	WorkspacePath   string
	ClaudeSessionID sql.NullString
	PID             sql.NullInt64
	Status          string
	LastActivityAt  int64
	CreatedAt       int64
}

// TopicRow represents a row from the topics table joined with chat info.
type TopicRow struct {
	ID        int
	ChatID    int64
	ThreadID  int64
	Name      sql.NullString
	ChatTitle sql.NullString
}

// GetActiveSessions returns all sessions with status IN (active, idle, resuming).
// Used by the reconciler at startup.
func (s *Store) GetActiveSessions() ([]SessionRow, error) {
	rows, err := s.DB.Query(`
		SELECT id, topic_id, tmux_session, tmux_window, workspace_path,
		       claude_session_id, pid, status, last_activity_at, created_at
		FROM sessions
		WHERE status IN ('active', 'idle', 'resuming')
		ORDER BY last_activity_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []SessionRow
	for rows.Next() {
		var r SessionRow
		if err := rows.Scan(
			&r.ID, &r.TopicID, &r.TmuxSession, &r.TmuxWindow, &r.WorkspacePath,
			&r.ClaudeSessionID, &r.PID, &r.Status,
			&r.LastActivityAt, &r.CreatedAt,
		); err != nil {
			return nil, err
		}
		sessions = append(sessions, r)
	}
	return sessions, rows.Err()
}

// GetSupervisedSessions returns all sessions with status IN (active, idle).
// Used by the supervisor for health checks.
func (s *Store) GetSupervisedSessions() ([]SessionRow, error) {
	rows, err := s.DB.Query(`
		SELECT id, topic_id, tmux_session, tmux_window, workspace_path,
		       claude_session_id, pid, status, last_activity_at, created_at
		FROM sessions
		WHERE status IN ('active', 'idle')
		ORDER BY last_activity_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []SessionRow
	for rows.Next() {
		var r SessionRow
		if err := rows.Scan(
			&r.ID, &r.TopicID, &r.TmuxSession, &r.TmuxWindow, &r.WorkspacePath,
			&r.ClaudeSessionID, &r.PID, &r.Status,
			&r.LastActivityAt, &r.CreatedAt,
		); err != nil {
			return nil, err
		}
		sessions = append(sessions, r)
	}
	return sessions, rows.Err()
}

// UpdateSessionStatus updates the status of a session.
func (s *Store) UpdateSessionStatus(id string, status string) error {
	_, err := s.DB.Exec(`UPDATE sessions SET status = ? WHERE id = ?`, status, id)
	return err
}

// UpdateSessionPID updates the PID of a session.
func (s *Store) UpdateSessionPID(id string, pid int) error {
	_, err := s.DB.Exec(`UPDATE sessions SET pid = ? WHERE id = ?`, pid, id)
	return err
}

// UpdateSessionActivity updates the last_activity_at timestamp.
func (s *Store) UpdateSessionActivity(id string) error {
	now := time.Now().UnixMilli()
	_, err := s.DB.Exec(`UPDATE sessions SET last_activity_at = ? WHERE id = ?`, now, id)
	return err
}

// GetTopicByID returns a topic row by its ID.
func (s *Store) GetTopicByID(topicID int) (*TopicRow, error) {
	row := s.DB.QueryRow(`
		SELECT t.id, t.chat_id, t.thread_id, t.name, c.title
		FROM topics t
		LEFT JOIN chats c ON c.chat_id = t.chat_id
		WHERE t.id = ?
	`, topicID)
	var r TopicRow
	err := row.Scan(&r.ID, &r.ChatID, &r.ThreadID, &r.Name, &r.ChatTitle)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

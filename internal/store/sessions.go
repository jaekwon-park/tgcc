package store

import (
	"database/sql"
	"strings"
)

// Session represents a Claude Code session.
type Session struct {
	ID              string
	TopicID         int64
	TmuxSession     string
	TmuxWindow      string
	WorkspacePath   string
	ClaudeSessionID string
	PID             int64
	Status          string
	LastActivityAt  int64
	CreatedAt       int64
}

// InsertSession creates a new session record.
func (s *Store) InsertSession(session *Session) error {
	_, err := s.DB.Exec(
		`INSERT INTO sessions
		(id, topic_id, tmux_session, tmux_window, workspace_path, claude_session_id, pid, status, last_activity_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID, session.TopicID, session.TmuxSession, session.TmuxWindow, session.WorkspacePath,
		nullString(session.ClaudeSessionID), nullInt64(session.PID), session.Status, session.LastActivityAt, session.CreatedAt,
	)
	return err
}

// SessionByID returns a session by its UUID.
func (s *Store) SessionByID(id string) (*Session, error) {
	row := s.DB.QueryRow(
		`SELECT id, topic_id, tmux_session, tmux_window, workspace_path, claude_session_id, pid, status, last_activity_at, created_at
		FROM sessions WHERE id = ?`,
		id,
	)
	session, err := scanSession(row.Scan)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return session, nil
}

// SessionByTopicID returns the active session for a topic.
func (s *Store) SessionByTopicID(topicID int64) (*Session, error) {
	row := s.DB.QueryRow(
		`SELECT id, topic_id, tmux_session, tmux_window, workspace_path, claude_session_id, pid, status, last_activity_at, created_at
		FROM sessions WHERE topic_id = ?`,
		topicID,
	)
	session, err := scanSession(row.Scan)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return session, nil
}

// ActiveSessions returns all sessions with status in the given list.
func (s *Store) ActiveSessions(statuses []string) ([]*Session, error) {
	if len(statuses) == 0 {
		return []*Session{}, nil
	}

	placeholders := make([]string, len(statuses))
	args := make([]interface{}, len(statuses))
	for i, status := range statuses {
		placeholders[i] = "?"
		args[i] = status
	}

	query := `SELECT id, topic_id, tmux_session, tmux_window, workspace_path, claude_session_id, pid, status, last_activity_at, created_at
		FROM sessions WHERE status IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sessions := make([]*Session, 0)
	for rows.Next() {
		session, err := scanSession(rows.Scan)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return sessions, nil
}

// ActiveSessionCount returns the count of sessions matching the given statuses.
func (s *Store) ActiveSessionCount(statuses []string) (int, error) {
	if len(statuses) == 0 {
		return 0, nil
	}

	placeholders := make([]string, len(statuses))
	args := make([]interface{}, len(statuses))
	for i, status := range statuses {
		placeholders[i] = "?"
		args[i] = status
	}

	query := `SELECT COUNT(*) FROM sessions WHERE status IN (` + strings.Join(placeholders, ",") + `)`
	var count int
	err := s.DB.QueryRow(query, args...).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// UpdateSessionStatus changes the session status.
func (s *Store) UpdateSessionStatus(id string, status string) error {
	_, err := s.DB.Exec(`UPDATE sessions SET status = ?, last_activity_at = ? WHERE id = ?`, status, CurrentTimeMs(), id)
	return err
}

// UpdateSessionPID updates the session PID.
func (s *Store) UpdateSessionPID(id string, pid int64) error {
	_, err := s.DB.Exec(`UPDATE sessions SET pid = ?, last_activity_at = ? WHERE id = ?`, pid, CurrentTimeMs(), id)
	return err
}

// UpdateSessionClaudeID updates the claude_session_id.
func (s *Store) UpdateSessionClaudeID(id string, claudeSessionID string) error {
	_, err := s.DB.Exec(`UPDATE sessions SET claude_session_id = ?, last_activity_at = ? WHERE id = ?`, claudeSessionID, CurrentTimeMs(), id)
	return err
}

// DeleteSession removes a session record.
func (s *Store) DeleteSession(id string) error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

func scanSession(scan func(dest ...interface{}) error) (*Session, error) {
	session := &Session{}
	var claudeSessionID sql.NullString
	var pid sql.NullInt64
	err := scan(
		&session.ID, &session.TopicID, &session.TmuxSession, &session.TmuxWindow, &session.WorkspacePath,
		&claudeSessionID, &pid, &session.Status, &session.LastActivityAt, &session.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	if claudeSessionID.Valid {
		session.ClaudeSessionID = claudeSessionID.String
	}
	if pid.Valid {
		session.PID = pid.Int64
	}
	return session, nil
}

func nullString(v string) interface{} {
	if v == "" {
		return nil
	}
	return v
}

func nullInt64(v int64) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

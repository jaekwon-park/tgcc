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
	CorrelationID   string
	PID             int64
	Status          string
	LastActivityAt  int64
	CreatedAt       int64
	ArchivedAt      int64
	TranscriptPath  string
	TranscriptBytes int64
	TurnCount       int64
	CompactCount    int64
	LastCompactAt   int64
}

func (s *Store) InsertSession(session *Session) error {
	_, err := s.DB.Exec(
		`INSERT INTO sessions
		(id, topic_id, tmux_session, tmux_window, workspace_path, claude_session_id, correlation_id, pid, status, last_activity_at, created_at, archived_at, transcript_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID, session.TopicID, session.TmuxSession, session.TmuxWindow, session.WorkspacePath,
		nullString(session.ClaudeSessionID), nullString(session.CorrelationID), nullInt64(session.PID), session.Status, session.LastActivityAt, session.CreatedAt, nil, nullString(session.TranscriptPath),
	)
	return err
}

func (s *Store) SessionByID(id string) (*Session, error) {
	row := s.DB.QueryRow(
		`SELECT id, topic_id, tmux_session, tmux_window, workspace_path, claude_session_id, correlation_id, pid, status, last_activity_at, created_at,
		archived_at, transcript_path, transcript_bytes, turn_count, compact_count, last_compact_at
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

func (s *Store) SessionByTopicID(topicID int64) (*Session, error) {
	row := s.DB.QueryRow(
		`SELECT id, topic_id, tmux_session, tmux_window, workspace_path, claude_session_id, correlation_id, pid, status, last_activity_at, created_at,
		archived_at, transcript_path, transcript_bytes, turn_count, compact_count, last_compact_at
		FROM sessions WHERE topic_id = ? AND archived_at IS NULL`,
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

	query := `SELECT id, topic_id, tmux_session, tmux_window, workspace_path, claude_session_id, correlation_id, pid, status, last_activity_at, created_at,
		archived_at, transcript_path, transcript_bytes, turn_count, compact_count, last_compact_at
		FROM sessions WHERE status IN (` + strings.Join(placeholders, ",") + `) AND archived_at IS NULL`
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

	query := `SELECT COUNT(*) FROM sessions WHERE status IN (` + strings.Join(placeholders, ",") + `) AND archived_at IS NULL`
	var count int
	err := s.DB.QueryRow(query, args...).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) UpdateSessionStatus(id string, status string) error {
	_, err := s.DB.Exec(`UPDATE sessions SET status = ?, last_activity_at = ? WHERE id = ?`, status, CurrentTimeMs(), id)
	return err
}

// UpdateSessionStatusIf updates the session status only if it currently matches expectedStatus.
// M4 fix: prevents stale timer goroutines from overwriting a session status that has
// already changed (e.g. active overwriting failed/crashed/stopped).
func (s *Store) UpdateSessionStatusIf(id string, expectedStatus string, status string) error {
	_, err := s.DB.Exec(
		`UPDATE sessions SET status = ?, last_activity_at = ? WHERE id = ? AND status = ?`,
		status, CurrentTimeMs(), id, expectedStatus,
	)
	return err
}

func (s *Store) UpdateSessionPID(id string, pid int64) error {
	_, err := s.DB.Exec(`UPDATE sessions SET pid = ?, last_activity_at = ? WHERE id = ?`, pid, CurrentTimeMs(), id)
	return err
}

// UpdateSessionTmuxWindow persists the actual tmux window identifier (typically
// the @N pane id returned by tmux new-window -P) so ForwardMessage and other
// callers can target the live pane after Spawn/Resume/FreshRestart. Without
// this the DB keeps the originally-requested window NAME, which goes stale the
// moment Resume creates "<name>-r" or tmux deduplicates names across topics.
func (s *Store) UpdateSessionTmuxWindow(id string, tmuxWindow string) error {
	_, err := s.DB.Exec(`UPDATE sessions SET tmux_window = ?, last_activity_at = ? WHERE id = ?`, tmuxWindow, CurrentTimeMs(), id)
	return err
}

func (s *Store) UpdateSessionClaudeID(id string, claudeSessionID string) error {
	_, err := s.DB.Exec(`UPDATE sessions SET claude_session_id = ?, last_activity_at = ? WHERE id = ?`, claudeSessionID, CurrentTimeMs(), id)
	return err
}

func (s *Store) UpdateSessionContext(sessionID string, transcriptBytes, turnCount int64) error {
	_, err := s.DB.Exec(
		`UPDATE sessions SET transcript_bytes = ?, turn_count = ?, last_activity_at = ? WHERE id = ?`,
		transcriptBytes, turnCount, CurrentTimeMs(), sessionID,
	)
	return err
}

func (s *Store) UpdateSessionCompact(sessionID string, compactCount int64, lastCompactAt int64) error {
	_, err := s.DB.Exec(
		`UPDATE sessions SET compact_count = ?, last_compact_at = ?, last_activity_at = ? WHERE id = ?`,
		compactCount, lastCompactAt, CurrentTimeMs(), sessionID,
	)
	return err
}

func (s *Store) ArchiveSession(id string, archivedAt int64) error {
	_, err := s.DB.Exec(`UPDATE sessions SET archived_at = ?, last_activity_at = ? WHERE id = ?`, archivedAt, CurrentTimeMs(), id)
	return err
}

// UpdateSessionTranscriptPath updates the transcript path for a session.
func (s *Store) UpdateSessionTranscriptPath(id string, path string) error {
	_, err := s.DB.Exec(`UPDATE sessions SET transcript_path = ?, last_activity_at = ? WHERE id = ?`, path, CurrentTimeMs(), id)
	return err
}

// ArchivedSessionsByTopic returns all archived sessions (archived_at IS NOT NULL) for a given topic.
func (s *Store) ArchivedSessionsByTopic(topicID int64) ([]*Session, error) {
	rows, err := s.DB.Query(
		`SELECT id, topic_id, tmux_session, tmux_window, workspace_path, claude_session_id, correlation_id, pid, status, last_activity_at, created_at,
		archived_at, transcript_path, transcript_bytes, turn_count, compact_count, last_compact_at
		FROM sessions WHERE topic_id = ? AND archived_at IS NOT NULL ORDER BY archived_at DESC`,
		topicID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []*Session
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

// SessionByClaudeID returns a session by its Claude Code internal session ID.
func (s *Store) SessionByClaudeID(claudeID string) (*Session, error) {
	if claudeID == "" {
		return nil, nil
	}
	row := s.DB.QueryRow(
		`SELECT id, topic_id, tmux_session, tmux_window, workspace_path, claude_session_id, correlation_id, pid, status, last_activity_at, created_at,
		archived_at, transcript_path, transcript_bytes, turn_count, compact_count, last_compact_at
		FROM sessions WHERE claude_session_id = ? AND archived_at IS NULL`,
		claudeID,
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

// SessionByCorrelationID returns a session by its correlation_id, used for
// 1:1 matching between spawn and SessionStart hook.
func (s *Store) SessionByCorrelationID(correlationID string) (*Session, error) {
	if correlationID == "" {
		return nil, nil
	}
	row := s.DB.QueryRow(
		`SELECT id, topic_id, tmux_session, tmux_window, workspace_path, claude_session_id, correlation_id, pid, status, last_activity_at, created_at,
		archived_at, transcript_path, transcript_bytes, turn_count, compact_count, last_compact_at
		FROM sessions WHERE correlation_id = ? AND archived_at IS NULL`,
		correlationID,
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

// SessionByWorkspaceNullClaudeID returns the first non-archived session matching
// the given workspace path where claude_session_id IS NULL, regardless of status.
// This is used by the session-start hook as a fallback when correlation_id matching
// fails, to avoid the race condition where the 2-second timer has already transitioned
// the session from spawning to active.
func (s *Store) SessionByWorkspaceNullClaudeID(workspacePath string) (*Session, error) {
	row := s.DB.QueryRow(
		`SELECT id, topic_id, tmux_session, tmux_window, workspace_path, claude_session_id, correlation_id, pid, status, last_activity_at, created_at, archived_at, transcript_path, transcript_bytes, turn_count, compact_count, last_compact_at
		FROM sessions WHERE workspace_path = ? AND claude_session_id IS NULL AND archived_at IS NULL LIMIT 1`,
		workspacePath,
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

func (s *Store) DeleteSession(id string) error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// SessionByWorkspaceAndStatus returns the first non-archived session matching
// the given workspace path and one of the given statuses.
func (s *Store) SessionByWorkspaceAndStatus(workspacePath string, statuses []string) (*Session, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(statuses))
	args := make([]interface{}, len(statuses)+1)
	args[0] = workspacePath
	for i, status := range statuses {
		placeholders[i] = "?"
		args[i+1] = status
	}
	query := "SELECT id, topic_id, tmux_session, tmux_window, workspace_path, claude_session_id, correlation_id, pid, status, last_activity_at, created_at, archived_at, transcript_path, transcript_bytes, turn_count, compact_count, last_compact_at FROM sessions WHERE workspace_path = ? AND status IN (" + strings.Join(placeholders, ",") + ") AND archived_at IS NULL LIMIT 1"
	row := s.DB.QueryRow(query, args...)
	session, err := scanSession(row.Scan)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return session, nil
}

func scanSession(scan func(dest ...interface{}) error) (*Session, error) {
	session := &Session{}
	var claudeSessionID sql.NullString
	var correlationID sql.NullString
	var pid sql.NullInt64
	var archivedAt sql.NullInt64
	var transcriptPath sql.NullString
	var lastCompactAt sql.NullInt64
	err := scan(
		&session.ID, &session.TopicID, &session.TmuxSession, &session.TmuxWindow, &session.WorkspacePath,
		&claudeSessionID, &correlationID, &pid, &session.Status, &session.LastActivityAt, &session.CreatedAt,
		&archivedAt, &transcriptPath, &session.TranscriptBytes, &session.TurnCount, &session.CompactCount, &lastCompactAt,
	)
	if err != nil {
		return nil, err
	}
	if claudeSessionID.Valid {
		session.ClaudeSessionID = claudeSessionID.String
	}
	if correlationID.Valid {
		session.CorrelationID = correlationID.String
	}
	if pid.Valid {
		session.PID = pid.Int64
	}
	if archivedAt.Valid {
		session.ArchivedAt = archivedAt.Int64
	}
	if transcriptPath.Valid {
		session.TranscriptPath = transcriptPath.String
	}
	if lastCompactAt.Valid {
		session.LastCompactAt = lastCompactAt.Int64
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

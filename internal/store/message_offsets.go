// Package store — message_offsets CRUD for transcript deduplication.
package store

import "database/sql"

// MessageOffset tracks the last processed position in a session's transcript JSONL file.
// Used for deduplication so the same assistant message is not relayed twice.
type MessageOffset struct {
	SessionID       string
	LastHookEventID string // byte offset (as string) in the transcript file
	LastCaptureHash string // reserved for future use
	UpdatedAt       int64
}

// GetMessageOffset returns the message offset for a session, or nil if none exists.
func (s *Store) GetMessageOffset(sessionID string) (*MessageOffset, error) {
	row := s.DB.QueryRow(
		`SELECT session_id, last_hook_event_id, last_capture_hash, updated_at
		FROM message_offsets WHERE session_id = ?`,
		sessionID,
	)
	var mo MessageOffset
	var lastHookEventID sql.NullString
	var lastCaptureHash sql.NullString
	err := row.Scan(&mo.SessionID, &lastHookEventID, &lastCaptureHash, &mo.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if lastHookEventID.Valid {
		mo.LastHookEventID = lastHookEventID.String
	}
	if lastCaptureHash.Valid {
		mo.LastCaptureHash = lastCaptureHash.String
	}
	return &mo, nil
}

// UpsertMessageOffset inserts or updates the message offset for a session.
func (s *Store) UpsertMessageOffset(sessionID, lastHookEventID string) error {
	_, err := s.DB.Exec(
		`INSERT OR REPLACE INTO message_offsets (session_id, last_hook_event_id, last_capture_hash, updated_at)
		VALUES (?, ?, ?, ?)`,
		sessionID, nullString(lastHookEventID), nil, CurrentTimeMs(),
	)
	return err
}

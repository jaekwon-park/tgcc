package store

import (
	"database/sql"
	"encoding/json"
)

// AuditEntry is a single audit log record.
type AuditEntry struct {
	ID          int64
	Timestamp   int64
	ActorUserID *int64
	EventType   string
	Resource    string
	Detail      string
}

// InsertAudit writes a new audit log entry.
func (s *Store) InsertAudit(actorUserID *int64, eventType, resource string, detail map[string]interface{}) error {
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	var actor interface{}
	if actorUserID != nil {
		actor = *actorUserID
	}
	var resourceValue interface{}
	if resource != "" {
		resourceValue = resource
	}
	_, err = s.DB.Exec(
		`INSERT INTO audit_log (timestamp, actor_user_id, event_type, resource, detail) VALUES (?, ?, ?, ?, ?)`,
		CurrentTimeMs(), actor, eventType, resourceValue, string(detailJSON),
	)
	return err
}

// QueryAudit returns audit log entries with cursor-based pagination.
func (s *Store) QueryAudit(since int64, limit int) ([]*AuditEntry, int64, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.DB.Query(
		`SELECT id, timestamp, actor_user_id, event_type, resource, detail
		FROM audit_log
		WHERE timestamp > ?
		ORDER BY timestamp DESC, id DESC
		LIMIT ?`,
		since, limit,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	entries := make([]*AuditEntry, 0)
	var nextSince int64
	for rows.Next() {
		e := &AuditEntry{}
		var actor sql.NullInt64
		var resource sql.NullString
		var detail sql.NullString
		if err := rows.Scan(&e.ID, &e.Timestamp, &actor, &e.EventType, &resource, &detail); err != nil {
			return nil, 0, err
		}
		if actor.Valid {
			e.ActorUserID = &actor.Int64
		}
		if resource.Valid {
			e.Resource = resource.String
		}
		if detail.Valid {
			e.Detail = detail.String
		}
		if nextSince == 0 {
			nextSince = e.Timestamp
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return entries, nextSince, nil
}

package store

import (
	"database/sql"
	"time"
)

// User represents a registered telegram user.
type User struct {
	UserID     int64
	Username   sql.NullString
	Role       string
	CreatedAt  int64 // unix milliseconds
	LastSeenAt sql.NullInt64
}

// InsertUser creates a new user record.
func (s *Store) InsertUser(userID int64, username string, role string) error {
	var usernameValue interface{}
	if username == "" {
		usernameValue = nil
	} else {
		usernameValue = username
	}
	_, err := s.DB.Exec(
		`INSERT INTO users (user_id, username, role, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?)`,
		userID, usernameValue, role, CurrentTimeMs(), nil,
	)
	return err
}

// UserByID returns a user by telegram user_id.
func (s *Store) UserByID(userID int64) (*User, error) {
	u := &User{}
	err := s.DB.QueryRow(
		`SELECT user_id, username, role, created_at, last_seen_at FROM users WHERE user_id = ?`,
		userID,
	).Scan(&u.UserID, &u.Username, &u.Role, &u.CreatedAt, &u.LastSeenAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return u, nil
}

// UpdateLastSeen updates the last_seen_at timestamp.
func (s *Store) UpdateLastSeen(userID int64) error {
	_, err := s.DB.Exec(`UPDATE users SET last_seen_at = ? WHERE user_id = ?`, CurrentTimeMs(), userID)
	return err
}

// CurrentTimeMs returns current unix time in milliseconds.
func CurrentTimeMs() int64 {
	return time.Now().UnixMilli()
}

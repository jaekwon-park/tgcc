package store

import "database/sql"

// PairingCode represents a one-time pairing code.
type PairingCode struct {
	Code      string
	UserID    int64
	ExpiresAt int64
	UsedAt    *int64
	CreatedAt int64
}

// InsertPairingCode creates a new pairing code.
func (s *Store) InsertPairingCode(code string, userID int64, expiresAt, createdAt int64) error {
	_, err := s.DB.Exec(
		`INSERT INTO pairing_codes (code, user_id, expires_at, used_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		code, userID, expiresAt, nil, createdAt,
	)
	return err
}

// ActivePairingCodeByUser returns the unexpired, unused pairing code for a user.
func (s *Store) ActivePairingCodeByUser(userID int64) (*PairingCode, error) {
	pc := &PairingCode{}
	var usedAt sql.NullInt64
	err := s.DB.QueryRow(
		`SELECT code, user_id, expires_at, used_at, created_at
		FROM pairing_codes
		WHERE user_id = ? AND used_at IS NULL AND expires_at > ?
		ORDER BY created_at DESC
		LIMIT 1`,
		userID, CurrentTimeMs(),
	).Scan(&pc.Code, &pc.UserID, &pc.ExpiresAt, &usedAt, &pc.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if usedAt.Valid {
		v := usedAt.Int64
		pc.UsedAt = &v
	}
	return pc, nil
}

// PairingCodeByCode returns a pairing code by its code value.
func (s *Store) PairingCodeByCode(code string) (*PairingCode, error) {
	pc := &PairingCode{}
	var usedAt sql.NullInt64
	err := s.DB.QueryRow(
		`SELECT code, user_id, expires_at, used_at, created_at FROM pairing_codes WHERE code = ?`,
		code,
	).Scan(&pc.Code, &pc.UserID, &pc.ExpiresAt, &usedAt, &pc.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if usedAt.Valid {
		v := usedAt.Int64
		pc.UsedAt = &v
	}
	return pc, nil
}

// MarkPairingCodeUsed marks a pairing code as used.
func (s *Store) MarkPairingCodeUsed(code string, usedAt int64) error {
	_, err := s.DB.Exec(`UPDATE pairing_codes SET used_at = ? WHERE code = ?`, usedAt, code)
	return err
}

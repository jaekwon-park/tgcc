package acl

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"time"

	"github.com/jaekwon-park/tgcc/internal/store"
)

// PairingManager handles pairing code generation and verification.
type PairingManager struct {
	store *store.Store
	ttl   time.Duration
}

// NewPairingManager creates a new PairingManager with default 10-minute TTL.
func NewPairingManager(st *store.Store) *PairingManager {
	return &PairingManager{
		store: st,
		ttl:   10 * time.Minute,
	}
}

// GenerateCode creates a 6-digit pairing code for the given user.
// Reuses an existing unexpired code if one exists.
func (pm *PairingManager) GenerateCode(ctx context.Context, userID int64) (string, error) {
	_ = ctx

	existing, err := pm.store.ActivePairingCodeByUser(userID)
	if err != nil {
		return "", fmt.Errorf("lookup active pairing code: %w", err)
	}
	if existing != nil {
		return existing.Code, nil
	}

	code, err := randomCode()
	if err != nil {
		return "", fmt.Errorf("generate random code: %w", err)
	}

	now := time.Now().UnixMilli()
	expiresAt := time.Now().Add(pm.ttl).UnixMilli()
	if err := pm.store.InsertPairingCode(code, userID, expiresAt, now); err != nil {
		return "", fmt.Errorf("insert pairing code: %w", err)
	}
	return code, nil
}

// VerifyCode validates a pairing code and registers the user as owner.
func (pm *PairingManager) VerifyCode(ctx context.Context, code string) (int64, error) {
	_ = ctx

	pc, err := pm.store.PairingCodeByCode(code)
	if err != nil {
		return 0, fmt.Errorf("lookup pairing code: %w", err)
	}
	if pc == nil {
		return 0, fmt.Errorf("invalid code")
	}
	if pc.UsedAt != nil {
		return 0, fmt.Errorf("code already used")
	}

	now := time.Now().UnixMilli()
	if pc.ExpiresAt < now {
		return 0, fmt.Errorf("code expired")
	}

	if err := pm.store.InsertUser(pc.UserID, "", "owner"); err != nil {
		return 0, fmt.Errorf("insert owner user: %w", err)
	}
	if err := pm.store.MarkPairingCodeUsed(code, now); err != nil {
		return 0, fmt.Errorf("mark pairing code used: %w", err)
	}

	return pc.UserID, nil
}

// randomCode generates a 6-digit numeric code.
func randomCode() (string, error) {
	max := big.NewInt(1000000)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// Package acl provides authentication and authorization logic.
package acl

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/store"
)

// Decision represents an ACL decision.
type Decision int

const (
	Allow Decision = iota
	DenyUnknownUser
	DenyRole
	DenyChatNotRegistered
	HandlePairing
)

// Guard enforces ACL decisions per docs/02_ARCHITECTURE.md §3.4.
type Guard struct {
	store  *store.Store
	logger *slog.Logger
}

// NewGuard creates a new ACL Guard.
func NewGuard(st *store.Store, logger *slog.Logger) *Guard {
	return &Guard{store: st, logger: logger}
}

// Check evaluates an incoming Telegram Update against the ACL flow.
// Returns the decision and relevant metadata.
func (g *Guard) Check(ctx context.Context, update bot.Update) (Decision, *store.User) {
	if update.Message == nil || update.Message.From == nil {
		return DenyUnknownUser, nil
	}

	userID := update.Message.From.ID
	user, err := g.store.UserByID(userID)
	if err != nil {
		g.logger.Error("failed to query user", "error", err, "user_id", userID)
		return DenyUnknownUser, nil
	}
	if user == nil {
		if strings.HasPrefix(update.Message.Text, "/pair") {
			return HandlePairing, nil
		}
		return DenyUnknownUser, nil
	}

	if user.Role != "owner" {
		return DenyRole, user
	}

	if update.Message.Chat != nil && update.Message.Chat.Type != "private" {
		chat, err := g.store.ChatByID(update.Message.Chat.ID)
		if err != nil {
			g.logger.Error("failed to query chat", "error", err, "chat_id", update.Message.Chat.ID)
			return DenyChatNotRegistered, user
		}
		if chat == nil {
			return DenyChatNotRegistered, user
		}
	}

	return Allow, user
}

// RecordDenial logs a denied access to audit_log.
func (g *Guard) RecordDenial(ctx context.Context, actorUserID *int64, reason string) {
	if err := g.store.InsertAudit(actorUserID, "auth.denied", "", map[string]interface{}{
		"reason": reason,
	}); err != nil {
		g.logger.ErrorContext(ctx, "failed to record denial", "error", fmt.Errorf("insert audit log: %w", err))
	}
}

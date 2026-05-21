// Package acl provides authentication and authorization logic.
package acl

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
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
	// topicAllowLists maps "chatID:threadID" → list of allowed user IDs.
	// Empty list or missing key means all owners are allowed (backward compatible).
	topicAllowLists map[string][]int64
}

// NewGuard creates a new ACL Guard.
// topicAllowLists maps "chatID:threadID" → list of user IDs that are allowed
// access to that specific topic. An empty list or missing key means all
// owners can access (backward compatible).
func NewGuard(st *store.Store, logger *slog.Logger, topicAllowLists map[string][]int64) *Guard {
	return &Guard{store: st, logger: logger, topicAllowLists: topicAllowLists}
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

	// Topic-level allowlist check. Only applies when the topic has allow_users
	// configured in tgcc.toml. Empty list or missing key = all owners allowed.
	if update.Message.Chat != nil {
		topicKey := strconv.FormatInt(update.Message.Chat.ID, 10) + ":" + strconv.FormatInt(update.Message.MessageThreadID, 10)
		if allowUsers, ok := g.topicAllowLists[topicKey]; ok && len(allowUsers) > 0 {
			if !slices.Contains(allowUsers, userID) {
				g.logger.Warn("user denied by topic allowlist",
					"user_id", userID,
					"chat_id", update.Message.Chat.ID,
					"thread_id", update.Message.MessageThreadID,
				)
				return DenyRole, user
			}
		}
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

// Package router maps telegram topics to Claude Code sessions and dispatches messages.
package router

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jaekwon-park/tgcc/internal/acl"
	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/store"
)

// Router handles topic ↔ session mapping and message dispatch.
type Router struct {
	store      *store.Store
	logger     *slog.Logger
	sender     *bot.Sender
	guard      *acl.Guard
	pairingMgr *acl.PairingManager
}

// NewRouter creates a new Router.
func NewRouter(st *store.Store, logger *slog.Logger, sender *bot.Sender, guard *acl.Guard, pairingMgr *acl.PairingManager) *Router {
	return &Router{store: st, logger: logger, sender: sender, guard: guard, pairingMgr: pairingMgr}
}

// Route dispatches an incoming message from an allowed user to the appropriate handler.
func (r *Router) Route(ctx context.Context, update bot.Update, user *store.User) error {
	if update.Message == nil || update.Message.Chat == nil {
		return nil
	}
	text := strings.TrimSpace(update.Message.Text)
	if strings.HasPrefix(text, "/") {
		return r.handleCommand(ctx, update, user)
	}
	if user == nil {
		return r.handleStart(ctx, update, user)
	}
	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   update.Message.Chat.ID,
		ThreadID: update.Message.MessageThreadID,
		Text:     "이 토픽에 활성 세션이 없습니다. /new <workspace>로 시작하세요.",
	})
	return nil
}

// handleCommand dispatches bot commands (docs/03_API.md §1.2).
func (r *Router) handleCommand(ctx context.Context, update bot.Update, user *store.User) error {
	text := strings.TrimSpace(update.Message.Text)
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil
	}

	switch fields[0] {
	case "/start":
		return r.handleStart(ctx, update, user)
	case "/pair":
		return r.handlePair(ctx, update, user)
	case "/register":
		return r.handleRegister(ctx, update, user)
	case "/help":
		return r.handleHelp(ctx, update, user)
	case "/whoami":
		return r.handleWhoami(ctx, update, user)
	default:
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   update.Message.Chat.ID,
			ThreadID: update.Message.MessageThreadID,
			Text:     "알 수 없는 명령입니다. /help로 확인.",
		})
	}
	return nil
}

// handleStart responds to the /start command.
func (r *Router) handleStart(ctx context.Context, update bot.Update, user *store.User) error {
	_ = ctx
	text := "tgcc에 오신 것을 환영합니다! 활성 세션과 함께 /help로 명령을 확인하세요."
	if user == nil || user.Role != "owner" {
		text = "페어링이 필요합니다.\n/pair 명령으로 코드를 발급받고 터미널에서 tgcc pair <code>를 실행하세요."
	}
	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   update.Message.Chat.ID,
		ThreadID: update.Message.MessageThreadID,
		Text:     text,
	})
	return nil
}

// handlePair responds to the /pair command.
func (r *Router) handlePair(ctx context.Context, update bot.Update, user *store.User) error {
	_ = user
	if update.Message == nil || update.Message.From == nil || update.Message.Chat == nil {
		return nil
	}
	code, err := r.pairingMgr.GenerateCode(ctx, update.Message.From.ID)
	if err != nil {
		return err
	}
	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   update.Message.Chat.ID,
		ThreadID: update.Message.MessageThreadID,
		Text: fmt.Sprintf("🔑 페어링 코드: %s\n유효 시간: 10분\n\n터미널에서 다음 명령을 실행하세요:\n  tgcc pair %s", code, code),
	})
	return nil
}

// handleHelp responds to the /help command.
func (r *Router) handleHelp(ctx context.Context, update bot.Update, user *store.User) error {
	_ = ctx
	_ = user
	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   update.Message.Chat.ID,
		ThreadID: update.Message.MessageThreadID,
		Text: "/start\n/pair\n/register\n/new\n/resume\n/stop\n/kill\n/status\n/list\n/workspaces\n/help\n/whoami",
	})
	return nil
}

// handleWhoami responds to the /whoami command.
func (r *Router) handleWhoami(ctx context.Context, update bot.Update, user *store.User) error {
	_ = ctx
	if user == nil {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   update.Message.Chat.ID,
			ThreadID: update.Message.MessageThreadID,
			Text:     "사용자 정보를 찾을 수 없습니다.",
		})
		return nil
	}
	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   update.Message.Chat.ID,
		ThreadID: update.Message.MessageThreadID,
		Text:     fmt.Sprintf("👤 User ID: %d\n🎭 Role: %s", user.UserID, user.Role),
	})
	return nil
}

// handleRegister responds to the /register command.
func (r *Router) handleRegister(ctx context.Context, update bot.Update, user *store.User) error {
	_ = ctx
	if update.Message == nil || update.Message.Chat == nil {
		return nil
	}
	chat := update.Message.Chat
	threadID := update.Message.MessageThreadID
	if !chat.IsForum {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "이 명령은 포럼(토픽) 그룹에서만 사용할 수 있습니다.",
		})
		return nil
	}
	if user == nil {
		return nil
	}
	if err := r.store.InsertChat(chat.ID, chat.Title, chat.IsForum, user.UserID); err != nil {
		return err
	}
	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chat.ID,
		ThreadID: threadID,
		Text:     "✅ 이 포럼 그룹이 등록되었습니다.",
	})
	return nil
}

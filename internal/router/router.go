// Package router maps telegram topics to Claude Code sessions and dispatches messages.
package router

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jaekwon-park/tgcc/internal/acl"
	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/session"
	"github.com/jaekwon-park/tgcc/internal/store"
)

// Router handles topic ↔ session mapping and message dispatch.
type Router struct {
	store      *store.Store
	logger     *slog.Logger
	sender     *bot.Sender
	guard      *acl.Guard
	pairingMgr *acl.PairingManager
	mgr        *session.Manager
}

// NewRouter creates a new Router.
func NewRouter(st *store.Store, logger *slog.Logger, sender *bot.Sender, guard *acl.Guard, pairingMgr *acl.PairingManager, mgr *session.Manager) *Router {
	return &Router{store: st, logger: logger, sender: sender, guard: guard, pairingMgr: pairingMgr, mgr: mgr}
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
	// Non-command message: try to forward to active session
	return r.handlePlainMessage(ctx, update, user)
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
	// M2 session commands
	case "/new":
		return r.handleNew(ctx, update, user, fields)
	case "/resume":
		return r.handleResume(ctx, update, user)
	case "/stop":
		return r.handleStop(ctx, update, user)
	case "/kill":
		return r.handleKill(ctx, update, user)
	case "/status":
		return r.handleStatus(ctx, update, user)
	case "/list":
		return r.handleList(ctx, update, user)
	case "/workspaces":
		return r.handleWorkspaces(ctx, update, user)
	default:
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   update.Message.Chat.ID,
			ThreadID: update.Message.MessageThreadID,
			Text:     "알 수 없는 명령입니다. /help로 확인.",
		})
	}
	return nil
}

// handlePlainMessage forwards non-command text to the active Claude session for this topic.
func (r *Router) handlePlainMessage(ctx context.Context, update bot.Update, user *store.User) error {
	if user == nil {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   update.Message.Chat.ID,
			ThreadID: update.Message.MessageThreadID,
			Text:     "이 토픽에 활성 세션이 없습니다. /new <workspace>로 시작하세요.",
		})
		return nil
	}

	chat := update.Message.Chat
	threadID := update.Message.MessageThreadID

	// Find or create topic
	topic, err := r.ensureTopic(ctx, chat.ID, threadID, "")
	if err != nil {
		return fmt.Errorf("ensure topic: %w", err)
	}

	// Find active session for this topic
	sess, err := r.mgr.GetSessionByTopic(topic.ID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}
	if sess == nil {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "이 토픽에 활성 세션이 없습니다. /new <workspace>로 시작하세요.",
		})
		return nil
	}

	// Forward message to claude
	text := strings.TrimSpace(update.Message.Text)
	if text == "" {
		return nil
	}
	if err := r.mgr.ForwardMessage(ctx, sess.ID, text); err != nil {
		r.logger.Error("forward message failed", "error", err, "session_id", sess.ID)
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "⚠️ 메시지 전송 실패. 세션 상태를 확인하세요.",
		})
		return err
	}

	return nil
}

// ============================================================================
// Existing command handlers (M1)
// ============================================================================

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
		Text:     fmt.Sprintf("🔑 페어링 코드: %s\n유효 시간: 10분\n\n터미널에서 다음 명령을 실행하세요:\n  tgcc pair %s", code, code),
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
		Text:     "/start — 봇 소개\n/pair — 페어링 코드 발급\n/register — 그룹 등록\n/new [workspace] — 새 세션 생성\n/resume — 세션 복구\n/stop — 세션 종료\n/kill — 강제 종료\n/status — 세션 상태\n/list — 활성 세션 목록\n/workspaces — 사용 가능한 디렉토리 목록\n/help — 도움말\n/whoami — 본인 정보",
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

// ============================================================================
// M2 session command handlers
// ============================================================================

// handleNew processes /new [workspace] — spawn a new session.
func (r *Router) handleNew(ctx context.Context, update bot.Update, user *store.User, fields []string) error {
	if user == nil || update.Message == nil || update.Message.Chat == nil {
		return nil
	}

	chat := update.Message.Chat
	threadID := update.Message.MessageThreadID

	// Ensure topic exists
	topic, err := r.ensureTopic(ctx, chat.ID, threadID, "")
	if err != nil {
		return fmt.Errorf("ensure topic: %w", err)
	}

	// Determine workspace path
	var workspacePath string
	if len(fields) >= 2 {
		workspacePath = fields[1]
	} else {
		// No workspace specified — list available workspaces
		return r.handleWorkspaces(ctx, update, user)
	}

	// Resolve workspace path (handle ~ expansion)
	if strings.HasPrefix(workspacePath, "~") {
		homeDir, err := os.UserHomeDir()
		if err == nil {
			workspacePath = filepath.Join(homeDir, workspacePath[1:])
		}
	}

	// Validate workspace exists
	if _, err := os.Stat(workspacePath); os.IsNotExist(err) {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     fmt.Sprintf("❌ 워크스페이스를 찾을 수 없습니다: %s\n/workspaces 로 사용 가능한 디렉토리를 확인하세요.", workspacePath),
		})
		return nil
	}

	// Update topic workspace
	if err := r.store.UpdateTopicWorkspace(topic.ID, workspacePath); err != nil {
		r.logger.Warn("update topic workspace failed", "error", err)
	}

	// Spawn session
	sess, err := r.mgr.Spawn(ctx, topic.ID, workspacePath, chat.ID, threadID)
	if err != nil {
		r.logger.Error("spawn session failed", "error", err)
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     fmt.Sprintf("❌ 세션 생성 실패: %v", err),
		})
		return err
	}

	_ = sess
	return nil
}

// handleResume processes /resume — resume a crashed/stopped session.
func (r *Router) handleResume(ctx context.Context, update bot.Update, user *store.User) error {
	if update.Message == nil || update.Message.Chat == nil {
		return nil
	}

	chat := update.Message.Chat
	threadID := update.Message.MessageThreadID

	// Find topic
	topic, err := r.store.TopicByChatThread(chat.ID, threadID)
	if err != nil {
		return fmt.Errorf("find topic: %w", err)
	}
	if topic == nil {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "이 토픽에 등록된 세션이 없습니다. /new <workspace>로 시작하세요.",
		})
		return nil
	}

	// Find session for this topic
	sess, err := r.mgr.GetSessionByTopic(topic.ID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}
	if sess == nil {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "복구할 세션이 없습니다. /new <workspace>로 시작하세요.",
		})
		return nil
	}

	// Resume the session
	_, err = r.mgr.Resume(ctx, sess.ID)
	if err != nil {
		r.logger.Error("resume failed", "error", err, "session_id", sess.ID)
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     fmt.Sprintf("❌ 복구 실패: %v\n/new <workspace>로 새로 시작하세요.", err),
		})
		return err
	}

	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chat.ID,
		ThreadID: threadID,
		Text:     "✅ resumed",
	})
	return nil
}

// handleStop processes /stop — gracefully stop the session.
func (r *Router) handleStop(ctx context.Context, update bot.Update, user *store.User) error {
	if update.Message == nil || update.Message.Chat == nil {
		return nil
	}

	chat := update.Message.Chat
	threadID := update.Message.MessageThreadID

	topic, err := r.store.TopicByChatThread(chat.ID, threadID)
	if err != nil || topic == nil {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "이 토픽에 활성 세션이 없습니다.",
		})
		return nil
	}

	sess, err := r.mgr.GetSessionByTopic(topic.ID)
	if err != nil || sess == nil {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "이 토픽에 활성 세션이 없습니다.",
		})
		return nil
	}

	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chat.ID,
		ThreadID: threadID,
		Text:     "🛑 세션 종료 중...",
	})

	if err := r.mgr.Stop(ctx, sess.ID); err != nil {
		r.logger.Error("stop failed", "error", err)
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     fmt.Sprintf("⚠️ 종료 중 오류: %v\n/kill 로 강제 종료할 수 있습니다.", err),
		})
		return err
	}

	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chat.ID,
		ThreadID: threadID,
		Text:     "🛑 stopped",
	})
	return nil
}

// handleKill processes /kill — force kill a session.
func (r *Router) handleKill(ctx context.Context, update bot.Update, user *store.User) error {
	if update.Message == nil || update.Message.Chat == nil {
		return nil
	}

	chat := update.Message.Chat
	threadID := update.Message.MessageThreadID

	topic, err := r.store.TopicByChatThread(chat.ID, threadID)
	if err != nil || topic == nil {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "이 토픽에 활성 세션이 없습니다.",
		})
		return nil
	}

	sess, err := r.mgr.GetSessionByTopic(topic.ID)
	if err != nil || sess == nil {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "이 토픽에 활성 세션이 없습니다.",
		})
		return nil
	}

	if err := r.mgr.Kill(ctx, sess.ID); err != nil {
		r.logger.Error("kill failed", "error", err)
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     fmt.Sprintf("❌ 강제 종료 실패: %v", err),
		})
		return err
	}

	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chat.ID,
		ThreadID: threadID,
		Text:     "🛑 강제 종료됨",
	})
	return nil
}

// handleStatus processes /status — show current session status.
func (r *Router) handleStatus(ctx context.Context, update bot.Update, user *store.User) error {
	if update.Message == nil || update.Message.Chat == nil {
		return nil
	}

	chat := update.Message.Chat
	threadID := update.Message.MessageThreadID

	topic, err := r.store.TopicByChatThread(chat.ID, threadID)
	if err != nil || topic == nil {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "이 토픽에 세션이 없습니다. /new <workspace>로 시작하세요.",
		})
		return nil
	}

	sess, err := r.mgr.GetSessionByTopic(topic.ID)
	if err != nil || sess == nil {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "이 토픽에 세션이 없습니다. /new <workspace>로 시작하세요.",
		})
		return nil
	}

	now := time.Now().UnixMilli()
	elapsed := time.Duration(now-sess.CreatedAt) * time.Millisecond
	lastAct := time.Duration(now-sess.LastActivityAt) * time.Millisecond

	statusIcon := "🟢"
	switch sess.Status {
	case "active":
		statusIcon = "🟢"
	case "idle":
		statusIcon = "🟡"
	case "crashed":
		statusIcon = "🔴"
	case "pending", "spawning":
		statusIcon = "⏳"
	case "stopping":
		statusIcon = "🛑"
	case "failed":
		statusIcon = "❌"
	}

	text := fmt.Sprintf("📊 세션 상태\n\nID: %s\n상태: %s %s\n워크스페이스: %s\nPID: %d\n시작: %s 전\n마지막 활동: %s 전\ntmux: %s:%s",
		sess.ID[:8],
		statusIcon, sess.Status,
		sess.WorkspacePath,
		sess.PID,
		formatDuration(elapsed),
		formatDuration(lastAct),
		sess.TmuxSession, sess.TmuxWindow,
	)

	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chat.ID,
		ThreadID: threadID,
		Text:     text,
	})
	return nil
}

// handleList processes /list — list all active sessions.
func (r *Router) handleList(ctx context.Context, update bot.Update, user *store.User) error {
	if update.Message == nil || update.Message.Chat == nil {
		return nil
	}

	sessions, err := r.mgr.ListActiveSessions(ctx)
	if err != nil {
		r.logger.Error("list sessions failed", "error", err)
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   update.Message.Chat.ID,
			ThreadID: update.Message.MessageThreadID,
			Text:     fmt.Sprintf("❌ 세션 목록 조회 실패: %v", err),
		})
		return nil
	}

	if len(sessions) == 0 {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   update.Message.Chat.ID,
			ThreadID: update.Message.MessageThreadID,
			Text:     "📋 활성 세션이 없습니다.",
		})
		return nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 세션 목록 (%d개)\n\n", len(sessions)))

	for _, s := range sessions {
		icon := "🟢"
		switch s.Status {
		case "active":
			icon = "🟢"
		case "idle":
			icon = "🟡"
		case "crashed":
			icon = "🔴"
		case "pending", "spawning":
			icon = "⏳"
		default:
			icon = "⚪"
		}

		topic, _ := r.store.TopicByID(s.TopicID)
		topicName := "unknown"
		if topic != nil {
			topicName = topic.Name
			if topicName == "" {
				topicName = fmt.Sprintf("topic-%d", topic.ThreadID)
			}
		}

		sb.WriteString(fmt.Sprintf("%s %s  %s  %s\n",
			icon, s.ID[:8], s.Status, filepath.Base(s.WorkspacePath)))
		_ = topicName
	}

	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   update.Message.Chat.ID,
		ThreadID: update.Message.MessageThreadID,
		Text:     sb.String(),
	})
	return nil
}

// handleWorkspaces processes /workspaces — list available workspace directories.
func (r *Router) handleWorkspaces(ctx context.Context, update bot.Update, user *store.User) error {
	if update.Message == nil || update.Message.Chat == nil {
		return nil
	}

	dirs, err := r.mgr.ListWorkspaces()
	if err != nil {
		r.logger.Error("list workspaces failed", "error", err)
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   update.Message.Chat.ID,
			ThreadID: update.Message.MessageThreadID,
			Text:     fmt.Sprintf("❌ 워크스페이스 목록 조회 실패: %v", err),
		})
		return nil
	}

	if len(dirs) == 0 {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   update.Message.Chat.ID,
			ThreadID: update.Message.MessageThreadID,
			Text:     "워크스페이스로 사용 가능한 디렉토리가 없습니다.\n\n사용법: /new /path/to/workspace",
		})
		return nil
	}

	var sb strings.Builder
	sb.WriteString("📁 사용 가능한 워크스페이스:\n\n")
	for _, d := range dirs {
		sb.WriteString(fmt.Sprintf("• %s\n", d))
	}
	sb.WriteString(fmt.Sprintf("\n사용법: /new %s/<디렉토리명>", dirs[0][:strings.LastIndex(dirs[0], "/")+1]))

	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   update.Message.Chat.ID,
		ThreadID: update.Message.MessageThreadID,
		Text:     sb.String(),
	})
	return nil
}

// ============================================================================
// Helpers
// ============================================================================

// ensureTopic finds or creates a topic record for the given chat+thread.
func (r *Router) ensureTopic(ctx context.Context, chatID int64, threadID int64, name string) (*store.Topic, error) {
	topic, err := r.store.TopicByChatThread(chatID, threadID)
	if err != nil {
		return nil, err
	}
	if topic == nil {
		topic, err = r.store.InsertTopic(chatID, threadID, name)
		if err != nil {
			return nil, err
		}
	}
	return topic, nil
}

// formatDuration formats a duration in a human-readable way.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d초", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d분", int(d.Minutes()))
	}
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	if minutes > 0 {
		return fmt.Sprintf("%d시간 %d분", hours, minutes)
	}
	return fmt.Sprintf("%d시간", hours)
}

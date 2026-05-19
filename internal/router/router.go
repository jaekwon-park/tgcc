// Package router maps telegram topics to Claude Code sessions and dispatches messages.
package router

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jaekwon-park/tgcc/internal/acl"
	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/config"
	tmuxctx "github.com/jaekwon-park/tgcc/internal/context"
	"github.com/jaekwon-park/tgcc/internal/honcho"
	"github.com/jaekwon-park/tgcc/internal/session"
	"github.com/jaekwon-park/tgcc/internal/store"
)

// Router handles topic ↔ session mapping and message dispatch.
type Router struct {
	store        *store.Store
	logger       *slog.Logger
	sender       *bot.Sender
	guard        *acl.Guard
	pairingMgr   *acl.PairingManager
	mgr          *session.Manager
	ctxMon       *tmuxctx.Monitor
	honchoClient *honcho.HonchoClient
	groupConfigs []config.GroupConfig
	tgccTomlPath string
	exeDir       string
	botClient    *bot.Client
}

// NewRouter creates a new Router.
func NewRouter(st *store.Store, logger *slog.Logger, sender *bot.Sender, guard *acl.Guard, pairingMgr *acl.PairingManager, mgr *session.Manager, ctxMon *tmuxctx.Monitor, honchoClient *honcho.HonchoClient, groupConfigs []config.GroupConfig, tgccTomlPath, exeDir string, botClient *bot.Client) *Router {
	return &Router{store: st, logger: logger, sender: sender, guard: guard, pairingMgr: pairingMgr, mgr: mgr, ctxMon: ctxMon, honchoClient: honchoClient, groupConfigs: groupConfigs, tgccTomlPath: tgccTomlPath, exeDir: exeDir, botClient: botClient}
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
		return r.handleRegister(ctx, update, user, fields)
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
	// M6 context lifecycle commands
	case "/compact":
		return r.handleCompact(ctx, update, user)
	case "/ctxstatus":
		return r.handleCtxStatus(ctx, update, user)
	case "/refresh":
		return r.handleRefresh(ctx, update, user)
	// M9 squash + override commands
	case "/squash":
		return r.handleSquash(ctx, update, user, fields)
	case "/ctxconfig":
		return r.handleCtxConfig(ctx, update, user, fields)
	case "/list-archived":
		return r.handleListArchived(ctx, update, user)
	// ADM-984 topic model
	case "/model":
		return r.handleModel(ctx, update, user, fields)
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

	if sess.Status == "hibernated" {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "💭 메모리 복구 중...",
		})
		summary := ""
		if sess.TranscriptPath != "" {
			summary, _ = r.mgr.SummarizeLastNTurns(sess.TranscriptPath, 10)
		}
		honchoSessionID := topic.HonchoSessionID()
		summary = r.honchoClient.BuildResumeContext(ctx, honchoSessionID, summary)
		if _, err := r.mgr.FreshRestart(ctx, sess.ID, summary, chat.ID, threadID); err != nil {
			r.logger.Error("hibernate recovery failed", "error", err)
			r.sender.Enqueue(bot.OutgoingMsg{
				ChatID:   chat.ID,
				ThreadID: threadID,
				Text:     fmt.Sprintf("❌ 복구 실패: %v", err),
			})
		}
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
	if update.Message == nil || update.Message.From == nil || update.Message.Chat == nil {
		return nil
	}
	if update.Message.Chat.Type != "private" {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   update.Message.Chat.ID,
			ThreadID: update.Message.MessageThreadID,
			Text:     "❌ /pair 는 DM(1:1 채팅)에서만 사용할 수 있습니다.",
		})
		return nil
	}
	if user != nil && user.Role == "owner" {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   update.Message.Chat.ID,
			ThreadID: update.Message.MessageThreadID,
			Text:     "이미 인증된 사용자입니다. /help 로 명령을 확인하세요.",
		})
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
		Text:     "/start — 봇 소개\n/pair — 페어링 코드 발급\n/register — 그룹 등록\n/new [workspace] — 새 세션 생성\n/resume — 세션 복구\n/stop — 세션 종료\n/kill — 강제 종료\n/status — 세션 상태\n/list — 활성 세션 목록\n/workspaces — 사용 가능한 디렉토리 목록\n/compact — 컨텍스트 정리\n/ctxstatus — 컨텍스트 상태 확인\n/refresh — 세션 새로고침 (아카이브 후 새 세션)\n/squash N — 오래된 N턴 압축 (Honcho 필요)\n/ctxconfig soft_warn=N hard_compact=N — 토픽별 컨텍스트 임계치 설정\n/list-archived — 아카이브된 세션 목록\n/model — 현재 모델 확인\n/model <name> — 모델 변경 후 세션 재시작\n/help — 도움말\n/whoami — 본인 정보",
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
// Supports optional parameters:
//
//	honcho_session=<session-id>  — set honcho session ID for this topic
func (r *Router) handleRegister(ctx context.Context, update bot.Update, user *store.User, fields []string) error {
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

	// Parse optional parameters: honcho_session=<value>
	var honchoSessionID string
	for i := 1; i < len(fields); i++ {
		parts := strings.SplitN(fields[i], "=", 2)
		if len(parts) == 2 && parts[0] == "honcho_session" {
			honchoSessionID = parts[1]
		}
	}

	// If honcho_session_id is provided, set it on the topic (register implies topic creation or update)
	if honchoSessionID != "" {
		topic, err := r.ensureTopic(ctx, chat.ID, threadID, "")
		if err != nil {
			r.logger.Warn("ensure topic for honcho_session update failed", "error", err)
		} else if topic != nil {
			if err := r.store.UpdateTopicHonchoSession(topic.ID, honchoSessionID); err != nil {
				r.logger.Warn("update honcho_session_id failed", "error", err)
			}
		}
	}

	msg := "✅ 이 포럼 그룹이 등록되었습니다."
	if honchoSessionID != "" {
		msg += fmt.Sprintf("\nHoncho 세션: %s", honchoSessionID)
	}
	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chat.ID,
		ThreadID: threadID,
		Text:     msg,
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
	model := ""
	if topic.ClaudeModel.Valid {
		model = topic.ClaudeModel.String
	}
	sess, err := r.mgr.Spawn(ctx, topic.ID, workspacePath, chat.ID, threadID, model)
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
	case "hibernated":
		statusIcon = "💤"
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
		case "hibernated":
			icon = "💤"
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

		sb.WriteString(fmt.Sprintf("%s %s  %s  %s  %s\n",
			icon, topicName, s.Status, filepath.Base(s.WorkspacePath), s.ID[:8]))
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

// handleModel processes /model [name] command.
// No args: show current model for this topic.
// With arg: update model in DB, restart session if active.
func (r *Router) handleModel(ctx context.Context, update bot.Update, user *store.User, fields []string) error {
	if update.Message == nil || update.Message.Chat == nil {
		return nil
	}
	chat := update.Message.Chat
	threadID := update.Message.MessageThreadID

	topic, err := r.ensureTopic(ctx, chat.ID, threadID, "")
	if err != nil {
		return fmt.Errorf("ensure topic: %w", err)
	}

	if len(fields) < 2 {
		model := "기본값"
		if topic.ClaudeModel.Valid {
			model = topic.ClaudeModel.String
		}
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     fmt.Sprintf("현재 모델: %s", model),
		})
		return nil
	}

	newModel := fields[1]
	if err := r.store.UpdateTopicModel(topic.ID, newModel); err != nil {
		return fmt.Errorf("update topic model: %w", err)
	}

	sess, _ := r.mgr.GetSessionByTopic(topic.ID)
	if sess != nil {
		if killErr := r.mgr.Kill(ctx, sess.ID); killErr != nil {
			r.logger.Warn("kill session for model restart", "error", killErr)
		}
		if _, spawnErr := r.mgr.Spawn(ctx, topic.ID, sess.WorkspacePath, chat.ID, threadID, newModel); spawnErr != nil {
			r.logger.Error("respawn with new model failed", "error", spawnErr)
			r.sender.Enqueue(bot.OutgoingMsg{
				ChatID:   chat.ID,
				ThreadID: threadID,
				Text:     fmt.Sprintf("모델이 %s(으)로 변경되었으나 세션 재시작에 실패했습니다: %v\n/new로 수동 시작하세요.", newModel, spawnErr),
			})
			return spawnErr
		}
	}

	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chat.ID,
		ThreadID: threadID,
		Text:     fmt.Sprintf("모델이 %s(으)로 변경되었습니다. 세션을 재시작합니다.", newModel),
	})
	return nil
}

// ============================================================================
// M6 context lifecycle command handlers
// ============================================================================

// handleCompact processes /compact — trigger context compaction for the active session.
func (r *Router) handleCompact(ctx context.Context, update bot.Update, user *store.User) error {
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

	if err := r.ctxMon.CompactSession(ctx, sess.ID, chat.ID, threadID); err != nil {
		r.logger.Error("compact failed", "error", err)
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     fmt.Sprintf("❌ compact 실패: %v", err),
		})
		return err
	}
	return nil
}

// handleCtxStatus processes /ctxstatus — show context usage for the active session.
func (r *Router) handleCtxStatus(ctx context.Context, update bot.Update, user *store.User) error {
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
			Text:     "이 토픽에 세션이 없습니다.",
		})
		return nil
	}

	sess, err := r.mgr.GetSessionByTopic(topic.ID)
	if err != nil || sess == nil {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "이 토픽에 세션이 없습니다.",
		})
		return nil
	}

	// Parse topic-level overrides if present
	overrides, overrideErr := tmuxctx.ParseOverrides(topic.ContextOverrides)
	if overrideErr != nil {
		r.logger.Warn("parse context overrides failed", "error", overrideErr)
	}
	status := r.ctxMon.CtxStatusWithOverrides(ctx, sess.ID, overrides)
	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chat.ID,
		ThreadID: threadID,
		Text:     status,
	})
	return nil
}

// handleRefresh processes /refresh — archive current session and spawn fresh one.
func (r *Router) handleRefresh(ctx context.Context, update bot.Update, user *store.User) error {
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

	summary := ""
	if sess.TranscriptPath != "" {
		summary, _ = r.mgr.SummarizeLastNTurns(sess.TranscriptPath, 10)
	}
	honchoSessionID := topic.HonchoSessionID()
	summary = r.honchoClient.BuildResumeContext(ctx, honchoSessionID, summary)

	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chat.ID,
		ThreadID: threadID,
		Text:     "🔄 세션 새로고침 중...",
	})

	if _, err := r.mgr.FreshRestart(ctx, sess.ID, summary, chat.ID, threadID); err != nil {
		r.logger.Error("refresh failed", "error", err)
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     fmt.Sprintf("❌ 새로고침 실패: %v", err),
		})
		return err
	}

	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chat.ID,
		ThreadID: threadID,
		Text:     "🔄 세션 새로고침 완료",
	})
	return nil
}

// ============================================================================
// M9 squash + override command handlers
// ============================================================================

// handleSquash processes /squash N — compress oldest N turns via Honcho and fresh restart.
func (r *Router) handleSquash(ctx context.Context, update bot.Update, user *store.User, fields []string) error {
	if update.Message == nil || update.Message.Chat == nil {
		return nil
	}

	chat := update.Message.Chat
	threadID := update.Message.MessageThreadID

	// Parse N
	if len(fields) < 2 {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "사용법: /squash N (N=압축할 턴 수)",
		})
		return nil
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil || n <= 0 {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "N은 1 이상의 정수여야 합니다.",
		})
		return nil
	}

	// Find topic + session
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

	if sess.TranscriptPath == "" {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "트랜스크립트 파일이 없습니다. 세션이 너무 짧을 수 있습니다.",
		})
		return nil
	}

	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chat.ID,
		ThreadID: threadID,
		Text:     fmt.Sprintf("🗜️ 가장 오래된 %d턴 압축 시작...", n),
	})

	// Extract oldest N turns
	oldestTurns, remainingLines, totalTurns, err := r.mgr.SquashOldestNTurns(sess.TranscriptPath, n)
	if err != nil {
		r.logger.Error("squash: extract turns failed", "error", err)
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     fmt.Sprintf("❌ 트랜스크립트 추출 실패: %v", err),
		})
		return err
	}

	if totalTurns == 0 {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "압축할 턴이 없습니다.",
		})
		return nil
	}

	var summary string

	// Try Honcho compression
	if r.honchoClient != nil && r.honchoClient.IsEnabled() {
		honchoSessionID := fmt.Sprintf("tgcc-squash-%s", uuid.New().String())
		compressed, hErr := r.honchoClient.SummarizeTurns(ctx, honchoSessionID, oldestTurns)
		if hErr != nil {
			r.logger.Warn("squash: honcho summarization failed, using raw text", "error", hErr)
			// Fallback: use the oldest turns text as-is
			summary = oldestTurns
		} else {
			summary = compressed
		}
	} else {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "⚠️ Honcho 연동이 비활성화되어 있습니다. tgcc.toml에서 [honcho] enabled=true 설정이 필요합니다.\n원본 텍스트로 대체합니다.",
		})
		summary = oldestTurns
	}

	// Write squashed transcript
	newPath, err := r.mgr.WriteSquashedTranscript(sess.TranscriptPath, summary, remainingLines)
	if err != nil {
		r.logger.Error("squash: write transcript failed", "error", err)
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     fmt.Sprintf("❌ 트랜스크립트 저장 실패: %v", err),
		})
		return err
	}

	// Update session transcript path
	if err := r.store.UpdateSessionTranscriptPath(sess.ID, newPath); err != nil {
		r.logger.Warn("squash: update transcript path failed", "error", err)
	}

	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chat.ID,
		ThreadID: threadID,
		Text:     fmt.Sprintf("🗜️ 가장 오래된 %d턴 압축 완료\n전체 %d턴 → %d턴 보존", n, totalTurns, len(remainingLines)),
	})

	return nil
}

// handleCtxConfig processes /ctxconfig — set topic-level context threshold overrides.
func (r *Router) handleCtxConfig(ctx context.Context, update bot.Update, user *store.User, fields []string) error {
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
			Text:     "이 토픽이 등록되지 않았습니다. /new <workspace>로 시작하세요.",
		})
		return nil
	}

	// Show current overrides if no args
	if len(fields) < 2 {
		if topic.ContextOverrides != "" {
			r.sender.Enqueue(bot.OutgoingMsg{
				ChatID:   chat.ID,
				ThreadID: threadID,
				Text:     fmt.Sprintf("📊 현재 토픽 컨텍스트 설정:\n%s", topic.ContextOverrides),
			})
		} else {
			r.sender.Enqueue(bot.OutgoingMsg{
				ChatID:   chat.ID,
				ThreadID: threadID,
				Text:     "📊 토픽별 오버라이드가 설정되지 않았습니다. 전역 기본값을 사용 중입니다.\n\n설정: /ctxconfig soft_warn=120000 hard_compact=200000\n초기화: /ctxconfig reset",
			})
		}
		return nil
	}

	// Handle reset
	if fields[1] == "reset" {
		if err := r.store.UpdateTopicContextOverrides(topic.ID, ""); err != nil {
			r.logger.Error("ctxconfig: reset failed", "error", err)
			r.sender.Enqueue(bot.OutgoingMsg{
				ChatID:   chat.ID,
				ThreadID: threadID,
				Text:     fmt.Sprintf("❌ 초기화 실패: %v", err),
			})
			return err
		}
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "✅ 컨텍스트 오버라이드 초기화 완료. 전역 기본값을 사용합니다.",
		})
		return nil
	}

	// Parse key=value pairs
	overrides := &tmuxctx.ContextOverrides{}
	for _, f := range fields[1:] {
		parts := strings.SplitN(f, "=", 2)
		if len(parts) != 2 {
			r.sender.Enqueue(bot.OutgoingMsg{
				ChatID:   chat.ID,
				ThreadID: threadID,
				Text:     fmt.Sprintf("형식 오류: '%s'. key=value 형식으로 지정하세요.", f),
			})
			return nil
		}
		val, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || val <= 0 {
			r.sender.Enqueue(bot.OutgoingMsg{
				ChatID:   chat.ID,
				ThreadID: threadID,
				Text:     fmt.Sprintf("값 오류: '%s'는 유효한 양의 정수가 아닙니다.", parts[1]),
			})
			return nil
		}

		switch parts[0] {
		case "soft_warn":
			overrides.SoftWarnBytes = &val
		case "hard_compact":
			overrides.HardCompactBytes = &val
		default:
			r.sender.Enqueue(bot.OutgoingMsg{
				ChatID:   chat.ID,
				ThreadID: threadID,
				Text:     fmt.Sprintf("알 수 없는 키: '%s'. 사용 가능: soft_warn, hard_compact", parts[0]),
			})
			return nil
		}
	}

	// Save to DB
	jsonStr, err := overrides.ToJSON()
	if err != nil {
		r.logger.Error("ctxconfig: marshal failed", "error", err)
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     fmt.Sprintf("❌ 설정 저장 실패: %v", err),
		})
		return err
	}

	if err := r.store.UpdateTopicContextOverrides(topic.ID, jsonStr); err != nil {
		r.logger.Error("ctxconfig: update failed", "error", err)
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     fmt.Sprintf("❌ 설정 저장 실패: %v", err),
		})
		return err
	}

	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chat.ID,
		ThreadID: threadID,
		Text:     fmt.Sprintf("✅ 컨텍스트 오버라이드 설정 완료:\n%s", jsonStr),
	})

	return nil
}

// handleListArchived processes /list-archived — show archived sessions for this topic.
func (r *Router) handleListArchived(ctx context.Context, update bot.Update, user *store.User) error {
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
			Text:     "이 토픽에 등록된 세션이 없습니다.",
		})
		return nil
	}

	archived, err := r.store.ArchivedSessionsByTopic(topic.ID)
	if err != nil {
		r.logger.Error("list-archived: query failed", "error", err)
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     fmt.Sprintf("❌ 조회 실패: %v", err),
		})
		return nil
	}

	if len(archived) == 0 {
		r.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chat.ID,
			ThreadID: threadID,
			Text:     "📋 아카이브된 세션이 없습니다.",
		})
		return nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 아카이브된 세션 (%d개)\n\n", len(archived)))
	for _, s := range archived {
		sizeStr := "N/A"
		if s.TranscriptBytes > 0 {
			sizeStr = fmt.Sprintf("%d KB", s.TranscriptBytes/1024)
		}
		archivedAt := "unknown"
		if s.ArchivedAt > 0 {
			archivedAt = time.UnixMilli(s.ArchivedAt).Format("2006-01-02 15:04")
		}
		sb.WriteString(fmt.Sprintf("• %s  %s  %d턴  %s\n",
			s.ID[:8], sizeStr, s.TurnCount, archivedAt))
	}

	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chat.ID,
		ThreadID: threadID,
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
		// Auto-register if this chat belongs to a known group
		for _, gc := range r.groupConfigs {
			if gc.ChatID == chatID {
				if err := r.autoRegisterTopic(ctx, chatID, threadID, gc); err != nil {
					r.logger.Warn("auto-register topic failed", "chat_id", chatID, "thread_id", threadID, "error", err)
				}
				break
			}
		}
		// M1 fix: re-query after autoRegisterTopic updates fields
		// (WorkspacePath, HonchoSessionID) that the insert-time pointer lacks.
		topic, err = r.store.TopicByChatThread(chatID, threadID)
		if err != nil {
			return nil, err
		}
	}
	return topic, nil
}

// ============================================================================
// Auto-registration helpers (ADM-994)
// ============================================================================

// autoRegisterTopic performs full auto-registration for a new forum topic:
// fetches topic name via Telegram API, creates workspace, upserts DB, writes toml.
func (r *Router) autoRegisterTopic(ctx context.Context, chatID int64, threadID int64, gc config.GroupConfig) error {
	// Get topic name from Telegram API
	topicName := ""
	if r.botClient != nil {
		ft, err := r.botClient.GetForumTopicInfo(ctx, chatID, threadID)
		if err != nil {
			r.logger.Warn("getForumTopicInfo failed, using fallback", "chat_id", chatID, "thread_id", threadID, "error", err)
		} else if ft != nil && ft.Name != "" {
			topicName = ft.Name
		}
	}
	if topicName == "" {
		topicName = fmt.Sprintf("topic-%d", threadID)
	}

	// Create slug from topic name (M2: use topic-{threadID} as fallback for non-ASCII names)
	slug := slugifyName(topicName, fmt.Sprintf("topic-%d", threadID))

	// Build honcho session ID and workspace path
	// M5 fix: sanitize gc.Name to prevent path traversal (e.g. "../" in group name).
	groupSlug := slugifyName(gc.Name, "group")
	honchoSessionID := fmt.Sprintf("topic-%s", slug)
	workspacePath := filepath.Join(r.exeDir, "workspace", groupSlug, slug)

	// Verify workspace path stays within exeDir after cleaning
	cleanPath := filepath.Clean(workspacePath)
	cleanExeDir := filepath.Clean(r.exeDir)
	if !strings.HasPrefix(cleanPath, cleanExeDir+string(filepath.Separator)) {
		return fmt.Errorf("workspace path escapes exeDir: %s", cleanPath)
	}

	// Create workspace directory
	if err := os.MkdirAll(workspacePath, 0700); err != nil {
		return fmt.Errorf("create workspace dir: %w", err)
	}

	// Create CLAUDE.md template
	claudeMDPath := filepath.Join(workspacePath, "CLAUDE.md")
	if _, err := os.Stat(claudeMDPath); os.IsNotExist(err) {
		claudeMDContent := fmt.Sprintf("# %s\n\nThis workspace is auto-generated by tgcc.\n", topicName)
		if err := os.WriteFile(claudeMDPath, []byte(claudeMDContent), 0600); err != nil {
			return fmt.Errorf("create CLAUDE.md: %w", err)
		}
	}

	// DB upsert
	if _, err := r.store.UpsertTopicFull(chatID, threadID, topicName, honchoSessionID, "", workspacePath); err != nil {
		return fmt.Errorf("upsert topic: %w", err)
	}

	// tgcc.toml write-back
	if err := appendTopicToToml(r.tgccTomlPath, gc.ChatID, threadID, honchoSessionID, workspacePath); err != nil {
		r.logger.Warn("toml write-back failed", "error", err)
	}

	r.logger.Info("auto-registered topic", "chat_id", chatID, "thread_id", threadID, "name", topicName, "workspace", workspacePath)
	r.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chatID,
		ThreadID: threadID,
		Text:     fmt.Sprintf("✅ 새 토픽 자동 등록 완료\n이름: %s\n워크스페이스: %s", topicName, workspacePath),
	})
	return nil
}

// slugifyName converts a name to a URL-safe slug.
// Lowercase, spaces to hyphens, remove non-ASCII characters.
// M2 fix: accepts fallback string for when slug is empty (e.g. Korean-only names).
// Previously all non-ASCII names collapsed to "untitled", causing workspace/Honcho collisions.
func slugifyName(name string, fallback string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(name) {
		if r == ' ' || r == '-' || r == '_' {
			sb.WriteRune('-')
		} else if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			sb.WriteRune(r)
		}
	}
	slug := strings.Trim(sb.String(), "-")
	if slug == "" {
		if fallback != "" {
			slug = fallback
		} else {
			slug = "untitled"
		}
	}
	return slug
}

// appendTopicToToml appends a [[group.topic]] entry to the correct [[group]] block in tgcc.toml.
// H3 fix: checks for duplicate thread_id before appending to prevent re-registration dups.
func appendTopicToToml(path string, chatID, threadID int64, honchoSessionID, workspacePath string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no toml file, skip
		}
		return fmt.Errorf("read tgcc.toml: %w", err)
	}

	lines := strings.Split(string(content), "\n")

	// H3: check if thread_id already exists in the file — skip if duplicate
	threadIDStr := fmt.Sprintf("thread_id = %d", threadID)
	for _, line := range lines {
		if strings.Contains(strings.TrimSpace(line), threadIDStr) {
			return nil // already registered, skip
		}
	}

	// Build the topic entry
	topicEntry := fmt.Sprintf("  [[group.topic]]\n  thread_id         = %d\n  honcho_session_id = \"%s\"\n  workspace_path    = \"%s\"", threadID, honchoSessionID, workspacePath)

	// Find the target group by chat_id
	targetChatStr := fmt.Sprintf("chat_id = %d", chatID)
	insertPos := -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "chat_id") && strings.Contains(trimmed, targetChatStr) {
			// Found our target group — find where to insert (before next [[group]] or EOF)
			for j := i + 1; j < len(lines); j++ {
				if strings.HasPrefix(strings.TrimSpace(lines[j]), "[[group]]") {
					insertPos = j
					break
				}
			}
			if insertPos < 0 {
				insertPos = len(lines) // append at end
			}
			break
		}
	}

	// Build result with insertion
	var result strings.Builder
	for i, line := range lines {
		if i == insertPos {
			result.WriteString(topicEntry)
			result.WriteString("\n")
		}
		result.WriteString(line)
		if i < len(lines)-1 {
			result.WriteString("\n")
		}
	}
	if insertPos < 0 || insertPos >= len(lines) {
		// Group not found or target is the last group — append at end
		result.WriteString("\n")
		result.WriteString(topicEntry)
		result.WriteString("\n")
	}

	return os.WriteFile(path, []byte(result.String()), 0600)
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

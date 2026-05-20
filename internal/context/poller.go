// Package context — transcript poller: the primary mechanism for relaying
// Claude Code responses to Telegram. Unlike the Stop-hook relay (which fires
// once per turn and only forwards the single last assistant message), the
// poller scans each active session's transcript on a fixed interval and
// forwards EVERY new assistant text block since the last byte offset. This
// mirrors ccgram/ccbot's SessionMonitor and is resilient to missed hooks,
// multi-message turns, and offset races.
package context

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/store"
)

// pollInterval is how often the poller scans active transcripts.
const pollInterval = 2 * time.Second

// telegramMaxMessageLen is Telegram's hard per-message character cap.
const telegramMaxMessageLen = 4096

// activePollStatuses are the session statuses whose transcripts we tail.
var activePollStatuses = []string{"active", "idle", "compacting", "resuming"}

// Poller tails active session transcripts and relays new assistant text to Telegram.
type Poller struct {
	store     *store.Store
	sender    *bot.Sender
	logger    *slog.Logger
	typingMgr *bot.TypingManager
	home      string

	// fileMTimes caches the last-seen mtime per session id so unchanged files
	// are skipped without a read. Keyed by tgcc session id.
	fileMTimes map[string]int64
}

// NewPoller creates a transcript poller.
func NewPoller(st *store.Store, sender *bot.Sender, typingMgr *bot.TypingManager, logger *slog.Logger) *Poller {
	if logger == nil {
		logger = slog.Default()
	}
	home, _ := os.UserHomeDir()
	return &Poller{
		store:      st,
		sender:     sender,
		typingMgr:  typingMgr,
		logger:     logger,
		home:       home,
		fileMTimes: make(map[string]int64),
	}
}

// resumeArtifacts are assistant texts Claude Code emits on `--resume` for the
// synthetic "Continue from where you left off." continuation. They are not
// real responses to user input and must not be relayed to Telegram.
var resumeArtifacts = map[string]bool{
	"No response requested.": true,
}

// isRelayableText reports whether an assistant text should be forwarded.
func isRelayableText(text string) bool {
	return !resumeArtifacts[strings.TrimSpace(text)]
}

// Start runs the poll loop until ctx is cancelled.
func (p *Poller) Start(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	p.logger.Info("transcript poller starting", "interval", pollInterval.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pollOnce(ctx)
		}
	}
}

// pollOnce scans every active session once.
func (p *Poller) pollOnce(ctx context.Context) {
	sessions, err := p.store.ActiveSessions(activePollStatuses)
	if err != nil {
		p.logger.Warn("poller: list active sessions failed", "error", err)
		return
	}
	for _, sess := range sessions {
		if ctx.Err() != nil {
			return
		}
		p.pollSession(ctx, sess)
	}
}

// pollSession tails one session's transcript and relays any new assistant text.
func (p *Poller) pollSession(ctx context.Context, sess *store.Session) {
	transcriptPath := p.transcriptPath(sess)
	if transcriptPath == "" {
		return
	}

	st, err := os.Stat(transcriptPath)
	if err != nil {
		return // transcript not created yet
	}
	mtime := st.ModTime().UnixNano()
	size := st.Size()

	// Read current offset.
	var lastOffset int64
	off, oerr := p.store.GetMessageOffset(sess.ID)
	if oerr != nil {
		p.logger.Warn("poller: get offset failed", "session_id", sess.ID, "error", oerr)
	}
	if off != nil && off.LastHookEventID != "" {
		if _, err := fmt.Sscanf(off.LastHookEventID, "%d", &lastOffset); err != nil {
			lastOffset = 0
		}
	} else {
		// First time we see this session: start at EOF so we don't replay the
		// whole backlog (matches ccgram's "initialize offset to end of file").
		if err := p.store.UpsertMessageOffset(sess.ID, fmt.Sprintf("%d", size)); err != nil {
			p.logger.Warn("poller: init offset failed", "session_id", sess.ID, "error", err)
		}
		p.fileMTimes[sess.ID] = mtime
		return
	}

	// Skip unchanged files (mtime cache + size guard).
	if mtime <= p.fileMTimes[sess.ID] && size <= lastOffset {
		return
	}

	// Detect truncation / rotation (e.g. /clear started a new file).
	if size < lastOffset {
		lastOffset = 0
	}

	texts, newOffset, err := readNewAssistantTexts(transcriptPath, lastOffset)
	if err != nil {
		p.logger.Warn("poller: read transcript failed", "session_id", sess.ID, "error", err)
		return
	}
	p.fileMTimes[sess.ID] = mtime

	if len(texts) == 0 {
		// Advance the offset even when only non-text entries (tool calls,
		// system records) were appended, so we don't re-scan them forever.
		if newOffset > lastOffset {
			if err := p.store.UpsertMessageOffset(sess.ID, fmt.Sprintf("%d", newOffset)); err != nil {
				p.logger.Warn("poller: advance offset failed", "session_id", sess.ID, "error", err)
			}
		}
		return
	}

	topic, err := p.store.TopicByID(sess.TopicID)
	if err != nil || topic == nil {
		p.logger.Warn("poller: topic lookup failed", "session_id", sess.ID, "topic_id", sess.TopicID, "error", err)
		return
	}

	relayed := 0
	for _, text := range texts {
		if !isRelayableText(text) {
			continue // skip resume artifacts ("No response requested.")
		}
		for _, chunk := range splitMessage(text, telegramMaxMessageLen) {
			p.sender.Enqueue(bot.OutgoingMsg{
				ChatID:   topic.ChatID,
				ThreadID: topic.ThreadID,
				Text:     chunk,
			})
		}
		relayed++
	}

	// Response delivered — stop the "typing…" indicator for this topic.
	if relayed > 0 && p.typingMgr != nil {
		p.typingMgr.Clear(topic.ChatID, topic.ThreadID)
	}

	if err := p.store.UpsertMessageOffset(sess.ID, fmt.Sprintf("%d", newOffset)); err != nil {
		p.logger.Warn("poller: update offset failed", "session_id", sess.ID, "error", err)
	}
	if relayed > 0 {
		p.logger.Info("poller: relayed messages", "session_id", sess.ID, "count", relayed, "offset", newOffset)
	}
}

// transcriptPath derives the JSONL transcript path for a session. Prefers the
// hook-recorded path when present; otherwise reconstructs it from the workspace
// path and Claude session id the way Claude Code lays out ~/.claude/projects.
func (p *Poller) transcriptPath(sess *store.Session) string {
	if sess.TranscriptPath != "" {
		return sess.TranscriptPath
	}
	if sess.ClaudeSessionID == "" || sess.WorkspacePath == "" || p.home == "" {
		return ""
	}
	// Claude Code encodes the cwd by replacing every '/' with '-'. A leading
	// slash becomes a leading '-'. (e.g. /opt/tgcc → -opt-tgcc)
	encoded := strings.ReplaceAll(sess.WorkspacePath, "/", "-")
	return filepath.Join(p.home, ".claude", "projects", encoded, sess.ClaudeSessionID+".jsonl")
}

// readNewAssistantTexts reads the transcript from lastOffset to EOF and returns
// every assistant text block in order (not just the last one). Returns the new
// byte offset (EOF after the last fully-read line).
func readNewAssistantTexts(path string, lastOffset int64) (texts []string, newOffset int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, lastOffset, err
	}
	defer f.Close()

	if _, err := f.Seek(lastOffset, 0); err != nil {
		return nil, lastOffset, err
	}

	offset := lastOffset
	reader := bufio.NewReader(f)
	for {
		line, rerr := reader.ReadBytes('\n')
		if len(line) > 0 {
			// Only advance the committed offset for complete (newline-terminated)
			// lines so a half-written trailing line is re-read next cycle.
			if line[len(line)-1] == '\n' {
				offset += int64(len(line))
				if t, ok := extractAssistantText(line); ok {
					texts = append(texts, t)
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	return texts, offset, nil
}

// extractAssistantText returns the joined text blocks of an assistant entry.
func extractAssistantText(line []byte) (string, bool) {
	var entry transcriptEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return "", false
	}
	if entry.Type != "assistant" {
		return "", false
	}
	var msg assistantMessageContent
	if err := json.Unmarshal(entry.Message, &msg); err != nil {
		return "", false
	}
	var parts []string
	for _, b := range msg.Content {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

// splitMessage splits text into Telegram-safe chunks (<= maxLen), preferring
// newline boundaries and keeping fenced code blocks balanced across chunks.
// Port of ccgram's telegram_sender.split_message.
func splitMessage(text string, maxLen int) []string {
	if len([]rune(text)) <= maxLen {
		return []string{text}
	}

	var chunks []string
	var current strings.Builder
	inCodeBlock := false
	codeFence := ""

	flush := func(closeFence bool) {
		s := strings.TrimRight(current.String(), "\n")
		if closeFence && inCodeBlock {
			s += "\n```"
		}
		chunks = append(chunks, s)
		current.Reset()
	}

	for _, line := range strings.Split(text, "\n") {
		stripped := strings.TrimSpace(line)
		if strings.HasPrefix(stripped, "```") {
			if !inCodeBlock {
				inCodeBlock = true
				codeFence = stripped
			} else {
				inCodeBlock = false
			}
		}

		lineLen := len([]rune(line))
		switch {
		case lineLen > maxLen:
			// Single oversized line: flush current, then hard-split the line.
			if current.Len() > 0 {
				flush(true)
				if inCodeBlock {
					current.WriteString(codeFence + "\n")
				}
			}
			runes := []rune(line)
			for i := 0; i < len(runes); i += maxLen {
				end := i + maxLen
				if end > len(runes) {
					end = len(runes)
				}
				chunks = append(chunks, string(runes[i:end]))
			}
		case len([]rune(current.String()))+lineLen+1 > maxLen:
			flush(true)
			if inCodeBlock {
				current.WriteString(codeFence + "\n")
			}
			current.WriteString(line + "\n")
		default:
			current.WriteString(line + "\n")
		}
	}
	if current.Len() > 0 {
		flush(false)
	}
	return chunks
}

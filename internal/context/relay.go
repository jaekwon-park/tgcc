// Package context — transcript relay: reads Claude Code transcript JSONL,
// extracts the last assistant message, and relays it to Telegram.
package context

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/store"
)

// transcriptEntry represents a single line in the Claude Code transcript JSONL file.
type transcriptEntry struct {
	Type    string          `json:"type"`    // "user", "assistant", "system"
	Message json.RawMessage `json:"message"` // Anthropic Messages API message object
}

// assistantMessageContent is the minimal structure to extract text from an assistant message.
type assistantMessageContent struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

// contentBlock is a single content block in an assistant message.
type contentBlock struct {
	Type string `json:"type"` // "text", "tool_use", etc.
	Text string `json:"text,omitempty"`
}

// RelayResponse reads the transcript JSONL file, extracts the last assistant message,
// and if new (not previously relayed), sends it to the corresponding Telegram topic.
//
// Flow:
//  1. Look up session by sessionID → get topic_id
//  2. Look up topic by topic_id → get chat_id, thread_id
//  3. Read message_offsets for dedup
//  4. Read transcript JSONL from last offset
//  5. Extract last assistant text message
//  6. Send to Telegram via sender
//  7. Update message_offsets
func RelayResponse(ctx context.Context, st *store.Store, sender *bot.Sender, logger *slog.Logger, sessionID, transcriptPath string) error {
	// TODO: implement the full flow
	// 1. Look up session
	session, err := st.SessionByID(sessionID)
	if err != nil {
		return fmt.Errorf("relay: look up session: %w", err)
	}
	if session == nil {
		return fmt.Errorf("relay: session %s not found", sessionID)
	}

	// 2. Look up topic
	topic, err := st.TopicByID(session.TopicID)
	if err != nil {
		return fmt.Errorf("relay: look up topic: %w", err)
	}
	if topic == nil {
		return fmt.Errorf("relay: topic %d not found", session.TopicID)
	}

	// 3. Get last offset
	offset, err := st.GetMessageOffset(sessionID)
	if err != nil {
		logger.Warn("relay: get message offset failed", "error", err)
	}

	// 4. Read transcript and extract new assistant messages
	lastOffset := int64(0)
	if offset != nil && offset.LastHookEventID != "" {
		if _, err := fmt.Sscanf(offset.LastHookEventID, "%d", &lastOffset); err != nil {
			lastOffset = 0
		}
	}

	text, newOffset, err := readTranscriptTail(transcriptPath, lastOffset)
	if err != nil {
		return fmt.Errorf("relay: read transcript: %w", err)
	}

	if text == "" {
		// No new assistant message found
		return nil
	}

	// 5. Send to Telegram
	sender.Enqueue(bot.OutgoingMsg{
		ChatID:   topic.ChatID,
		ThreadID: topic.ThreadID,
		Text:     text,
	})

	// 6. Update offset
	newOffsetStr := fmt.Sprintf("%d", newOffset)
	if err := st.UpsertMessageOffset(sessionID, newOffsetStr); err != nil {
		logger.Warn("relay: upsert message offset failed", "error", err)
	}

	logger.Info("relay: response sent", "session_id", sessionID, "offset", newOffsetStr)
	return nil
}

// readTranscriptTail reads the transcript JSONL file from lastOffset to EOF,
// extracts the last assistant text message, and returns it along with the new file size.
// Returns empty string if no assistant message is found.
func readTranscriptTail(path string, lastOffset int64) (text string, newOffset int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", lastOffset, err
	}
	defer f.Close()

	if _, err := f.Seek(lastOffset, io.SeekStart); err != nil {
		return "", lastOffset, err
	}

	var lastText string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		var entry transcriptEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.Type != "assistant" {
			continue
		}
		var msg assistantMessageContent
		if err := json.Unmarshal(entry.Message, &msg); err != nil {
			continue
		}
		var parts []string
		for _, block := range msg.Content {
			if block.Type == "text" && block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		if len(parts) > 0 {
			lastText = strings.Join(parts, "\n")
		}
	}
	if err := scanner.Err(); err != nil {
		return "", lastOffset, err
	}

	newOffset, err = f.Seek(0, io.SeekCurrent)
	if err != nil {
		return "", lastOffset, err
	}

	return lastText, newOffset, nil
}

// Package bot handles Telegram Bot API communication.
package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const telegramBaseURL = "https://api.telegram.org"

// RetryAfterError is returned when Telegram API responds with 429 Too Many Requests.
// The After field indicates how long to wait before retrying.
type RetryAfterError struct {
	After time.Duration
}

func (e *RetryAfterError) Error() string {
	return fmt.Sprintf("telegram api error: 429 Too Many Requests (retry after %.0fs)", e.After.Seconds())
}

// Client is a thin wrapper around Telegram Bot API.
type Client struct {
	token      string
	httpClient *http.Client
	baseURL    string
}

// NewClient creates a new Telegram Bot API client.
func NewClient(token string) *Client {
	return &Client{
		token:      token,
		httpClient: &http.Client{Timeout: 60 * time.Second},
		baseURL:    telegramBaseURL,
	}
}

// Update represents a Telegram Bot API Update object.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
	// v0.2: CallbackQuery, etc.
}

// Message represents a Telegram message.
type Message struct {
	MessageID         int64              `json:"message_id"`
	From              *User              `json:"from,omitempty"`
	Chat              *Chat              `json:"chat"`
	Date              int64              `json:"date"`
	Text              string             `json:"text,omitempty"`
	MessageThreadID   int64              `json:"message_thread_id,omitempty"`
	IsTopicMessage    bool               `json:"is_topic_message,omitempty"`
	ForumTopicCreated *ForumTopicCreated `json:"forum_topic_created,omitempty"`
	Entities          []MessageEntity    `json:"entities,omitempty"`
	ReplyToMessage    *Message           `json:"reply_to_message,omitempty"`
}

// MessageEntity is a special entity in a text message (mentions, commands, etc.).
type MessageEntity struct {
	Type   string `json:"type"`   // "mention", "text_mention", "bot_command", ...
	Offset int    `json:"offset"` // UTF-16 code unit offset
	Length int    `json:"length"` // UTF-16 code unit length
	User   *User  `json:"user,omitempty"`
}

// User represents a Telegram user.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
}

// Chat represents a Telegram chat.
type Chat struct {
	ID      int64  `json:"id"`
	Type    string `json:"type"` // "private", "group", "supergroup"
	Title   string `json:"title,omitempty"`
	IsForum bool   `json:"is_forum,omitempty"`
}

// GetMe returns the bot's own account (used to learn the bot username for
// @mention detection).
func (c *Client) GetMe(ctx context.Context) (*User, error) {
	raw, err := c.apiRequest(ctx, "getMe", map[string]interface{}{})
	if err != nil {
		return nil, fmt.Errorf("getMe request failed: %w", err)
	}
	u := &User{}
	if err := json.Unmarshal(raw, u); err != nil {
		return nil, fmt.Errorf("decode getMe result: %w", err)
	}
	return u, nil
}

// SendChatAction shows a transient status (e.g. "typing…") in the chat/topic.
// Telegram auto-expires it after ~5s, so callers refresh periodically while
// work is in progress. Best-effort.
func (c *Client) SendChatAction(ctx context.Context, chatID int64, threadID int64, action string) error {
	params := map[string]interface{}{
		"chat_id": chatID,
		"action":  action,
	}
	if threadID > 0 {
		params["message_thread_id"] = threadID
	}
	if _, err := c.apiRequest(ctx, "sendChatAction", params); err != nil {
		return fmt.Errorf("sendChatAction request failed: %w", err)
	}
	return nil
}

// EditMessageText edits an existing message's text (used for the animated
// "thinking" bubble). parse_mode is left default (plain) since the bubble is
// short status text.
func (c *Client) EditMessageText(ctx context.Context, chatID, messageID int64, text string) error {
	params := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	}
	if _, err := c.apiRequest(ctx, "editMessageText", params); err != nil {
		return fmt.Errorf("editMessageText request failed: %w", err)
	}
	return nil
}

// DeleteMessage removes a message (used to clear the thinking bubble before
// the real response is sent).
func (c *Client) DeleteMessage(ctx context.Context, chatID, messageID int64) error {
	params := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
	}
	if _, err := c.apiRequest(ctx, "deleteMessage", params); err != nil {
		return fmt.Errorf("deleteMessage request failed: %w", err)
	}
	return nil
}

// GetUpdates fetches new updates using long-polling.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	params := map[string]interface{}{
		"offset":  offset,
		"limit":   10,
		"timeout": timeout,
	}

	raw, err := c.apiRequest(ctx, "getUpdates", params)
	if err != nil {
		return nil, fmt.Errorf("getUpdates request failed: %w", err)
	}

	updates := make([]Update, 0)
	if err := json.Unmarshal(raw, &updates); err != nil {
		return nil, fmt.Errorf("decode getUpdates result: %w", err)
	}
	return updates, nil
}

// htmlEscaper replaces characters that are reserved in Telegram HTML parse_mode.
var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// SendMessage sends a text message to a chat.
// L1 fix: HTML-escape text to prevent Telegram API errors from unescaped &, <, >.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, replyToMessageID int64, messageThreadID int64) (*Message, error) {
	escapedText := htmlEscaper.Replace(text)
	params := map[string]interface{}{
		"chat_id":    chatID,
		"text":       escapedText,
		"parse_mode": "HTML",
	}
	if replyToMessageID > 0 {
		params["reply_to_message_id"] = replyToMessageID
	}
	if messageThreadID > 0 {
		params["message_thread_id"] = messageThreadID
	}

	raw, err := c.apiRequest(ctx, "sendMessage", params)
	if err != nil {
		return nil, fmt.Errorf("sendMessage request failed: %w", err)
	}

	msg := &Message{}
	if err := json.Unmarshal(raw, msg); err != nil {
		return nil, fmt.Errorf("decode sendMessage result: %w", err)
	}
	return msg, nil
}

// SendDocument uploads a file to a chat/topic via multipart/form-data. Used by
// the outbox watcher to deliver files an agent drops in its workspace outbox.
// apiRequest can't be reused here since it serialises params as JSON, whereas
// sendDocument needs a streamed multipart body.
func (c *Client) SendDocument(ctx context.Context, chatID, threadID int64, filePath string) (*Message, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open document %s: %w", filePath, err)
	}
	defer f.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if err := writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return nil, fmt.Errorf("write chat_id field: %w", err)
	}
	if threadID > 0 {
		if err := writer.WriteField("message_thread_id", strconv.FormatInt(threadID, 10)); err != nil {
			return nil, fmt.Errorf("write message_thread_id field: %w", err)
		}
	}
	part, err := writer.CreateFormFile("document", filepath.Base(filePath))
	if err != nil {
		return nil, fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return nil, fmt.Errorf("copy document body: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close multipart writer: %w", err)
	}

	url := fmt.Sprintf("%s/bot%s/sendDocument", c.baseURL, c.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("create sendDocument request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("perform sendDocument request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read sendDocument response: %w", err)
	}

	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("decode sendDocument response: %w", err)
	}
	if !envelope.OK {
		if envelope.ErrorCode == 429 {
			after := time.Duration(envelope.Parameters.RetryAfter) * time.Second
			if after == 0 {
				after = time.Second // 429 without retry_after: 1s fallback
			}
			return nil, &RetryAfterError{After: after}
		}
		return nil, fmt.Errorf("telegram api error: %s", envelope.Description)
	}

	msg := &Message{}
	if err := json.Unmarshal(envelope.Result, msg); err != nil {
		return nil, fmt.Errorf("decode sendDocument result: %w", err)
	}
	return msg, nil
}

// apiRequest is a helper to call Telegram API endpoints.
func (c *Client) apiRequest(ctx context.Context, method string, params map[string]interface{}) (json.RawMessage, error) {
	url := fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.token, method)

	body := bytes.NewBuffer(nil)
	if err := json.NewEncoder(body).Encode(params); err != nil {
		return nil, fmt.Errorf("encode request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("perform request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("decode api response: %w", err)
	}
	if !envelope.OK {
		if envelope.ErrorCode == 429 {
			after := time.Duration(envelope.Parameters.RetryAfter) * time.Second
			if after == 0 {
				after = time.Second // 429 without retry_after: 1s fallback
			}
			return nil, &RetryAfterError{After: after}
		}
		if envelope.Description == "" {
			return nil, fmt.Errorf("telegram api error: ok=false")
		}
		return nil, fmt.Errorf("telegram api error: %s", envelope.Description)
	}
	return envelope.Result, nil
}

// ForumTopicCreated represents newly created forum topic information in a message.
type ForumTopicCreated struct {
	Name              string `json:"name"`
	IconColor         int64  `json:"icon_color"`
	IconCustomEmojiID string `json:"icon_custom_emoji_id,omitempty"`
}

// ForumTopic represents a forum topic.
type ForumTopic struct {
	MessageThreadID   int64  `json:"message_thread_id"`
	Name              string `json:"name"`
	IconColor         int64  `json:"icon_color"`
	IconCustomEmojiID string `json:"icon_custom_emoji_id,omitempty"`
}

// GetForumTopicInfo returns topic information for a forum thread.
func (c *Client) GetForumTopicInfo(ctx context.Context, chatID int64, messageThreadID int64) (*ForumTopic, error) {
	params := map[string]interface{}{
		"chat_id":           chatID,
		"message_thread_id": messageThreadID,
	}

	raw, err := c.apiRequest(ctx, "getForumTopicInfo", params)
	if err != nil {
		return nil, fmt.Errorf("getForumTopicInfo request failed: %w", err)
	}

	topic := &ForumTopic{}
	if err := json.Unmarshal(raw, topic); err != nil {
		return nil, fmt.Errorf("decode getForumTopicInfo result: %w", err)
	}
	return topic, nil
}

// Package bot handles Telegram Bot API communication.
package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const telegramBaseURL = "https://api.telegram.org"

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
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("decode api response: %w", err)
	}
	if !envelope.OK {
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

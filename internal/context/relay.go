// Package context — shared transcript JSONL types. Response relaying lives in
// poller.go (the transcript Poller); these types are the minimal shapes needed
// to extract assistant text from Claude Code transcript lines.
package context

import "encoding/json"

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

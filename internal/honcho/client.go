// Package honcho provides a minimal HTTP client for the Honcho context API.
// Implements representation retrieval for L2 crash recovery and /refresh.
package honcho

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

// HonchoConfig holds configuration for the Honcho client.
type HonchoConfig struct {
	Enabled   bool   `toml:"enabled"`
	BaseURL   string `toml:"url"`
	Workspace string `toml:"workspace"`
}

// DefaultHonchoConfig returns sensible defaults.
func DefaultHonchoConfig() HonchoConfig {
	return HonchoConfig{
		Enabled:   false,
		BaseURL:   "http://localhost:8000",
		Workspace: "work",
	}
}

// HonchoClient is a minimal HTTP client for the Honcho memory API.
// Only the representation endpoint is needed for L2 crash recovery.
type HonchoClient struct {
	baseURL    string
	workspace  string
	enabled    bool
	httpClient *http.Client
}

// New creates a new HonchoClient from the given configuration.
// Returns nil if Enabled is false.
func New(cfg HonchoConfig) *HonchoClient {
	if !cfg.Enabled {
		return nil
	}
	return &HonchoClient{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		workspace:  cfg.Workspace,
		enabled:    true,
		httpClient: &http.Client{},
	}
}

// IsEnabled returns true if the Honcho integration is active.
func (c *HonchoClient) IsEnabled() bool {
	if c == nil {
		return false
	}
	return c.enabled
}

// GetRepresentation fetches the Honcho representation for a given peer+session pair.
// Calls GET /v3/workspaces/{workspace}/sessions/{sessionID}/context
// Returns the representation text or an error.
func (c *HonchoClient) GetRepresentation(ctx context.Context, sessionID string) (string, error) {
	url := fmt.Sprintf("%s/v3/workspaces/%s/sessions/%s/context", c.baseURL, c.workspace, sessionID)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("honcho: create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("honcho: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("honcho: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("honcho: read body: %w", err)
	}

	var cr contextResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", fmt.Errorf("honcho: decode response: %w", err)
	}

	if cr.Context != "" {
		return cr.Context, nil
	}
	var parts []string
	if cr.PeerRepresentation != "" {
		parts = append(parts, cr.PeerRepresentation)
	}
	parts = append(parts, cr.PeerCard...)
	if len(cr.Summary) > 0 && string(cr.Summary) != "null" {
		var summary string
		if err := json.Unmarshal(cr.Summary, &summary); err != nil {
			var structured struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(cr.Summary, &structured); err != nil {
				return "", fmt.Errorf("honcho: decode summary: %w", err)
			}
			summary = structured.Content
		}
		if summary != "" {
			parts = append(parts, summary)
		}
	}
	for _, message := range cr.Messages {
		if message.Content != "" {
			parts = append(parts, message.PeerID+": "+message.Content)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// BuildResumeContext combines Honcho representation (if enabled) with the
// last-N-turns summary into a single context string for fresh restart.
// If Honcho is disabled or returns an error, only the summary is returned.
func (c *HonchoClient) BuildResumeContext(ctx context.Context, sessionID string, summary string) string {
	if !c.IsEnabled() {
		return summary
	}

	memory, err := c.GetRepresentation(ctx, sessionID)
	if err != nil || memory == "" {
		return summary
	}

	return fmt.Sprintf("[Honcho Memory]\n%s\n\n[Recent Conversation]\n%s", memory, summary)
}

// CreateMessage sends a user message to a Honcho session for dialectic processing.
// POST /v3/workspaces/{workspace}/sessions/{sessionID}/messages
func (c *HonchoClient) CreateMessage(ctx context.Context, sessionID, role, content string) error {
	runes := []rune(content)
	var messages []map[string]string
	for len(runes) > 25000 {
		messages = append(messages, map[string]string{"peer_id": role, "content": string(runes[:25000])})
		runes = runes[25000:]
	}
	messages = append(messages, map[string]string{"peer_id": role, "content": string(runes)})
	for len(messages) > 0 {
		count := min(len(messages), 100)
		if err := c.createMessages(ctx, sessionID, messages[:count]); err != nil {
			return err
		}
		messages = messages[count:]
	}
	return nil
}

func (c *HonchoClient) createMessages(ctx context.Context, sessionID string, messages []map[string]string) error {
	url := fmt.Sprintf("%s/v3/workspaces/%s/sessions/%s/messages", c.baseURL, c.workspace, sessionID)

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	body := map[string]any{"messages": messages}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("honcho: marshal message: %w", err)
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("honcho: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("honcho: message create failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("honcho: message create status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}

// SummarizeTurns sends a batch of conversation turns to a Honcho session
// for dialectic compression and returns the resulting context representation.
// sessionID should be a unique ID for this compression session.
func (c *HonchoClient) SummarizeTurns(ctx context.Context, sessionID, turnsText string) (string, error) {
	// Send the turns as a user message for dialectic processing
	if err := c.CreateMessage(ctx, sessionID, "user", turnsText); err != nil {
		return "", fmt.Errorf("honcho: send turns for summarization: %w", err)
	}

	// Retrieve the compressed context representation
	representation, err := c.GetRepresentation(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("honcho: get summary representation: %w", err)
	}

	return representation, nil
}

// contextResponse is the expected JSON shape from Honcho's /context endpoint.
type contextResponse struct {
	Context            string          `json:"context"`
	Summary            json.RawMessage `json:"summary"`
	PeerRepresentation string          `json:"peer_representation"`
	PeerCard           []string        `json:"peer_card"`
	Messages           []struct {
		PeerID  string `json:"peer_id"`
		Content string `json:"content"`
	} `json:"messages"`
}

// Package honcho provides a minimal HTTP client for the Honcho context API.
// Implements representation retrieval for L2 crash recovery and /refresh.
package honcho

import (
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
	BaseURL   string `toml:"base_url"`
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
		baseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		workspace: cfg.Workspace,
		enabled:   true,
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

	return cr.Context, nil
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

// contextResponse is the expected JSON shape from Honcho's /context endpoint.
type contextResponse struct {
	Context string `json:"context"`
}

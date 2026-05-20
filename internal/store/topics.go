package store

import (
	"database/sql"
	"fmt"
)

// Topic represents a telegram forum topic.
type Topic struct {
	ID               int64
	ChatID           int64
	ThreadID         int64
	Name             string
	WorkspacePath    string
	honchoSessionID  sql.NullString
	ContextOverrides string
	ClaudeModel      sql.NullString
	RequireMention   bool
	CreatedAt        int64
}

// Chat represents a registered telegram chat/group.
type Chat struct {
	ChatID       int64
	Title        string
	IsForum      bool
	RegisteredBy int64
	RegisteredAt int64
}

// InsertChat registers a new chat.
func (s *Store) InsertChat(chatID int64, title string, isForum bool, registeredBy int64) error {
	_, err := s.DB.Exec(
		`INSERT INTO chats (chat_id, title, is_forum, registered_by, registered_at) VALUES (?, ?, ?, ?, ?)`,
		chatID, title, isForum, registeredBy, CurrentTimeMs(),
	)
	return err
}

// ChatByID returns a chat by its telegram chat_id.
func (s *Store) ChatByID(chatID int64) (*Chat, error) {
	c := &Chat{}
	err := s.DB.QueryRow(
		`SELECT chat_id, title, is_forum, registered_by, registered_at FROM chats WHERE chat_id = ?`,
		chatID,
	).Scan(&c.ChatID, &c.Title, &c.IsForum, &c.RegisteredBy, &c.RegisteredAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return c, nil
}

// InsertTopic creates a new topic record.
func (s *Store) InsertTopic(chatID int64, threadID int64, name string) (*Topic, error) {
	now := CurrentTimeMs()
	res, err := s.DB.Exec(
		`INSERT INTO topics (chat_id, thread_id, name, workspace_path, honcho_session_id, context_overrides, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		chatID, threadID, name, nil, nil, nil, now,
	)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Topic{
		ID:        id,
		ChatID:    chatID,
		ThreadID:  threadID,
		Name:      name,
		CreatedAt: now,
	}, nil
}

// TopicByChatThread returns a topic by chat_id and thread_id.
func (s *Store) TopicByChatThread(chatID int64, threadID int64) (*Topic, error) {
	var workspace sql.NullString
	var honchoSession sql.NullString
	var ctxOverrides sql.NullString
	t := &Topic{}
	err := s.DB.QueryRow(
		`SELECT id, chat_id, thread_id, name, workspace_path, honcho_session_id, context_overrides, claude_model, require_mention, created_at FROM topics WHERE chat_id = ? AND thread_id = ?`,
		chatID, threadID,
	).Scan(&t.ID, &t.ChatID, &t.ThreadID, &t.Name, &workspace, &honchoSession, &ctxOverrides, &t.ClaudeModel, &t.RequireMention, &t.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if workspace.Valid {
		t.WorkspacePath = workspace.String
	}
	t.honchoSessionID = honchoSession
	if ctxOverrides.Valid {
		t.ContextOverrides = ctxOverrides.String
	}
	return t, nil
}

// TopicByID returns a topic by its internal ID.
func (s *Store) TopicByID(id int64) (*Topic, error) {
	var workspace sql.NullString
	var honchoSession sql.NullString
	var ctxOverrides sql.NullString
	t := &Topic{}
	err := s.DB.QueryRow(
		`SELECT id, chat_id, thread_id, name, workspace_path, honcho_session_id, context_overrides, claude_model, require_mention, created_at FROM topics WHERE id = ?`,
		id,
	).Scan(&t.ID, &t.ChatID, &t.ThreadID, &t.Name, &workspace, &honchoSession, &ctxOverrides, &t.ClaudeModel, &t.RequireMention, &t.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if workspace.Valid {
		t.WorkspacePath = workspace.String
	}
	t.honchoSessionID = honchoSession
	if ctxOverrides.Valid {
		t.ContextOverrides = ctxOverrides.String
	}
	return t, nil
}

// UpdateTopicRequireMention sets the require_mention flag for a topic.
func (s *Store) UpdateTopicRequireMention(topicID int64, require bool) error {
	_, err := s.DB.Exec(`UPDATE topics SET require_mention = ? WHERE id = ?`, require, topicID)
	return err
}

// UpdateTopicWorkspace sets the workspace_path for a topic.
func (s *Store) UpdateTopicWorkspace(topicID int64, workspacePath string) error {
	_, err := s.DB.Exec(`UPDATE topics SET workspace_path = ? WHERE id = ?`, workspacePath, topicID)
	return err
}

// UpdateTopicContextOverrides sets the context_overrides JSON for a topic.
func (s *Store) UpdateTopicContextOverrides(topicID int64, overridesJSON string) error {
	_, err := s.DB.Exec(`UPDATE topics SET context_overrides = ? WHERE id = ?`, nullString(overridesJSON), topicID)
	return err
}

// UpdateTopicName sets the name for a topic.
func (s *Store) UpdateTopicName(topicID int64, name string) error {
	_, err := s.DB.Exec(`UPDATE topics SET name = ? WHERE id = ?`, name, topicID)
	return err
}

// UpdateTopicHonchoSession sets the honcho_session_id for a topic.
func (s *Store) UpdateTopicHonchoSession(topicID int64, honchoSessionID string) error {
	_, err := s.DB.Exec(`UPDATE topics SET honcho_session_id = ? WHERE id = ?`, nullString(honchoSessionID), topicID)
	return err
}

// UpdateTopicModel sets the claude_model for a topic.
func (s *Store) UpdateTopicModel(topicID int64, model string) error {
	_, err := s.DB.Exec(`UPDATE topics SET claude_model = ? WHERE id = ?`, model, topicID)
	return err
}

// HonchoSessionID returns the effective Honcho session ID for this topic.
// If honcho_session_id is set in the DB, it returns that value.
// Otherwise, falls back to the legacy format "tgcc-topic-{ID}".
func (t *Topic) HonchoSessionID() string {
	if t.honchoSessionID.Valid && t.honchoSessionID.String != "" {
		return t.honchoSessionID.String
	}
	return fmt.Sprintf("tgcc-topic-%d", t.ID)
}

// UpsertTopic inserts or updates a topic record by (chat_id, thread_id).
func (s *Store) UpsertTopic(chatID int64, threadID int64, name string, model string) error {
	now := CurrentTimeMs()
	_, err := s.DB.Exec(
		`INSERT INTO topics (chat_id, thread_id, name, claude_model, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, thread_id) DO UPDATE SET name = excluded.name, claude_model = excluded.claude_model`,
		chatID, threadID, name, nullString(model), now,
	)
	return err
}

// UpsertTopicFull inserts or updates a topic record by (chat_id, thread_id),
// also setting name, honcho_session_id, claude_model, and workspace_path.
// Returns the topic's internal ID as a string.
func (s *Store) UpsertTopicFull(chatID, threadID int64, name, honchoSession, model, workspacePath string) (string, error) {
	existing, err := s.TopicByChatThread(chatID, threadID)
	if err != nil {
		return "", fmt.Errorf("lookup topic: %w", err)
	}

	if existing != nil {
		if name != "" {
			if err := s.UpdateTopicName(existing.ID, name); err != nil {
				return "", fmt.Errorf("update name: %w", err)
			}
		}
		if honchoSession != "" {
			if err := s.UpdateTopicHonchoSession(existing.ID, honchoSession); err != nil {
				return "", fmt.Errorf("update honcho_session: %w", err)
			}
		}
		if model != "" {
			if err := s.UpdateTopicModel(existing.ID, model); err != nil {
				return "", fmt.Errorf("update model: %w", err)
			}
		}
		if workspacePath != "" {
			if err := s.UpdateTopicWorkspace(existing.ID, workspacePath); err != nil {
				return "", fmt.Errorf("update workspace: %w", err)
			}
		}
		return fmt.Sprintf("%d", existing.ID), nil
	}

	if name == "" {
		name = fmt.Sprintf("topic-%d", threadID)
	}
	topic, err := s.InsertTopic(chatID, threadID, name)
	if err != nil {
		return "", fmt.Errorf("insert topic: %w", err)
	}

	if honchoSession != "" {
		if err := s.UpdateTopicHonchoSession(topic.ID, honchoSession); err != nil {
			return "", fmt.Errorf("update honcho_session: %w", err)
		}
	}
	if model != "" {
		if err := s.UpdateTopicModel(topic.ID, model); err != nil {
			return "", fmt.Errorf("update model: %w", err)
		}
	}
	if workspacePath != "" {
		if err := s.UpdateTopicWorkspace(topic.ID, workspacePath); err != nil {
			return "", fmt.Errorf("update workspace: %w", err)
		}
	}

	return fmt.Sprintf("%d", topic.ID), nil
}

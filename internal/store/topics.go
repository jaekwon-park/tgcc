package store

import "database/sql"

// Topic represents a telegram forum topic.
type Topic struct {
	ID            int64
	ChatID        int64
	ThreadID      int64
	Name          string
	WorkspacePath string
	CreatedAt     int64
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
		`INSERT INTO topics (chat_id, thread_id, name, workspace_path, created_at) VALUES (?, ?, ?, ?, ?)`,
		chatID, threadID, name, nil, now,
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
	t := &Topic{}
	err := s.DB.QueryRow(
		`SELECT id, chat_id, thread_id, name, workspace_path, created_at FROM topics WHERE chat_id = ? AND thread_id = ?`,
		chatID, threadID,
	).Scan(&t.ID, &t.ChatID, &t.ThreadID, &t.Name, &workspace, &t.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if workspace.Valid {
		t.WorkspacePath = workspace.String
	}
	return t, nil
}

// TopicByID returns a topic by its internal ID.
func (s *Store) TopicByID(id int64) (*Topic, error) {
	var workspace sql.NullString
	t := &Topic{}
	err := s.DB.QueryRow(
		`SELECT id, chat_id, thread_id, name, workspace_path, created_at FROM topics WHERE id = ?`,
		id,
	).Scan(&t.ID, &t.ChatID, &t.ThreadID, &t.Name, &workspace, &t.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if workspace.Valid {
		t.WorkspacePath = workspace.String
	}
	return t, nil
}

// UpdateTopicWorkspace sets the workspace_path for a topic.
func (s *Store) UpdateTopicWorkspace(topicID int64, workspacePath string) error {
	_, err := s.DB.Exec(`UPDATE topics SET workspace_path = ? WHERE id = ?`, workspacePath, topicID)
	return err
}

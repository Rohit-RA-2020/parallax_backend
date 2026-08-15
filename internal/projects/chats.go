package projects

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"parallax/internal/llm"
)

var ErrChatNotFound = errors.New("chat not found")

type ChatMeta struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Chat struct {
	ChatMeta
	Messages []llm.Message `json:"messages"`
}

type chatIndex struct {
	Chats []ChatMeta `json:"chats"`
}

func (s *Store) ListChats(projectID string) ([]ChatMeta, error) {
	p, err := s.Get(projectID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, err := readChatIndex(p)
	if err != nil {
		return nil, err
	}
	out := append([]ChatMeta(nil), idx.Chats...)
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

func (s *Store) CreateChat(projectID, title string) (Chat, error) {
	p, err := s.Get(projectID)
	if err != nil {
		return Chat{}, err
	}
	now := time.Now().UTC()
	chat := Chat{
		ChatMeta: ChatMeta{
			ID:        newID(),
			Title:     chatTitle(title, nil),
			CreatedAt: now,
			UpdatedAt: now,
		},
		Messages: []llm.Message{},
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeChat(p, chat); err != nil {
		return Chat{}, err
	}
	if err := upsertChatMeta(p, chat.ChatMeta); err != nil {
		return Chat{}, err
	}
	return chat, nil
}

func (s *Store) GetChat(projectID, chatID string) (Chat, error) {
	p, err := s.Get(projectID)
	if err != nil {
		return Chat{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readChat(p, chatID)
}

func (s *Store) GetOrCreateChat(projectID, chatID string) (Chat, error) {
	p, err := s.Get(projectID)
	if err != nil {
		return Chat{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(chatID) != "" {
		chat, err := readChat(p, chatID)
		if err == nil {
			return chat, nil
		}
		if !errors.Is(err, ErrChatNotFound) {
			return Chat{}, err
		}
	}
	now := time.Now().UTC()
	chat := Chat{
		ChatMeta: ChatMeta{
			ID:        newID(),
			Title:     "New chat",
			CreatedAt: now,
			UpdatedAt: now,
		},
		Messages: []llm.Message{},
	}
	if err := writeChat(p, chat); err != nil {
		return Chat{}, err
	}
	if err := upsertChatMeta(p, chat.ChatMeta); err != nil {
		return Chat{}, err
	}
	return chat, nil
}

func (s *Store) SaveChatMessages(projectID, chatID string, msgs []llm.Message) (Chat, error) {
	p, err := s.Get(projectID)
	if err != nil {
		return Chat{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	chat, err := readChat(p, chatID)
	if err != nil {
		return Chat{}, err
	}
	chat.Messages = append([]llm.Message(nil), msgs...)
	chat.UpdatedAt = time.Now().UTC()
	if title := chatTitle(chat.Title, msgs); title != chat.Title {
		chat.Title = title
	}
	if err := writeChat(p, chat); err != nil {
		return Chat{}, err
	}
	if err := upsertChatMeta(p, chat.ChatMeta); err != nil {
		return Chat{}, err
	}
	return chat, nil
}

func (s *Store) RenameChat(projectID, chatID, title string) (Chat, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Chat{}, errors.New("chat title is required")
	}
	if utf8.RuneCountInString(title) > 80 {
		return Chat{}, errors.New("chat title is too long")
	}
	p, err := s.Get(projectID)
	if err != nil {
		return Chat{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	chat, err := readChat(p, chatID)
	if err != nil {
		return Chat{}, err
	}
	chat.Title = title
	chat.UpdatedAt = time.Now().UTC()
	if err := writeChat(p, chat); err != nil {
		return Chat{}, err
	}
	if err := upsertChatMeta(p, chat.ChatMeta); err != nil {
		return Chat{}, err
	}
	return chat, nil
}

func (s *Store) DeleteChat(projectID, chatID string) error {
	p, err := s.Get(projectID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := readChat(p, chatID); err != nil {
		return err
	}
	if err := os.Remove(chatPath(p, chatID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	idx, err := readChatIndex(p)
	if err != nil {
		return err
	}
	next := idx.Chats[:0]
	for _, item := range idx.Chats {
		if item.ID != chatID {
			next = append(next, item)
		}
	}
	idx.Chats = next
	return writeChatIndex(p, idx)
}

func chatsDir(p Project) string {
	return filepath.Join(p.Dir, ".parallax", "chats")
}

func chatPath(p Project, id string) string {
	return filepath.Join(chatsDir(p), id+".json")
}

func chatIndexPath(p Project) string {
	return filepath.Join(chatsDir(p), "index.json")
}

func readChatIndex(p Project) (chatIndex, error) {
	b, err := os.ReadFile(chatIndexPath(p))
	if err != nil {
		if os.IsNotExist(err) {
			return chatIndex{Chats: []ChatMeta{}}, nil
		}
		return chatIndex{}, err
	}
	var idx chatIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return chatIndex{}, err
	}
	if idx.Chats == nil {
		idx.Chats = []ChatMeta{}
	}
	return idx, nil
}

func writeChatIndex(p Project, idx chatIndex) error {
	if err := os.MkdirAll(chatsDir(p), 0o700); err != nil {
		return err
	}
	if idx.Chats == nil {
		idx.Chats = []ChatMeta{}
	}
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	tmp := chatIndexPath(p) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, chatIndexPath(p))
}

func upsertChatMeta(p Project, meta ChatMeta) error {
	idx, err := readChatIndex(p)
	if err != nil {
		return err
	}
	found := false
	for i, item := range idx.Chats {
		if item.ID == meta.ID {
			idx.Chats[i] = meta
			found = true
			break
		}
	}
	if !found {
		idx.Chats = append(idx.Chats, meta)
	}
	return writeChatIndex(p, idx)
}

func readChat(p Project, id string) (Chat, error) {
	if id == "" || strings.ContainsAny(id, `/\`) {
		return Chat{}, ErrChatNotFound
	}
	b, err := os.ReadFile(chatPath(p, id))
	if err != nil {
		if os.IsNotExist(err) {
			return Chat{}, ErrChatNotFound
		}
		return Chat{}, err
	}
	var chat Chat
	if err := json.Unmarshal(b, &chat); err != nil {
		return Chat{}, err
	}
	if chat.ID != id {
		return Chat{}, ErrChatNotFound
	}
	if chat.Messages == nil {
		chat.Messages = []llm.Message{}
	}
	return chat, nil
}

func writeChat(p Project, chat Chat) error {
	if err := os.MkdirAll(chatsDir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(chat, "", "  ")
	if err != nil {
		return err
	}
	tmp := chatPath(p, chat.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, chatPath(p, chat.ID))
}

func chatTitle(current string, msgs []llm.Message) string {
	current = strings.TrimSpace(current)
	if current != "" && !isDefaultChatTitle(current) {
		return current
	}
	for _, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		if title := firstLineTitle(m.Content); title != "" {
			return title
		}
	}
	if current != "" {
		return current
	}
	return "New chat"
}

func isDefaultChatTitle(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "new chat", "untitled", "director":
		return true
	}
	return false
}

func firstLineTitle(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexAny(s, "\n\r"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if space || b.Len() == 0 {
				continue
			}
			space = true
			b.WriteByte(' ')
			continue
		}
		space = false
		b.WriteRune(r)
		if utf8.RuneCountInString(b.String()) >= 48 {
			return strings.TrimSpace(b.String()) + "…"
		}
	}
	return strings.TrimSpace(b.String())
}

func PublicChatMessages(msgs []llm.Message) []map[string]string {
	out := make([]map[string]string, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != llm.RoleUser && m.Role != llm.RoleAssistant {
			continue
		}
		text := strings.TrimSpace(m.Content)
		if text == "" {
			continue
		}
		out = append(out, map[string]string{
			"role":    string(m.Role),
			"content": text,
		})
	}
	return out
}

package projects

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	Messages          []llm.Message               `json:"messages"`
	ResponseDurations map[string]int64            `json:"response_durations,omitempty"`
	ResponseTraces    map[string][]ChatTraceEvent `json:"response_traces,omitempty"`
}

type ChatTraceEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type chatIndex struct {
	Chats []ChatMeta `json:"chats"`
}

func (s *Store) ListChats(projectID string) ([]ChatMeta, error) {
	if s.pg != nil {
		if _, err := s.Get(projectID); err != nil {
			return nil, err
		}
		rows, err := s.pg.Query(context.Background(), `SELECT id,title,created_at,updated_at FROM chats WHERE project_id=$1 ORDER BY updated_at DESC`, projectID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []ChatMeta{}
		for rows.Next() {
			var m ChatMeta
			if err = rows.Scan(&m.ID, &m.Title, &m.CreatedAt, &m.UpdatedAt); err != nil {
				return nil, err
			}
			out = append(out, m)
		}
		return out, rows.Err()
	}
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
	out := append([]ChatMeta{}, idx.Chats...)
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

func (s *Store) CreateChat(projectID, title string) (Chat, error) {
	if s.pg != nil {
		p, err := s.Get(projectID)
		if err != nil {
			return Chat{}, err
		}
		id := uuid.New()
		owner, err := uuid.Parse(p.OwnerID)
		if err != nil {
			return Chat{}, err
		}
		now := time.Now().UTC()
		c := Chat{ChatMeta: ChatMeta{ID: id.String(), Title: chatTitle(title, nil), CreatedAt: now, UpdatedAt: now}, Messages: []llm.Message{}}
		_, err = s.pg.Exec(context.Background(), `INSERT INTO chats(id,project_id,created_by,title,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5)`, id, projectID, owner, c.Title, now)
		return c, err
	}
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

// SetChatResponseMetadata records elapsed time and a compact event trace for
// the final assistant turn of one Director request. Keys are message indexes
// so existing chat files remain readable and repeated response text does not
// collide.
func (s *Store) SetChatResponseMetadata(projectID, chatID string, msgs []llm.Message, durationMS int64, trace []ChatTraceEvent) error {
	if s.pg != nil {
		if _, err := s.Get(projectID); err != nil {
			return err
		}
		index := -1
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == llm.RoleAssistant && strings.TrimSpace(msgs[i].Content) != "" {
				index = i
				break
			}
		}
		if index < 0 {
			return nil
		}
		_, err := s.pg.Exec(context.Background(), `UPDATE chat_messages SET response_duration_ms=$4 WHERE id=(SELECT id FROM chat_messages WHERE chat_id=$1 AND sequence_no=$2) AND EXISTS(SELECT 1 FROM chats WHERE id=$1 AND project_id=$3)`, chatID, index, projectID, durationMS)
		if err != nil {
			return err
		}
		if len(trace) > 0 {
			runID := uuid.New()
			p, _ := s.Get(projectID)
			owner, _ := uuid.Parse(p.OwnerID)
			_, _ = s.pg.Exec(context.Background(), `INSERT INTO agent_runs(id,project_id,chat_id,user_id,state,started_at,completed_at) VALUES($1,$2,$3,$4,'completed',now(),now())`, runID, projectID, chatID, owner)
			for i, event := range trace {
				_, _ = s.pg.Exec(context.Background(), `INSERT INTO agent_events(run_id,sequence_no,event_type,payload) VALUES($1,$2,$3,$4)`, runID, i, event.Type, event.Data)
			}
		}
		return nil
	}
	if durationMS < 0 {
		durationMS = 0
	}
	p, err := s.Get(projectID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	chat, err := readChat(p, chatID)
	if err != nil {
		return err
	}
	index := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleAssistant && strings.TrimSpace(msgs[i].Content) != "" {
			index = i
			break
		}
	}
	if index < 0 {
		return nil
	}
	if chat.ResponseDurations == nil {
		chat.ResponseDurations = map[string]int64{}
	}
	chat.ResponseDurations[strconv.Itoa(index)] = durationMS
	if len(trace) > 0 {
		if chat.ResponseTraces == nil {
			chat.ResponseTraces = map[string][]ChatTraceEvent{}
		}
		chat.ResponseTraces[strconv.Itoa(index)] = append([]ChatTraceEvent(nil), trace...)
	}
	return writeChat(p, chat)
}

func (s *Store) GetChat(projectID, chatID string) (Chat, error) {
	if s.pg != nil {
		if _, err := s.Get(projectID); err != nil {
			return Chat{}, err
		}
		var c Chat
		err := s.pg.QueryRow(context.Background(), `SELECT id,title,created_at,updated_at FROM chats WHERE id=$1 AND project_id=$2`, chatID, projectID).Scan(&c.ID, &c.Title, &c.CreatedAt, &c.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return Chat{}, ErrChatNotFound
		}
		if err != nil {
			return Chat{}, err
		}
		rows, err := s.pg.Query(context.Background(), `SELECT sequence_no,message,response_duration_ms FROM chat_messages WHERE chat_id=$1 ORDER BY sequence_no`, chatID)
		if err != nil {
			return Chat{}, err
		}
		defer rows.Close()
		c.Messages = []llm.Message{}
		c.ResponseDurations = map[string]int64{}
		for rows.Next() {
			var seq int
			var body []byte
			var duration *int64
			if err = rows.Scan(&seq, &body, &duration); err != nil {
				return Chat{}, err
			}
			var msg llm.Message
			if err = json.Unmarshal(body, &msg); err != nil {
				return Chat{}, err
			}
			c.Messages = append(c.Messages, msg)
			if duration != nil {
				c.ResponseDurations[strconv.Itoa(seq)] = *duration
			}
		}
		return c, rows.Err()
	}
	p, err := s.Get(projectID)
	if err != nil {
		return Chat{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readChat(p, chatID)
}

func (s *Store) GetOrCreateChat(projectID, chatID string) (Chat, error) {
	if s.pg != nil {
		if strings.TrimSpace(chatID) != "" {
			c, err := s.GetChat(projectID, chatID)
			if err == nil {
				return c, nil
			}
			if !errors.Is(err, ErrChatNotFound) {
				return Chat{}, err
			}
		}
		return s.CreateChat(projectID, "")
	}
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
	if s.pg != nil {
		c, err := s.GetChat(projectID, chatID)
		if err != nil {
			return Chat{}, err
		}
		tx, err := s.pg.Begin(context.Background())
		if err != nil {
			return Chat{}, err
		}
		defer tx.Rollback(context.Background())
		if _, err = tx.Exec(context.Background(), `DELETE FROM chat_messages WHERE chat_id=$1`, chatID); err != nil {
			return Chat{}, err
		}
		for i, msg := range msgs {
			body, mErr := json.Marshal(msg)
			if mErr != nil {
				return Chat{}, mErr
			}
			messageID := uuid.New()
			if _, err = tx.Exec(context.Background(), `INSERT INTO chat_messages(id,chat_id,sequence_no,role,message) VALUES($1,$2,$3,$4,$5)`, messageID, chatID, i, msg.Role, body); err != nil {
				return Chat{}, err
			}
			for _, image := range msg.Images {
				path := filepath.ToSlash(strings.TrimSpace(image.Path))
				if path == "" {
					continue
				}
				_, err = tx.Exec(context.Background(), `INSERT INTO chat_attachments(id,message_id,storage_object_id,name,mime_type)
					SELECT $1,$2,v.storage_object_id,$3,$4 FROM assets a JOIN asset_versions v ON v.id=a.current_version_id WHERE a.project_id=$5 AND a.logical_path=$6 AND a.deleted_at IS NULL`, uuid.New(), messageID, image.Name, image.MIME, projectID, path)
				if err != nil {
					return Chat{}, err
				}
			}
		}
		title := chatTitle(c.Title, msgs)
		if _, err = tx.Exec(context.Background(), `UPDATE chats SET title=$2,updated_at=now() WHERE id=$1`, chatID, title); err != nil {
			return Chat{}, err
		}
		if err = tx.Commit(context.Background()); err != nil {
			return Chat{}, err
		}
		c.Messages = append([]llm.Message(nil), msgs...)
		c.Title = title
		c.UpdatedAt = time.Now().UTC()
		return c, nil
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
	chat.Messages = append([]llm.Message(nil), msgs...)
	pruneChatMetadata(&chat)
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
	if s.pg != nil {
		if _, err := s.Get(projectID); err != nil {
			return Chat{}, err
		}
		result, err := s.pg.Exec(context.Background(), `UPDATE chats SET title=$3,updated_at=now() WHERE id=$1 AND project_id=$2`, chatID, projectID, title)
		if err != nil {
			return Chat{}, err
		}
		if result.RowsAffected() == 0 {
			return Chat{}, ErrChatNotFound
		}
		return s.GetChat(projectID, chatID)
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
	if s.pg != nil {
		if _, err := s.Get(projectID); err != nil {
			return err
		}
		result, err := s.pg.Exec(context.Background(), `DELETE FROM chats WHERE id=$1 AND project_id=$2`, chatID, projectID)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return ErrChatNotFound
		}
		return nil
	}
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
		if len(m.Images) > 0 {
			if name := strings.TrimSpace(m.Images[0].Name); name != "" {
				return firstLineTitle(name)
			}
			return "Attached image"
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

func pruneChatMetadata(chat *Chat) {
	limit := len(chat.Messages)
	pruneIndexMap(chat.ResponseDurations, limit)
	if chat.ResponseTraces != nil {
		for key := range chat.ResponseTraces {
			index, err := strconv.Atoi(key)
			if err != nil || index < 0 || index >= limit {
				delete(chat.ResponseTraces, key)
			}
		}
		if len(chat.ResponseTraces) == 0 {
			chat.ResponseTraces = nil
		}
	}
}

func pruneIndexMap(values map[string]int64, limit int) {
	if values == nil {
		return
	}
	for key := range values {
		index, err := strconv.Atoi(key)
		if err != nil || index < 0 || index >= limit {
			delete(values, key)
		}
	}
}

func PublicChatMessages(projectID string, msgs []llm.Message, durations map[string]int64, traces map[string][]ChatTraceEvent) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for index, m := range msgs {
		if m.Role != llm.RoleUser && m.Role != llm.RoleAssistant {
			continue
		}
		text := strings.TrimSpace(m.Content)
		if text == "" && len(m.Images) == 0 {
			continue
		}
		item := map[string]any{
			"role":    string(m.Role),
			"content": text,
		}
		if images := publicChatImages(projectID, m.Images); len(images) > 0 {
			item["images"] = images
		}
		if m.Role == llm.RoleAssistant {
			if duration, ok := durations[strconv.Itoa(index)]; ok && duration > 0 {
				item["worked_ms"] = duration
			}
			if trace, ok := traces[strconv.Itoa(index)]; ok && len(trace) > 0 {
				item["trace_events"] = trace
			}
		}
		out = append(out, item)
	}
	return out
}

func publicChatImages(projectID string, images []llm.ImageRef) []map[string]any {
	if len(images) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(images))
	for _, img := range images {
		path := filepath.ToSlash(strings.TrimSpace(img.Path))
		if path == "" {
			continue
		}
		item := map[string]any{
			"path": path,
			"name": img.Name,
			"mime": img.MIME,
		}
		if projectID != "" {
			item["url"] = chatFileURL(projectID, path)
		}
		out = append(out, item)
	}
	return out
}

func chatFileURL(projectID, path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return "/v1/projects/" + url.PathEscape(projectID) + "/files/" + strings.Join(parts, "/")
}

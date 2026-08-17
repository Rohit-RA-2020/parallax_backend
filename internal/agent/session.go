package agent

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"parallax/internal/llm"
)

// Session is one conversation plus its tool-call history.
type Session struct {
	ID        string        `json:"id"`
	ProjectID string        `json:"project_id,omitempty"`
	Messages  []llm.Message `json:"messages"`
	UpdatedAt time.Time     `json:"updated_at"`
}

// Store is an in-memory session map. Fine for a single-node first slice.
type Store struct {
	mu   sync.RWMutex
	data map[string]*Session
}

func NewStore() *Store {
	return &Store{data: map[string]*Session{}}
}

func (s *Store) GetOrCreate(id string) *Session {
	return s.GetOrCreateForProject(id, "")
}

func (s *Store) GetOrCreateForProject(id, projectID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != "" {
		if sess, ok := s.data[id]; ok && sess.ProjectID == projectID {
			return sess
		}
	}
	sess := &Session{
		ID:        newID(),
		ProjectID: projectID,
		Messages:  []llm.Message{{Role: llm.RoleSystem, Content: SystemPrompt}},
		UpdatedAt: time.Now(),
	}
	s.data[sess.ID] = sess
	return sess
}

func (s *Store) Remember(sess *Session) {
	if sess == nil || sess.ID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[sess.ID] = sess
}

func (s *Store) Get(id string) (*Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.data[id]
	return sess, ok
}

func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, id)
}

// DeleteProject drops every in-memory session that belongs to a project.
func (s *Store) DeleteProject(projectID string) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.data {
		if sess != nil && sess.ProjectID == projectID {
			delete(s.data, id)
		}
	}
}

func (s *Store) ReplaceMessages(id string, msgs []llm.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.data[id]
	if !ok {
		return
	}
	sess.Messages = msgs
	sess.UpdatedAt = time.Now()
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// PublicHistory strips the system prompt for API responses.
func PublicHistory(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == llm.RoleSystem {
			continue
		}
		out = append(out, m)
	}
	return out
}

// Trim keeps the system prompt and a suffix of complete conversation turns.
// Starting at a user message is important for providers such as Gemini, which
// reject orphaned function calls or function responses at the start of replayed
// history.
func Trim(msgs []llm.Message, max int) []llm.Message {
	if max < 4 || len(msgs) <= max {
		return msgs
	}
	var system []llm.Message
	rest := msgs
	if len(msgs) > 0 && msgs[0].Role == llm.RoleSystem {
		system = msgs[:1]
		rest = msgs[1:]
	}
	budget := max - len(system)
	start := len(rest) - budget
	if start < 0 {
		start = 0
	}
	for start < len(rest) && rest[start].Role != llm.RoleUser {
		start++
	}
	return append(append([]llm.Message{}, system...), rest[start:]...)
}

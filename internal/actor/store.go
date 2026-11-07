package actor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// Who titled a session.
const (
	// Cohesion started it with a title, or picked it up from a thread gone:
	// it is never replaced.
	TitledGiven = "given"
	// Cohesion took the prompt's first line, so Codex's own name replaces it.
	TitledPending = "pending"
)

// Record is one session, by Cohesion's id for it.
type Record struct {
	// The actor's identifier.
	Actor string `json:"actor"`
	// The Codex thread the session runs in; empty until Codex has started it.
	ThreadID string `json:"thread_id,omitempty"`
	Mode     string `json:"mode"`
	// Empty for Codex's default.
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	// TitledGiven or TitledPending.
	Titled string `json:"titled,omitempty"`
	// The title given, or else the one last said to cohesiond.
	Title string `json:"title,omitempty"`
}

// Store is the sessions this client has worked on, kept on disk so a restart
// resumes each session's Codex thread rather than starting a new one.
type Store struct {
	path     string
	mu       sync.Mutex
	sessions map[string]Record
}

func OpenStore(directory string) (*Store, error) {
	store := &Store{path: filepath.Join(directory, "sessions.json"), sessions: map[string]Record{}}
	data, err := os.ReadFile(store.path)
	if os.IsNotExist(err) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	return store, json.Unmarshal(data, &store.sessions)
}

func (s *Store) Get(sessionID int64) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.sessions[strconv.FormatInt(sessionID, 10)]
	return record, ok
}

func (s *Store) Put(sessionID int64, record Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[strconv.FormatInt(sessionID, 10)] = record
	data, _ := json.MarshalIndent(s.sessions, "", "\t")
	_ = writeOwnerOnly(s.path, append(data, '\n'))
}

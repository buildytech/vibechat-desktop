// Package store keeps the host's local state: projects, agent sessions and transcripts, as JSON files under the app's
// data folder. It is the source of truth for the Studio (vibecode AGENTS.md rule 3); Cursor agent ids are handles.
package store

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Project is one project folder.
type Project struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	TemplateID string `json:"template_id"`
	Path       string `json:"path"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// Session is one agent chat, with its own git worktree and branch.
type Session struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Title     string `json:"title,omitempty"`
	AgentID   string `json:"agentId,omitempty"`
	// Runtime is the coding-agent runtime of the chat (codingagent.Runtime*); empty = cursor-sdk (chats made before runtimes).
	Runtime       string `json:"runtime,omitempty"`
	Worktree      string `json:"worktree,omitempty"`
	Branch        string `json:"branch,omitempty"`
	LastMessageAt string `json:"lastMessageAt,omitempty"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

// Message is one transcript row.
type Message struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	RunID     string `json:"runId"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	Seq       int    `json:"seq"`
	CreatedAt string `json:"createdAt"`
	ClientID  string `json:"clientMsgId,omitempty"`
	Feedback  string `json:"feedback,omitempty"`
}

type state struct {
	Projects []Project `json:"projects"`
	Sessions []Session `json:"sessions"`
}

// Store is safe for concurrent use.
type Store struct {
	dir string
	mu  sync.Mutex
	st  state
}

// ErrNotFound is a missing project or session.
var ErrNotFound = errors.New("not found")

// Open loads <dir>/state.json (missing: empty).
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "transcripts"), 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		if err := json.Unmarshal(raw, &s.st); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return s, nil
}

// NewID is a random 16-byte hex id.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Now is the timestamp format of every record.
func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s *Store) save() error {
	raw, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, "state.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "state.json"))
}

// Projects lists projects, newest first.
func (s *Store) Projects() []Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Project(nil), s.st.Projects...)
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out
}

// Project finds one project.
func (s *Store) Project(id string) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.st.Projects {
		if p.ID == id {
			return p, nil
		}
	}
	return Project{}, ErrNotFound
}

// PutProject inserts or replaces p.
func (s *Store) PutProject(p Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.st.Projects {
		if s.st.Projects[i].ID == p.ID {
			s.st.Projects[i] = p
			return s.save()
		}
	}
	s.st.Projects = append(s.st.Projects, p)
	return s.save()
}

// DeleteProject removes a project and its sessions and transcripts (not the folder).
func (s *Store) DeleteProject(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ps []Project
	for _, p := range s.st.Projects {
		if p.ID != id {
			ps = append(ps, p)
		}
	}
	var ss []Session
	for _, x := range s.st.Sessions {
		if x.ProjectID == id {
			_ = os.Remove(s.transcript(x.ID))
			continue
		}
		ss = append(ss, x)
	}
	s.st.Projects, s.st.Sessions = ps, ss
	return s.save()
}

// Sessions of a project, newest first.
func (s *Store) Sessions(projectID string) []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Session
	for _, x := range s.st.Sessions {
		if x.ProjectID == projectID {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// Session finds one session of a project.
func (s *Store) Session(projectID, id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.st.Sessions {
		if x.ID == id && x.ProjectID == projectID {
			return x, nil
		}
	}
	return Session{}, ErrNotFound
}

// PutSession inserts or replaces x.
func (s *Store) PutSession(x Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.st.Sessions {
		if s.st.Sessions[i].ID == x.ID {
			s.st.Sessions[i] = x
			return s.save()
		}
	}
	s.st.Sessions = append(s.st.Sessions, x)
	return s.save()
}

// DeleteSession removes a session and its transcript.
func (s *Store) DeleteSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ss []Session
	for _, x := range s.st.Sessions {
		if x.ID != id {
			ss = append(ss, x)
		}
	}
	s.st.Sessions = ss
	_ = os.Remove(s.transcript(id))
	return s.save()
}

func (s *Store) transcript(sid string) string {
	return filepath.Join(s.dir, "transcripts", filepath.Base(sid)+".jsonl")
}

// Messages reads a session's transcript (rows with seq > after).
func (s *Store) Messages(sid string, after int) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.messages(sid, after)
}

func (s *Store) messages(sid string, after int) ([]Message, error) {
	f, err := os.Open(s.transcript(sid))
	if errors.Is(err, os.ErrNotExist) {
		return []Message{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := []Message{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var m Message
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.Seq > after {
			out = append(out, m)
		}
	}
	return out, sc.Err()
}

// Append adds a transcript row (seq assigned); a repeated clientMsgId returns the existing row.
func (s *Store) Append(m Message) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.messages(m.SessionID, 0)
	if err != nil {
		return m, err
	}
	for _, x := range all {
		if m.ClientID != "" && x.ClientID == m.ClientID {
			return x, nil
		}
	}
	m.Seq = len(all) + 1
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.CreatedAt == "" {
		m.CreatedAt = Now()
	}
	raw, _ := json.Marshal(m)
	f, err := os.OpenFile(s.transcript(m.SessionID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return m, err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return m, err
}

// SetFeedback marks the assistant row of runID.
func (s *Store) SetFeedback(sid, runID, rating string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.messages(sid, 0)
	if err != nil {
		return err
	}
	found := false
	var buf []byte
	for _, m := range all {
		if m.RunID == runID && m.Role == "assistant" {
			m.Feedback = rating
			found = true
		}
		raw, _ := json.Marshal(m)
		buf = append(buf, append(raw, '\n')...)
	}
	if !found {
		return ErrNotFound
	}
	return os.WriteFile(s.transcript(sid), buf, 0o600)
}

// SessionByRun finds the session of a run in a project.
func (s *Store) SessionByRun(projectID, runID string) (Session, error) {
	for _, x := range s.Sessions(projectID) {
		msgs, _ := s.Messages(x.ID, 0)
		for _, m := range msgs {
			if m.RunID == runID {
				return x, nil
			}
		}
	}
	return Session{}, ErrNotFound
}

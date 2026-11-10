package actor

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/cohesionhq/actor-codex/internal/cohesion"
)

// Host answers cohesiond's calls to this client's actors, keeping a Session for
// each session it hands them.
type Host struct {
	Client *cohesion.Client

	store *Store
	usage *Usage
	log   func(string)

	mu       sync.Mutex
	actors   []Entry
	sessions map[int64]*Session
}

func NewHost(actors []Entry, store *Store, usage *Usage, log func(string)) *Host {
	return &Host{actors: actors, store: store, usage: usage, log: log, sessions: map[int64]*Session{}}
}

func (h *Host) SetActors(actors []Entry) {
	h.mu.Lock()
	h.actors = actors
	h.mu.Unlock()
}

// OnCall answers one of cohesiond's calls once the session has taken it; the
// work is reported as it goes.
func (h *Host) OnCall(call cohesion.Call) error {
	actor, err := h.actor(call.Actor)
	if err != nil {
		return err
	}
	switch call.Kind {
	case "startActorSession":
		h.start(call, actor)
	case "sendActorSessionMessage":
		if call.Content == nil {
			return fmt.Errorf("the message says nothing")
		}
		if err := h.hear(call.SessionID, actor, *call.Content, call.Images, call.Subagent); err != nil {
			return err
		}
	case "stopActorSession":
		if session := h.session(call.SessionID); session != nil {
			session.Stop()
		}
	case "setActorSessionMode":
		if err := h.setMode(call.SessionID, call.Mode); err != nil {
			return err
		}
	default:
		return fmt.Errorf("this client can't %s", call.Kind)
	}
	h.log(fmt.Sprintf("session %d on %s: %s", call.SessionID, call.Actor, call.Kind))
	return nil
}

// OnConnect reports each session still going on to a connection that just began.
func (h *Host) OnConnect(link *cohesion.Session) {
	h.mu.Lock()
	sessions := make([]*Session, 0, len(h.sessions))
	for _, session := range h.sessions {
		sessions = append(sessions, session)
	}
	h.mu.Unlock()
	for _, session := range sessions {
		session.Resync(link)
	}
}

// Close ends every session's Codex process.
func (h *Host) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, session := range h.sessions {
		session.Close()
	}
}

func (h *Host) actor(identifier string) (Entry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, actor := range h.actors {
		if actor.Identifier == identifier {
			if info, err := os.Stat(actor.Directory); err != nil || !info.IsDir() {
				return Entry{}, fmt.Errorf("%s doesn't exist", actor.Directory)
			}
			return actor, nil
		}
	}
	return Entry{}, fmt.Errorf("this client has no actor %s", identifier)
}

func (h *Host) session(id int64) *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[id]
}

func (h *Host) start(call cohesion.Call, actor Entry) {
	record := Record{Actor: actor.Identifier, Mode: modeOr(call.Mode), Model: deref(call.Model), Effort: deref(call.Effort), Titled: TitledPending}
	// A daemon from before titles says none: its title is the prompt's first line.
	if call.Title != nil && *call.Title != "" {
		record.Titled, record.Title = TitledGiven, *call.Title
	}
	h.store.Put(call.SessionID, record)
	h.open(call.SessionID, record, actor).Say(call.Prompt, call.Images)
}

// setMode runs a session in `mode` from its next turn on. A session this client
// has no record of takes it from cohesiond when picked up.
func (h *Host) setMode(sessionID int64, identifier *string) error {
	if identifier == nil {
		return fmt.Errorf("no mode given")
	}
	mode, ok := ModeNamed(*identifier)
	if !ok {
		return fmt.Errorf("Codex has no mode %s", *identifier)
	}
	if session := h.session(sessionID); session != nil {
		session.SetMode(mode.Identifier)
		return nil
	}
	if kept, ok := h.store.Get(sessionID); ok {
		kept.Mode = mode.Identifier
		h.store.Put(sessionID, kept)
	}
	return nil
}

// hear hands a session what the user said, picking the session up from disk after
// a restart, or from Cohesion's transcript when this client never had it.
func (h *Host) hear(sessionID int64, actor Entry, content cohesion.UserContent, images []cohesion.Image, subagent string) error {
	// A response to a subagent's request goes to that subagent. One lives
	// only in its session's Codex process, so there is nothing to recover it from.
	if subagent != "" {
		if session := h.session(sessionID); session != nil {
			return session.HearChild(subagent, content, images)
		}
		return fmt.Errorf("subagent %s of session %d is no longer running here", subagent, sessionID)
	}
	if session := h.session(sessionID); session != nil {
		session.Hear(content, images)
		return nil
	}
	if kept, ok := h.store.Get(sessionID); ok {
		h.open(sessionID, kept, actor).Hear(content, images)
		return nil
	}
	var held cohesion.ActorSession
	if err := h.Client.Request("getActorSession", map[string]any{"id": sessionID}, &held); err != nil {
		return err
	}
	var messages []cohesion.ActorSessionMessage
	if err := h.Client.Request("listActorSessionMessages", map[string]any{"session_id": sessionID}, &messages); err != nil {
		return err
	}
	record := Record{
		Actor:  actor.Identifier,
		Mode:   modeOr(held.Mode),
		Model:  deref(held.Model),
		Effort: deref(held.Effort),
		// Codex's name would describe the transcript it is handed, not the session.
		Titled: TitledGiven,
	}
	h.store.Put(sessionID, record)
	h.open(sessionID, record, actor).Say(
		"This session began in a Codex thread that is no longer available. Its transcript so far, for context:\n\n"+
			Transcript(messages)+"\n\nMy new message:\n\n"+Describe(content),
		images,
	)
	return nil
}

func (h *Host) open(sessionID int64, record Record, actor Entry) *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	if session, ok := h.sessions[sessionID]; ok {
		return session
	}
	session := NewSession(sessionID, record, SessionContext{Client: h.Client, Store: h.store, Cwd: actor.Directory, Usage: h.usage, Log: h.log})
	h.sessions[sessionID] = session
	return session
}

// Transcript is a session's transcript as text, oldest first.
func Transcript(messages []cohesion.ActorSessionMessage) string {
	var parts []string
	for _, message := range messages {
		var content cohesion.MessageContent
		if json.Unmarshal(message.Content, &content) != nil {
			continue
		}
		if content.Sender == "user" {
			if content.Kind == "text" {
				parts = append(parts, "User: "+content.Text)
			} else {
				parts = append(parts, fmt.Sprintf("User responded to a request (%s).", content.Kind))
			}
			continue
		}
		switch content.Kind {
		case "text":
			parts = append(parts, "Codex: "+content.Text)
		case "toolUse":
			parts = append(parts, fmt.Sprintf("[%s %s]", content.Tool, content.Summary))
		case "question":
			var asked []string
			for _, question := range content.Questions {
				asked = append(asked, question.Question)
			}
			parts = append(parts, "Codex asked: "+strings.Join(asked, " "))
		case "permission":
			what := "something"
			if content.Subject != nil {
				what = content.Subject.Kind
				if content.Subject.Kind == "tool" {
					what = content.Subject.Name
				}
			}
			parts = append(parts, fmt.Sprintf("Codex asked permission to use %s.", what))
		case "plan":
			parts = append(parts, "Codex proposed a plan:\n"+content.Plan)
		}
	}
	return strings.Join(parts, "\n\n")
}

func deref(text *string) string {
	if text == nil {
		return ""
	}
	return *text
}

package cohesion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// ErrSessionClosed is a request the session ended before answering.
var ErrSessionClosed = errors.New("the session with cohesiond closed")

// Hello is what cohesiond says first on a session.
type Hello struct {
	Protocol   int    `json:"protocol"`
	Server     string `json:"server"`
	ClientName string `json:"client_name"`
}

// CallHandler answers one of the daemon's calls; an error's message is shown
// to the user as written.
type CallHandler func(call Call) error

type outcome struct {
	data json.RawMessage
	err  error
}

// Session is one connection to cohesiond.
type Session struct {
	Hello Hello

	transport transport
	onCall    CallHandler

	mu      sync.Mutex
	nextID  int64
	waiting map[int64]chan outcome
	ended   bool
	evicted string
	done    chan struct{}
}

// openSession connects to `server`, returning once cohesiond has said hello.
// Calls go to `onCall`, which none reach before the session's offer.
func openSession(ctx context.Context, server Server, onCall CallHandler) (*Session, error) {
	transport, err := connect(ctx, server)
	if err != nil {
		return nil, err
	}
	first, err := transport.receive()
	if err != nil {
		transport.close()
		return nil, ErrSessionClosed
	}
	var hello struct {
		Type string `json:"type"`
		Hello
	}
	if err := json.Unmarshal(first, &hello); err != nil || hello.Type != "hello" {
		transport.close()
		return nil, fmt.Errorf("cohesiond opened the session with %q, not hello", hello.Type)
	}
	session := &Session{
		Hello:     hello.Hello,
		transport: transport,
		onCall:    onCall,
		nextID:    1,
		waiting:   map[int64]chan outcome{},
		done:      make(chan struct{}),
	}
	go session.read()
	return session, nil
}

// Done is closed once the session has ended.
func (s *Session) Done() <-chan struct{} { return s.done }

// Evicted is why the daemon ended the session, when it did: `replaced` or
// `revoked`; a client mustn't reconnect after either.
func (s *Session) Evicted() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evicted
}

// Close ends the session.
func (s *Session) Close() { s.transport.close() }

// Request runs request `kind` with `args` (nil for none) and decodes its
// reply's data into `into` (nil to drop it).
func (s *Session) Request(kind string, args any, into any) error {
	frame := map[string]any{"type": kind}
	if args != nil {
		frame["args"] = args
	}
	data, err := s.exchange(frame)
	if err != nil || into == nil {
		return err
	}
	return json.Unmarshal(data, into)
}

// Offer makes this session the one serving its client, offering `offers`.
func (s *Session) Offer(offers Capabilities) error {
	actors := offers.Actors
	if actors == nil {
		actors = []ActorDefinition{}
	}
	_, err := s.exchange(map[string]any{"type": "hello", "actors": actors})
	return err
}

// Report reports on actor session `sessionID`; a message returns the message stored.
func (s *Session) Report(sessionID int64, report Report) (*ActorSessionMessage, error) {
	frame := report.fields()
	frame["type"] = "actorSessionUpdate"
	frame["session_id"] = sessionID
	data, err := s.exchange(frame)
	if err != nil || report.Kind != "message" {
		return nil, err
	}
	var message ActorSessionMessage
	if err := json.Unmarshal(data, &message); err != nil {
		return nil, err
	}
	return &message, nil
}

// Usage says how much of its limits actor `actor` has used.
func (s *Session) Usage(actor string, usage ActorUsage) error {
	frame := map[string]any{"type": "actorUsage", "actor": actor}
	if len(usage.Plan) > 0 {
		frame["plan"] = usage.Plan
	}
	if usage.Pool != nil {
		frame["pool"] = usage.Pool
	}
	_, err := s.exchange(frame)
	return err
}

func (s *Session) exchange(frame map[string]any) (json.RawMessage, error) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return nil, ErrSessionClosed
	}
	id := s.nextID
	s.nextID++
	reply := make(chan outcome, 1)
	s.waiting[id] = reply
	s.mu.Unlock()

	frame["id"] = id
	text, err := json.Marshal(frame)
	if err != nil {
		s.forget(id)
		return nil, err
	}
	if err := s.transport.send(text); err != nil {
		s.forget(id)
		return nil, ErrSessionClosed
	}
	result := <-reply
	return result.data, result.err
}

func (s *Session) forget(id int64) {
	s.mu.Lock()
	delete(s.waiting, id)
	s.mu.Unlock()
}

type serverFrame struct {
	Type string `json:"type"`
	ID   *int64 `json:"id"`
	OK   *struct {
		Data json.RawMessage `json:"data"`
	} `json:"ok"`
	Error  *ServerError `json:"error"`
	CallID int64        `json:"call_id"`
	Call   *Call        `json:"call"`
	Reason string       `json:"reason"`
}

func (s *Session) read() {
	for {
		text, err := s.transport.receive()
		if err != nil {
			break
		}
		var frame serverFrame
		if err := json.Unmarshal(text, &frame); err != nil {
			continue
		}
		switch frame.Type {
		case "reply":
			if frame.ID == nil {
				continue
			}
			s.mu.Lock()
			reply, ok := s.waiting[*frame.ID]
			delete(s.waiting, *frame.ID)
			s.mu.Unlock()
			if !ok {
				continue
			}
			if frame.Error != nil {
				reply <- outcome{err: frame.Error}
			} else if frame.OK != nil {
				reply <- outcome{data: frame.OK.Data}
			} else {
				reply <- outcome{}
			}
		case "call":
			if frame.Call != nil {
				go s.answer(frame.CallID, *frame.Call)
			}
		case "evicted":
			s.mu.Lock()
			s.evicted = frame.Reason
			s.mu.Unlock()
		}
	}
	s.transport.close()
	s.mu.Lock()
	s.ended = true
	for id, reply := range s.waiting {
		reply <- outcome{err: ErrSessionClosed}
		delete(s.waiting, id)
	}
	s.mu.Unlock()
	close(s.done)
}

func (s *Session) answer(callID int64, call Call) {
	frame := map[string]any{"type": "callResult", "call_id": callID}
	if err := s.onCall(call); err != nil {
		frame["error"] = err.Error()
	} else {
		frame["ok"] = map[string]string{"kind": "accepted"}
	}
	text, _ := json.Marshal(frame)
	s.mu.Lock()
	ended := s.ended
	s.mu.Unlock()
	if !ended {
		_ = s.transport.send(text)
	}
}

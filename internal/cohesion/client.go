package cohesion

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"time"
)

// State is where the client stands with cohesiond, tagged by Kind:
// connecting, online, offline, evicted or stopped.
type State struct {
	Kind    string
	Session *Session      // online
	Err     error         // offline
	RetryIn time.Duration // offline
	Reason  string        // evicted: replaced or revoked
}

// Options are what a Client serves.
type Options struct {
	// Offers is sent as the client's hello on every session.
	Offers Capabilities
	// OnCall answers the daemon's calls.
	OnCall CallHandler
	// OnConnect runs on each new session once its hello is accepted, before
	// the updates held while away are sent: report, on the session, the
	// status and progress of each actor session still going on, which cohesiond forgot.
	OnConnect func(session *Session)
	OnState   func(state State)
}

// Pending is a report waiting on its reply.
type Pending struct {
	done    chan struct{}
	message *ActorSessionMessage
	err     error
}

// Wait returns the stored message, for a message, or why it was refused.
func (p *Pending) Wait() (*ActorSessionMessage, error) {
	<-p.done
	return p.message, p.err
}

type held struct {
	sessionID int64
	report    Report
	unsure    bool
	pending   *Pending
}

const (
	firstRetry = 500 * time.Millisecond
	lastRetry  = 30 * time.Second
)

// Client keeps one session with cohesiond open: it reconnects with a backoff
// when the session drops (never after an eviction), offers again, says its
// actors' usage again, and holds actor session reports while away, sending them in
// order once back, as "Staying in sync" in cohesion's docs/actors.md says.
type Client struct {
	server  Server
	options Options

	mu      sync.Mutex
	session *Session
	stopped bool
	queue   []*held
	usages  map[string]ActorUsage
	sending bool
	wake    chan struct{}
}

func NewClient(server Server, options Options) *Client {
	return &Client{server: server, options: options, usages: map[string]ActorUsage{}, wake: make(chan struct{}, 1)}
}

func (c *Client) state(state State) {
	if c.options.OnState != nil {
		c.options.OnState(state)
	}
}

// Run connects, and keeps connecting until stopped or evicted.
func (c *Client) Run() {
	retry := firstRetry
	for !c.isStopped() {
		c.state(State{Kind: "connecting"})
		session, err := c.open()
		if err == nil {
			retry = firstRetry
			c.state(State{Kind: "online", Session: session})
			go c.flush()
			<-session.Done()
			c.mu.Lock()
			c.session = nil
			c.mu.Unlock()
			if reason := session.Evicted(); reason != "" {
				c.state(State{Kind: "evicted", Reason: reason})
				return
			}
		} else if !c.isStopped() {
			c.state(State{Kind: "offline", Err: err, RetryIn: retry})
		}
		if c.isStopped() {
			break
		}
		select {
		case <-time.After(retry):
		case <-c.wake:
		}
		retry = min(retry*2, lastRetry)
	}
	c.state(State{Kind: "stopped"})
}

func (c *Client) open() (*Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := openSession(ctx, c.server, func(call Call) error { return c.options.OnCall(call) })
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	offers := c.options.Offers
	c.mu.Unlock()
	if err := session.Offer(offers); err != nil {
		session.Close()
		return nil, err
	}
	if c.options.OnConnect != nil {
		c.options.OnConnect(session)
	}
	c.mu.Lock()
	c.session = session
	usages := make(map[string]ActorUsage, len(c.usages))
	for actor, usage := range c.usages {
		usages[actor] = usage
	}
	c.mu.Unlock()
	for actor, usage := range usages {
		go session.Usage(actor, usage)
	}
	return session, nil
}

func (c *Client) isStopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}

// Stop ends the session and Run with it.
func (c *Client) Stop() {
	c.mu.Lock()
	c.stopped = true
	session := c.session
	c.mu.Unlock()
	if session != nil {
		session.Close()
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Offer replaces what the client offers, and tells cohesiond now when connected.
func (c *Client) Offer(offers Capabilities) error {
	c.mu.Lock()
	c.options.Offers = offers
	for actor := range c.usages {
		offered := false
		for _, definition := range offers.Actors {
			offered = offered || definition.Identifier == actor
		}
		if !offered {
			delete(c.usages, actor)
		}
	}
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return nil
	}
	return session.Offer(offers)
}

// Usage says how much of its plan's limits actor `actor` has used, now when
// connected and again on every new session; with no plan, it says nothing.
// It fails when refused, as a daemon that doesn't know usage refuses it.
func (c *Client) Usage(actor string, usage ActorUsage) error {
	c.mu.Lock()
	if len(usage.Plan) > 0 {
		c.usages[actor] = usage
	} else {
		delete(c.usages, actor)
	}
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return nil
	}
	return session.Usage(actor, usage)
}

// Request runs a request on the current session; it fails while offline.
func (c *Client) Request(kind string, args any, into any) error {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return ErrSessionClosed
	}
	return session.Request(kind, args, into)
}

// Report reports on actor session `sessionID`, now or once connected, in order
// with every other report. An actor session cohesiond no longer has is refused as not
// found.
func (c *Client) Report(sessionID int64, report Report) *Pending {
	pending := &Pending{done: make(chan struct{})}
	c.mu.Lock()
	c.queue = append(c.queue, &held{sessionID: sessionID, report: report, pending: pending})
	c.mu.Unlock()
	go c.flush()
	return pending
}

// flush sends the held reports in order, one reply at a time, while online.
func (c *Client) flush() {
	c.mu.Lock()
	if c.sending {
		c.mu.Unlock()
		return
	}
	c.sending = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.sending = false
		again := len(c.queue) > 0 && c.session != nil
		c.mu.Unlock()
		// One held while this flush was ending.
		if again {
			go c.flush()
		}
	}()
	for {
		c.mu.Lock()
		if len(c.queue) == 0 || c.session == nil {
			c.mu.Unlock()
			return
		}
		next, session := c.queue[0], c.session
		c.mu.Unlock()

		var message *ActorSessionMessage
		var err error
		// A subagent's transcript is cohesiond's own session, which this client has no id for.
		if next.unsure && next.report.Kind == "message" && next.report.Subagent == "" {
			message, err = stored(session, next.sessionID, next.report.Content)
		}
		if err == nil && message == nil {
			message, err = session.Report(next.sessionID, next.report)
		}
		if errors.Is(err, ErrSessionClosed) {
			// Stays first, to send again on the next session.
			next.unsure = true
			return
		}
		c.mu.Lock()
		c.queue = c.queue[1:]
		c.mu.Unlock()
		next.pending.message, next.pending.err = message, err
		close(next.pending.done)
	}
}

// stored is the last thing the actor said in session `sessionID`, when it is
// `content`: a report sent before a session dropped, unanswered, may have
// been stored. A tool's `ended_at` is cohesiond's, not what the actor said.
func stored(session *Session, sessionID int64, content *SaidContent) (*ActorSessionMessage, error) {
	var messages []ActorSessionMessage
	if err := session.Request("listActorSessionMessages", map[string]any{"session_id": sessionID}, &messages); err != nil {
		return nil, err
	}
	for i := len(messages) - 1; i >= 0; i-- {
		var said map[string]any
		if json.Unmarshal(messages[i].Content, &said) != nil || said["sender"] != "actor" {
			continue
		}
		delete(said, "sender")
		delete(said, "ended_at")
		var sent map[string]any
		raw, _ := json.Marshal(content)
		_ = json.Unmarshal(raw, &sent)
		if reflect.DeepEqual(said, sent) {
			return &messages[i], nil
		}
		return nil, nil
	}
	return nil, nil
}

// Resolved is a report already answered, for a stand-in client in tests.
func Resolved(message *ActorSessionMessage, err error) *Pending {
	pending := &Pending{done: make(chan struct{}), message: message, err: err}
	close(pending.done)
	return pending
}

package actor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cohesionhq/actor-codex/internal/codex"
	"github.com/cohesionhq/actor-codex/internal/cohesion"
)

// One actor session, run as one Codex thread: what the user says goes in as
// the thread's input, and what Codex does comes back as the session's
// transcript, status and progress.

var (
	// Linger is how long a thread's process stays up with nothing to do
	// before it exits; the next message resumes the thread.
	Linger = 10 * time.Minute
	// Resume is how long after its background agents are done a session whose
	// turn had ended waits for Codex to take their results up in a turn of
	// its own, before it is idle without one.
	Resume = 10 * time.Second
)

// SessionContext is what a session needs from the client around it.
type SessionContext struct {
	Client Reporter
	Store  *Store
	// The actor's directory.
	Cwd   string
	Usage *Usage
	Log   func(string)
}

// Reporter is where a session's reports go: the Cohesion client.
type Reporter interface {
	Report(sessionID int64, report cohesion.Report) *cohesion.Pending
}

// stretch is a running Codex process for the session, with its thread loaded.
type stretch struct {
	process  *codex.Process
	threadID string
	// The model the thread runs on, which plan mode's settings name.
	model string
	// The mode whose approval policy and sandbox the thread was loaded with.
	mode string
	// The turn under way, or "".
	turnID string
	linger *time.Timer
	// Whether it is ending or has ended, so it answers nothing more.
	closed bool
}

// waiting is a request waiting on the user.
type waiting struct {
	// The stretch whose Codex waits on it; nil for a plan, which waits on
	// nothing but the user.
	stretch *stretch
	// Codex's id for the request, as `serverRequest/resolved` names it.
	rpcID string
	// answer hands Codex the user's response; nil is a request stopped.
	answer func(response *cohesion.UserContent)
}

// use is a tool use waiting on its result.
type use struct {
	content cohesion.SaidContent
	// Shown only once its result comes, with any images.
	held bool
	// The ids of the messages showing it.
	shown []int64
}

// snapshot is what a reconnect reads of the session without waiting on its loop.
type snapshot struct {
	status   string
	progress *string
	titled   string
	title    string
	process  *codex.Process
	// The subagents launched, in order, for a reconnect to launch again.
	children []childSnap
}

// Session is one actor session. Everything it does runs on its own loop, one
// event at a time, so its state needs no locks; what a reconnect reads is
// kept apart in `snap`.
type Session struct {
	ID int64

	ctx    SessionContext
	events *queue

	snapMu sync.Mutex
	snap   snapshot

	// Owned by the loop.
	record   Record
	status   string
	progress *string
	stretch  *stretch
	// Requests waiting on the user, by their message's id.
	waiting map[int64]*waiting
	// Tool uses waiting on their results, by Codex's item id.
	uses map[string]*use
	// The file changes Codex showed, by item id, for their approval.
	changes  map[string][]FileUpdate
	stopping bool
	said     *cohesion.ContextUsage
	// The plan Codex wrote in this turn, in plan mode.
	plan string
	// The subagents Codex runs in their own threads, by thread id: until
	// they are done the turn isn't, since Codex takes their results up.
	background map[string]bool
	// The turn under way in each subagent's thread, to interrupt on stop.
	childTurns map[string]string
	// Set while Codex's turn has ended with background agents running, so
	// the session stays working for them; with the timer that makes it idle
	// should that work end with no turn of Codex's to follow.
	held *hold
	// The subagents launched so far, by Codex's thread id for each, and the
	// order they were launched in (a parent before its children).
	children map[string]*child
	order    []string
	// What the launching tool calls said of the threads they name, and the
	// spawns begun that name none yet.
	calls    map[string]*call
	spawning []spawn
}

type hold struct{ resume *time.Timer }

// NewSession makes a session kept as `record`, and starts its loop.
func NewSession(id int64, record Record, ctx SessionContext) *Session {
	t := &Session{
		ID:         id,
		ctx:        ctx,
		events:     newQueue(),
		record:     record,
		status:     cohesion.Idle,
		waiting:    map[int64]*waiting{},
		uses:       map[string]*use{},
		changes:    map[string][]FileUpdate{},
		background: map[string]bool{},
		childTurns: map[string]string{},
		children:   map[string]*child{},
		calls:      map[string]*call{},
	}
	t.snap = snapshot{status: cohesion.Idle, titled: record.Titled, title: record.Title}
	go t.events.run()
	return t
}

// ---- What the host asks -----------------------------------------------------

// Say starts a turn with `text` and `images`, or adds them to the turn under way.
func (t *Session) Say(text string, images []cohesion.Image) {
	t.events.push(func() { t.say(text, images) })
}

// Hear hands the session what the user said: more to do, or a response to a
// request. Words said while requests wait pass them over: Codex, held on
// them, takes them as declined, and hears the words.
func (t *Session) Hear(content cohesion.UserContent, images []cohesion.Image) {
	t.events.push(func() { t.hear(content, images) })
}

// HearChild hands a subagent what the user said: the response to one of its
// requests, as cohesiond sends it with the subagent's key. It fails when this
// process has no such subagent, which the user sees.
func (t *Session) HearChild(key string, content cohesion.UserContent, images []cohesion.Image) error {
	done := make(chan error, 1)
	t.events.push(func() { done <- t.hearChild(key, content, images) })
	return <-done
}

// Stop stops the turn under way; the session is idle until the user says more.
func (t *Session) Stop() { t.events.push(t.stop) }

// SetMode runs the session in `mode` from its next turn on, leaving its status as it is.
func (t *Session) SetMode(mode string) {
	t.events.push(func() {
		t.record.Mode = mode
		t.ctx.Store.Put(t.ID, t.record)
	})
}

// Resync reports where the session stands, for a connection that just began:
// cohesiond forgot.
func (t *Session) Resync(link *cohesion.Session) {
	t.snapMu.Lock()
	snap := t.snap
	t.snapMu.Unlock()
	report := func(r cohesion.Report) {
		if _, err := link.Report(t.ID, r); err != nil {
			t.logf("resync refused: %v", err)
		}
	}
	// Every subagent again, parents first, and where each stands: cohesiond
	// settled the working ones idle when the last connection dropped.
	for _, c := range snap.children {
		report(c.launch)
		report(cohesion.Report{Kind: "status", Subagent: c.key, Status: c.status})
	}
	if snap.status != cohesion.Working && snap.status != cohesion.NeedsAttention {
		return
	}
	report(cohesion.Report{Kind: "status", Status: snap.status})
	report(cohesion.Report{Kind: "progress", Progress: snap.progress})
	// Said while cohesiond was away, it may not have kept it.
	if snap.titled == TitledPending && snap.title != "" {
		report(cohesion.Report{Kind: "title", Title: snap.title})
	}
}

// Close ends the session's Codex process, should one run.
func (t *Session) Close() {
	t.snapMu.Lock()
	process := t.snap.process
	t.snapMu.Unlock()
	if process != nil {
		process.Close()
	}
}

// ---- Turns -----------------------------------------------------------------

func (t *Session) say(text string, images []cohesion.Image) {
	if strings.TrimSpace(text) == "" && len(images) == 0 {
		return
	}
	st := t.stretch
	// A mode with another sandbox or approval policy takes a thread loaded
	// with it; one under way keeps its own until its turn ends.
	if st != nil && st.turnID == "" && !sameSandbox(st.mode, t.record.Mode) {
		t.close(st)
		st = nil
	}
	if st == nil {
		if st = t.open(); st == nil {
			return
		}
	}
	if st.linger != nil {
		st.linger.Stop()
		st.linger = nil
	}
	input := t.input(text, images)
	// cohesiond sets a session the user speaks to working itself.
	t.setLocalStatus(cohesion.Working)
	if st.turnID != "" {
		err := st.process.Call(context.Background(), "turn/steer", map[string]any{
			"threadId": st.threadID, "input": input, "expectedTurnId": st.turnID,
		}, nil)
		if err == nil {
			return
		}
		// The turn ended meanwhile: the words start the next.
		t.logf("couldn't add to the turn under way, so it starts another: %v", err)
	}
	t.resume()
	t.startTurn(st, input)
}

func (t *Session) startTurn(st *stretch, input []any) {
	mode, _ := ModeNamed(t.record.Mode)
	if mode.Identifier == "" {
		mode = Modes[0]
	}
	params := map[string]any{"threadId": st.threadID, "input": input}
	if t.record.Effort != "" {
		params["effort"] = t.record.Effort
	}
	if st.model != "" {
		var effort any
		if t.record.Effort != "" {
			effort = t.record.Effort
		}
		// Every turn names its collaboration mode: one carries on otherwise.
		params["collaborationMode"] = map[string]any{
			"mode":     mode.Collaboration,
			"settings": map[string]any{"model": st.model, "reasoning_effort": effort, "developer_instructions": nil},
		}
	}
	var reply struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := st.process.Call(context.Background(), "turn/start", params, &reply); err != nil {
		t.logf("couldn't start a turn: %v", err)
		t.reportWait(text(fmt.Sprintf("Codex couldn't start: %v", err)))
		t.setProgress(nil)
		t.setStatus(cohesion.Failed)
		t.linger(st)
		return
	}
	if st.turnID == "" {
		st.turnID = reply.Turn.ID
	}
	t.plan = ""
}

// input is what the user said as Codex takes it: their words, then their
// images, saved where Codex can read them.
func (t *Session) input(text string, images []cohesion.Image) []any {
	input := []any{}
	if text != "" {
		input = append(input, map[string]any{"type": "text", "text": text, "text_elements": []any{}})
	}
	for _, image := range images {
		path, err := t.saveImage(image)
		if err != nil {
			t.logf("couldn't save an image for Codex: %v", err)
			continue
		}
		input = append(input, map[string]any{"type": "localImage", "path": path})
	}
	return input
}

func (t *Session) saveImage(image cohesion.Image) (string, error) {
	data, err := decodeBase64(Tidy(image).Data)
	if err != nil {
		return "", err
	}
	extension := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}[Tidy(image).MediaType]
	if extension == "" {
		return "", fmt.Errorf("it is %s", image.MediaType)
	}
	directory := filepath.Join(os.TempDir(), "actor-codex", fmt.Sprint(t.ID))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	name := make([]byte, 8)
	_, _ = rand.Read(name)
	path := filepath.Join(directory, hex.EncodeToString(name)+extension)
	return path, os.WriteFile(path, data, 0o600)
}

// ---- Stretches -------------------------------------------------------------

// open starts Codex and loads the session's thread: resumed when it has one,
// else a new one. It reports why when it can't, and returns nil.
func (t *Session) open() *stretch {
	st := &stretch{mode: t.record.Mode}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	process, err := codex.Start(ctx, t.ctx.Cwd, codex.Handler{
		Notify:  func(n codex.Notification) { t.events.push(func() { t.notify(st, n) }) },
		Request: func(r codex.ServerRequest) { t.events.push(func() { t.request(st, r) }) },
	}, func(text string) { t.logf("codex: %s", text) })
	if err != nil {
		t.logf("%v", err)
		t.reportWait(text(fmt.Sprintf("Codex couldn't start: %v", err)))
		t.setStatus(cohesion.Failed)
		return nil
	}
	st.process = process
	mode, ok := ModeNamed(t.record.Mode)
	if !ok {
		mode = Modes[0]
	}
	params := map[string]any{
		"cwd":                   t.ctx.Cwd,
		"approvalPolicy":        mode.Approval,
		"sandbox":               mode.Sandbox,
		"developerInstructions": Instructions,
	}
	if t.record.Model != "" {
		params["model"] = t.record.Model
	}
	var reply struct {
		Thread struct {
			ID   string  `json:"id"`
			Name *string `json:"name"`
		} `json:"thread"`
		Model string `json:"model"`
	}
	resumed := false
	if t.record.ThreadID != "" {
		resume := map[string]any{"threadId": t.record.ThreadID, "excludeTurns": true}
		for key, value := range params {
			resume[key] = value
		}
		if err := process.Call(ctx, "thread/resume", resume, &reply); err != nil {
			t.logf("couldn't resume thread %s, so a new one starts: %v", t.record.ThreadID, err)
			t.report(text("(The Codex thread this session ran in couldn't be resumed, so it carries on in a new one.)"))
		} else {
			resumed = true
		}
	}
	if !resumed {
		if err := process.Call(ctx, "thread/start", params, &reply); err != nil {
			process.Close()
			t.logf("couldn't start a thread: %v", err)
			t.reportWait(text(fmt.Sprintf("Codex couldn't start: %v", err)))
			t.setStatus(cohesion.Failed)
			return nil
		}
	}
	st.threadID, st.model = reply.Thread.ID, reply.Model
	if !resumed {
		t.record.ThreadID = st.threadID
		t.ctx.Store.Put(t.ID, t.record)
		// The thread goes by the session's given title rather than one of its own.
		if t.record.Titled == TitledGiven && t.record.Title != "" {
			if err := process.Call(ctx, "thread/name/set", map[string]any{"threadId": st.threadID, "name": t.record.Title}, nil); err != nil {
				t.logf("couldn't name the thread: %v", err)
			}
		}
	} else if reply.Thread.Name != nil {
		t.titleFrom(*reply.Thread.Name)
	}
	t.stretch = st
	t.background = map[string]bool{}
	t.childTurns = map[string]string{}
	t.snapMu.Lock()
	t.snap.process = process
	t.snapMu.Unlock()
	t.ctx.Usage.Track(process)
	go t.ctx.Usage.Pull(process, false)
	go func() {
		<-process.Done()
		t.events.push(func() { t.exited(st) })
	}()
	return st
}

// close lets `st`'s process go: its thread stays on disk for the next.
func (t *Session) close(st *stretch) {
	st.closed = true
	if st.linger != nil {
		st.linger.Stop()
	}
	if t.stretch == st {
		t.stretch = nil
	}
	st.process.Close()
}

// exited is `st`'s process gone, on purpose or not.
func (t *Session) exited(st *stretch) {
	t.ctx.Usage.Untrack(st.process)
	t.snapMu.Lock()
	if t.snap.process == st.process {
		t.snap.process = nil
	}
	t.snapMu.Unlock()
	// Its requests can't be answered now: a response goes as words instead.
	for id, w := range t.waiting {
		if w.stretch == st {
			delete(t.waiting, id)
		}
	}
	deliberate := st.closed
	st.closed = true
	if t.stretch != st {
		return
	}
	t.stretch = nil
	t.release()
	t.background = map[string]bool{}
	t.childrenGone(st)
	if !deliberate && !t.stopping && (t.status == cohesion.Working || t.status == cohesion.NeedsAttention) {
		why := "it exited"
		if err := st.process.Err(); err != nil && !errors.Is(err, codex.ErrClosed) {
			why = err.Error()
		}
		t.reportWait(text("Codex stopped: " + why))
		t.setProgress(nil)
		t.setStatus(cohesion.Failed)
		return
	}
	if t.held != nil {
		t.settle()
	}
}

// linger lets `st`'s process go once it has had nothing to do for a while.
func (t *Session) linger(st *stretch) {
	if st.linger != nil {
		st.linger.Stop()
	}
	st.linger = time.AfterFunc(Linger, func() {
		t.events.push(func() {
			if t.stretch != st || st.turnID != "" || t.status == cohesion.Working || t.held != nil {
				return
			}
			for _, w := range t.waiting {
				if w.stretch == st {
					return
				}
			}
			t.close(st)
		})
	})
}

// ---- What Codex says -------------------------------------------------------

type threadParams struct {
	ThreadID string          `json:"threadId"`
	Turn     *turn           `json:"turn"`
	Item     *Item           `json:"item"`
	Name     *string         `json:"threadName"`
	Usage    *TokenUsage     `json:"tokenUsage"`
	Plan     []planStep      `json:"plan"`
	Request  json.RawMessage `json:"requestId"`
}

type turn struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type planStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

func (t *Session) notify(st *stretch, n codex.Notification) {
	switch n.Method {
	case "account/rateLimits/updated":
		t.ctx.Usage.RateLimitsUpdated(n.Params)
		return
	case "account/updated":
		t.ctx.Usage.AccountChanged()
		go t.ctx.Usage.Pull(st.process, true)
		return
	}
	if st != t.stretch || st.closed {
		return
	}
	var params threadParams
	if json.Unmarshal(n.Params, &params) != nil {
		return
	}
	if params.ThreadID != "" && params.ThreadID != st.threadID {
		// A subagent's own thread: its steps stay inside its tool use.
		t.child(st, params, n.Method)
		return
	}
	switch n.Method {
	case "turn/started":
		if params.Turn != nil {
			st.turnID = params.Turn.ID
		}
		t.plan = ""
		// A turn begins, perhaps Codex's own on its agents' results.
		t.resume()
		t.working()
	case "item/started":
		if params.Item != nil {
			t.started(*params.Item)
		}
	case "item/completed":
		if params.Item != nil {
			t.completed(st, *params.Item)
		}
	case "turn/plan/updated":
		t.planUpdated(params.Plan)
	case "thread/tokenUsage/updated":
		if params.Usage != nil {
			t.setContext(ContextOf(*params.Usage))
		}
	case "thread/name/updated":
		if params.Name != nil {
			t.titleFrom(*params.Name)
		}
	case "serverRequest/resolved":
		// Codex cleared it itself, such as at the turn's end.
		for id, w := range t.waiting {
			if w.stretch == st && w.rpcID == string(params.Request) {
				delete(t.waiting, id)
			}
		}
	case "turn/completed":
		if params.Turn != nil {
			t.turnCompleted(st, *params.Turn)
		}
	}
}

// working reports the session working, unless it waits on the user.
func (t *Session) working() {
	if t.status != cohesion.Working && len(t.waiting) == 0 {
		t.setStatus(cohesion.Working)
	}
}

func (t *Session) started(item Item) {
	if item.Type == "fileChange" {
		t.changes[item.ID] = item.Changes
	}
	if !IsTool(item.Type) {
		return
	}
	t.resume()
	t.working()
	content := ToolUse(item, t.ctx.Cwd)
	u := &use{content: content, held: ShowsLater(item)}
	t.uses[item.ID] = u
	if !u.held {
		u.shown = append(u.shown, t.show(content, nil))
	}
	t.noteCall("", item, t.uses)
	progress := Progress(item, t.ctx.Cwd)
	t.setProgress(&progress)
}

func (t *Session) completed(st *stretch, item Item) {
	switch item.Type {
	case "agentMessage":
		if strings.TrimSpace(item.Text) != "" {
			t.working()
			t.show(cohesion.SaidContent{Kind: "text", Text: item.Text}, nil)
		}
		return
	case "plan":
		t.plan = item.Text
		return
	case "collabAgentToolCall":
		t.noteCall("", item, t.uses)
		for thread, state := range item.AgentsStates {
			if state.Status == "pendingInit" || state.Status == "running" {
				t.background[thread] = true
			} else {
				delete(t.background, thread)
			}
		}
		if t.held != nil {
			t.hold(st)
		}
	case "subAgentActivity":
		t.activity(st, "", item)
		switch item.Kind {
		case "started", "interacted":
			t.background[item.AgentThreadID] = true
		case "completed", "interrupted":
			delete(t.background, item.AgentThreadID)
		}
		if t.held != nil {
			t.hold(st)
		}
		return
	}
	if !IsTool(item.Type) {
		return
	}
	t.finishTool("", t.uses, t.changes, item)
}

// imagesOf is the images an item shows: one Codex viewed or drew, or an MCP tool's.
func (t *Session) imagesOf(item Item) []cohesion.Image {
	var images []cohesion.Image
	switch item.Type {
	case "imageView":
		if image, ok := ImageFile(item.Path); ok {
			images = append(images, image)
		}
	case "imageGeneration":
		if item.Base64 != nil && *item.Base64 != "" {
			images = append(images, cohesion.Image{MediaType: "image/png", Data: *item.Base64})
		} else if item.SavedPath != nil {
			if image, ok := ImageFile(*item.SavedPath); ok {
				images = append(images, image)
			}
		}
	case "mcpToolCall":
		if item.Result != nil {
			images = McpImages(item.Result.Content)
		}
	}
	return images
}

// McpImages is the images among an MCP tool's content blocks; one by URL is skipped.
func McpImages(content []json.RawMessage) []cohesion.Image {
	var images []cohesion.Image
	for _, raw := range content {
		var block struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		}
		if json.Unmarshal(raw, &block) == nil && block.Type == "image" && block.Data != "" {
			images = append(images, cohesion.Image{MediaType: block.MimeType, Data: block.Data})
		}
	}
	return images
}

// planUpdated shows Codex's to-do list as a tool use, as Claude Code's
// TodoWrite shows: what it is on now.
func (t *Session) planUpdated(steps []planStep) {
	if len(steps) == 0 {
		return
	}
	done, current := 0, ""
	for _, step := range steps {
		if step.Status == "completed" {
			done++
		} else if step.Status == "inProgress" && current == "" {
			current = step.Step
		}
	}
	summary := fmt.Sprintf("%d of %d done", done, len(steps))
	if current != "" {
		summary = line(current)
	}
	if id := t.show(cohesion.SaidContent{Kind: "toolUse", Tool: "UpdatePlan", Category: cohesion.CategoryOther, Summary: summary}, nil); id != -1 {
		t.report(cohesion.Report{Kind: "end", MessageID: id})
	}
}

func (t *Session) turnCompleted(st *stretch, done turn) {
	if st.turnID == done.ID || st.turnID == "" {
		st.turnID = ""
	}
	stopped := t.stopping
	t.stopping = false
	t.release()
	go t.measure(st, t.record.Titled == TitledPending)
	// Requests Codex cleared itself with the turn.
	for id, w := range t.waiting {
		if w.stretch == st {
			delete(t.waiting, id)
		}
	}
	defer t.linger(st)
	switch done.Status {
	case "completed":
		if t.plan != "" && t.record.Mode == "plan" && !stopped {
			t.askPlan(t.plan)
			t.plan = ""
			return
		}
		// The turn isn't over while its background agents run.
		if len(t.background) > 0 && !stopped {
			t.hold(st)
			return
		}
		t.setProgress(nil)
		t.setStatus(cohesion.Idle)
	case "failed":
		t.setProgress(nil)
		if stopped {
			t.setLocalStatus(cohesion.Idle)
			return
		}
		why := "Codex failed."
		if done.Error != nil && done.Error.Message != "" {
			why = done.Error.Message
		}
		t.reportWait(text(why))
		t.setStatus(cohesion.Failed)
	default:
		// Interrupted: by the user's stop, which cohesiond has seen, or by
		// an aborted request, which it hasn't.
		t.setProgress(nil)
		if stopped {
			t.setLocalStatus(cohesion.Idle)
		} else {
			t.setStatus(cohesion.Idle)
		}
	}
}

// measure reads, after a turn, the plan's limits and the thread's name.
func (t *Session) measure(st *stretch, titling bool) {
	// Every turn moves the plan's limits.
	t.ctx.Usage.Pull(st.process, true)
	if !titling {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var reply struct {
		Thread struct {
			Name *string `json:"name"`
		} `json:"thread"`
	}
	if err := st.process.Call(ctx, "thread/read", map[string]any{"threadId": st.threadID}, &reply); err != nil {
		if !errors.Is(err, codex.ErrClosed) {
			t.logf("couldn't read the thread's name: %v", err)
		}
		return
	}
	if reply.Thread.Name != nil {
		name := *reply.Thread.Name
		t.events.push(func() { t.titleFrom(name) })
	}
}

// release reports the tool uses still held, whose results never came.
func (t *Session) release() {
	for id, u := range t.uses {
		if u.held {
			t.show(u.content, nil)
		}
		delete(t.uses, id)
	}
}

// ---- Requests --------------------------------------------------------------

// request is Codex asking the user: an approval or a question, made a
// request in the transcript, needing attention until the user responds.
func (t *Session) request(st *stretch, r codex.ServerRequest) {
	switch r.Method {
	case CommandApproval, FileChangeApproval, PermissionsRequest, UserInputRequest, ElicitationRequest:
	default:
		_ = st.process.RespondError(r.ID, "This client can't "+r.Method+".")
		return
	}
	if st.closed {
		return
	}
	var params ApprovalParams
	if err := json.Unmarshal(r.Params, &params); err != nil {
		_ = st.process.RespondError(r.ID, "Malformed request: "+err.Error())
		return
	}
	if params.ThreadID != "" && params.ThreadID != st.threadID {
		t.childRequest(st, r, params)
		return
	}
	asked := Request(r.Method, params, t.changes[params.ItemID], t.ctx.Cwd)
	id := t.reportWait(asked)
	if id == -1 {
		// Refused, such as for a session cohesiond no longer has: declined.
		_ = st.process.Respond(r.ID, Response(r.Method, params, &cohesion.UserContent{Kind: "text"}))
		return
	}
	method := r.Method
	t.waiting[id] = &waiting{stretch: st, rpcID: string(r.ID), answer: func(response *cohesion.UserContent) {
		if st.closed {
			return
		}
		if err := st.process.Respond(r.ID, Response(method, params, response)); err != nil {
			t.logf("couldn't answer Codex: %v", err)
		}
		if response == nil {
			return
		}
		if len(t.waiting) == 0 && !t.stopping {
			t.setStatus(cohesion.Working)
		}
		// What the user said with a denial follows as words.
		if note := Note(response); note != "" && response.Decision == cohesion.Deny {
			t.say(note, nil)
		}
	}}
	t.setStatus(cohesion.NeedsAttention)
}

// askPlan shows the plan Codex wrote in plan mode for the user to approve:
// approved, the session leaves plan mode and Codex implements it.
func (t *Session) askPlan(plan string) {
	id := t.reportWait(cohesion.SaidContent{Kind: "plan", Plan: plan})
	t.setProgress(nil)
	if id == -1 {
		t.setStatus(cohesion.Idle)
		return
	}
	t.waiting[id] = &waiting{answer: func(response *cohesion.UserContent) {
		if response == nil || response.Kind != "plan" {
			return
		}
		t.respondToPlan(*response, nil)
	}}
	t.setStatus(cohesion.NeedsAttention)
}

func (t *Session) respondToPlan(response cohesion.UserContent, images []cohesion.Image) {
	if response.Approved && t.record.Mode == "plan" {
		// The approved plan moves the session to Auto; a later process mustn't go back.
		t.record.Mode = AfterPlan
		t.ctx.Store.Put(t.ID, t.record)
	}
	t.setStatus(cohesion.Working)
	t.say(Describe(response), images)
}

func (t *Session) hear(content cohesion.UserContent, images []cohesion.Image) {
	if content.Kind == "text" {
		// Words in place of a response: every request waiting is passed over.
		for id, w := range t.waiting {
			delete(t.waiting, id)
			if w.stretch != nil {
				w.answer(&content)
			}
		}
		t.say(content.Text, images)
		return
	}
	w, ok := t.waiting[content.RequestID]
	if !ok {
		// Asked by a process that has since ended: Codex reads it as words.
		if content.Kind == "plan" {
			t.respondToPlan(content, images)
			return
		}
		t.setStatus(cohesion.Working)
		t.say(Describe(content), images)
		return
	}
	delete(t.waiting, content.RequestID)
	w.answer(&content)
	// A response can't carry images to Codex, so they follow it.
	if len(images) > 0 {
		t.say("The images I attached to my response:", images)
	}
}

func (t *Session) stop() {
	busy := t.status == cohesion.Working || t.status == cohesion.NeedsAttention
	wasHeld := t.held != nil
	t.unhold()
	t.setLocalStatus(cohesion.Idle)
	t.setLocalProgress(nil)
	// A plan waits on nothing but the user: stopping drops it.
	for id, w := range t.waiting {
		if w.stretch == nil {
			delete(t.waiting, id)
		}
	}
	st := t.stretch
	// What a subagent waits on is abandoned with the session's turn.
	for _, c := range t.children {
		for id, w := range c.waiting {
			delete(c.waiting, id)
			w.answer(nil)
		}
	}
	if st == nil || !busy {
		return
	}
	// The background agents go too, or Codex would take them up again once done.
	for thread, turnID := range t.childTurns {
		go func(thread, turnID string) {
			if err := st.process.Call(context.Background(), "turn/interrupt", map[string]any{"threadId": thread, "turnId": turnID}, nil); err != nil {
				t.logf("couldn't stop agent %s: %v", thread, err)
			}
		}(thread, turnID)
	}
	// A held turn has ended already: there's nothing to interrupt.
	if wasHeld || st.turnID == "" {
		return
	}
	t.stopping = true
	for id, w := range t.waiting {
		delete(t.waiting, id)
		w.answer(nil)
	}
	turnID := st.turnID
	go func() {
		if err := st.process.Call(context.Background(), "turn/interrupt", map[string]any{"threadId": st.threadID, "turnId": turnID}, nil); err != nil {
			t.logf("couldn't stop the turn: %v", err)
		}
	}()
}

// ---- Holding for background agents -----------------------------------------

// hold keeps the session working once Codex's turn has ended with background
// agents running, saying what it waits on. Once they are done Codex takes
// their results up in a turn of its own, which ends the wait; should none
// begin soon, the session is idle anyway.
func (t *Session) hold(st *stretch) {
	if t.held == nil {
		t.held = &hold{}
	}
	h := t.held
	if h.resume != nil {
		h.resume.Stop()
		h.resume = nil
	}
	if n := len(t.background); n > 0 {
		progress := "Waiting on a background agent"
		if n > 1 {
			progress = fmt.Sprintf("Waiting on %d background agents", n)
		}
		t.setProgress(&progress)
		return
	}
	t.setProgress(nil)
	h.resume = time.AfterFunc(Resume, func() {
		t.events.push(func() {
			if t.held != h || t.stretch != st {
				return
			}
			t.settle()
			t.linger(st)
		})
	})
}

func (t *Session) unhold() {
	if t.held != nil && t.held.resume != nil {
		t.held.resume.Stop()
	}
	t.held = nil
}

// resume ends the wait on background agents for a turn that begins.
func (t *Session) resume() {
	if t.held == nil {
		return
	}
	t.unhold()
	t.setProgress(nil)
}

// settle makes a session that waited on its background agents idle.
func (t *Session) settle() {
	t.unhold()
	t.setProgress(nil)
	t.setStatus(cohesion.Idle)
}

// ---- Reports ---------------------------------------------------------------

// titleFrom says Codex's name for the thread as the session's title, for a
// session Cohesion titled with its prompt's first line, once Codex has one and
// whenever it changes.
func (t *Session) titleFrom(name string) {
	name = strings.TrimSpace(name)
	if t.record.Titled != TitledPending || name == "" || name == t.record.Title {
		return
	}
	t.record.Title = name
	t.ctx.Store.Put(t.ID, t.record)
	t.snapMu.Lock()
	t.snap.title = name
	t.snapMu.Unlock()
	t.report(cohesion.Report{Kind: "title", Title: name})
}

// setContext says how full the context is now, unless unchanged.
func (t *Session) setContext(context *cohesion.ContextUsage) {
	if context == nil || (t.said != nil && *t.said == *context) {
		return
	}
	t.said = context
	t.report(cohesion.Report{Kind: "context", Context: context})
}

func (t *Session) setStatus(status string) {
	if t.status == status {
		return
	}
	t.setLocalStatus(status)
	t.report(cohesion.Report{Kind: "status", Status: status})
}

// setLocalStatus is a status cohesiond set itself, so it isn't reported.
func (t *Session) setLocalStatus(status string) {
	t.status = status
	t.snapMu.Lock()
	t.snap.status = status
	t.snapMu.Unlock()
}

func (t *Session) setProgress(progress *string) {
	if equal(t.progress, progress) {
		return
	}
	t.setLocalProgress(progress)
	t.report(cohesion.Report{Kind: "progress", Progress: progress})
}

func (t *Session) setLocalProgress(progress *string) {
	t.progress = progress
	t.snapMu.Lock()
	t.snap.progress = progress
	t.snapMu.Unlock()
}

// show reports `content` in the transcript with the images cohesiond will
// take, saying so when there are some it can't show: it refuses a whole
// message for one image, so a refused one goes again without them. It
// returns the message's id, or -1 when refused.
func (t *Session) show(content cohesion.SaidContent, images []cohesion.Image) int64 {
	return t.showTo("", content, images)
}

// showTo is `show` in subagent `sub`'s transcript, or the session's when "".
func (t *Session) showTo(sub string, content cohesion.SaidContent, images []cohesion.Image) int64 {
	kept, dropped := Sendable(images)
	for _, why := range dropped {
		t.logf("an image can't be shown: %s", why)
	}
	unshown := len(dropped)
	var id int64
	if len(kept) > 0 {
		message, err := t.ctx.Client.Report(t.ID, cohesion.Report{Kind: "message", Subagent: sub, Content: &content, Images: kept}).Wait()
		if err != nil {
			t.logf("update with images refused: %v", err)
			unshown += len(kept)
			id = t.reportWaitTo(sub, content)
		} else {
			id = message.ID
		}
	} else {
		id = t.reportWaitTo(sub, content)
	}
	if unshown > 0 && id != -1 {
		note := "(An image couldn't be shown here.)"
		if unshown > 1 {
			note = fmt.Sprintf("(%d images couldn't be shown here.)", unshown)
		}
		t.report(cohesion.Report{Kind: "message", Subagent: sub, Content: &cohesion.SaidContent{Kind: "text", Text: note}})
	}
	return id
}

// reportWait reports a message and waits for its id, or -1 when refused.
func (t *Session) reportWait(content cohesion.SaidContent) int64 {
	return t.reportWaitTo("", content)
}

// reportWaitTo is `reportWait` in subagent `sub`'s transcript, or the session's when "".
func (t *Session) reportWaitTo(sub string, content cohesion.SaidContent) int64 {
	message, err := t.ctx.Client.Report(t.ID, cohesion.Report{Kind: "message", Subagent: sub, Content: &content}).Wait()
	if err != nil || message == nil {
		if err != nil {
			t.logf("update refused: %v", err)
		}
		return -1
	}
	return message.ID
}

// report sends a report in order, without waiting on its reply; a refused
// one, such as for a session cohesiond no longer has, is dropped.
func (t *Session) report(r any) {
	var report cohesion.Report
	switch r := r.(type) {
	case cohesion.Report:
		report = r
	case cohesion.SaidContent:
		report = cohesion.Report{Kind: "message", Content: &r}
	}
	pending := t.ctx.Client.Report(t.ID, report)
	go func() {
		if _, err := pending.Wait(); err != nil {
			t.logf("update refused: %v", err)
		}
	}()
}

func (t *Session) logf(format string, args ...any) {
	t.ctx.Log(fmt.Sprintf("session %d: ", t.ID) + fmt.Sprintf(format, args...))
}

// ---- Helpers ---------------------------------------------------------------

func text(words string) cohesion.SaidContent {
	return cohesion.SaidContent{Kind: "text", Text: words}
}

// sameSandbox is whether two modes load a thread alike: the same approval
// policy and sandbox. Plan mode differs only per turn.
func sameSandbox(a, b string) bool {
	ma, _ := ModeNamed(a)
	mb, _ := ModeNamed(b)
	return ma.Approval == mb.Approval && ma.Sandbox == mb.Sandbox
}

func equal(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// queue runs functions one at a time, in the order pushed, never blocking a push.
type queue struct {
	mu    sync.Mutex
	items []func()
	ready chan struct{}
}

func newQueue() *queue { return &queue{ready: make(chan struct{}, 1)} }

func (q *queue) push(f func()) {
	q.mu.Lock()
	q.items = append(q.items, f)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *queue) run() {
	for range q.ready {
		for {
			q.mu.Lock()
			if len(q.items) == 0 {
				q.mu.Unlock()
				break
			}
			f := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			f()
		}
	}
}

package actor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cohesionhq/actor-codex/internal/codex"
	"github.com/cohesionhq/actor-codex/internal/cohesion"
)

// Subagents: the Codex threads an agent spawns, which run over the same
// app-server connection and report through the same notifications, each
// carrying its own threadId. Cohesion gives one no id of its own; it is
// reported under the session, named by Codex's thread id, and launched first.
// Everything here runs on the session's loop.

// cohesiond's limits on subagents.
const (
	subagentLimit = 256
	subagentDepth = 4
	agentTypeMax  = 64
	modelMax      = 128
	keyMax        = 256
)

// child is a subagent: one Codex thread, launched as far as cohesiond knows.
type child struct {
	thread string
	parent string // the launching subagent's thread, "" for the session
	depth  int
	status string
	// The turn under way in its thread, or "".
	turnID string
	// The launch as last said, to say again.
	launch cohesion.Launch
	// Refused by cohesiond (or beyond its limits): its events are dropped.
	refused bool
	uses    map[string]*use
	changes map[string][]FileUpdate
	// Requests waiting on the user, by their message's id in its transcript.
	waiting map[int64]*waiting
	said    *cohesion.ContextUsage
}

// call is what a tool call (a collabAgentToolCall) said of the thread it launched.
type call struct {
	// The launching subagent's thread, "" for the session.
	parent string
	// The toolUse message that showed the call, 0 when none.
	messageID int64
	prompt    string
	model     string
	// Whether the thread goes on after the call has ended.
	background bool
}

// spawn is a spawn call begun that names no thread yet.
type spawn struct {
	item string
	call *call
}

// childSnap is a subagent as a reconnect reads it, without waiting on the loop.
type childSnap struct {
	key    string
	launch cohesion.Report
	status string
}

// launchReport is the launch as the wire says it.
func (c *child) launchReport() cohesion.Report {
	launch := c.launch
	return cohesion.Report{Kind: "launch", Subagent: c.thread, Launch: &launch}
}

// snapChildren publishes the subagents for a reconnect.
func (t *Session) snapChildren() {
	list := make([]childSnap, 0, len(t.order))
	for _, key := range t.order {
		c := t.children[key]
		list = append(list, childSnap{key: key, launch: c.launchReport(), status: c.status})
	}
	t.snapMu.Lock()
	t.snap.children = list
	t.snapMu.Unlock()
}

// ---- Following a thread ------------------------------------------------------

// child follows a notification from a thread that isn't the session's: a
// subagent's. The first one launches it.
func (t *Session) child(st *stretch, params threadParams, method string) {
	switch method {
	case "turn/started":
		if params.Turn != nil {
			t.childTurns[params.ThreadID] = params.Turn.ID
		}
	case "turn/completed":
		delete(t.childTurns, params.ThreadID)
	}
	c := t.adopt(st, params.ThreadID)
	if c == nil || c.refused {
		return
	}
	switch method {
	case "turn/started":
		if params.Turn != nil {
			c.turnID = params.Turn.ID
		}
		t.childWorking(c)
	case "item/started":
		if params.Item != nil {
			t.childStarted(c, *params.Item)
		}
	case "item/completed":
		if params.Item != nil {
			t.childCompleted(st, c, *params.Item)
		}
	case "turn/completed":
		if params.Turn != nil {
			t.childTurnCompleted(c, *params.Turn)
		}
	case "thread/tokenUsage/updated":
		if params.Usage != nil {
			if context := ContextOf(*params.Usage); context != nil && (c.said == nil || *c.said != *context) {
				c.said = context
				t.report(cohesion.Report{Kind: "context", Subagent: c.thread, Context: context})
			}
		}
	case "thread/name/updated":
		if params.Name != nil {
			if name := strings.TrimSpace(*params.Name); name != "" && line(name) != c.launch.Title {
				c.launch.Title = line(name)
				t.snapChildren()
				t.report(cohesion.Report{Kind: "title", Subagent: c.thread, Title: c.launch.Title})
			}
		}
	case "serverRequest/resolved":
		for id, w := range c.waiting {
			if w.rpcID == string(params.Request) {
				delete(c.waiting, id)
			}
		}
	}
}

// childWorking says the subagent is working, unless it waits on the user.
func (t *Session) childWorking(c *child) {
	if len(c.waiting) == 0 {
		t.childStatus(c, cohesion.Working)
	}
}

func (t *Session) childStatus(c *child, status string) {
	if c.status == status {
		return
	}
	c.status = status
	t.snapChildren()
	t.report(cohesion.Report{Kind: "status", Subagent: c.thread, Status: status})
}

func (t *Session) childStarted(c *child, item Item) {
	if item.Type == "fileChange" {
		c.changes[item.ID] = item.Changes
	}
	if !IsTool(item.Type) {
		if item.Type == "subAgentActivity" {
			t.activity(t.stretch, c.thread, item)
		}
		return
	}
	t.childWorking(c)
	u := &use{content: ToolUse(item, t.ctx.Cwd), held: ShowsLater(item)}
	c.uses[item.ID] = u
	if !u.held {
		u.shown = append(u.shown, t.showTo(c.thread, u.content, nil))
	}
	t.noteCall(c.thread, item, c.uses)
}

func (t *Session) childCompleted(st *stretch, c *child, item Item) {
	switch item.Type {
	case "agentMessage":
		if strings.TrimSpace(item.Text) != "" {
			t.childWorking(c)
			t.showTo(c.thread, cohesion.SaidContent{Kind: "text", Text: item.Text}, nil)
		}
		return
	case "plan":
		return
	case "subAgentActivity":
		t.activity(st, c.thread, item)
		return
	case "collabAgentToolCall":
		t.noteCall(c.thread, item, c.uses)
	}
	if !IsTool(item.Type) {
		return
	}
	t.finishTool(c.thread, c.uses, c.changes, item)
}

func (t *Session) childTurnCompleted(c *child, done turn) {
	c.turnID = ""
	for id, u := range c.uses {
		if u.held {
			t.showTo(c.thread, u.content, nil)
		}
		delete(c.uses, id)
	}
	for id := range c.waiting {
		delete(c.waiting, id)
	}
	switch done.Status {
	case "failed":
		why := "The agent failed."
		if done.Error != nil && done.Error.Message != "" {
			why = done.Error.Message
		}
		t.showTo(c.thread, text(why), nil)
		t.childStatus(c, cohesion.Failed)
	default:
		// Completed or interrupted: a killed turn can only be idle.
		t.childStatus(c, cohesion.Idle)
	}
}

// childrenGone settles the subagents of a Codex process that ended: their
// threads stopped with it, and what they waited on can't be answered.
func (t *Session) childrenGone(st *stretch) {
	for _, key := range t.order {
		c := t.children[key]
		for id, w := range c.waiting {
			if w.stretch == st {
				delete(c.waiting, id)
			}
		}
		c.turnID = ""
		if c.status == cohesion.Working || c.status == cohesion.NeedsAttention {
			t.childStatus(c, cohesion.Idle)
		}
	}
}

// finishTool is a tool use's item completed, in the session's transcript or
// subagent `sub`'s: what the tool gave back, failed or not. A held tool use is
// reported now, with its images, and the tool has ended.
func (t *Session) finishTool(sub string, uses map[string]*use, changes map[string][]FileUpdate, item Item) {
	u, ok := uses[item.ID]
	if !ok {
		// Begun before this process heard of it: shown now, as it ended.
		u = &use{held: true}
	}
	delete(uses, item.ID)
	delete(changes, item.ID)
	images := t.imagesOf(item)
	if u.held {
		// Its summary is surest now.
		u.content = ToolUse(item, t.ctx.Cwd)
	}
	// One reported already that gave images after all is shown again, with them.
	if u.held || len(images) > 0 {
		u.shown = append(u.shown, t.showTo(sub, u.content, images))
	}
	for _, id := range u.shown {
		// A refused one has nothing to end.
		if id != -1 {
			t.report(cohesion.Report{Kind: "end", Subagent: sub, MessageID: id})
		}
	}
}

// ---- Launching ---------------------------------------------------------------

// noteCall learns from a tool call that launches or addresses threads: which
// toolUse message launched each, and from whom. A spawn names its thread only
// once it has completed, so a thread's own first event may come first; the
// launch said again then carries the message.
func (t *Session) noteCall(owner string, item Item, uses map[string]*use) {
	if item.Type != "collabAgentToolCall" || item.Tool != "spawnAgent" {
		return
	}
	said := &call{parent: owner, background: true}
	if u := uses[item.ID]; u != nil && len(u.shown) > 0 && u.shown[0] != -1 {
		said.messageID = u.shown[0]
	}
	if item.Prompt != nil {
		said.prompt = *item.Prompt
	}
	if item.Model != nil {
		said.model = *item.Model
	}
	if len(item.ReceiverThreadIDs) == 0 {
		for _, s := range t.spawning {
			if s.item == item.ID {
				return
			}
		}
		t.spawning = append(t.spawning, spawn{item: item.ID, call: said})
		return
	}
	for i, s := range t.spawning {
		if s.item == item.ID {
			t.spawning = append(t.spawning[:i], t.spawning[i+1:]...)
			break
		}
	}
	for _, thread := range item.ReceiverThreadIDs {
		t.calls[thread] = said
		if c, ok := t.children[thread]; ok {
			t.relaunch(c, said)
		} else {
			t.adopt(t.stretch, thread)
		}
	}
}

// activity is a subAgentActivity item, in the session's thread or subagent
// `owner`'s: the agent exists, and is going or done.
func (t *Session) activity(st *stretch, owner string, item Item) {
	if item.AgentThreadID == "" || st == nil {
		return
	}
	c := t.adopt(st, item.AgentThreadID)
	if c == nil || c.refused {
		return
	}
	switch item.Kind {
	case "started", "interacted":
		t.childWorking(c)
	case "completed", "interrupted":
		if len(c.waiting) == 0 && c.turnID == "" {
			t.childStatus(c, cohesion.Idle)
		}
	}
}

// relaunch says a subagent again with what `said` now knows of its launch.
func (t *Session) relaunch(c *child, said *call) {
	if c.refused || said.messageID == 0 || (c.launch.MessageID != nil && *c.launch.MessageID == said.messageID) {
		return
	}
	id := said.messageID
	c.launch.MessageID = &id
	if said.model != "" && c.launch.Model == nil {
		model := cutBytes(said.model, modelMax)
		c.launch.Model = &model
	}
	t.snapChildren()
	t.report(c.launchReport())
}

// threadInfo is what Codex says of a thread: its name, what kind of agent it
// is, and the thread that spawned it.
type threadInfo struct {
	name, nickname, role, parent string
}

// readThread asks Codex about `thread`; a thread it can't read yields nothing.
func (t *Session) readThread(st *stretch, thread string) threadInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var reply struct {
		Thread struct {
			Name          *string         `json:"name"`
			AgentNickname *string         `json:"agentNickname"`
			AgentRole     *string         `json:"agentRole"`
			RawSource     json.RawMessage `json:"source"`
		} `json:"thread"`
	}
	if err := st.process.Call(ctx, "thread/read", map[string]any{"threadId": thread}, &reply); err != nil {
		return threadInfo{}
	}
	info := threadInfo{name: deref(reply.Thread.Name), nickname: deref(reply.Thread.AgentNickname), role: deref(reply.Thread.AgentRole)}
	// Its source is `{"subAgent": {"thread_spawn": {...}}}` for a spawned
	// thread, and a bare string for the rest.
	var source struct {
		SubAgent struct {
			ThreadSpawn struct {
				ParentThreadID string `json:"parent_thread_id"`
				AgentNickname  string `json:"agent_nickname"`
				AgentRole      string `json:"agent_role"`
			} `json:"thread_spawn"`
		} `json:"subAgent"`
	}
	if json.Unmarshal(reply.Thread.RawSource, &source) == nil {
		spawned := source.SubAgent.ThreadSpawn
		info.parent = spawned.ParentThreadID
		if info.nickname == "" {
			info.nickname = spawned.AgentNickname
		}
		if info.role == "" {
			info.role = spawned.AgentRole
		}
	}
	return info
}

// adopt is the subagent for `thread`, launching it first when it is new, so
// cohesiond has heard of it before any report about it. Nil when there is no
// Codex process to ask. A subagent cohesiond refuses is kept, refused, so it
// is asked about once.
func (t *Session) adopt(st *stretch, thread string) *child {
	if c, ok := t.children[thread]; ok {
		return c
	}
	if st == nil || thread == "" || thread == st.threadID {
		return nil
	}
	c := &child{
		thread:  thread,
		status:  cohesion.Working,
		uses:    map[string]*use{},
		changes: map[string][]FileUpdate{},
		waiting: map[int64]*waiting{},
	}
	t.children[thread] = c
	if len(t.order) >= subagentLimit || len(thread) > keyMax {
		c.refused = true
		t.logf("subagent %s is beyond cohesiond's limits and isn't shown", thread)
		return c
	}
	info := t.readThread(st, thread)
	said := t.calls[thread]
	if said == nil {
		if len(t.spawning) > 0 && (info.parent == "" || info.parent == st.threadID || t.children[info.parent] != nil) {
			said = t.spawning[0].call
			t.spawning = t.spawning[1:]
		} else {
			said = &call{background: true}
		}
		t.calls[thread] = said
	}
	parent := said.parent
	if parent == "" && info.parent != "" && info.parent != st.threadID {
		if above, ok := t.children[info.parent]; ok && !above.refused {
			parent = info.parent
		}
	}
	c.parent = parent
	c.depth = 1
	if parent != "" {
		above := t.children[parent]
		if above == nil || above.refused {
			c.refused = true
			return c
		}
		c.depth = above.depth + 1
	}
	if c.depth > subagentDepth {
		c.refused = true
		t.logf("subagent %s is deeper than cohesiond takes and isn't shown", thread)
		return c
	}
	title := line(said.prompt)
	for _, option := range []string{info.name, info.nickname, info.role, "Subagent"} {
		if title != "" {
			break
		}
		title = line(option)
	}
	c.launch = cohesion.Launch{Title: title, Background: said.background}
	if parent != "" {
		c.launch.Parent = &parent
	}
	if said.messageID != 0 {
		id := said.messageID
		c.launch.MessageID = &id
	}
	if info.role != "" {
		role := cutBytes(info.role, agentTypeMax)
		c.launch.AgentType = &role
	}
	if said.model != "" {
		model := cutBytes(said.model, modelMax)
		c.launch.Model = &model
	}
	// Waited on: nothing about it goes out before cohesiond has it.
	if _, err := t.ctx.Client.Report(t.ID, c.launchReport()).Wait(); err != nil {
		c.refused = true
		t.logf("subagent %s refused: %v", thread, err)
		return c
	}
	t.order = append(t.order, thread)
	t.snapChildren()
	return c
}

// cutBytes is `text` cut to at most `n` bytes, on a character boundary.
func cutBytes(text string, n int) string {
	if len(text) <= n {
		return text
	}
	for n > 0 && n < len(text) && text[n]&0xC0 == 0x80 {
		n--
	}
	return text[:n]
}

// ---- Requests and answers ----------------------------------------------------

// childRequest is a subagent's Codex asking the user: reported in the
// subagent's own transcript, which then needs attention.
func (t *Session) childRequest(st *stretch, r codex.ServerRequest, params ApprovalParams) {
	decline := func() {
		_ = st.process.Respond(r.ID, Response(r.Method, params, &cohesion.UserContent{Kind: "text"}))
	}
	c := t.adopt(st, params.ThreadID)
	if c == nil || c.refused {
		decline()
		return
	}
	id := t.reportWaitTo(c.thread, Request(r.Method, params, c.changes[params.ItemID], t.ctx.Cwd))
	if id == -1 {
		decline()
		return
	}
	method := r.Method
	c.waiting[id] = &waiting{stretch: st, rpcID: string(r.ID), answer: func(response *cohesion.UserContent) {
		if st.closed {
			return
		}
		if err := st.process.Respond(r.ID, Response(method, params, response)); err != nil {
			t.logf("couldn't answer Codex: %v", err)
		}
		if response != nil && len(c.waiting) == 0 {
			t.childStatus(c, cohesion.Working)
		}
	}}
	t.childStatus(c, cohesion.NeedsAttention)
}

// hearChild is the user's response to a request of subagent `key`.
func (t *Session) hearChild(key string, content cohesion.UserContent, images []cohesion.Image) error {
	c, ok := t.children[key]
	if !ok {
		return fmt.Errorf("subagent %s is no longer running here", key)
	}
	if content.Kind == "text" {
		// Words in place of a response pass its requests over.
		for id, w := range c.waiting {
			delete(c.waiting, id)
			w.answer(&content)
		}
		return t.sayChild(c, content.Text, images)
	}
	w, ok := c.waiting[content.RequestID]
	if !ok {
		// Asked by a process that has since ended: Codex reads it as words.
		return t.sayChild(c, Describe(content), images)
	}
	delete(c.waiting, content.RequestID)
	w.answer(&content)
	if len(images) > 0 {
		return t.sayChild(c, "The images I attached to my response:", images)
	}
	return nil
}

// sayChild gives subagent `c`'s thread words: added to its turn under way,
// else a turn of their own.
func (t *Session) sayChild(c *child, words string, images []cohesion.Image) error {
	if strings.TrimSpace(words) == "" && len(images) == 0 {
		return nil
	}
	st := t.stretch
	if st == nil {
		return fmt.Errorf("the Codex process of subagent %s has ended", c.thread)
	}
	input := t.input(words, images)
	t.childStatus(c, cohesion.Working)
	if c.turnID != "" {
		err := st.process.Call(context.Background(), "turn/steer", map[string]any{"threadId": c.thread, "input": input, "expectedTurnId": c.turnID}, nil)
		if err == nil {
			return nil
		}
		t.logf("couldn't add to the turn of subagent %s: %v", c.thread, err)
	}
	var reply struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := st.process.Call(context.Background(), "turn/start", map[string]any{"threadId": c.thread, "input": input}, &reply); err != nil {
		t.childStatus(c, cohesion.Idle)
		return fmt.Errorf("couldn't reach subagent %s: %v", c.thread, err)
	}
	if c.turnID == "" {
		c.turnID = reply.Turn.ID
	}
	return nil
}

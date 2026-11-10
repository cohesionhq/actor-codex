package actor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cohesionhq/actor-codex/internal/codex"
	"github.com/cohesionhq/actor-codex/internal/cohesion"
)

// The test binary stands in for `codex app-server` when run with
// COHESION_FAKE_CODEX set: a script of what Codex says for each prompt.
func TestMain(m *testing.M) {
	if log := os.Getenv("COHESION_FAKE_CODEX"); log != "" {
		fakeCodex(log)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeCodex(logPath string) {
	logFile, _ := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	defer logFile.Close()
	var writeMu sync.Mutex
	send := func(message map[string]any) {
		line, _ := json.Marshal(message)
		writeMu.Lock()
		os.Stdout.Write(append(line, '\n'))
		writeMu.Unlock()
	}
	notify := func(method string, params map[string]any) {
		params["threadId"] = "thread-1"
		send(map[string]any{"method": method, "params": params})
	}
	notifyOn := func(thread, method string, params map[string]any) {
		params["threadId"] = thread
		send(map[string]any{"method": method, "params": params})
	}
	answers := map[string]chan json.RawMessage{}
	var answersMu sync.Mutex
	ask := func(id, method string, params map[string]any) json.RawMessage {
		reply := make(chan json.RawMessage, 1)
		answersMu.Lock()
		answers[id] = reply
		answersMu.Unlock()
		params["threadId"] = "thread-1"
		send(map[string]any{"id": id, "method": method, "params": params})
		return <-reply
	}
	askOn := func(thread, id, method string, params map[string]any) json.RawMessage {
		reply := make(chan json.RawMessage, 1)
		answersMu.Lock()
		answers[id] = reply
		answersMu.Unlock()
		params["threadId"] = thread
		send(map[string]any{"id": id, "method": method, "params": params})
		return <-reply
	}
	interrupted := make(chan struct{}, 1)
	turns := 0
	agentMessage := func(id, text string) {
		notify("item/completed", map[string]any{"turnId": "t", "item": map[string]any{"type": "agentMessage", "id": id, "text": text}})
	}
	complete := func(turnID, status string) {
		notify("turn/completed", map[string]any{"turn": map[string]any{"id": turnID, "status": status, "items": []any{}, "error": nil}})
	}
	play := func(turnID, prompt string, mode string) {
		notify("turn/started", map[string]any{"turn": map[string]any{"id": turnID, "status": "inProgress"}})
		switch {
		case strings.Contains(prompt, "approve"):
			reply := ask("req-1", "item/commandExecution/requestApproval", map[string]any{"turnId": turnID, "itemId": "c1", "command": "bash -lc 'rm -rf build'", "cwd": "/w", "reason": "Clean up"})
			var decision struct{ Decision string }
			_ = json.Unmarshal(reply, &decision)
			agentMessage("m1", "decision: "+decision.Decision)
			complete(turnID, "completed")
		case strings.Contains(prompt, "hang"):
			<-interrupted
			complete(turnID, "interrupted")
		case strings.Contains(prompt, "delegate"):
			spawn := func(threads []string, status string) map[string]any {
				states := map[string]any{}
				for _, thread := range threads {
					states[thread] = map[string]any{"status": "running"}
				}
				return map[string]any{"type": "collabAgentToolCall", "id": "k1", "tool": "spawnAgent", "status": status, "prompt": "Map the db layer\nthen report", "model": "gpt-child", "receiverThreadIds": threads, "agentsStates": states}
			}
			notify("item/started", map[string]any{"turnId": turnID, "item": spawn([]string{}, "inProgress")})
			notifyOn("child-1", "turn/started", map[string]any{"turn": map[string]any{"id": "ct1", "status": "inProgress"}})
			notify("item/completed", map[string]any{"turnId": turnID, "item": spawn([]string{"child-1"}, "completed")})
			if strings.Contains(prompt, "ask") {
				reply := askOn("child-1", "req-c", "item/commandExecution/requestApproval", map[string]any{"turnId": "ct1", "itemId": "c9", "command": "bash -lc 'rm -rf db'", "cwd": "/w", "reason": "Clean"})
				var decision struct{ Decision string }
				_ = json.Unmarshal(reply, &decision)
				notifyOn("child-1", "item/completed", map[string]any{"turnId": "ct1", "item": map[string]any{"type": "agentMessage", "id": "cm0", "text": "decision: " + decision.Decision}})
			}
			notifyOn("child-1", "item/started", map[string]any{"turnId": "ct1", "item": map[string]any{"type": "commandExecution", "id": "c1", "command": "bash -lc 'ls'", "cwd": "/w", "status": "inProgress", "commandActions": []any{}}})
			notifyOn("child-1", "item/completed", map[string]any{"turnId": "ct1", "item": map[string]any{"type": "commandExecution", "id": "c1", "command": "bash -lc 'ls'", "cwd": "/w", "status": "completed", "commandActions": []any{}, "exitCode": 0}})
			notifyOn("child-1", "item/completed", map[string]any{"turnId": "ct1", "item": map[string]any{"type": "agentMessage", "id": "cm1", "text": "Found it."}})
			notifyOn("child-1", "thread/tokenUsage/updated", map[string]any{"turnId": "ct1", "tokenUsage": map[string]any{"last": map[string]any{"totalTokens": 50}, "modelContextWindow": 1000}})
			notifyOn("child-1", "turn/completed", map[string]any{"turn": map[string]any{"id": "ct1", "status": "completed", "items": []any{}, "error": nil}})
			notify("item/completed", map[string]any{"turnId": turnID, "item": map[string]any{"type": "subAgentActivity", "id": "s9", "kind": "completed", "agentThreadId": "child-1", "agentPath": "x"}})
			complete(turnID, "completed")
		case strings.Contains(prompt, "agent"):
			notify("item/completed", map[string]any{"turnId": turnID, "item": map[string]any{"type": "subAgentActivity", "id": "s1", "kind": "started", "agentThreadId": "child-1", "agentPath": "x"}})
			complete(turnID, "completed")
			time.Sleep(150 * time.Millisecond)
			notify("item/completed", map[string]any{"turnId": turnID, "item": map[string]any{"type": "subAgentActivity", "id": "s2", "kind": "completed", "agentThreadId": "child-1", "agentPath": "x"}})
		case mode == "plan":
			notify("item/completed", map[string]any{"turnId": turnID, "item": map[string]any{"type": "plan", "id": "p1", "text": "1. Do it"}})
			complete(turnID, "completed")
		default:
			notify("item/started", map[string]any{"turnId": turnID, "item": map[string]any{"type": "commandExecution", "id": "c1", "command": "bash -lc 'cargo test'", "cwd": "/w", "status": "inProgress", "commandActions": []any{}}})
			notify("item/completed", map[string]any{"turnId": turnID, "item": map[string]any{"type": "commandExecution", "id": "c1", "command": "bash -lc 'cargo test'", "cwd": "/w", "status": "completed", "commandActions": []any{}, "exitCode": 0}})
			agentMessage("m1", "Done.")
			notify("thread/tokenUsage/updated", map[string]any{"turnId": turnID, "tokenUsage": map[string]any{"last": map[string]any{"totalTokens": 1200}, "modelContextWindow": 272000}})
			complete(turnID, "completed")
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		fmt.Fprintln(logFile, scanner.Text())
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(scanner.Bytes(), &message)
		if message.Method == "" {
			var id string
			_ = json.Unmarshal(message.ID, &id)
			answersMu.Lock()
			reply := answers[id]
			answersMu.Unlock()
			if reply != nil {
				reply <- message.Result
			}
			continue
		}
		result := map[string]any{}
		var params struct {
			Input []struct {
				Text string `json:"text"`
			} `json:"input"`
			CollaborationMode *struct {
				Mode string `json:"mode"`
			} `json:"collaborationMode"`
		}
		_ = json.Unmarshal(message.Params, &params)
		prompt := ""
		for _, input := range params.Input {
			prompt += input.Text
		}
		switch message.Method {
		case "initialized":
			continue
		case "thread/start", "thread/resume":
			result = map[string]any{"thread": map[string]any{"id": "thread-1", "name": nil}, "model": "gpt-test"}
		case "thread/read":
			result = map[string]any{"thread": map[string]any{"id": "thread-1", "name": "Fix the tests"}}
		case "turn/start":
			turns++
			turnID := fmt.Sprintf("turn-%d", turns)
			result = map[string]any{"turn": map[string]any{"id": turnID, "status": "inProgress"}}
			mode := ""
			if params.CollaborationMode != nil {
				mode = params.CollaborationMode.Mode
			}
			go func() {
				time.Sleep(10 * time.Millisecond)
				play(turnID, prompt, mode)
			}()
		case "turn/interrupt":
			select {
			case interrupted <- struct{}{}:
			default:
			}
		case "account/read":
			result = map[string]any{"account": map[string]any{"type": "apiKey"}, "requiresOpenaiAuth": true}
		}
		send(map[string]any{"id": message.ID, "result": result})
	}
}

// fakeClient is cohesiond: it stores each report, numbering messages.
type fakeClient struct {
	mu      sync.Mutex
	reports []cohesion.Report
	nextID  int64
}

func (c *fakeClient) Report(_ int64, report cohesion.Report) *cohesion.Pending {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reports = append(c.reports, report)
	if report.Kind != "message" {
		return cohesion.Resolved(nil, nil)
	}
	c.nextID++
	return cohesion.Resolved(&cohesion.ActorSessionMessage{ID: c.nextID}, nil)
}

// describe is each report in a line, such as `status idle` or `message toolUse Shell cargo test`.
func (c *fakeClient) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var lines []string
	for _, r := range c.reports {
		// A subagent's reports read as its own: `[child-1] status idle`.
		before := len(lines)
		switch r.Kind {
		case "launch":
			launch := r.Launch
			lines = append(lines, fmt.Sprintf("launch %q parent=%s message=%s type=%s model=%s background=%v", launch.Title, orNone(launch.Parent), idOrNone(launch.MessageID), orNone(launch.AgentType), orNone(launch.Model), launch.Background))
		case "message":
			content := r.Content
			switch content.Kind {
			case "text":
				lines = append(lines, "message text "+content.Text)
			case "toolUse":
				lines = append(lines, fmt.Sprintf("message toolUse %s %s", content.Tool, content.Summary))
			case "permission":
				lines = append(lines, "message permission "+content.Subject.Command)
			default:
				lines = append(lines, "message "+content.Kind)
			}
		case "status":
			lines = append(lines, "status "+r.Status)
		case "progress":
			if r.Progress == nil {
				lines = append(lines, "progress -")
			} else {
				lines = append(lines, "progress "+*r.Progress)
			}
		case "end":
			lines = append(lines, fmt.Sprintf("end %d", r.MessageID))
		case "context":
			lines = append(lines, fmt.Sprintf("context %d/%d", r.Context.UsedTokens, r.Context.TotalTokens))
		case "title":
			lines = append(lines, "title "+r.Title)
		}
		if r.Subagent != "" && len(lines) > before {
			lines[before] = "[" + r.Subagent + "] " + lines[before]
		}
	}
	return lines
}

func orNone(text *string) string {
	if text == nil {
		return "-"
	}
	return *text
}

func idOrNone(id *int64) string {
	if id == nil {
		return "-"
	}
	return fmt.Sprint(*id)
}

func (c *fakeClient) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range c.lines() {
			if line == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("never reported %q; reported:\n%s", want, strings.Join(c.lines(), "\n"))
}

type harness struct {
	session *Session
	client  *fakeClient
	log     string
	store   *Store
}

func newHarness(t *testing.T, record Record) *harness {
	t.Helper()
	dir := t.TempDir()
	log := dir + "/codex.log"
	t.Setenv("COHESION_FAKE_CODEX", log)
	executable, _ := os.Executable()
	codex.Binary = executable
	store, _ := OpenStore(dir)
	client := &fakeClient{}
	usage := NewUsage(func(string, cohesion.ActorUsage) error { return nil }, func() []string { return nil }, func(string) {})
	session := NewSession(1, record, SessionContext{Client: client, Store: store, Cwd: dir, Usage: usage, Log: func(string) {}})
	t.Cleanup(session.Close)
	return &harness{session: session, client: client, log: log, store: store}
}

// sent is the requests the fake Codex heard of `method`, as JSON.
func (h *harness) sent(method string) []string {
	data, _ := os.ReadFile(h.log)
	var found []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, `"method":"`+method+`"`) {
			found = append(found, line)
		}
	}
	return found
}

func (h *harness) waitSent(t *testing.T, method, containing string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range h.sent(method) {
			if strings.Contains(line, containing) {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("never sent %s with %s; sent: %v", method, containing, h.sent(method))
}

func TestTurnReportsToolsTextContextAndTitle(t *testing.T) {
	h := newHarness(t, Record{Actor: "a", Mode: "auto", Titled: TitledPending})
	h.session.Say("Fix the tests", nil)
	h.client.waitFor(t, "status idle")
	h.client.waitFor(t, "title Fix the tests")
	lines := strings.Join(h.client.lines(), "\n")
	want := "message toolUse Shell cargo test\nprogress Running cargo test\nend 1\nmessage text Done.\ncontext 1200/272000\nprogress -\nstatus idle"
	if !strings.Contains(lines, want) {
		t.Errorf("reported:\n%s\nwant within it:\n%s", lines, want)
	}
	// The thread is kept, to resume after a restart.
	if record, _ := h.store.Get(1); record.ThreadID != "thread-1" || record.Title != "Fix the tests" {
		t.Errorf("record %+v", record)
	}
	start := h.sent("thread/start")
	if len(start) != 1 || !strings.Contains(start[0], `"sandbox":"workspace-write"`) || !strings.Contains(start[0], `"approvalPolicy":"on-request"`) {
		t.Errorf("thread/start %v", start)
	}
}

func TestApprovalWaitsOnTheUser(t *testing.T) {
	h := newHarness(t, Record{Actor: "a", Mode: "auto", Titled: TitledGiven, Title: "Clean"})
	h.session.Say("approve this", nil)
	h.client.waitFor(t, "message permission rm -rf build")
	h.client.waitFor(t, "status needsAttention")
	h.session.Hear(cohesion.UserContent{Kind: "permission", RequestID: 1, Decision: cohesion.AllowForSession}, nil)
	h.client.waitFor(t, "message text decision: acceptForSession")
	h.client.waitFor(t, "status idle")
	lines := strings.Join(h.client.lines(), "\n")
	if !strings.Contains(lines, "status needsAttention\nstatus working") {
		t.Errorf("not working again after the response:\n%s", lines)
	}
	// A given title names the thread, and is never replaced.
	h.waitSent(t, "thread/name/set", `"name":"Clean"`)
	if strings.Contains(lines, "title ") {
		t.Errorf("a given title was replaced:\n%s", lines)
	}
}

func TestWordsPassOverARequest(t *testing.T) {
	h := newHarness(t, Record{Actor: "a", Mode: "auto"})
	h.session.Say("approve this", nil)
	h.client.waitFor(t, "status needsAttention")
	h.session.Hear(cohesion.UserContent{Kind: "text", Text: "Leave the build alone."}, nil)
	h.client.waitFor(t, "message text decision: decline")
	// The words reach Codex: added to the turn, or the next one's.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.sent("turn/steer"))+len(h.sent("turn/start")) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	all := strings.Join(append(h.sent("turn/steer"), h.sent("turn/start")...), "\n")
	if !strings.Contains(all, "Leave the build alone.") {
		t.Errorf("the words never reached Codex:\n%s", all)
	}
}

func TestPlanApprovalImplementsIt(t *testing.T) {
	h := newHarness(t, Record{Actor: "a", Mode: "plan"})
	h.session.Say("Plan the fix", nil)
	h.client.waitFor(t, "message plan")
	h.client.waitFor(t, "status needsAttention")
	start := h.sent("thread/start")
	if !strings.Contains(start[0], `"sandbox":"read-only"`) || !strings.Contains(h.sent("turn/start")[0], `"mode":"plan"`) {
		t.Fatalf("plan mode not asked for: %v %v", start, h.sent("turn/start"))
	}
	h.session.Hear(cohesion.UserContent{Kind: "plan", RequestID: 1, Approved: true}, nil)
	// Auto's sandbox takes a thread loaded with it: resumed, then the plan implemented.
	h.waitSent(t, "thread/resume", `"sandbox":"workspace-write"`)
	h.waitSent(t, "turn/start", `"mode":"default"`)
	h.waitSent(t, "turn/start", ImplementPlan)
	if record, _ := h.store.Get(1); record.Mode != "auto" {
		t.Errorf("mode after the plan: %q", record.Mode)
	}
}

func TestStopIsIdleWithoutAReport(t *testing.T) {
	h := newHarness(t, Record{Actor: "a", Mode: "auto"})
	h.session.Say("hang on", nil)
	h.waitSent(t, "turn/start", "hang on")
	time.Sleep(50 * time.Millisecond)
	h.session.Stop()
	h.waitSent(t, "turn/interrupt", `"turnId":"turn-1"`)
	time.Sleep(100 * time.Millisecond)
	for _, line := range h.client.lines() {
		if line == "status idle" || line == "status failed" {
			t.Fatalf("a stopped session reported %q: cohesiond set it idle itself", line)
		}
	}
}

func TestBackgroundAgentHoldsTheTurn(t *testing.T) {
	Resume = 50 * time.Millisecond
	t.Cleanup(func() { Resume = 10 * time.Second })
	h := newHarness(t, Record{Actor: "a", Mode: "auto"})
	h.session.Say("start an agent", nil)
	h.client.waitFor(t, "progress Waiting on a background agent")
	for _, line := range h.client.lines() {
		if line == "status idle" {
			t.Fatal("idle while an agent works")
		}
	}
	h.client.waitFor(t, "status idle")
}

func TestResponseToAGoneRequestIsWords(t *testing.T) {
	h := newHarness(t, Record{Actor: "a", Mode: "auto"})
	message := "Not now."
	h.session.Hear(cohesion.UserContent{Kind: "permission", RequestID: 99, Decision: cohesion.Deny, Message: &message}, nil)
	h.waitSent(t, "turn/start", "About your earlier request: no. Not now.")
}

func indexOf(lines []string, want string) int {
	for i, line := range lines {
		if line == want {
			return i
		}
	}
	return -1
}

func TestSubagentIsLaunchedBeforeItsEvents(t *testing.T) {
	h := newHarness(t, Record{Actor: "a", Mode: "auto", Titled: TitledGiven, Title: "T"})
	h.session.Say("delegate this", nil)
	h.client.waitFor(t, "status idle")
	h.client.waitFor(t, "[child-1] status idle")
	lines := h.client.lines()
	// The collabAgentToolCall is message 1; the launch names it, and carries what the call said.
	launch := indexOf(lines, `[child-1] launch "Map the db layer" parent=- message=1 type=- model=gpt-child background=true`)
	// The child's turn began before the call said its thread: its launch still came first, and was said again with the message.
	first := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "[child-1] ") {
			first = i
			break
		}
	}
	if launch == -1 || first == -1 || launch > first {
		t.Fatalf("the subagent wasn't launched first:\n%s", strings.Join(lines, "\n"))
	}
	for _, want := range []string{"[child-1] message toolUse Shell ls", "[child-1] end 2", "[child-1] message text Found it.", "[child-1] context 50/1000", "[child-1] status idle"} {
		if indexOf(lines, want) == -1 {
			t.Errorf("never reported %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "message") && strings.Contains(line, "Found it") {
			t.Errorf("a subagent's words landed in the session's transcript: %q", line)
		}
	}
	// A reconnect launches it again, with where it stands.
	h.session.snapMu.Lock()
	children := h.session.snap.children
	h.session.snapMu.Unlock()
	if len(children) != 1 || children[0].key != "child-1" || children[0].status != cohesion.Idle || children[0].launch.Launch.MessageID == nil {
		t.Errorf("snapshot of subagents: %+v", children)
	}
}

func TestSubagentRequestsGoInItsOwnTranscript(t *testing.T) {
	h := newHarness(t, Record{Actor: "a", Mode: "auto", Titled: TitledGiven, Title: "T"})
	h.session.Say("delegate and ask", nil)
	h.client.waitFor(t, "[child-1] message permission rm -rf db")
	h.client.waitFor(t, "[child-1] status needsAttention")
	for _, line := range h.client.lines() {
		if line == "status needsAttention" || line == "message permission rm -rf db" {
			t.Fatalf("the child's request showed in the session: %q", line)
		}
	}
	// Collab message is 1, the permission 2: answered by key, to the child.
	if err := h.session.HearChild("child-1", cohesion.UserContent{Kind: "permission", RequestID: 2, Decision: cohesion.AllowForSession}, nil); err != nil {
		t.Fatal(err)
	}
	h.client.waitFor(t, "[child-1] message text decision: acceptForSession")
	h.client.waitFor(t, "[child-1] status working")
	h.client.waitFor(t, "[child-1] status idle")
	if err := h.session.HearChild("nobody", cohesion.UserContent{Kind: "text", Text: "x"}, nil); err == nil {
		t.Error("an unknown subagent took a response")
	}
}

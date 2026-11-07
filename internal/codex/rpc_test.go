package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test binary stands in for `codex app-server` when this is set: see
// fakeAppServer.
const fakeEnv = "ACTOR_CODEX_FAKE_APP_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) != "" {
		fakeAppServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeAppServer answers one JSON-RPC message per line on stdin:
//   - initialize: {} once it carries clientInfo and experimentalApi.
//   - echo: its params.
//   - fail: an error.
//   - first, second: once both have arrived, whichever came second is
//     answered first, each with its own name.
//   - ask: first a malformed line, a notification and a request of its own
//     (`approve`), then its answer once the client has answered `approve`,
//     with what the client answered.
//   - big: a 2 MB string.
//   - exit: exits with status 3, answering nothing.
func fakeAppServer() {
	out := json.NewEncoder(os.Stdout)
	in := bufio.NewReader(os.Stdin)
	var ask json.RawMessage
	var pair []json.RawMessage
	var names []string
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(line, &m) != nil {
			os.Exit(2)
		}
		reply := func(result any) { _ = out.Encode(map[string]any{"id": m.ID, "result": result}) }
		switch m.Method {
		case "initialize":
			if !strings.Contains(string(m.Params), `"clientInfo"`) || !strings.Contains(string(m.Params), `"experimentalApi":true`) {
				os.Exit(4)
			}
			reply(map[string]any{})
		case "initialized":
		case "echo":
			reply(m.Params)
		case "fail":
			_ = out.Encode(map[string]any{"id": m.ID, "error": map[string]any{"code": -32000, "message": "no such thread"}})
		case "first", "second":
			pair, names = append(pair, m.ID), append(names, m.Method)
			if len(pair) == 2 {
				_ = out.Encode(map[string]any{"id": pair[1], "result": names[1]})
				_ = out.Encode(map[string]any{"id": pair[0], "result": names[0]})
			}
		case "ask":
			ask = m.ID
			os.Stdout.WriteString("{not json\n")
			_ = out.Encode(map[string]any{"method": "turn/started", "params": map[string]any{"n": 1}})
			_ = out.Encode(map[string]any{"id": "s1", "method": "approve", "params": map[string]any{}})
		case "big":
			reply(strings.Repeat("x", 2<<20))
		case "exit":
			os.Exit(3)
		case "":
			// The client's answer to `approve`.
			answer := m.Result
			if len(m.Error) > 0 {
				answer = m.Error
			}
			_ = out.Encode(map[string]any{"id": ask, "result": answer})
		}
	}
}

type recorder struct {
	mu            sync.Mutex
	notifications []Notification
	logs          []string
}

func (r *recorder) log(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, s)
}

func start(t *testing.T, handler Handler) (*Process, *recorder) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	previous := Binary
	Binary = executable
	t.Cleanup(func() { Binary = previous })
	t.Setenv(fakeEnv, "1")
	r := &recorder{}
	if handler.Notify == nil {
		handler.Notify = func(n Notification) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.notifications = append(r.notifications, n)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := Start(ctx, t.TempDir(), handler, r.log)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(p.Close)
	return p, r
}

func call(t *testing.T, p *Process, method string, params any, into any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.Call(ctx, method, params, into)
}

func TestACallGetsItsOwnAnswerWhateverTheOrder(t *testing.T) {
	p, _ := start(t, Handler{})
	got := map[string]*string{"first": new(string), "second": new(string)}
	var wg sync.WaitGroup
	for method, into := range got {
		wg.Go(func() {
			if err := call(t, p, method, nil, into); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for method, into := range got {
		if *into != method {
			t.Errorf("%s got %q", method, *into)
		}
	}

	var echoed map[string]int
	if err := call(t, p, "echo", map[string]int{"n": 7}, &echoed); err != nil || echoed["n"] != 7 {
		t.Fatalf("echo: %v %v", echoed, err)
	}
}

func TestAnErrorAnswerIsAnRPCError(t *testing.T) {
	p, _ := start(t, Handler{})
	err := call(t, p, "fail", nil, nil)
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != -32000 || rpc.Message == "" {
		t.Fatalf("got %#v", err)
	}
	// The process carries on.
	if err := call(t, p, "echo", 1, nil); err != nil {
		t.Fatal(err)
	}
}

func TestARequestFromCodexWithNoHandlerIsRefusedAndTheRestStillArrives(t *testing.T) {
	p, r := start(t, Handler{})
	var answer struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := call(t, p, "ask", nil, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Code == 0 || !strings.Contains(answer.Message, "approve") {
		t.Fatalf("Codex got %+v", answer)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.notifications) != 1 || r.notifications[0].Method != "turn/started" {
		t.Fatalf("notifications: %+v", r.notifications)
	}
	if len(r.logs) == 0 {
		t.Fatal("the malformed line wasn't logged")
	}
}

func TestARequestFromCodexIsHandedToItsHandler(t *testing.T) {
	var p *Process
	p, _ = start(t, Handler{Request: func(request ServerRequest) {
		if request.Method != "approve" {
			t.Errorf("request %q", request.Method)
		}
		go func() { _ = p.Respond(request.ID, map[string]string{"decision": "accept"}) }()
	}})
	var answer map[string]string
	if err := call(t, p, "ask", nil, &answer); err != nil {
		t.Fatal(err)
	}
	if answer["decision"] != "accept" {
		t.Fatalf("Codex got %v", answer)
	}
}

func TestALineLongerThanTheReadBufferArrivesWhole(t *testing.T) {
	p, _ := start(t, Handler{})
	var big string
	if err := call(t, p, "big", nil, &big); err != nil {
		t.Fatal(err)
	}
	if len(big) != 2<<20 {
		t.Fatalf("got %d bytes", len(big))
	}
}

func TestACallOutstandingWhenCodexExitsEndsAndSoDoesTheProcess(t *testing.T) {
	p, _ := start(t, Handler{})
	if err := call(t, p, "exit", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done wasn't closed")
	}
	if p.Err() == nil || errors.Is(p.Err(), ErrClosed) {
		t.Fatalf("an unexpected exit should say so, got %v", p.Err())
	}
	if err := call(t, p, "echo", 1, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("a call after the exit got %v", err)
	}
}

func TestClosingEndsTheProcessQuietly(t *testing.T) {
	p, _ := start(t, Handler{})
	p.Close()
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done wasn't closed")
	}
	if !errors.Is(p.Err(), ErrClosed) {
		t.Fatalf("got %v", p.Err())
	}
}

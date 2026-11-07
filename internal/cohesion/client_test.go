package cohesion

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeDaemon is cohesiond on a Unix socket: it accepts a local identity,
// says hello, accepts offers, stores messages, and can drop a session.
type fakeDaemon struct {
	t        *testing.T
	listener net.Listener
	mu       sync.Mutex
	conns    []net.Conn
	frames   []map[string]any
	stored   []map[string]any
	nextID   int64
	// Whether to drop the session instead of replying to the next message.
	dropNext bool
	seq      int
}

func newFakeDaemon(t *testing.T) (*fakeDaemon, string) {
	socket := filepath.Join(t.TempDir(), "d.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDaemon{t: t, listener: listener}
	t.Cleanup(func() {
		listener.Close()
		d.mu.Lock()
		for _, conn := range d.conns {
			conn.Close()
		}
		d.mu.Unlock()
	})
	go d.accept()
	return d, socket
}

func (d *fakeDaemon) accept() {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			return
		}
		d.mu.Lock()
		d.conns = append(d.conns, conn)
		d.mu.Unlock()
		go d.serve(conn)
	}
}

func (d *fakeDaemon) serve(conn net.Conn) {
	reader := bufio.NewReader(conn)
	var writeMu sync.Mutex
	write := func(frame map[string]any) {
		d.mu.Lock()
		d.seq++
		frame["seq"] = d.seq
		d.mu.Unlock()
		line, _ := json.Marshal(frame)
		writeMu.Lock()
		conn.Write(append(line, '\n'))
		writeMu.Unlock()
	}
	identity, err := reader.ReadBytes('\n')
	if err != nil {
		return
	}
	var local LocalIdentity
	_ = json.Unmarshal(identity, &local)
	conn.Write([]byte(`{"outcome":"accepted"}` + "\n"))
	write(map[string]any{"type": "hello", "protocol": 1, "server": "test", "client_name": local.Name, "web_bundle_hash": nil, "vocabulary": map[string]any{}})
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var frame map[string]any
		_ = json.Unmarshal(line, &frame)
		d.mu.Lock()
		d.frames = append(d.frames, frame)
		drop := d.dropNext && frame["type"] == "actorSessionUpdate"
		if drop {
			d.dropNext = false
		}
		var data any = map[string]any{}
		switch frame["type"] {
		case "actorSessionUpdate":
			if frame["kind"] == "message" {
				d.nextID++
				content := frame["content"].(map[string]any)
				stored := map[string]any{"sender": "actor"}
				for key, value := range content {
					stored[key] = value
				}
				message := map[string]any{"id": d.nextID, "session_id": frame["session_id"], "sent_at": 0, "content": stored, "answered": false, "attachment_count": 0}
				d.stored = append(d.stored, message)
				data = message
			}
		case "listActorSessionMessages":
			data = d.stored
		}
		d.mu.Unlock()
		if drop {
			// Stored, but the reply never comes.
			conn.Close()
			return
		}
		if frame["type"] == "callResult" {
			continue
		}
		write(map[string]any{"type": "reply", "id": frame["id"], "ok": map[string]any{"type": "x", "data": data}})
	}
}

// call sends a call on the latest session.
func (d *fakeDaemon) call(frame map[string]any) {
	d.mu.Lock()
	conn := d.conns[len(d.conns)-1]
	d.seq++
	frame["seq"] = d.seq
	d.mu.Unlock()
	line, _ := json.Marshal(frame)
	conn.Write(append(line, '\n'))
}

func (d *fakeDaemon) framesOf(kind string) []map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	var found []map[string]any
	for _, frame := range d.frames {
		if frame["type"] == kind {
			found = append(found, frame)
		}
	}
	return found
}

// saved is a local server as the client's configuration keeps it: indented.
func saved(t *testing.T, socket string) Server {
	server, _ := LocalServer(socket, "Test laptop")
	text, _ := json.MarshalIndent(server, "", "\t")
	var kept Server
	if err := json.Unmarshal(text, &kept); err != nil {
		t.Fatal(err)
	}
	return kept
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("never: %s", what)
}

func TestClientOffersAnswersAndReportsInOrder(t *testing.T) {
	daemon, socket := newFakeDaemon(t)
	server := saved(t, socket)
	calls := make(chan Call, 1)
	online := make(chan *Session, 4)
	client := NewClient(server, Options{
		Offers: Capabilities{Actors: []ActorDefinition{{Identifier: "app", Product: "Codex", Scope: "/app", Modes: []ActorMode{}, Models: []ActorModel{}}}},
		OnCall: func(call Call) error {
			calls <- call
			if call.Kind == "stopActorSession" {
				return errors.New("Nothing to stop.")
			}
			return nil
		},
		OnState: func(state State) {
			if state.Kind == "online" {
				online <- state.Session
			}
		},
	})
	go client.Run()
	t.Cleanup(client.Stop)
	session := <-online
	if session.Hello.ClientName != "Test laptop" {
		t.Errorf("hello %+v", session.Hello)
	}
	if hellos := daemon.framesOf("hello"); len(hellos) != 1 || hellos[0]["actors"].([]any)[0].(map[string]any)["identifier"] != "app" {
		t.Fatalf("hello frames %v", hellos)
	}

	daemon.call(map[string]any{"type": "call", "call_id": 7, "call": map[string]any{"kind": "startActorSession", "actor": "app", "session_id": 3, "prompt": "Hi", "title": nil, "mode": nil, "model": nil, "effort": nil}})
	if call := <-calls; call.Kind != "startActorSession" || call.SessionID != 3 || call.Prompt != "Hi" || call.Title != nil {
		t.Errorf("call %+v", call)
	}
	eventually(t, "the call answered", func() bool { return len(daemon.framesOf("callResult")) == 1 })
	if result := daemon.framesOf("callResult")[0]; result["ok"].(map[string]any)["kind"] != "accepted" {
		t.Errorf("result %v", result)
	}
	daemon.call(map[string]any{"type": "call", "call_id": 8, "call": map[string]any{"kind": "stopActorSession", "actor": "app", "session_id": 3}})
	<-calls
	eventually(t, "the refusal sent", func() bool { return len(daemon.framesOf("callResult")) == 2 })
	if result := daemon.framesOf("callResult")[1]; result["error"] != "Nothing to stop." {
		t.Errorf("refusal %v", result)
	}

	first := client.Report(3, Report{Kind: "message", Content: &SaidContent{Kind: "toolUse", Tool: "Shell", Category: "command", Summary: "ls"}})
	client.Report(3, Report{Kind: "status", Status: Idle})
	message, err := first.Wait()
	if err != nil || message.ID != 1 {
		t.Fatalf("message %+v %v", message, err)
	}
	end := client.Report(3, Report{Kind: "end", MessageID: message.ID})
	if _, err := end.Wait(); err != nil {
		t.Fatal(err)
	}
	updates := daemon.framesOf("actorSessionUpdate")
	if len(updates) != 3 || updates[0]["kind"] != "message" || updates[1]["kind"] != "status" || updates[2]["message_id"] != float64(1) {
		t.Errorf("updates %v", updates)
	}
	if _, ok := updates[0]["content"].(map[string]any)["ended_at"]; ok {
		t.Error("a toolUse carries no ended_at")
	}
}

func TestClientResendsWithoutDuplicating(t *testing.T) {
	daemon, socket := newFakeDaemon(t)
	server := saved(t, socket)
	connected := 0
	var mu sync.Mutex
	client := NewClient(server, Options{
		OnCall: func(Call) error { return nil },
		OnConnect: func(session *Session) {
			mu.Lock()
			connected++
			mu.Unlock()
		},
	})
	go client.Run()
	t.Cleanup(client.Stop)
	eventually(t, "online", func() bool { mu.Lock(); defer mu.Unlock(); return connected == 1 })

	daemon.mu.Lock()
	daemon.dropNext = true
	daemon.mu.Unlock()
	said := client.Report(3, Report{Kind: "message", Content: &SaidContent{Kind: "text", Text: "Done."}})
	after := client.Report(3, Report{Kind: "status", Status: Idle})
	message, err := said.Wait()
	if err != nil || message.ID != 1 {
		t.Fatalf("message %+v %v", message, err)
	}
	if _, err := after.Wait(); err != nil {
		t.Fatal(err)
	}
	// Stored before the drop: found in the transcript, not sent again.
	if len(daemon.stored) != 1 {
		t.Errorf("stored %d messages", len(daemon.stored))
	}
	if len(daemon.framesOf("listActorSessionMessages")) != 1 {
		t.Error("the transcript wasn't checked")
	}
	mu.Lock()
	defer mu.Unlock()
	if connected != 2 {
		t.Errorf("connected %d times", connected)
	}
}

func TestDecodeSetupCode(t *testing.T) {
	code, err := DecodeSetupCode("a2V5fDE5Mi4xNjguMS4yOjkxMTl8YWJj") // key|192.168.1.2:9119|abc
	if err != nil || code.Key != "key" || code.Host != "192.168.1.2" || code.Port != 9119 || code.Fingerprint != "abc" {
		t.Errorf("got %+v %v", code, err)
	}
	if code, _ := DecodeSetupCode("a2V5fGhvc3QubG9jYWx8YWJj"); code.Host != "host.local" || code.Port != DefaultPort { // key|host.local|abc
		t.Errorf("default port: %+v", code)
	}
	if _, err := DecodeSetupCode("nonsense"); err == nil {
		t.Error("nonsense decoded")
	}
}

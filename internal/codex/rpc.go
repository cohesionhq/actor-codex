// Package codex drives `codex app-server`: one process, spoken to in
// JSON-RPC over its stdin and stdout, one message per line.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrClosed is a request the process ended before answering.
var ErrClosed = errors.New("codex app-server exited")

// RPCError is an error response from Codex.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// Notification is a message from Codex that wants no answer.
type Notification struct {
	Method string
	Params json.RawMessage
}

// ServerRequest is a request from Codex, such as an approval, which wants
// an answer through Respond.
type ServerRequest struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

// Handler hears what the process sends of its own accord. Each is called on
// the process's reading goroutine, in order: it mustn't block on a request
// to the same process.
type Handler struct {
	Notify  func(Notification)
	Request func(ServerRequest)
}

// Process is one running `codex app-server`.
type Process struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	handler Handler
	log     func(string)

	writeMu sync.Mutex
	nextID  atomic.Int64

	mu      sync.Mutex
	waiting map[int64]chan response
	closed  bool
	done    chan struct{}
	err     error
}

type response struct {
	result json.RawMessage
	err    error
}

// Binary is the `codex` executable run; $COHESION_ACTOR_CODEX_BINARY overrides it.
var Binary = "codex"

// Start runs `codex app-server` in `cwd` and initializes it, naming this
// client to Codex.
func Start(ctx context.Context, cwd string, handler Handler, log func(string)) (*Process, error) {
	cmd := exec.Command(Binary, "app-server")
	cmd.Dir = cwd
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("couldn't start Codex (%s app-server): %w", Binary, err)
	}
	p := &Process{cmd: cmd, stdin: stdin, handler: handler, log: log, waiting: map[int64]chan response{}, done: make(chan struct{})}
	go p.logStderr(stderr)
	go p.read(stdout)
	if err := p.initialize(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

// Done is closed once the process has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// Err is why the process exited, once it has.
func (p *Process) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Close ends the process: closing its input asks it to exit.
func (p *Process) Close() {
	p.mu.Lock()
	closed := p.closed
	p.closed = true
	p.mu.Unlock()
	if closed {
		return
	}
	p.stdin.Close()
	go func() {
		select {
		case <-p.done:
		case <-afterSeconds(5):
			_ = p.cmd.Process.Kill()
		}
	}()
}

// Call sends request `method` and decodes its result into `into` (nil to drop it).
func (p *Process) Call(ctx context.Context, method string, params any, into any) error {
	id := p.nextID.Add(1)
	reply := make(chan response, 1)
	p.mu.Lock()
	if p.err != nil || p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	p.waiting[id] = reply
	p.mu.Unlock()
	if err := p.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		p.forget(id)
		return err
	}
	select {
	case r := <-reply:
		if r.err != nil {
			return r.err
		}
		if into == nil || len(r.result) == 0 {
			return nil
		}
		return json.Unmarshal(r.result, into)
	case <-ctx.Done():
		p.forget(id)
		return ctx.Err()
	}
}

// Notify sends notification `method`.
func (p *Process) Notify(method string, params any) error {
	message := map[string]any{"method": method}
	if params != nil {
		message["params"] = params
	}
	return p.send(message)
}

// Respond answers server request `id` with `result`.
func (p *Process) Respond(id json.RawMessage, result any) error {
	return p.send(map[string]any{"id": id, "result": result})
}

// RespondError refuses server request `id`.
func (p *Process) RespondError(id json.RawMessage, message string) error {
	return p.send(map[string]any{"id": id, "error": map[string]any{"code": -32603, "message": message}})
}

func (p *Process) forget(id int64) {
	p.mu.Lock()
	delete(p.waiting, id)
	p.mu.Unlock()
}

func (p *Process) send(message map[string]any) error {
	line, err := json.Marshal(message)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if _, err := p.stdin.Write(append(line, '\n')); err != nil {
		return ErrClosed
	}
	return nil
}

type incoming struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

func (p *Process) read(stdout io.Reader) {
	reader := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, err := reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			p.dispatch(line)
		}
		if err != nil {
			break
		}
	}
	waitErr := p.cmd.Wait()
	p.mu.Lock()
	if waitErr != nil && !p.closed {
		p.err = fmt.Errorf("codex app-server exited: %w", waitErr)
	} else {
		p.err = ErrClosed
	}
	for id, reply := range p.waiting {
		reply <- response{err: ErrClosed}
		delete(p.waiting, id)
	}
	p.mu.Unlock()
	close(p.done)
}

func (p *Process) dispatch(line []byte) {
	var message incoming
	if err := json.Unmarshal(line, &message); err != nil {
		p.log(fmt.Sprintf("codex sent a malformed message: %v", err))
		return
	}
	hasID := len(message.ID) > 0 && string(message.ID) != "null"
	switch {
	case message.Method != "" && hasID:
		if p.handler.Request != nil {
			p.handler.Request(ServerRequest{ID: message.ID, Method: message.Method, Params: message.Params})
		} else {
			_ = p.RespondError(message.ID, "This client can't "+message.Method+".")
		}
	case message.Method != "":
		if p.handler.Notify != nil {
			p.handler.Notify(Notification{Method: message.Method, Params: message.Params})
		}
	case hasID:
		var id int64
		if json.Unmarshal(message.ID, &id) != nil {
			return
		}
		p.mu.Lock()
		reply, ok := p.waiting[id]
		delete(p.waiting, id)
		p.mu.Unlock()
		if !ok {
			return
		}
		if message.Error != nil {
			reply <- response{err: message.Error}
		} else {
			reply <- response{result: message.Result}
		}
	}
}

// ansi is a terminal colour code, which Codex's logs carry.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func (p *Process) logStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		if text := strings.TrimSpace(ansi.ReplaceAllString(scanner.Text(), "")); text != "" {
			p.log(text)
		}
	}
}

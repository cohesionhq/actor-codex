package actor

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cohesionhq/actor-codex/internal/codex"
	"github.com/cohesionhq/actor-codex/internal/cohesion"
)

// TestLiveCodex runs one turn on a real Codex, for checking this client
// against a new Codex by hand; it is skipped unless COHESION_ACTOR_CODEX_LIVE
// names the `codex` to run. Run it with a scratch CODEX_HOME to keep your own
// out of it, or your own to see a real turn:
//
//	COHESION_ACTOR_CODEX_LIVE=$(which codex) go test -run TestLiveCodex -v ./internal/actor/
func TestLiveCodex(t *testing.T) {
	binary := os.Getenv("COHESION_ACTOR_CODEX_LIVE")
	if binary == "" {
		t.Skip("COHESION_ACTOR_CODEX_LIVE names no codex")
	}
	codex.Binary = binary
	log := func(text string) { t.Log(text) }
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	models, err := AvailableModels(ctx, log)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range models {
		t.Logf("model %s (%s), %d efforts", model.Identifier, model.Title, len(model.Efforts))
	}
	dir := t.TempDir()
	store, _ := OpenStore(dir)
	client := &fakeClient{}
	usage := NewUsage(func(_ string, u cohesion.ActorUsage) error { t.Logf("usage %+v", u); return nil }, func() []string { return []string{"live"} }, log)
	usage.tick()
	session := NewSession(1, Record{Actor: "live", Mode: "auto", Titled: TitledPending}, SessionContext{Client: client, Store: store, Cwd: dir, Usage: usage, Log: log})
	defer session.Close()
	session.Say("Run `ls` and then say hi.", nil)
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		lines := strings.Join(client.lines(), "\n")
		if strings.Contains(lines, "status idle") || strings.Contains(lines, "status failed") || strings.Contains(lines, "status needsAttention") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(time.Second)
	t.Logf("reported:\n%s", strings.Join(client.lines(), "\n"))
	record, _ := store.Get(1)
	if record.ThreadID == "" {
		t.Error("no thread started")
	}
}

package actor

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/cohesionhq/actor-codex/internal/cohesion"
)

func TestLabels(t *testing.T) {
	minutes := func(n int64) *int64 { return &n }
	cases := []struct {
		name, which string
		minutes     *int64
		want        string
	}{
		{"", "primary", minutes(300), "5-hour limit"},
		{"", "secondary", minutes(10080), "Weekly limit"},
		{"", "primary", minutes(1440), "Daily limit"},
		{"", "primary", minutes(90), "90-minute limit"},
		{"", "primary", nil, "Short-term limit"},
		{"GPT-6.1-Spark", "secondary", minutes(10080), "Weekly GPT-6.1-Spark limit"},
	}
	for _, c := range cases {
		if got := Label(c.name, c.which, c.minutes); got != c.want {
			t.Errorf("Label(%q, %q) = %q, want %q", c.name, c.which, got, c.want)
		}
	}
}

func TestRateLimitsMergeAndOrder(t *testing.T) {
	var sent []cohesion.ActorUsage
	var mu sync.Mutex
	done := make(chan struct{}, 10)
	usage := NewUsage(func(_ string, u cohesion.ActorUsage) error {
		mu.Lock()
		sent = append(sent, u)
		mu.Unlock()
		done <- struct{}{}
		return nil
	}, func() []string { return []string{"a"} }, func(string) {})
	email, name := "ada@example.com", "Spark"
	usage.setAccount(&Account{Type: "chatgpt", Email: &email})

	usage.RateLimitsUpdated(json.RawMessage(`{"rateLimits":{"limitId":"spark","limitName":"Spark","primary":{"usedPercent":5,"windowDurationMins":300,"resetsAt":100}}}`))
	<-done
	usage.RateLimitsUpdated(json.RawMessage(`{"rateLimits":{"limitId":"codex","primary":{"usedPercent":40,"windowDurationMins":300,"resetsAt":200},"secondary":{"usedPercent":150,"windowDurationMins":10080,"resetsAt":300}}}`))
	<-done
	// Sparse: a later update naming only one window keeps the other.
	usage.RateLimitsUpdated(json.RawMessage(`{"rateLimits":{"limitId":"codex","primary":{"usedPercent":41,"windowDurationMins":300,"resetsAt":200},"secondary":null}}`))
	<-done
	_ = name

	got := usage.UsageOf()
	if len(got.Plan) != 4-1 {
		t.Fatalf("plan %+v", got.Plan)
	}
	labels := []string{got.Plan[0].Label, got.Plan[1].Label, got.Plan[2].Label}
	if labels[0] != "5-hour limit" || labels[1] != "Weekly limit" || labels[2] != "5-hour Spark limit" {
		t.Errorf("order %v", labels)
	}
	if got.Plan[0].Used != 0.41 || got.Plan[1].Used != 1 {
		t.Errorf("used %v %v", got.Plan[0].Used, got.Plan[1].Used)
	}
	if got.Pool == nil || got.Pool.ID != "codex:ada@example.com" {
		t.Errorf("pool %+v", got.Pool)
	}
	// Nothing changed: nothing more is said.
	usage.Flush()
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 3 {
		t.Errorf("sent %d times", len(sent))
	}
}

func TestNoPlanWithoutChatGPT(t *testing.T) {
	usage := NewUsage(func(string, cohesion.ActorUsage) error { return nil }, func() []string { return nil }, func(string) {})
	usage.setAccount(&Account{Type: "apiKey"})
	usage.RateLimitsUpdated(json.RawMessage(`{"rateLimits":{"primary":{"usedPercent":5,"windowDurationMins":300}}}`))
	if got := usage.UsageOf(); len(got.Plan) != 0 || got.Pool != nil {
		t.Errorf("got %+v", got)
	}
	if PoolOf(&Account{Type: "chatgpt"}) != nil {
		t.Error("a pool without an email")
	}
}

func TestContextOf(t *testing.T) {
	var usage TokenUsage
	_ = json.Unmarshal([]byte(`{"total":{"totalTokens":900000},"last":{"totalTokens":41800},"modelContextWindow":272000}`), &usage)
	got := ContextOf(usage)
	if got == nil || got.UsedTokens != 41800 || got.TotalTokens != 272000 {
		t.Errorf("got %+v", got)
	}
	_ = json.Unmarshal([]byte(`{"last":{"totalTokens":5},"modelContextWindow":null}`), &usage)
	if ContextOf(usage) != nil {
		t.Error("a context without a window")
	}
}

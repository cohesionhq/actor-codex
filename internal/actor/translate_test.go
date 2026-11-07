package actor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cohesionhq/actor-codex/internal/cohesion"
)

func item(t *testing.T, raw string) Item {
	t.Helper()
	var it Item
	if err := json.Unmarshal([]byte(raw), &it); err != nil {
		t.Fatal(err)
	}
	return it
}

func TestUnwrapShell(t *testing.T) {
	cases := map[string]string{
		`/bin/bash -lc 'cargo test'`:          "cargo test",
		`bash -lc "ls -la"`:                   "ls -la",
		`zsh -c 'echo '\''hi'\'''`:            "echo 'hi'",
		`cargo build`:                         "cargo build",
		`/usr/bin/zsh -lc 'go test ./... -v'`: "go test ./... -v",
	}
	for in, want := range cases {
		if got := UnwrapShell(in); got != want {
			t.Errorf("UnwrapShell(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToolUseCategories(t *testing.T) {
	cwd := "/src/app"
	cases := []struct {
		raw                     string
		tool, category, summary string
	}{
		{`{"type":"commandExecution","id":"1","command":"bash -lc 'cargo test'","commandActions":[{"type":"unknown","command":"cargo test"}]}`, "Shell", "command", "cargo test"},
		{`{"type":"commandExecution","id":"1","command":"bash -lc 'cat src/a.rs'","commandActions":[{"type":"read","command":"cat src/a.rs","name":"a.rs","path":"/src/app/src/a.rs"}]}`, "Read", "read", "src/a.rs"},
		{`{"type":"commandExecution","id":"1","command":"ls","commandActions":[{"type":"listFiles","command":"ls","path":null}]}`, "List", "read", "."},
		{`{"type":"commandExecution","id":"1","command":"rg foo src","commandActions":[{"type":"search","command":"rg foo src","query":"foo","path":"src"}]}`, "Search", "search", "foo in src"},
		{`{"type":"commandExecution","id":"1","command":"cat a && rg b","commandActions":[{"type":"read","command":"cat a","name":"a","path":"a"},{"type":"search","command":"rg b","query":"b","path":null}]}`, "Shell", "command", "cat a && rg b"},
		{`{"type":"fileChange","id":"1","changes":[{"path":"/src/app/a.go","kind":{"type":"update","move_path":null},"diff":""},{"path":"/src/app/b.go","kind":{"type":"add"},"diff":""}],"status":"inProgress"}`, "Edit", "edit", "a.go and 1 more"},
		{`{"type":"mcpToolCall","id":"1","server":"github","tool":"search_issues","arguments":{"query":"flaky","description":"a long one"},"status":"inProgress","result":null}`, "Github: search issues", "other", "flaky"},
		{`{"type":"webSearch","id":"1","query":"go generics","action":{"type":"search","query":"go generics"}}`, "WebSearch", "web", "go generics"},
		{`{"type":"imageView","id":"1","path":"/src/app/shot.png"}`, "ViewImage", "read", "shot.png"},
		{`{"type":"collabAgentToolCall","id":"1","tool":"spawnAgent","status":"inProgress","prompt":"Look into the tests\nmore","receiverThreadIds":[]}`, "Agent", "agent", "Look into the tests"},
	}
	for _, c := range cases {
		got := ToolUse(item(t, c.raw), cwd)
		if got.Tool != c.tool || got.Category != c.category || got.Summary != c.summary {
			t.Errorf("%s:\n got %q %q %q\nwant %q %q %q", c.raw, got.Tool, got.Category, got.Summary, c.tool, c.category, c.summary)
		}
	}
}

func TestProgress(t *testing.T) {
	cwd := "/src/app"
	if got := Progress(item(t, `{"type":"commandExecution","command":"bash -lc 'make'","commandActions":[]}`), cwd); got != "Running make" {
		t.Errorf("got %q", got)
	}
	if got := Progress(item(t, `{"type":"mcpToolCall","server":"x","tool":"y","arguments":{}}`), cwd); got != "Using X: y" {
		t.Errorf("got %q", got)
	}
	if got := Progress(item(t, `{"type":"collabAgentToolCall","tool":"wait"}`), cwd); got != "Waiting on agents" {
		t.Errorf("got %q", got)
	}
}

func TestItemResultShapes(t *testing.T) {
	mcp := item(t, `{"type":"mcpToolCall","result":{"content":[{"type":"text","text":"hi"},{"type":"image","data":"iVBORw0KGgo=","mimeType":"image/png"}]}}`)
	if mcp.Result == nil || len(McpImages(mcp.Result.Content)) != 1 {
		t.Fatalf("mcp result images not read: %+v", mcp.Result)
	}
	generated := item(t, `{"type":"imageGeneration","result":"iVBORw0KGgo="}`)
	if generated.Base64 == nil || *generated.Base64 != "iVBORw0KGgo=" || generated.Result != nil {
		t.Fatalf("image generation result not read: %+v", generated)
	}
}

func TestCommandApproval(t *testing.T) {
	command, cwd := "/bin/bash -lc 'cargo test'", "/src/app/sub"
	reason := "To check the fix."
	params := ApprovalParams{Command: &command, Cwd: &cwd, Reason: &reason}
	asked := Request(CommandApproval, params, nil, "/src/app")
	if asked.Kind != "permission" || asked.Subject.Kind != "command" || asked.Subject.Command != "cargo test" || *asked.Subject.Cwd != cwd {
		t.Fatalf("got %+v %+v", asked, asked.Subject)
	}
	if strings.Join(asked.Decisions, ",") != "allow,allowForSession,deny,abort" {
		t.Errorf("decisions %v", asked.Decisions)
	}
	cases := map[string]string{cohesion.Allow: "accept", cohesion.AllowForSession: "acceptForSession", cohesion.Deny: "decline", cohesion.Abort: "cancel"}
	for decision, want := range cases {
		got := Response(CommandApproval, params, &cohesion.UserContent{Kind: "permission", Decision: decision}).(map[string]any)["decision"]
		if got != want {
			t.Errorf("%s: got %v, want %s", decision, got, want)
		}
	}
	// Words in place of a response decline it; a stopped request cancels.
	if got := Response(CommandApproval, params, &cohesion.UserContent{Kind: "text", Text: "no"}).(map[string]any)["decision"]; got != "decline" {
		t.Errorf("text: got %v", got)
	}
	if got := Response(CommandApproval, params, nil).(map[string]any)["decision"]; got != "cancel" {
		t.Errorf("stopped: got %v", got)
	}
}

func TestFileChangeApprovalDiffs(t *testing.T) {
	var changes []FileUpdate
	_ = json.Unmarshal([]byte(`[
		{"path":"/src/app/new.txt","kind":{"type":"add"},"diff":"one\ntwo\n"},
		{"path":"/src/app/old.txt","kind":{"type":"delete"},"diff":"gone\n"},
		{"path":"/src/app/main.go","kind":{"type":"update","move_path":null},"diff":"@@ -1 +1 @@\n-a\n+b"}
	]`), &changes)
	asked := Request(FileChangeApproval, ApprovalParams{}, changes, "/src/app")
	if asked.Subject.Kind != "fileChange" || len(asked.Subject.Changes) != 3 {
		t.Fatalf("got %+v", asked.Subject)
	}
	add, del, update := asked.Subject.Changes[0], asked.Subject.Changes[1], asked.Subject.Changes[2]
	if add.Change != "add" || *add.Diff != "--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1,2 @@\n+one\n+two" {
		t.Errorf("add: %q", *add.Diff)
	}
	if del.Change != "delete" || *del.Diff != "--- a/old.txt\n+++ /dev/null\n@@ -1,1 +0,0 @@\n-gone" {
		t.Errorf("delete: %q", *del.Diff)
	}
	if update.Change != "update" || *update.Diff != "--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-a\n+b" {
		t.Errorf("update: %q", *update.Diff)
	}
}

func TestUserInputQuestions(t *testing.T) {
	var params ApprovalParams
	_ = json.Unmarshal([]byte(`{"questions":[
		{"id":"q1","header":"Approach","question":"How?","isOther":true,"isSecret":false,"options":[{"label":"Patch","description":"Small"},{"label":"Refactor","description":""}]},
		{"id":"q2","header":"","question":"Name?","isOther":false,"isSecret":false,"options":null}
	]}`), &params)
	asked := Request(UserInputRequest, params, nil, "/")
	if asked.Kind != "question" || len(asked.Questions) != 2 {
		t.Fatalf("got %+v", asked)
	}
	if !asked.Questions[0].AllowsOther || asked.Questions[0].Options[1].Description != nil || *asked.Questions[0].Header != "Approach" {
		t.Errorf("first question %+v", asked.Questions[0])
	}
	// A question with no options is answered in words.
	if !asked.Questions[1].AllowsOther || asked.Questions[1].Header != nil {
		t.Errorf("second question %+v", asked.Questions[1])
	}
	other := "Ada"
	got, _ := json.Marshal(Response(UserInputRequest, params, &cohesion.UserContent{Kind: "answers", Answers: []cohesion.Answer{{Selected: []string{"Patch"}}, {Other: &other}}}))
	if string(got) != `{"answers":{"q1":{"answers":["Patch"]},"q2":{"answers":["Ada"]}}}` {
		t.Errorf("answers %s", got)
	}
}

func TestElicitationForm(t *testing.T) {
	var params ApprovalParams
	_ = json.Unmarshal([]byte(`{"serverName":"deploy","mode":"form","message":"Deploy where?","requestedSchema":{"type":"object","properties":{
		"env":{"type":"string","enum":["prod","staging"],"enumNames":["Production","Staging"]},
		"force":{"type":"boolean"}},"required":["env"]}}`), &params)
	asked := Request(ElicitationRequest, params, nil, "/")
	if asked.Kind != "question" || len(asked.Questions) != 2 || asked.Questions[0].Options[0].Label != "Production" {
		t.Fatalf("got %+v", asked)
	}
	got, _ := json.Marshal(Response(ElicitationRequest, params, &cohesion.UserContent{Kind: "answers", Answers: []cohesion.Answer{{Selected: []string{"Staging"}}, {Selected: []string{"Yes"}}}}))
	if string(got) != `{"action":"accept","content":{"env":"staging","force":true}}` {
		t.Errorf("response %s", got)
	}
}

func TestDisplayName(t *testing.T) {
	if got := DisplayName("claude_ai_Gmail", "search_threads"); got != "Claude ai Gmail: search threads" {
		t.Errorf("got %q", got)
	}
	if got := DisplayName("", ""); got != "Tool" {
		t.Errorf("got %q", got)
	}
}

func TestSaidContentWire(t *testing.T) {
	// An empty summary is still sent; ended_at never is.
	got, _ := json.Marshal(cohesion.SaidContent{Kind: "toolUse", Tool: "Shell", Category: "command"})
	if string(got) != `{"kind":"toolUse","tool":"Shell","category":"command","summary":""}` {
		t.Errorf("toolUse %s", got)
	}
	cwd := "/x"
	got, _ = json.Marshal(cohesion.SaidContent{Kind: "permission", Subject: &cohesion.Subject{Kind: "command", Command: "ls", Cwd: &cwd}, Decisions: []string{"allow"}})
	if string(got) != `{"kind":"permission","subject":{"kind":"command","command":"ls","cwd":"/x"},"reason":null,"decisions":["allow"]}` {
		t.Errorf("permission %s", got)
	}
}

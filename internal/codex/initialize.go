package codex

import (
	"context"
	"time"
)

// Version is this client's, as it names itself to Codex.
const Version = "0.1.0"

// The notifications this client never reads: streamed text it takes whole
// from each item's completion instead.
var optOut = []string{
	"item/agentMessage/delta",
	"item/plan/delta",
	"item/reasoning/summaryTextDelta",
	"item/reasoning/summaryPartAdded",
	"item/reasoning/textDelta",
	"item/commandExecution/outputDelta",
	"item/commandExecution/terminalInteraction",
	"item/fileChange/outputDelta",
}

// initialize names this client to Codex and opts in to the experimental
// API, which plan mode (`collaborationMode`) needs.
func (p *Process) initialize(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := p.Call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "cohesion_codex", "title": "Cohesion", "version": Version},
		"capabilities": map[string]any{
			"experimentalApi":           true,
			"optOutNotificationMethods": optOut,
		},
	}, nil)
	if err != nil {
		return err
	}
	return p.Notify("initialized", map[string]any{})
}

func afterSeconds(seconds int) <-chan time.Time {
	return time.After(time.Duration(seconds) * time.Second)
}

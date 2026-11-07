package actor

import (
	"context"
	"os"
	"strings"

	"github.com/cohesionhq/actor-codex/internal/codex"
	"github.com/cohesionhq/actor-codex/internal/cohesion"
)

// Product is what each actor is, as Cohesion titles it.
const Product = "Codex"

// Mode is one way a session runs: Codex's approval policy, sandbox and
// collaboration mode.
type Mode struct {
	Identifier  string
	Title       string
	Description string
	// AskForApproval: when Codex asks before acting.
	Approval string
	// SandboxMode: what Codex may touch without asking.
	Sandbox string
	// The collaboration mode: `plan` or `default`.
	Collaboration string
}

// Modes are the modes offered, the first the default.
var Modes = []Mode{
	{"auto", "Auto", "Edits files and runs commands in its directory; asks before going outside it or online.", "on-request", "workspace-write", "default"},
	{"readOnly", "Read only", "Reads files; asks before editing them or running commands that change anything.", "on-request", "read-only", "default"},
	{"plan", "Plan", "Plans first, and changes nothing until you approve.", "on-request", "read-only", "plan"},
	{"fullAccess", "Full access", "Runs every command and edits any file without asking.", "never", "danger-full-access", "default"},
}

// AfterPlan is the mode a session leaves Plan for once its plan is approved.
const AfterPlan = "auto"

// ModeNamed is the mode `identifier` names, or false.
func ModeNamed(identifier string) (Mode, bool) {
	for _, mode := range Modes {
		if mode.Identifier == identifier {
			return mode, true
		}
	}
	return Mode{}, false
}

// modeOr is the mode cohesiond names, or the default when it names none or one unknown.
func modeOr(identifier *string) string {
	if identifier != nil {
		if mode, ok := ModeNamed(*identifier); ok {
			return mode.Identifier
		}
	}
	return Modes[0].Identifier
}

var effortTitles = map[string]string{
	"none":    "None",
	"minimal": "Minimal",
	"low":     "Low",
	"medium":  "Medium",
	"high":    "High",
	"xhigh":   "Extra high",
	"max":     "Max",
	"ultra":   "Ultra",
}

// Definitions are the actors offered, one per directory.
func Definitions(entries []Entry, models []cohesion.ActorModel) []cohesion.ActorDefinition {
	modes := make([]cohesion.ActorMode, len(Modes))
	for i, mode := range Modes {
		description := mode.Description
		modes[i] = cohesion.ActorMode{Identifier: mode.Identifier, Title: mode.Title, Description: &description}
	}
	if models == nil {
		models = []cohesion.ActorModel{}
	}
	definitions := make([]cohesion.ActorDefinition, len(entries))
	for i, entry := range entries {
		definitions[i] = cohesion.ActorDefinition{Identifier: entry.Identifier, Product: Product, Scope: entry.Directory, Modes: modes, Models: models}
	}
	return definitions
}

// ModelInfo is one of `model/list`'s models.
type ModelInfo struct {
	ID                        string `json:"id"`
	Model                     string `json:"model"`
	DisplayName               string `json:"displayName"`
	Hidden                    bool   `json:"hidden"`
	IsDefault                 bool   `json:"isDefault"`
	SupportedReasoningEfforts []struct {
		ReasoningEffort string `json:"reasoningEffort"`
	} `json:"supportedReasoningEfforts"`
}

// AvailableModels are the models Codex offers this account, its default
// first. Asking starts Codex without a thread; nothing is sent to a model.
func AvailableModels(ctx context.Context, log func(string)) ([]cohesion.ActorModel, error) {
	home, _ := os.UserHomeDir()
	process, err := codex.Start(ctx, home, codex.Handler{}, func(text string) { log("codex: " + text) })
	if err != nil {
		return nil, err
	}
	defer process.Close()
	var all []ModelInfo
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data       []ModelInfo `json:"data"`
			NextCursor *string     `json:"nextCursor"`
		}
		if err := process.Call(ctx, "model/list", params, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Data...)
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		cursor = *page.NextCursor
	}
	return ActorModels(all), nil
}

// ActorModels are Codex's models as Cohesion offers them: the hidden ones
// left out, and its default first.
func ActorModels(models []ModelInfo) []cohesion.ActorModel {
	var first, rest []cohesion.ActorModel
	for _, model := range models {
		if model.Hidden {
			continue
		}
		identifier := model.Model
		if identifier == "" {
			identifier = model.ID
		}
		title := model.DisplayName
		if title == "" {
			title = identifier
		}
		efforts := []cohesion.ActorEffort{}
		for _, effort := range model.SupportedReasoningEfforts {
			name := effort.ReasoningEffort
			effortTitle, ok := effortTitles[name]
			if !ok && name != "" {
				effortTitle = strings.ToUpper(name[:1]) + name[1:]
			}
			efforts = append(efforts, cohesion.ActorEffort{Identifier: name, Title: effortTitle})
		}
		offered := cohesion.ActorModel{Identifier: identifier, Title: title, Efforts: efforts}
		if model.IsDefault {
			first = append(first, offered)
		} else {
			rest = append(rest, offered)
		}
	}
	return append(first, rest...)
}

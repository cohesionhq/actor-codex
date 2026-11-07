package actor

import (
	"strings"
	"testing"

	"github.com/cohesionhq/actor-codex/internal/cohesion"
)

func TestActorModels(t *testing.T) {
	models := ActorModels([]ModelInfo{
		{ID: "a", Model: "gpt-mini", DisplayName: "GPT Mini"},
		{ID: "b", Model: "gpt-hidden", Hidden: true},
		{ID: "c", Model: "gpt-main", DisplayName: "GPT Main", IsDefault: true, SupportedReasoningEfforts: []struct {
			ReasoningEffort string `json:"reasoningEffort"`
		}{{"low"}, {"xhigh"}, {"ultra"}, {"turbo"}}},
	})
	if len(models) != 2 || models[0].Identifier != "gpt-main" || models[1].Identifier != "gpt-mini" {
		t.Fatalf("got %+v", models)
	}
	var titles []string
	for _, effort := range models[0].Efforts {
		titles = append(titles, effort.Title)
	}
	if strings.Join(titles, ",") != "Low,Extra high,Ultra,Turbo" {
		t.Errorf("efforts %v", titles)
	}
	if models[1].Efforts == nil {
		t.Error("efforts must be a list, not null")
	}
}

func TestModes(t *testing.T) {
	if Modes[0].Identifier != "auto" {
		t.Error("Auto must be the default")
	}
	if modeOr(nil) != "auto" {
		t.Error("no mode is the default")
	}
	unknown := "bypassPermissions"
	if modeOr(&unknown) != "auto" {
		t.Error("an unknown mode is the default")
	}
	if !sameSandbox("readOnly", "plan") || sameSandbox("auto", "fullAccess") {
		t.Error("plan and read only load a thread alike; auto and full access don't")
	}
	definitions := Definitions([]Entry{{Identifier: "x", Directory: "/x"}}, nil)
	if definitions[0].Product != "Codex" || len(definitions[0].Modes) != len(Modes) || definitions[0].Models == nil {
		t.Errorf("got %+v", definitions[0])
	}
	_ = cohesion.ActorModel{}
}

package cohesion

import (
	"encoding/json"
	"testing"
)

func wire(t *testing.T, report Report) string {
	t.Helper()
	frame := report.fields()
	frame["type"], frame["id"], frame["session_id"] = "actorSessionUpdate", 40, 12
	text, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}

func TestLaunchReportWire(t *testing.T) {
	parent, id, kind, model := "p1", int64(311), "Explore", "haiku"
	got := wire(t, Report{Kind: "launch", Subagent: "a1b2c3", Launch: &Launch{Title: "Map the db layer", Parent: &parent, MessageID: &id, AgentType: &kind, Model: &model, Background: true}})
	want := `{"agent_type":"Explore","background":true,"id":40,"kind":"launch","message_id":311,"model":"haiku","parent":"p1","session_id":12,"subagent":"a1b2c3","title":"Map the db layer","type":"actorSessionUpdate"}`
	if got != want {
		t.Errorf("launch:\n%s\nwant\n%s", got, want)
	}
	// A launch from the session itself says null, not nothing.
	got = wire(t, Report{Kind: "launch", Subagent: "a", Launch: &Launch{Title: "T"}})
	want = `{"agent_type":null,"background":false,"id":40,"kind":"launch","message_id":null,"model":null,"parent":null,"session_id":12,"subagent":"a","title":"T","type":"actorSessionUpdate"}`
	if got != want {
		t.Errorf("launch:\n%s\nwant\n%s", got, want)
	}
}

func TestReportsAreAddressedOnlyWhenForASubagent(t *testing.T) {
	if got, want := wire(t, Report{Kind: "status", Status: Idle}), `{"id":40,"kind":"status","session_id":12,"status":"idle","type":"actorSessionUpdate"}`; got != want {
		t.Errorf("session report changed:\n%s\nwant\n%s", got, want)
	}
	if got, want := wire(t, Report{Kind: "status", Subagent: "a", Status: Idle}), `{"id":40,"kind":"status","session_id":12,"status":"idle","subagent":"a","type":"actorSessionUpdate"}`; got != want {
		t.Errorf("subagent report:\n%s\nwant\n%s", got, want)
	}
}

func TestSendCallCarriesTheSubagent(t *testing.T) {
	var call Call
	text := `{"kind":"sendActorSessionMessage","actor":"x","session_id":12,"subagent":"a1","message_id":5,"content":{"kind":"permission","request_id":9,"decision":"allow"},"images":[]}`
	if err := json.Unmarshal([]byte(text), &call); err != nil || call.Subagent != "a1" || call.SessionID != 12 {
		t.Errorf("call %+v, %v", call, err)
	}
}

// Package cohesion is a client for cohesiond, as cohesion-ts is for
// TypeScript: it reaches the daemon over its Unix socket or the paired
// `/connect` WebSocket, offers actors, answers the daemon's calls, and
// reports on actor sessions, holding reports while offline. The wire types
// mirror cohesion-core's (envelope.rs, capabilities.rs and the generated
// models); wire fields are snake_case and enum values camelCase, as the
// daemon sends them.
package cohesion

import "encoding/json"

// ActorMode is one way an actor can run a session.
type ActorMode struct {
	Identifier  string  `json:"identifier"`
	Title       string  `json:"title"`
	Description *string `json:"description"`
}

// ActorEffort is a reasoning effort a model takes.
type ActorEffort struct {
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
}

// ActorModel is a model an actor can run a session on.
type ActorModel struct {
	Identifier string        `json:"identifier"`
	Title      string        `json:"title"`
	Efforts    []ActorEffort `json:"efforts"`
}

// ActorDefinition is an actor as its client offers it in `hello`.
type ActorDefinition struct {
	Identifier string       `json:"identifier"`
	Product    string       `json:"product"`
	Scope      string       `json:"scope"`
	Modes      []ActorMode  `json:"modes"`
	Models     []ActorModel `json:"models"`
}

// Capabilities is everything a client offers.
type Capabilities struct {
	Actors []ActorDefinition `json:"actors"`
}

// Image is one the user sent or an actor shows: PNG, JPEG, GIF or WebP,
// base64-encoded.
type Image struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// Call is one of the daemon's calls to an actor, tagged by Kind:
// startActorSession, sendActorSessionMessage, stopActorSession or setActorSessionMode.
type Call struct {
	Kind      string `json:"kind"`
	Actor     string `json:"actor"`
	SessionID int64  `json:"session_id"`
	// startActorSession.
	Prompt string  `json:"prompt,omitempty"`
	Title  *string `json:"title,omitempty"`
	Model  *string `json:"model,omitempty"`
	Effort *string `json:"effort,omitempty"`
	// startActorSession (null for the default) and setActorSessionMode.
	Mode *string `json:"mode,omitempty"`
	// startActorSession and sendActorSessionMessage.
	Images []Image `json:"images,omitempty"`
	// sendActorSessionMessage.
	MessageID int64        `json:"message_id,omitempty"`
	Content   *UserContent `json:"content,omitempty"`
	// sendActorSessionMessage: the key of the subagent whose request the
	// content responds to, as its launch named it; empty for the session.
	Subagent string `json:"subagent,omitempty"`
}

// Answer is the answer to one question.
type Answer struct {
	Selected []string `json:"selected"`
	Other    *string  `json:"other,omitempty"`
}

// Permission decisions.
const (
	Allow           = "allow"
	AllowForSession = "allowForSession"
	Deny            = "deny"
	Abort           = "abort"
)

// UserContent is what the user says in a session, tagged by Kind: text,
// answers, permission or plan.
type UserContent struct {
	Kind      string   `json:"kind"`
	Text      string   `json:"text,omitempty"`
	RequestID int64    `json:"request_id,omitempty"`
	Answers   []Answer `json:"answers,omitempty"`
	Decision  string   `json:"decision,omitempty"`
	Message   *string  `json:"message,omitempty"`
	Approved  bool     `json:"approved,omitempty"`
	Feedback  *string  `json:"feedback,omitempty"`
}

// QuestionOption is an answer to pick.
type QuestionOption struct {
	Label       string  `json:"label"`
	Description *string `json:"description"`
}

// Question is one question an actor asks.
type Question struct {
	Header      *string          `json:"header"`
	Question    string           `json:"question"`
	Options     []QuestionOption `json:"options"`
	MultiSelect bool             `json:"multi_select"`
	AllowsOther bool             `json:"allows_other"`
}

// FileChange is one file an actor asks to change.
type FileChange struct {
	Path   string  `json:"path"`
	Change string  `json:"change"` // add, update or delete
	Diff   *string `json:"diff"`
}

// Subject is what an actor asks leave to do, tagged by Kind: command,
// fileChange or tool.
type Subject struct {
	Kind    string       `json:"kind"`
	Command string       `json:"command,omitempty"`
	Cwd     *string      `json:"cwd,omitempty"`
	Changes []FileChange `json:"changes,omitempty"`
	Name    string       `json:"name,omitempty"`
	Input   any          `json:"input,omitempty"`
}

// MarshalJSON writes only the fields of the subject's kind, with the
// nullable ones present.
func (s Subject) MarshalJSON() ([]byte, error) {
	switch s.Kind {
	case "command":
		return json.Marshal(struct {
			Kind    string  `json:"kind"`
			Command string  `json:"command"`
			Cwd     *string `json:"cwd"`
		}{s.Kind, s.Command, s.Cwd})
	case "fileChange":
		changes := s.Changes
		if changes == nil {
			changes = []FileChange{}
		}
		return json.Marshal(struct {
			Kind    string       `json:"kind"`
			Changes []FileChange `json:"changes"`
		}{s.Kind, changes})
	default:
		return json.Marshal(struct {
			Kind  string `json:"kind"`
			Name  string `json:"name"`
			Input any    `json:"input"`
		}{s.Kind, s.Name, s.Input})
	}
}

// Tool categories.
const (
	CategoryCommand = "command"
	CategoryRead    = "read"
	CategoryEdit    = "edit"
	CategorySearch  = "search"
	CategoryWeb     = "web"
	CategoryAgent   = "agent"
	CategorySkill   = "skill"
	CategoryOther   = "other"
)

// SaidContent is what an actor says, tagged by Kind: text, toolUse,
// question, permission or plan. A toolUse leaves out `ended_at`, which
// cohesiond stamps.
type SaidContent struct {
	Kind string `json:"kind"`
	// text.
	Text string `json:"text,omitempty"`
	// toolUse.
	Tool     string `json:"tool,omitempty"`
	Category string `json:"category,omitempty"`
	Summary  string `json:"summary,omitempty"`
	// question.
	Questions []Question `json:"questions,omitempty"`
	// permission.
	Subject   *Subject `json:"subject,omitempty"`
	Reason    *string  `json:"reason,omitempty"`
	Decisions []string `json:"decisions,omitempty"`
	// plan.
	Plan string `json:"plan,omitempty"`
}

// MarshalJSON writes only the fields of the content's kind, so an empty
// text or summary is still sent.
func (c SaidContent) MarshalJSON() ([]byte, error) {
	switch c.Kind {
	case "text":
		return json.Marshal(struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		}{c.Kind, c.Text})
	case "toolUse":
		return json.Marshal(struct {
			Kind     string `json:"kind"`
			Tool     string `json:"tool"`
			Category string `json:"category,omitempty"`
			Summary  string `json:"summary"`
		}{c.Kind, c.Tool, c.Category, c.Summary})
	case "question":
		questions := c.Questions
		if questions == nil {
			questions = []Question{}
		}
		return json.Marshal(struct {
			Kind      string     `json:"kind"`
			Questions []Question `json:"questions"`
		}{c.Kind, questions})
	case "permission":
		return json.Marshal(struct {
			Kind      string   `json:"kind"`
			Subject   *Subject `json:"subject"`
			Reason    *string  `json:"reason"`
			Decisions []string `json:"decisions"`
		}{c.Kind, c.Subject, c.Reason, c.Decisions})
	default:
		return json.Marshal(struct {
			Kind string `json:"kind"`
			Plan string `json:"plan"`
		}{c.Kind, c.Plan})
	}
}

// ActorSession statuses.
const (
	Working        = "working"
	NeedsAttention = "needsAttention"
	Idle           = "idle"
	Failed         = "failed"
)

// ContextUsage is how full a session's context window is, in tokens.
type ContextUsage struct {
	UsedTokens  int64 `json:"used_tokens"`
	TotalTokens int64 `json:"total_tokens"`
}

// Launch is what an actor says of a subagent it launched, in a `launch`
// report: an upsert, sent before any other report about the subagent.
type Launch struct {
	// What it is doing; not blank.
	Title string
	// The launching subagent's key; nil is the session itself.
	Parent *string
	// The toolUse message one level up that launched it.
	MessageID *int64
	// What kind of subagent it is, as the actor names them.
	AgentType *string
	// The model it runs on; nil is the session's.
	Model      *string
	Background bool
}

// Report is one thing an actor reports about a session, tagged by Kind:
// launch, message, status, progress, end, context or title. With Subagent
// set, it is about that subagent of the session (progress only marks it
// working) rather than the session; a launch always names one.
type Report struct {
	Kind string
	// The key the actor gave the subagent; empty for the session itself.
	Subagent string
	// launch.
	Launch *Launch
	// message.
	Content *SaidContent
	Images  []Image
	// status.
	Status string
	// progress; nil clears it.
	Progress *string
	// end.
	MessageID int64
	// context.
	Context *ContextUsage
	// title.
	Title string
}

// fields is the report's own fields, as an actorSessionUpdate carries them.
func (r Report) fields() map[string]any {
	fields := map[string]any{"kind": r.Kind}
	if r.Subagent != "" {
		fields["subagent"] = r.Subagent
	}
	switch r.Kind {
	case "launch":
		fields["title"] = r.Launch.Title
		fields["parent"] = r.Launch.Parent
		fields["message_id"] = r.Launch.MessageID
		fields["agent_type"] = r.Launch.AgentType
		fields["model"] = r.Launch.Model
		fields["background"] = r.Launch.Background
	case "message":
		fields["content"] = r.Content
		if len(r.Images) > 0 {
			fields["images"] = r.Images
		}
	case "status":
		fields["status"] = r.Status
	case "progress":
		fields["progress"] = r.Progress
	case "end":
		fields["message_id"] = r.MessageID
	case "context":
		fields["context"] = r.Context
	case "title":
		fields["title"] = r.Title
	}
	return fields
}

// PlanUsage is one limit of an actor's plan.
type PlanUsage struct {
	Label    string  `json:"label"`
	Used     float64 `json:"used"`
	ResetsAt *int64  `json:"resets_at,omitempty"`
}

// UsagePool is an account several actors draw on.
type UsagePool struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ActorUsage is how much of its plan's limits an actor has used.
type ActorUsage struct {
	Plan []PlanUsage `json:"plan,omitempty"`
	Pool *UsagePool  `json:"pool,omitempty"`
}

// ActorSessionMessage is one entry in an actor session's transcript.
type ActorSessionMessage struct {
	ID        int64           `json:"id"`
	SessionID int64           `json:"session_id"`
	SentAt    int64           `json:"sent_at"`
	Content   json.RawMessage `json:"content"`
	Answered  bool            `json:"answered"`
}

// MessageContent is a transcript entry's content, either side's: the
// fields this client reads.
type MessageContent struct {
	Sender    string     `json:"sender"`
	Kind      string     `json:"kind"`
	Text      string     `json:"text"`
	Tool      string     `json:"tool"`
	Summary   string     `json:"summary"`
	Questions []Question `json:"questions"`
	Subject   *struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"subject"`
	Plan string `json:"plan"`
}

// ActorSession is an actor session as cohesiond holds it: the fields this client reads.
type ActorSession struct {
	ID     int64   `json:"id"`
	Title  string  `json:"title"`
	Mode   *string `json:"mode"`
	Model  *string `json:"model"`
	Effort *string `json:"effort"`
	Status string  `json:"status"`
}

// ServerError is why cohesiond refused a request.
type ServerError struct {
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Field   *string `json:"field,omitempty"`
}

func (e *ServerError) Error() string { return e.Message }

package actor

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cohesionhq/actor-codex/internal/cohesion"
)

// Between Codex's thread items and requests and Cohesion's transcript: what
// an item shows as, the request an approval becomes, and what the user's
// response tells Codex.

// Item is a Codex thread item: the fields this client reads, of every type.
type Item struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	// agentMessage, plan.
	Text string `json:"text"`
	// commandExecution.
	Command        string          `json:"command"`
	Cwd            string          `json:"cwd"`
	CommandActions []CommandAction `json:"commandActions"`
	// commandExecution, fileChange, mcpToolCall, collabAgentToolCall, dynamicToolCall.
	Status string `json:"status"`
	// fileChange.
	Changes []FileUpdate `json:"changes"`
	// mcpToolCall.
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    *struct {
		Content []json.RawMessage `json:"content"`
	} `json:"result"`
	// collabAgentToolCall.
	Prompt            *string                            `json:"prompt"`
	ReceiverThreadIDs []string                           `json:"receiverThreadIds"`
	AgentsStates      map[string]struct{ Status string } `json:"agentsStates"`
	// subAgentActivity.
	Kind          string `json:"kind"`
	AgentThreadID string `json:"agentThreadId"`
	// webSearch.
	Query  string `json:"query"`
	Action *struct {
		Type    string  `json:"type"`
		Query   *string `json:"query"`
		URL     *string `json:"url"`
		Pattern *string `json:"pattern"`
	} `json:"action"`
	// imageView.
	Path string `json:"path"`
	// imageGeneration.
	RevisedPrompt *string `json:"revisedPrompt"`
	SavedPath     *string `json:"savedPath"`
	Base64        *string `json:"result_base64"`
	// dynamicToolCall.
	Namespace *string `json:"namespace"`
}

// CommandAction is what Codex makes of a command it runs.
type CommandAction struct {
	Type    string  `json:"type"` // read, listFiles, search or unknown
	Command string  `json:"command"`
	Name    string  `json:"name"`
	Path    *string `json:"path"`
	Query   *string `json:"query"`
}

// FileUpdate is one file a fileChange item changes.
type FileUpdate struct {
	Path string `json:"path"`
	Kind struct {
		Type     string  `json:"type"` // add, delete or update
		MovePath *string `json:"move_path"`
	} `json:"kind"`
	Diff string `json:"diff"`
}

// UnmarshalJSON reads an imageGeneration's `result` (base64) apart from an
// mcpToolCall's `result` (an object), which share the name.
func (i *Item) UnmarshalJSON(data []byte) error {
	type plain Item
	var raw struct {
		plain
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*i = Item(raw.plain)
	i.Result = nil
	if len(raw.Result) > 0 && raw.Result[0] == '"' {
		var text string
		_ = json.Unmarshal(raw.Result, &text)
		i.Base64 = &text
	} else if len(raw.Result) > 0 && raw.Result[0] == '{' {
		_ = json.Unmarshal(raw.Result, &i.Result)
	}
	return nil
}

// IsTool is whether an item of `kind` is a tool use, shown in the transcript.
func IsTool(kind string) bool {
	switch kind {
	case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "collabAgentToolCall", "webSearch", "imageView", "imageGeneration":
		return true
	}
	return false
}

// ShowsLater is whether an item is shown only once it completes: one whose
// result may hold images, or whose start doesn't yet say what it does.
func ShowsLater(item Item) bool {
	switch item.Type {
	case "mcpToolCall", "imageView", "imageGeneration", "webSearch":
		return true
	}
	return false
}

// ToolUse is an item as the transcript shows it: the tool, its kind, and
// what it did in a line.
func ToolUse(item Item, cwd string) cohesion.SaidContent {
	tool, category, summary := describeItem(item, cwd)
	return cohesion.SaidContent{Kind: "toolUse", Tool: tool, Category: category, Summary: summary}
}

func describeItem(item Item, cwd string) (tool, category, summary string) {
	switch item.Type {
	case "commandExecution":
		return describeCommand(item, cwd)
	case "fileChange":
		paths := make([]string, 0, len(item.Changes))
		for _, change := range item.Changes {
			paths = append(paths, shown(change.Path, cwd))
		}
		return "Edit", cohesion.CategoryEdit, listed(paths)
	case "mcpToolCall":
		return DisplayName(item.Server, item.Tool), cohesion.CategoryOther, firstString(item.Arguments)
	case "dynamicToolCall":
		name := item.Tool
		if item.Namespace != nil && *item.Namespace != "" {
			name = DisplayName(*item.Namespace, item.Tool)
		}
		return name, cohesion.CategoryOther, firstString(item.Arguments)
	case "collabAgentToolCall":
		summary := agentTools[item.Tool]
		if item.Prompt != nil && strings.TrimSpace(*item.Prompt) != "" {
			summary = line(*item.Prompt)
		}
		return "Agent", cohesion.CategoryAgent, summary
	case "webSearch":
		return "WebSearch", cohesion.CategoryWeb, webSummary(item)
	case "imageView":
		return "ViewImage", cohesion.CategoryRead, shown(item.Path, cwd)
	case "imageGeneration":
		summary := ""
		if item.RevisedPrompt != nil {
			summary = line(*item.RevisedPrompt)
		}
		return "ImageGeneration", cohesion.CategoryOther, summary
	}
	return item.Type, cohesion.CategoryOther, ""
}

var agentTools = map[string]string{
	"spawnAgent":     "Starting an agent",
	"sendInput":      "Sending an agent input",
	"sendMessage":    "Messaging an agent",
	"resumeAgent":    "Resuming an agent",
	"wait":           "Waiting on agents",
	"closeAgent":     "Closing an agent",
	"followupTask":   "Giving an agent more to do",
	"interruptAgent": "Interrupting an agent",
	"listAgents":     "Listing agents",
}

// describeCommand names a command by what Codex makes of it: one that only
// reads, lists or searches files shows as that, the rest as a shell command.
func describeCommand(item Item, cwd string) (tool, category, summary string) {
	command := UnwrapShell(item.Command)
	kind := ""
	for _, action := range item.CommandActions {
		if kind == "" {
			kind = action.Type
		} else if kind != action.Type {
			kind = "unknown"
		}
	}
	switch kind {
	case "read":
		var paths []string
		for _, action := range item.CommandActions {
			if action.Path != nil {
				paths = append(paths, shown(*action.Path, cwd))
			} else {
				paths = append(paths, action.Name)
			}
		}
		return "Read", cohesion.CategoryRead, listed(paths)
	case "listFiles":
		path := "."
		if action := item.CommandActions[0]; action.Path != nil && *action.Path != "" {
			path = shown(*action.Path, cwd)
		}
		return "List", cohesion.CategoryRead, path
	case "search":
		action := item.CommandActions[0]
		summary := ""
		if action.Query != nil {
			summary = *action.Query
		}
		if action.Path != nil && *action.Path != "" {
			summary = strings.TrimSpace(summary + " in " + shown(*action.Path, cwd))
		}
		if summary == "" {
			summary = command
		}
		return "Search", cohesion.CategorySearch, line(summary)
	}
	return "Shell", cohesion.CategoryCommand, line(command)
}

func webSummary(item Item) string {
	if item.Action != nil {
		switch item.Action.Type {
		case "openPage":
			if item.Action.URL != nil {
				return *item.Action.URL
			}
		case "findInPage":
			if item.Action.Pattern != nil {
				return line(*item.Action.Pattern)
			}
		case "search":
			if item.Action.Query != nil {
				return line(*item.Action.Query)
			}
		}
	}
	return line(item.Query)
}

// Progress is what the actor is doing while an item runs, for the session's activity.
func Progress(item Item, cwd string) string {
	tool, _, summary := describeItem(item, cwd)
	if item.Type == "collabAgentToolCall" {
		if summary != "" {
			return summary
		}
		return "Working with a subagent"
	}
	verbs := map[string]string{
		"Shell":           "Running",
		"Read":            "Reading",
		"List":            "Listing",
		"Search":          "Searching for",
		"Edit":            "Editing",
		"ViewImage":       "Viewing",
		"ImageGeneration": "Drawing",
	}
	if item.Type == "webSearch" {
		if item.Action != nil && item.Action.Type == "openPage" {
			verbs["WebSearch"] = "Reading"
		} else {
			verbs["WebSearch"] = "Searching the web for"
		}
	}
	if verb, ok := verbs[tool]; ok && summary != "" {
		return verb + " " + summary
	}
	return "Using " + tool
}

// DisplayName is an MCP or dynamic tool as the user reads it: "Server: tool",
// such as "Github: search issues" for the server `github`'s `search_issues`.
func DisplayName(server, tool string) string {
	words := func(text string) string {
		return strings.TrimSpace(regexp.MustCompile(`[_\-\s]+`).ReplaceAllString(text, " "))
	}
	s, t := words(server), words(tool)
	if s != "" {
		s = strings.ToUpper(s[:1]) + s[1:]
	}
	switch {
	case s != "" && t != "":
		return s + ": " + t
	case s != "":
		return s
	case t != "":
		return t
	}
	return "Tool"
}

var shellWrapper = regexp.MustCompile(`^(?:/\S*/)?(?:ba|z|da|k)?sh\s+-l?c\s+([\s\S]+)$`)

// UnwrapShell is a command without the `bash -lc '...'` Codex runs it in.
func UnwrapShell(command string) string {
	command = strings.TrimSpace(command)
	match := shellWrapper.FindStringSubmatch(command)
	if match == nil {
		return command
	}
	inner := strings.TrimSpace(match[1])
	if len(inner) >= 2 && (inner[0] == '\'' || inner[0] == '"') && inner[len(inner)-1] == inner[0] {
		inner = inner[1 : len(inner)-1]
		if command[strings.Index(command, inner)-1] == '\'' {
			inner = strings.ReplaceAll(inner, `'\''`, `'`)
		}
	}
	return inner
}

// Request kinds, by Codex's method for them.
const (
	CommandApproval    = "item/commandExecution/requestApproval"
	FileChangeApproval = "item/fileChange/requestApproval"
	PermissionsRequest = "item/permissions/requestApproval"
	UserInputRequest   = "item/tool/requestUserInput"
	ElicitationRequest = "mcpServer/elicitation/request"
)

// ApprovalParams are a server request's params: the fields this client reads, of every kind.
type ApprovalParams struct {
	ThreadID string  `json:"threadId"`
	TurnID   string  `json:"turnId"`
	ItemID   string  `json:"itemId"`
	Reason   *string `json:"reason"`
	// commandExecution.
	Command                *string          `json:"command"`
	Cwd                    *string          `json:"cwd"`
	NetworkApprovalContext *json.RawMessage `json:"networkApprovalContext"`
	// permissions.
	Permissions json.RawMessage `json:"permissions"`
	// requestUserInput.
	Questions []InputQuestion `json:"questions"`
	// elicitation.
	ServerName      string             `json:"serverName"`
	Mode            string             `json:"mode"`
	Message         string             `json:"message"`
	URL             string             `json:"url"`
	RequestedSchema *ElicitationSchema `json:"requestedSchema"`
}

// InputQuestion is one question of a requestUserInput.
type InputQuestion struct {
	ID       string `json:"id"`
	Header   string `json:"header"`
	Question string `json:"question"`
	IsOther  bool   `json:"isOther"`
	IsSecret bool   `json:"isSecret"`
	Options  []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
}

// ElicitationSchema is the flat object schema an MCP form asks to be filled.
type ElicitationSchema struct {
	Properties map[string]struct {
		Type        string   `json:"type"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Enum        []string `json:"enum"`
		EnumNames   []string `json:"enumNames"`
	} `json:"properties"`
	Required []string `json:"required"`
}

// fields is the form's fields in a steady order: required first, then by name.
func (s *ElicitationSchema) fields() []string {
	if s == nil {
		return nil
	}
	var names []string
	for _, name := range s.Required {
		if _, ok := s.Properties[name]; ok {
			names = append(names, name)
		}
	}
	var rest []string
	for name := range s.Properties {
		if !contains(names, name) {
			rest = append(rest, name)
		}
	}
	sortStrings(rest)
	return append(names, rest...)
}

// Request is the request a server request of `method` becomes, with the
// file changes Codex showed for its item.
func Request(method string, params ApprovalParams, changes []FileUpdate, cwd string) cohesion.SaidContent {
	switch method {
	case UserInputRequest:
		questions := make([]cohesion.Question, 0, len(params.Questions))
		for _, asked := range params.Questions {
			options := make([]cohesion.QuestionOption, 0, len(asked.Options))
			for _, option := range asked.Options {
				options = append(options, cohesion.QuestionOption{Label: option.Label, Description: nullable(option.Description)})
			}
			questions = append(questions, cohesion.Question{
				Header:      nullable(asked.Header),
				Question:    asked.Question,
				Options:     options,
				AllowsOther: asked.IsOther || len(options) == 0,
			})
		}
		return cohesion.SaidContent{Kind: "question", Questions: questions}
	case ElicitationRequest:
		if params.Mode != "url" && len(params.RequestedSchema.fields()) > 0 {
			return cohesion.SaidContent{Kind: "question", Questions: formQuestions(params)}
		}
	}
	return cohesion.SaidContent{
		Kind:      "permission",
		Subject:   subject(method, params, changes, cwd),
		Reason:    params.Reason,
		Decisions: decisions(method),
	}
}

func decisions(method string) []string {
	if method == ElicitationRequest {
		return []string{cohesion.Allow, cohesion.Deny, cohesion.Abort}
	}
	return []string{cohesion.Allow, cohesion.AllowForSession, cohesion.Deny, cohesion.Abort}
}

func subject(method string, params ApprovalParams, changes []FileUpdate, cwd string) *cohesion.Subject {
	switch method {
	case CommandApproval:
		if params.Command != nil {
			where := cwd
			if params.Cwd != nil && *params.Cwd != "" {
				where = *params.Cwd
			}
			return &cohesion.Subject{Kind: "command", Command: UnwrapShell(*params.Command), Cwd: &where}
		}
		var network any
		if params.NetworkApprovalContext != nil {
			_ = json.Unmarshal(*params.NetworkApprovalContext, &network)
		}
		return &cohesion.Subject{Kind: "tool", Name: "Network access", Input: network}
	case FileChangeApproval:
		if len(changes) == 0 {
			return &cohesion.Subject{Kind: "tool", Name: "Edit", Input: map[string]any{}}
		}
		files := make([]cohesion.FileChange, 0, len(changes))
		for _, change := range changes {
			files = append(files, fileChange(change, cwd))
		}
		return &cohesion.Subject{Kind: "fileChange", Changes: files}
	case PermissionsRequest:
		var permissions any
		_ = json.Unmarshal(params.Permissions, &permissions)
		return &cohesion.Subject{Kind: "tool", Name: "Permissions", Input: permissions}
	case ElicitationRequest:
		input := map[string]any{"message": params.Message}
		if params.URL != "" {
			input["url"] = params.URL
		}
		return &cohesion.Subject{Kind: "tool", Name: DisplayName(params.ServerName, ""), Input: input}
	}
	return &cohesion.Subject{Kind: "tool", Name: method, Input: map[string]any{}}
}

func fileChange(change FileUpdate, cwd string) cohesion.FileChange {
	name := shown(change.Path, cwd)
	var diff string
	kind := change.Kind.Type
	switch kind {
	case "add":
		diff = wholeFileDiff(name, nil, change.Diff)
	case "delete":
		diff = wholeFileDiff(name, &change.Diff, "")
	default:
		kind = "update"
		diff = change.Diff
		if !strings.HasPrefix(diff, "---") {
			to := name
			if change.Kind.MovePath != nil {
				to = shown(*change.Kind.MovePath, cwd)
			}
			diff = fmt.Sprintf("--- a/%s\n+++ b/%s\n%s", name, to, diff)
		}
	}
	return cohesion.FileChange{Path: name, Change: kind, Diff: &diff}
}

// wholeFileDiff is a whole-file change as a unified diff: every old line out,
// every new line in. A nil `before` is a new file; an empty `after` a deleted one.
func wholeFileDiff(name string, before *string, after string) string {
	split := func(text string) []string {
		if text == "" {
			return nil
		}
		return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	}
	var removed []string
	if before != nil {
		removed = split(*before)
	}
	added := split(after)
	from, to := "--- a/"+name, "+++ b/"+name
	if before == nil {
		from = "--- /dev/null"
	}
	if before != nil && after == "" {
		to = "+++ /dev/null"
	}
	start := func(lines []string) int {
		if len(lines) > 0 {
			return 1
		}
		return 0
	}
	out := []string{from, to, fmt.Sprintf("@@ -%d,%d +%d,%d @@", start(removed), len(removed), start(added), len(added))}
	for _, text := range removed {
		out = append(out, "-"+text)
	}
	for _, text := range added {
		out = append(out, "+"+text)
	}
	return strings.Join(out, "\n")
}

func formQuestions(params ApprovalParams) []cohesion.Question {
	var questions []cohesion.Question
	for index, name := range params.RequestedSchema.fields() {
		field := params.RequestedSchema.Properties[name]
		title := field.Title
		if title == "" {
			title = name
		}
		question := title
		if field.Description != "" {
			question = field.Description
		}
		if index == 0 && params.Message != "" {
			question = params.Message + "\n\n" + question
		}
		var options []cohesion.QuestionOption
		switch {
		case len(field.Enum) > 0:
			for i, value := range field.Enum {
				label := value
				if i < len(field.EnumNames) {
					label = field.EnumNames[i]
				}
				options = append(options, cohesion.QuestionOption{Label: label})
			}
		case field.Type == "boolean":
			options = []cohesion.QuestionOption{{Label: "Yes"}, {Label: "No"}}
		}
		questions = append(questions, cohesion.Question{
			Header:      nullable(DisplayName(params.ServerName, "")),
			Question:    question,
			Options:     options,
			AllowsOther: len(options) == 0,
		})
	}
	return questions
}

// Response is what the user's `response` to server request `method` tells
// Codex; nil `response` is a request stopped. It also says whether the
// user's note, said with a denial, should follow as words.
func Response(method string, params ApprovalParams, response *cohesion.UserContent) any {
	switch method {
	case UserInputRequest:
		answers := map[string]any{}
		for index, asked := range params.Questions {
			picked := []string{}
			if response != nil && response.Kind == "answers" && index < len(response.Answers) {
				picked = picks(response.Answers[index])
			}
			answers[asked.ID] = map[string]any{"answers": picked}
		}
		return map[string]any{"answers": answers}
	case ElicitationRequest:
		return elicitationResponse(params, response)
	case PermissionsRequest:
		switch decision(response) {
		case cohesion.Allow:
			return map[string]any{"permissions": json.RawMessage(params.Permissions), "scope": "turn"}
		case cohesion.AllowForSession:
			return map[string]any{"permissions": json.RawMessage(params.Permissions), "scope": "session"}
		}
		return map[string]any{"permissions": map[string]any{}, "scope": "turn"}
	}
	// Command and file change approvals.
	switch decision(response) {
	case cohesion.Allow:
		return map[string]any{"decision": "accept"}
	case cohesion.AllowForSession:
		return map[string]any{"decision": "acceptForSession"}
	case cohesion.Abort:
		return map[string]any{"decision": "cancel"}
	}
	return map[string]any{"decision": "decline"}
}

// decision is the user's decision on a permission request: words in place
// of a response decline it, and a stopped one is aborted.
func decision(response *cohesion.UserContent) string {
	switch {
	case response == nil:
		return cohesion.Abort
	case response.Kind == "permission":
		return response.Decision
	}
	return cohesion.Deny
}

func elicitationResponse(params ApprovalParams, response *cohesion.UserContent) any {
	if params.Mode == "url" || len(params.RequestedSchema.fields()) == 0 {
		switch decision(response) {
		case cohesion.Allow, cohesion.AllowForSession:
			return map[string]any{"action": "accept", "content": nil}
		case cohesion.Abort:
			return map[string]any{"action": "cancel", "content": nil}
		}
		return map[string]any{"action": "decline", "content": nil}
	}
	if response == nil {
		return map[string]any{"action": "cancel", "content": nil}
	}
	if response.Kind != "answers" {
		return map[string]any{"action": "decline", "content": nil}
	}
	content := map[string]any{}
	for index, name := range params.RequestedSchema.fields() {
		if index >= len(response.Answers) {
			break
		}
		picked := picks(response.Answers[index])
		if len(picked) == 0 {
			continue
		}
		field := params.RequestedSchema.Properties[name]
		value := picked[0]
		for i, label := range field.EnumNames {
			if label == value && i < len(field.Enum) {
				value = field.Enum[i]
			}
		}
		switch field.Type {
		case "boolean":
			content[name] = value == "Yes"
		case "number", "integer":
			var number float64
			if _, err := fmt.Sscan(value, &number); err == nil {
				content[name] = number
			} else {
				content[name] = value
			}
		default:
			content[name] = value
		}
	}
	return map[string]any{"action": "accept", "content": content}
}

func picks(answer cohesion.Answer) []string {
	picked := append([]string{}, answer.Selected...)
	if answer.Other != nil && strings.TrimSpace(*answer.Other) != "" {
		picked = append(picked, *answer.Other)
	}
	return picked
}

// Note is what the user said with a denial, to follow as words, or "".
func Note(response *cohesion.UserContent) string {
	if response == nil || response.Kind != "permission" || response.Message == nil {
		return ""
	}
	if response.Decision != cohesion.Deny && response.Decision != cohesion.Abort {
		return ""
	}
	return strings.TrimSpace(*response.Message)
}

// Describe is what the user said, as words: a response to a request no
// longer waiting (its Codex process ended) is handed over as text.
func Describe(response cohesion.UserContent) string {
	switch response.Kind {
	case "answers":
		parts := make([]string, 0, len(response.Answers))
		for _, answer := range response.Answers {
			parts = append(parts, strings.Join(picks(answer), ", "))
		}
		return "My answers to your earlier questions: " + strings.Join(parts, "; ")
	case "permission":
		verdict := "no"
		if response.Decision == cohesion.Allow || response.Decision == cohesion.AllowForSession {
			verdict = "go ahead"
		}
		text := "About your earlier request: " + verdict + "."
		if response.Message != nil && *response.Message != "" {
			text += " " + *response.Message
		}
		return text
	case "plan":
		if response.Approved {
			return ImplementPlan
		}
		text := "I don't approve your plan."
		if response.Feedback != nil && *response.Feedback != "" {
			text += " " + *response.Feedback
		}
		return text
	}
	return response.Text
}

// ImplementPlan is what an approved plan has Codex do.
const ImplementPlan = "I approve your plan; go ahead and implement it."

// shown is `path` relative to the actor's directory when it is inside it.
func shown(path, cwd string) string {
	if !filepath.IsAbs(path) {
		return path
	}
	inside, err := filepath.Rel(cwd, path)
	if err != nil || inside == ".." || strings.HasPrefix(inside, "../") {
		return path
	}
	return inside
}

func listed(paths []string) string {
	switch len(paths) {
	case 0:
		return ""
	case 1:
		return paths[0]
	}
	return fmt.Sprintf("%s and %d more", paths[0], len(paths)-1)
}

func line(text string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if runes := []rune(first); len(runes) > 200 {
		return string(runes[:199]) + "…"
	}
	return first
}

// firstString is the first string among a tool's arguments, in a line.
func firstString(arguments json.RawMessage) string {
	var fields map[string]any
	if json.Unmarshal(arguments, &fields) != nil {
		return ""
	}
	// Map order is random: the shortest key name first is steady, and
	// favours `query`, `path`, `url` over long descriptions.
	var best string
	for key, value := range fields {
		if _, ok := value.(string); ok && (best == "" || len(key) < len(best) || len(key) == len(best) && key < best) {
			best = key
		}
	}
	if best == "" {
		return ""
	}
	return line(fields[best].(string))
}

func nullable(text string) *string {
	if text == "" {
		return nil
	}
	return &text
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func sortStrings(list []string) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j] < list[j-1]; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

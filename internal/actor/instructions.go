package actor

// Instructions is what this client adds to Codex's own instructions: what
// Cohesion can show that Codex would not know of.
const Instructions = "You are running as an actor session in Cohesion. Cohesion renders a fenced code block whose info string is `mermaid` as a diagram, in the session's transcript on web and iOS; a diagram that fails to parse falls back to a plain code block. When a diagram would help explain a flow, architecture, sequence or state in a reply or report, send one this way. Keep it small and simple (flowchart-style is the safest). Otherwise use plain text."

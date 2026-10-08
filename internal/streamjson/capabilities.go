package streamjson

import "github.com/greenthread-ai/klaudia/internal/version"

// ProtocolVersion is the version of the embedding contract in
// docs/embedding.md. It changes only when a stable line type or field is
// removed or changes meaning; additions (a new event type, a new field) do not
// bump it, and a driver must ignore what it does not recognise.
const ProtocolVersion = 1

// Capabilities is what `klaudia --capabilities` prints: enough for a driver to
// feature-detect the embedding contract instead of parsing --version.
type Capabilities struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Release  string `json:"release,omitempty"`
	Revision string `json:"revision,omitempty"`
	Modified bool   `json:"modified,omitempty"`

	// StreamJSON describes the stdin/stdout protocol.
	StreamJSON StreamJSONCaps `json:"stream_json"`
	// Session lists the session-identity features.
	Session []string `json:"session"`
	// PermissionModes are the values --permission-mode and set_permission_mode
	// accept.
	PermissionModes []string `json:"permission_modes"`
	// PermissionModeAliases are retired mode names still accepted, and the
	// mode each selects.
	PermissionModeAliases map[string]string `json:"permission_mode_aliases"`
	// Providers are the values of the config's provider key.
	Providers []string `json:"providers"`
}

// StreamJSONCaps is the stream-json part of Capabilities.
type StreamJSONCaps struct {
	Protocol int `json:"protocol"`
	// Input lines a driver may send, and the control_request subtypes
	// Klaudia answers.
	Input           []string `json:"input"`
	ControlRequests []string `json:"control_requests"`
	// Output lines Klaudia writes, and the control_request subtypes it sends
	// the driver.
	Output         []string `json:"output"`
	ControlAsks    []string `json:"control_asks"`
	ResultFields   []string `json:"result_fields"`
	UsageFields    []string `json:"usage_fields"`
	InitFields     []string `json:"init_fields"`
	PartialOutput  bool     `json:"partial_messages"`
	AskTimeoutFlag string   `json:"ask_timeout_flag"`
}

// GetCapabilities reports the running binary's capabilities.
func GetCapabilities(modes, providers []string) Capabilities {
	info := version.Get()
	return Capabilities{
		Name:     "klaudia",
		Version:  version.Version,
		Release:  info.Release,
		Revision: info.Revision,
		Modified: info.Modified,
		StreamJSON: StreamJSONCaps{
			Protocol:        ProtocolVersion,
			Input:           []string{"user", "control_request", "control_response"},
			ControlRequests: []string{"interrupt", "set_permission_mode", "set_model", "initialize"},
			Output:          []string{"system/init", "assistant", "user", "usage", "tool_progress", "subagent_started", "subagent_finished", "compaction", "warning", "notice", "control_request", "control_response", "result"},
			ControlAsks:     []string{"can_use_tool", "ask_user", "exit_plan"},
			ResultFields:    []string{"type", "subtype", "is_error", "result", "session_id", "duration_ms", "num_turns", "stop_reason", "total_cost_usd", "usage"},
			UsageFields:     []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"},
			InitFields:      []string{"session_id", "cwd", "model", "permissionMode", "resumed", "resumed_from", "history_messages"},
			PartialOutput:   true,
			AskTimeoutFlag:  "--ask-timeout",
		},
		Session:         []string{"session-id", "resume", "resume-across-cwd", "fork-session", "continue"},
		PermissionModes: modes,
		PermissionModeAliases: map[string]string{
			"default": "autonomous", "acceptEdits": "autonomous", "dontAsk": "autonomous",
		},
		Providers: providers,
	}
}

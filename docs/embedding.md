# Embedding Klaudia: the stream-json contract

This is the contract a program relies on when it drives Klaudia as a managed
agent — an editor integration, an SDK, or an orchestrator that starts, watches,
stops and resumes sessions. It is versioned: `klaudia --capabilities` reports
the protocol version, and `TestEmbeddingContract` (internal/cli) fails when an
embedded run stops matching this document.

```bash
klaudia --input-format stream-json --output-format stream-json --verbose \
        --permission-mode autonomous --session-id <id>
```

Once the session starts, Klaudia writes stream-json whatever `--output-format`
says. Pass `--output-format stream-json --verbose` anyway. Without it, a failure
*before* the session starts (no credential, a bad config, a bad flag) is
reported only on stderr. With it, that failure also arrives as a `result` line
on stdout with `"is_error":true`, the same as a turn's error.

## Stability

**Protocol version 1.** Everything marked *stable* below keeps its name, type
and meaning within a protocol version. The version is bumped only when a stable
line type or field is removed or changes meaning.

**Additions do not bump it.** New line types, new fields on existing lines, new
`subtype`s and new control requests can appear at any time. A driver must
ignore anything it does not recognise — an unknown line type, an unknown field
— rather than fail on it.

Anything not listed here is *internal*: present today, free to change.

## Probing: `klaudia --capabilities`

Prints JSON and exits, before reading config or credentials, so it works on an
unconfigured host:

```json
{
  "name": "klaudia",
  "version": "2.1.66-klaudia",
  "release": "v…", "revision": "…", "modified": false,
  "stream_json": {
    "protocol": 1,
    "input": ["user", "control_request", "control_response"],
    "control_requests": ["interrupt", "set_permission_mode", "set_model", "initialize"],
    "output": ["system/init", "assistant", "user", "usage", "tool_progress", "compaction",
               "warning", "notice", "control_request", "control_response", "result"],
    "control_asks": ["can_use_tool", "ask_user", "exit_plan"],
    "result_fields": ["type", "subtype", "is_error", "result", "session_id", "duration_ms",
                      "num_turns", "stop_reason", "total_cost_usd", "usage"],
    "usage_fields": ["input_tokens", "output_tokens", "cache_read_input_tokens",
                     "cache_creation_input_tokens"],
    "init_fields": ["session_id", "cwd", "model", "permissionMode", "resumed",
                    "resumed_from", "history_messages"],
    "partial_messages": true,
    "ask_timeout_flag": "--ask-timeout"
  },
  "session": ["session-id", "resume", "resume-across-cwd", "fork-session", "continue"],
  "permission_modes": ["autonomous", "plan", "bypassPermissions"],
  "permission_mode_aliases": {"default": "autonomous", "acceptEdits": "autonomous", "dontAsk": "autonomous"},
  "providers": ["anthropic", "openai"]
}
```

Feature-detect from `stream_json.protocol` and the lists, not from `version`.
`release`/`revision` are empty in a binary built without VCS stamping.

## Framing

One JSON object per line, UTF-8, `\n`-terminated, on stdin and stdout. Input
lines up to 16 MiB are accepted. A line that is not JSON is skipped.

**stderr is not part of the protocol.** It carries human-readable diagnostics:
warnings, MCP server logs, and a fatal error's text. Log it; do not parse it.

**stdin EOF ends the session.** Klaudia finishes the turns already queued,
writes their `result` lines, and exits 0. Exit codes for the failures that
happen before a session starts are in [jobs.md](jobs.md#exit-codes). Code 2
means invoked wrongly, and nothing ran.

## Input lines (driver → Klaudia)

### `user`: one turn (stable)

```json
{"type":"user","message":{"role":"user","content":"Fix the failing test"}}
```

`content` is a string, or an array of content blocks of which the `text`
blocks are concatenated. Turns run one at a time in order. A driver can send
ahead: lines queue without bound, and every `user` line gets exactly one
`result` line.

### `control_response`: answer to a `can_use_tool` ask (stable)

```json
{"type":"control_response","response":{"subtype":"success","request_id":"<id>",
 "response":{"behavior":"allow"}}}
{"type":"control_response","response":{"subtype":"success","request_id":"<id>",
 "response":{"behavior":"deny","message":"read-only driver"}}}
```

Anything other than `"subtype":"success"` with `"behavior":"allow"` is a deny.
An answer for an unknown or expired `request_id` is ignored.

### `control_request`: steer the session (stable)

```json
{"type":"control_request","request_id":"r1","request":{"subtype":"interrupt"}}
```

Each is answered with a `control_response` carrying the same `request_id`.
Success looks like `{"subtype":"success","request_id":"r1","response":{…}}`,
failure like `{"subtype":"error","request_id":"r1","error":"…"}`.

| subtype | fields | success payload | effect |
|---|---|---|---|
| `interrupt` | — | `{}` | Cancels the running turn (and a `can_use_tool` ask it is parked on) and every turn queued before the interrupt. Each still gets a `result` line with `"subtype":"error_during_execution"`. |
| `set_permission_mode` | `mode` | `{"mode":…}` | Applies from the next tool call, sub-agents included. `autonomous` needs the host gate enforcing; `bypassPermissions` is refused unless the session was launched in it. |
| `set_model` | `model` (omitted or `"default"` restores the launch model) | `{"model":…}` | Applies from the next turn. |
| `initialize` | Claude Code's | `{"commands":[],"models":[],…}` | Answered once. A request asking for `hooks`, `sdkMcpServers`, `agents`, `jsonSchema` or a system prompt is refused with an error: Klaudia cannot run an SDK's callbacks. Configure those in Klaudia's own config instead. |

## Output lines (Klaudia → driver)

### `system/init`: first line (stable)

```json
{"type":"system","subtype":"init","session_id":"chat-42","cwd":"/work/repo",
 "model":"claude-sonnet-5-5","permissionMode":"autonomous","resumed":true,
 "resumed_from":"chat-42","history_messages":14}
```

Written before any turn, so the driver learns the session id without sending
anything. `resumed_from` is present only when history was loaded; it differs
from `session_id` for a fork.

### `assistant` / `user`: the conversation (stable)

Every message the agent records, in the envelope Claude Code and `-p
--output-format stream-json` use:

```json
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"…"},
 {"type":"tool_use","id":"toolu_…","name":"Read","input":{"file_path":"…"}}]},
 "session_id":"chat-42","parent_tool_use_id":null,"uuid":"…"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result",
 "tool_use_id":"toolu_…","content":"…"}]},
 "session_id":"chat-42","parent_tool_use_id":null,"uuid":"…"}
```

Each turn opens with a `user` envelope echoing the driver's own prompt as
recorded. Tool results follow as `user` envelopes too, so the stream alone is a
complete transcript. `message` is the Anthropic Messages API message as sent to
or received from the model. **Stable:** the envelope fields, `role`, and the `text` / `tool_use` /
`tool_result` block shapes. Other block types (thinking, server tool blocks)
may appear; skip what you do not render.

### `control_request` `can_use_tool`: a permission ask (stable)

```json
{"type":"control_request","request_id":"<id>",
 "request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"make test"},
            "tool_use_id":"toolu_…","specifier":"make test","suggestion":"…",
            "host_change":{…}}}
```

`tool_use_id`, `specifier`, `suggestion` and `host_change` are optional and
present when known. `host_change` marks an ask about a change to this machine
rather than about a tool (summary, reason, paths, services, packages) — render
it as such; "allow Bash?" is the wrong question for `systemctl restart nginx`.

This is sent only for a call nothing else could settle. There are no
allow/deny rules (the per-command model was removed upstream, 0fa00a6): in
`autonomous`, project work runs without asking, so what reaches the driver is
a change to this machine or a write to project knowledge
(`.klaudia/KNOWLEDGE.md`); `bypassPermissions` asks nothing. The turn blocks
until the answer arrives, or until `--ask-timeout` passes (default 10m; `0`
waits forever). After the timeout the call is denied with a tool result saying
no answer came.

The retired modes `default`, `acceptEdits` and `dontAsk` are still accepted on
the command line, in config and over `set_permission_mode`, as aliases for
`autonomous` (a `note:` line on stderr says so), so a launcher written against
them keeps starting.

### `control_request` `ask_user` and `exit_plan` (stable)

The model's own questions (`AskUserQuestion`, and an MCP server's elicitation)
and plan approvals (`ExitPlanMode`) use the same channel:

```json
{"type":"control_request","request_id":"<id>",
 "request":{"subtype":"ask_user","question":"Which database?",
            "options":[{"label":"Postgres","description":"…"},{"label":"SQLite"}]}}
→ {"type":"control_response","response":{"subtype":"success","request_id":"<id>",
   "response":{"label":"Postgres"}}}

{"type":"control_request","request_id":"<id>","request":{"subtype":"exit_plan","plan":"1. …"}}
→ {"type":"control_response","response":{"subtype":"success","request_id":"<id>",
   "response":{"approved":true}}}
```

A driver that does not implement one answers `{"subtype":"error","error":"…"}`:
that reads as "cancelled" and the model carries on without inventing an answer.
A response with no `subtype` at all is treated as success. Both are bounded by
`--ask-timeout` like `can_use_tool`.

### `result`: end of a turn (stable)

```json
{"type":"result","subtype":"success","is_error":false,"result":"Fixed; tests pass.",
 "session_id":"chat-42","duration_ms":48211,"num_turns":7,"stop_reason":"end_turn",
 "total_cost_usd":0.0412,
 "usage":{"input_tokens":5120,"output_tokens":812,"cache_read_input_tokens":40210,
          "cache_creation_input_tokens":0}}
```

| field | meaning |
|---|---|
| `subtype` | `success`; `error_during_execution` (the turn failed or was interrupted); or the stop reason (`refusal`, `max_tokens`, …) with `is_error` true when the turn ended with **no text** — a refusal is not a finished task |
| `is_error` | the turn ended in an error |
| `result` | the final assistant text; for an error, `"Error: <readable reason>"` |
| `num_turns` | model calls in this turn (one per tool round-trip) |
| `stop_reason` | the last model call's stop reason |
| `usage` | tokens for **this turn**, summed over its model calls, not cumulative for the session |
| `total_cost_usd` | priced from Klaudia's model table; `0` for an unpriced model (every OpenAI-compatible one). 0 means unpriced, not free; `usage` is the reliable figure |

### Progress events (stable `type`, internal body)

These are flat lines with no message form. A driver can show them as live
status, and can skip them without losing any conversation content.

- `usage`: a per-model-call token delta (`input_delta`, `output_delta`, `turn_delta`). The `result` line's `usage` is the per-turn total.
- `tool_progress`: a status line from a long-running tool (`tool_name`, `tool_use_id`, `text`).
- `compaction`: the history was summarised (`content`).
- `warning`, `notice`: something worth showing a person (`content`).

### Partial messages (stable, opt-in)

`--include-partial-messages` adds Claude Code's `stream_event` lines (raw model
stream deltas) for token-by-token rendering. It applies only to `-p
--output-format stream-json`, not to the stdin-driven session.

## Background sub-agents

An `Agent` call with `background: true` returns a handle right away. The
sub-agent's result goes back to the model as a `user` message at the start of
a later turn. In the stdin-driven session, that is the next turn the driver
starts. Klaudia does not start a turn on its own when an agent finishes.

A `-p` run has only one turn. So before it writes `result`, it waits for
outstanding background agents and runs a follow-up turn with their results.
That follow-up counts toward `--max-turns` and `--max-budget-usd`, the wait is
capped at 10 minutes, and `result` covers every turn. If the run stops
waiting, it writes a `warning` line naming each agent whose result was
dropped, and stops those agents.

## Session lifecycle

Embedded runs are stateless unless told otherwise: no auto-resume. To make a
session survive restarts:

1. **First launch.** Pass `--session-id <id>`: letters, digits, `-` and `_`, at
   most 128 characters, and the id must be new. The transcript is written
   under that id and `system/init` echoes it.
2. **Every later launch.** Pass `--resume <id>` (`-r`). The conversation is
   restored, `system/init` reports `"resumed":true` and `history_messages`, and
   new turns append to the same transcript file.

**Resume works from any working directory.** `-r` finds the transcript by id
anywhere under the sessions root (`$KLAUDIA_CONFIG_DIR/sessions`, default
`~/.klaudia/sessions`) and keeps appending where it found it. The sessions root
can also be moved to another host, as long as `KLAUDIA_CONFIG_DIR` points at it.
`TestEmbeddingSessionIDResumesFromAnotherDirAndHost` covers this.

**Choosing between the two flags.** `--session-id <id>` for an id that already
exists is a usage error (exit 2, nothing runs), and `--resume <id>` for one that
does not is also an error. A driver decides between them by whether it has
started that session before. Passing both with the same id (`-r X
--session-id X`) is the same as `-r X`.

**Forking.** `-r <old> --session-id <new>`, or `--fork-session`, starts a new
session seeded from `<old>` and leaves `<old>` untouched.

A resumed session that has a persisted compaction summary is seeded from that
summary plus the messages recorded since. Session-scoped approvals are **not**
restored.

## Related

- README, [Embedding](../README.md#embedding-stream-json-over-stdin) and
  [Resuming](../README.md#resuming): the user-facing overview.
- `internal/streamjson/driver.go`: the implementation; its package comment is
  the protocol summary.
- [trust.md](trust.md): what the host gate refuses regardless of permission
  mode.

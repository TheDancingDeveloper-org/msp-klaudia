# Changelog

Notable changes to Klaudia. The version tracks the Claude Code reference the Go
port mirrors (see `internal/version`).

## Unreleased

### Added
- **`--prompt-interactive "<text>"`** opens the TUI and sends the text as the
  first message, exactly as if typed (slash commands, `@file`), and the session
  stays open. A positional prompt still means `-p` (headless, exit after one
  turn), which is why the Vogt Klaudia template's brief ended its session after
  one reply. Combining it with `-p`, a positional prompt, `--loop` or an
  embedding input format is a usage error.

### Fixed
- **Grep no longer hides what it did not search.** `-A`/`-B`/`-C` or `-n`
  without `output_mode` now selects `content`, where they used to be dropped
  and bare paths returned. When a Grep or Glob walk leaves out hidden or
  ignored directories, the result says which ones (and, when nothing matched,
  how many dotfiles and ignored files) and how to include them — instead of a
  bare "No matches found" that models read as "search is broken" before
  falling back to `rg` in the shell. Both tools take `hidden` and `no_ignore`;
  `no_ignore` also lifts the node_modules/vendor/__pycache__ default.
  Version-control directories (`.git`, `.svn`, `.hg`) are never walked unless
  named, and never reported. (#283)

### Merged from upstream (greenthread-ai/klaudia 2e644c9, 0fa00a6)

Upstream's "one turn contract, ACP, sub-agent worktrees, hooks, MCP
alignment" and "Two bugs" commits are merged; their entries follow below.
Where they met this fork's own work, the fork resolved them as follows:

- **Permission model removed, with aliases kept.** Allow/deny rules,
  `--allowedTools`/`--disallowedTools`, `/allow`/`/deny`, "always", the host
  gate's observe/off postures and `[permissions] allow`/`[trust]` are gone, as
  upstream intends. `default`, `acceptEdits` and `dontAsk` are still accepted
  (flag, config, `set_permission_mode`) as aliases for `autonomous`, with a
  `note:` line, so launchers built on them — the Vogt Klaudia template passes
  `acceptEdits` — keep starting instead of exiting 2.
- **Hooks are upstream's**: four events (SessionStart, UserPromptSubmit,
  PreToolUse, PostToolUse), flat `[[hooks]]` entries, a trust prompt for project
  hooks. The fork's per-event `[[hooks.PreToolUse]]` tables and its Stop event
  are gone; such a config needs rewriting.
- **Sub-agents**: upstream's seeded per-sub-agent worktrees for synchronous
  writers, alongside the fork's background sub-agents (`/agents`), whose
  writers keep their own isolation.
- **Dispatch**: upstream's parallel read-only dispatch and failure-state model,
  plus the fork's validate-before-gate ordering, the goal loop's gitguard
  CommandGuard and LSP diagnostics on edits.
- **stream-json**: the fork's driver (envelopes, interrupt, set_model,
  init line, per-turn usage, `--ask-timeout`) on upstream's `agent.Turn`, with
  upstream's `ask_user` and `exit_plan` requests, richer `can_use_tool`
  payloads and refusal-is-not-success on the result line. docs/embedding.md is
  updated; `--capabilities` lists the new asks and the mode aliases.
- **Instructions**: the fork's loader (@imports, parent directories,
  `.claude/rules`) now reads AGENTS.md and CLAUDE.md both, generic first, with
  upstream's symlink and duplicate-content dedup and `~/.klaudia/AGENTS.md`.
- **MCP**: upstream's elicitation and auto-revive on top of the fork's
  list-changed handler, per-call timeouts and rich results. An explicit `/mcp`
  disconnect is not undone by auto-revive, and only a "session not found" call
  is re-sent to a non-read-only tool — a closed connection may follow a call
  that already ran.

### Fixed
- **Background sub-agents deliver outside the TUI** (#276). An `Agent` call
  with `background: true` reports "on a later turn", but only the TUI
  collected results, so over `-p`, stream-json and ACP the result never
  reached the model. A `-p` run also exited while the agents were still
  running, with the model promising to report them. Collection is now set
  once, on the shared path, for every frontend. A stream-json or ACP session
  gets the result at the start of its next turn, and an ACP result goes back
  to the editor thread that launched the agent, not to any other. A `-p` run
  now waits for outstanding background agents before it prints `result`, then
  hands their results to the model as a follow-up turn. That turn counts
  toward `--max-turns` and `--max-budget-usd`, and the whole wait is capped
  at 10 minutes. If the run stops waiting (a cap, a failure, the timeout, an
  interrupt), it stops the agents and prints a `warning` naming each one
  whose result was dropped. `num_turns`, `usage` and `total_cost_usd` cover
  every turn.
- **A provider's 400 is quoted, not guessed at** (#245). An OpenAI-compatible
  host that sends `{"error":"unsupported model: …"}` (error as a string), a
  top-level `message`/`detail`, or plain text used to get "the model rejected
  the request shape; check the input", which pointed at the request when the
  model id was wrong. Those shapes are now parsed, and any other body is quoted
  (one line, truncated). When a `<prefix>/<name>` model id is refused, the error
  explains that ids are sent exactly as written and, if the endpoint listed the
  models it serves, names the bare id to use. The README and starter config no
  longer show `model = "openai/gpt-5.5"`, which read as "prefix with the
  provider" and broke every endpoint that lists bare ids.
- **`klaudia goal run` no longer runs as a prompt** (#248). There is no goal
  subcommand, so it used to run "goal run" headless. `goal`, `goal run [N]`,
  `goal stop` and `goal clear` as the whole positional prompt are now a usage
  error naming `--loop`, and `klaudia --help` describes the loop. The README
  states the Go 1.26 floor.

### Added
- **Goal loop without the git ceremony** (#246): `--no-branch` / `--no-commit`
  (`/goal run [N] no-branch|no-commit|artifact`), or a `mode: artifact` line in
  the spec, run the loop on the current branch and leave each iteration's work
  uncommitted — for goals whose output is reports or status files rather than a
  diff to merge. Under no-commit the prompts stop asking for commits, and stall
  detection watches the spec instead of HEAD. Code goals keep the branch and a
  commit per iteration by default.

- **A documented, versioned embedding contract** (#247). `docs/embedding.md`
  pins the stream-json protocol for drivers and orchestrators: which line types
  and fields are stable, the control-request round-trips and `--ask-timeout`,
  the result line's per-turn `usage`, and the `--session-id` / `--resume`
  lifecycle across restarts, working directories and hosts.
  `klaudia --capabilities` prints the protocol version and supported features as
  JSON (before reading any config) so a driver can feature-detect, and
  `TestEmbeddingContract` fails when an embedded run stops matching the
  document.
### Security
- **The goal loop can no longer discard uncommitted work that predates it.**
  Its first production run undid a one-line change of its own with
  `git checkout -- <14 files>`, reverting every one of them to HEAD and
  destroying uncommitted work it did not make (#250). The loop still runs on a
  dirty tree — no clean tree is required — but it now records what was
  uncommitted when it started, and for the whole run a guard refuses any git
  checkout/restore/reset --hard/clean/stash/rm -f that could reach those paths,
  and any add -A/./-u, commit -a or add of them that would sweep them into the
  loop's commits, in every permission mode and in sub-agents. They are left
  exactly as they were, uncommitted. `--loop-dirty commit|refuse` (`/goal run
  [N] commit|refuse`) opt into committing them to the goal branch first, or
  not starting.
- **Plan mode is no longer bypassed by allow rules.** `permission.Check`
  consulted allow rules before a tool's intrinsic decision, which is where plan
  mode's read-only refusal lives, so with `Bash(git:*)` or `Edit` allowed, plan
  mode ran the command or wrote the file. A tool that refuses in plan mode is
  now refused whatever the allow list says. One consequence: MCP tools, which
  refuse in plan mode, are refused even when their server is allow-listed.
  Upstream 2.1.136.
- **A project's `.mcp.json` servers start only in a trusted folder.** Both
  `./.mcp.json` and `./.klaudia/.mcp.json` arrive with the checkout, and every
  stdio server in them was started at launch and on each reload, so opening a
  cloned repository ran whatever command it named. In a folder that is not
  trusted (`klaudia --trust-project`, or `--trusted-project-config` from a
  launcher that wrote the file), those servers are not started and a warning
  names them; the global `~/.klaudia/.mcp.json` always applies. Trust is
  re-read on each reload. A `.mcp.json` that is not a regular file (a FIFO
  blocked startup forever) is now an error. Upstream 2.1.69/2.1.238/2.1.257.
- **A project's `.klaudia/config.toml` no longer raises its own privileges.**
  Every key in it used to overlay the user's config, so starting Klaudia in a
  cloned repository let that repository pick `bypassPermissions`, turn the host
  gate off, widen the sandbox, choose the Chrome binary, and point the
  OpenAI-compatible provider at its own server with `apiKeyEnv` naming any
  variable in the user's environment. In a folder not on
  `~/.klaudia/trusted-projects`, those keys are now ignored with a warning that
  names each one; preferences and deny rules still apply. `--trust-project`
  adds the folder, and `--create-config=local` trusts the folder it writes to.
  A launcher that renders the file itself, such as msp-agent's per-session
  config, passes `--trusted-project-config` to apply it for that run.
  An "always allow" saved from the TUI in an untrusted folder says it will not
  load until the folder is trusted. Upstream Claude Code closed the same class
  in 2.1.251/2.1.257.
- **Files that make code run later are asked about in auto-accept modes.**
  In `autonomous` and `acceptEdits`, writes to `.git/`, `.husky/`, `.klaudia/`,
  `.claude/`, `.devcontainer/`, `.vscode/`, `.mcp.json`, `.envrc`, `.npmrc`,
  `.yarnrc(.yml)`, `.pre-commit-config.yaml`, `bunfig.toml` and `.bazelrc` —
  hooks, server and task config, rc files — now prompt, including through a
  symlinked parent. Upstream 2.1.78/2.1.90/2.1.160.
- **Approval prompts show the whole command, with hidden characters escaped.**
  Commands were cut at 220 characters, so a dangerous tail could be padded out
  of view, and zero-width, bidi-override, control and look-alike space
  characters reached the terminal as-is (a raw ESC or CR can redraw the
  line). The full command is shown with those as `\u{XXXX}`. Upstream
  2.1.211/2.1.223.
- **Klaudia's own git lookups no longer run programs a repository names.** The
  startup status, `/diff` and branch lookups ran `core.fsmonitor` and
  external diff/textconv drivers from the repository's config — present in
  archives, vendored and nested repositories. They now run through
  `internal/gitprobe`, which switches those off; `/commit` and the goal loop
  still run plain git. Clean/smudge filters are not covered. Upstream 2.1.265.
- **OS confinement hides your credentials.** The bubblewrap sandbox mounted
  the whole filesystem read-only, and the macOS profile restricted only
  writes, so a command under `sandbox.mode = "os"` could read `~/.ssh`,
  `~/.aws`, `~/.kube/config`, `~/.netrc` and every other credential the host
  gate protects. Those are now hidden — an empty tmpfs over each directory and
  `/dev/null` over each file under bubblewrap, `deny file-read*` under
  sandbox-exec — with `~/.ssh/known_hosts` and `~/.ssh/config` left visible.
  `sandbox.readCredentials = true` turns this off for sandboxes that need
  them. Upstream 2.1.187/2.1.224.
- **Grep and Glob do not search into a denied directory from above it.** A
  `Read` deny rule covered a search rooted inside the denied directory, but
  one rooted above it walked straight in: with `Read(~/secrets/**)` denied,
  Grep of `~` returned `~/secrets`' contents. The walk now leaves out any path
  a `Read` deny rule covers, and the result says how many were left out.
  (Dot-directories such as `~/.ssh` were already skipped by default.)
  Upstream 2.1.162.
- **File permission rules match paths.** Read, Glob and Grep sent no
  specifier, so `Read(~/.ssh/**)` denied nothing, and Edit/Write compared the
  path as the model wrote it against the rule as a string: `~` was not
  expanded, `Edit(src/**)` became a prefix that never matched, and `./x` and
  `/abs/x` disagreed. File rules are now path patterns — `~` is home, relative
  patterns are relative to the project, `**` crosses directories, a trailing
  `/` or a glob-free pattern covers everything beneath — checked against the
  path as written, its absolute form and its symlink target. A `Read` deny
  also covers Glob and Grep (by search root) and an `Edit` deny covers Write
  and NotebookEdit; allow rules are not widened. Rules written for the old
  string matching keep matching. Upstream 2.1.162/2.1.163/2.1.172/2.1.176/
  2.1.214/2.1.268/2.1.280.
- **Bash permission rules check every command in a line.** They matched only
  the first command's two-word prefix, so `Bash(git status:*)` approved
  `git status && curl … | sh`, and `ls && rm -rf x`, `sudo rm x` or `/bin/rm x`
  got past a `Bash(rm:*)` deny; a deny with flags (`rm -rf:*`) never matched at
  all. Every command is now checked — including `$(…)`, subshells, `bash -c`
  and `eval` scripts (after unwrapping, so `sudo bash -c` too), and the
  command behind `sudo`/`env`/`timeout`/`xargs` — by its full text, its short
  "program subcommand" form, and its base name. A deny applies if any command
  matches; an allow only if every one does. A line that cannot be fully read
  (parse error, a program that is an expansion, over 10,000 characters) is
  never approved by an allow rule. The TUI's session rules use the same
  check. Upstream fixed this class across 2.1.72–2.1.282.
- **Invisible characters are stripped from what Klaudia injects, and outside
  text is framed.** CLAUDE.md, recalled memory and `.klaudia/KNOWLEDGE.md`
  reached the system prompt as written, including Unicode format characters —
  zero-width spaces, bidi overrides, and the tag characters that spell out
  ASCII invisibly — so a file could carry instructions a person reviewing it
  would not see. Those are now removed (ZWJ and ZWNJ are kept for emoji and
  scripts that need them); MCP tool descriptions get the same treatment and a
  2,048-character cap, since every request carries them; and a sub-agent's
  result is introduced as its report, not the user's words. Upstream
  2.1.84/2.1.277/2.1.280/2.1.284.
- **Secrets resolved from `${VAR}` stay out of MCP connect errors.** A token put
  into a server's URL or arguments by `${VAR}` expansion appeared verbatim in
  the SDK's error when the connection failed — `Post "https://…?key=<token>"`
  — and that error goes to the terminal, the TUI and the model. Each resolved
  value is now replaced by its `${NAME}` in connect errors, including the
  server stderr appended to them. Upstream 2.1.234/2.1.268/2.1.274.
### Changed
- **Consecutive read-only tool calls in one turn now run concurrently.** When a
  turn emits several tool_use blocks and a run of them are read-only (Read,
  Grep, Glob, and the LSP query tools Diagnostics/Definition/References), their
  `Execute` calls are fired concurrently (bounded to 8 at a time) instead of
  strictly one after another, cutting the wall-time of a turn dominated by
  I/O-bound reads to roughly its slowest call rather than their sum. Ordering is
  unchanged: results are stitched back in tool_use order so each tool_result
  still pairs with its tool_use, and a mutating or side-effecting tool
  (Edit/Write/NotebookEdit, Bash, anything not on the read-only allowlist) still
  runs sequentially and in order — it ends the current read-only run, which is
  fully joined before it executes, so observable side effects never reorder. A
  single read-only call, or a read-only call not adjacent to another, takes the
  unchanged sequential path.

  Read-only is a deliberate allowlist (`readOnlyForConcurrency`), not the
  `allowAlways` permission decision, which is also returned by tools that mutate
  state or the loop's shared maps (TaskCreate, KillShell, Memory, TodoWrite,
  Agent, ToolSearch). All per-call gating — loop-breakers, host gate,
  permission/approval, validation, `BeforeEdit`, and every read/write of the
  shared `failures`/`errStreaks` maps — stays on the loop goroutine and in
  order; only `Execute` overlaps, so no shared state is touched concurrently
  (verified with `go test -race`).

### Added
- **Layered testing, and an e2e layer that needs no credential.** `e2e/` builds
  the real binary and runs it against `FakeModel`, a scripted stand-in for the
  Anthropic Messages API reached through `KLAUDIA_CUSTOM_ENDPOINT`. It covers
  what sat between the unit tests and the live `smoke.sh`: tool round-trips,
  `--continue` (asserting the prior transcript is in the request, not that a
  model recalled a word), plan mode, the host-change gate and its exit code 4,
  exit codes 2/3/1, background jobs dying with the session, and editor commands
  failing fast. The suite runs in about two seconds. `scripts/hermetic.sh` runs
  the unit tests against a poisoned `$HOME`; its first run found four skill tests
  that failed only for developers with their own skills installed (now fixed).
  `make check` runs static checks, unit tests under the race detector, the
  hermetic run and e2e; CI runs the same, split into three jobs. `make cover`
  merges unit and e2e coverage per package; new tests across tools, tui, cli,
  streamjson, lsp, browser and mcp take statement coverage from 72.0% to 88.7%.
  The suite was stress-tested with shuffled order, `GOMAXPROCS=1`, and 2x CPU
  oversubscription under `-race`. See `docs/testing.md`.
- **Claude Opus 5.5, Sonnet 5.5 and Fable 5.1 in the model table.** Their
  context windows (1M) and output caps (128k) are known, so compaction and
  the status bar's context gauge are right when you pick one with
  `--model claude-opus-5-5` (or `claude-sonnet-5-5`, `claude-fable-5-1`).
  Before, they fell back to the unknown-model defaults: no context window,
  and an 8,192-token output cap. The `opus`/`sonnet`/`fable` aliases stay
  on the 5.0 models for now: Opus 5.5 and Fable 5.1 reject, for newer
  accounts, a conversation whose earlier turns were edited, and Klaudia's
  microcompaction and repair edit earlier turns.
- **Project instructions follow the layouts people use.** Klaudia read three
  fixed files — `~/.claude/CLAUDE.md`, the git root's `CLAUDE.md` and the
  working directory's. A `CLAUDE.md` that is only `@AGENTS.md` reached the
  model as that literal line, a workspace-level `CLAUDE.md` above the checkout
  was never read, and neither were `AGENTS.md` or `.claude/rules`. Now every
  directory from `/` down to the working directory contributes its
  `CLAUDE.md` (or `AGENTS.md` when there is none) and `.claude/rules/*.md`,
  with `~/.claude/rules` alongside `~/.claude/CLAUDE.md`; `@path` imports are
  resolved (five deep, cycles cut, not inside code fences); HTML comments are
  removed; each file is labelled with its path and included once. Upstream
  2.1.72/2.1.277.
- **Bundled skills: `code-review`, `review-pr` and `feature-dev`.** They ship in
  the binary, so a fresh install can review a diff, run a multi-aspect pre-merge
  review through read-only `Explore` sub-agents, or walk a feature from
  exploration through design to a reviewed implementation. They take the shape
  of Claude Code's `code-review`, `pr-review-toolkit` and `feature-dev` plugins
  but are original prompts written for Klaudia's tools: the upstream repository
  is all-rights-reserved. A skill of the same name in any skills directory
  replaces the bundled one, and `/doctor` lists them with scope `bundled`. One
  consequence: the `Skill` tool is now always registered.
- **`Memory` can forget.** `operation=remove` deletes one session note from
  MEMORY.md — the one `query` matches, with the same all-terms matching as
  `search` — or, given `name`, a detail note under `.klaudia/memory/` and its
  pointer in the index. A query that matches more than one note removes
  nothing and lists the candidates; the timestamp on each note tells apart
  similar ones. Until now a wrong note stayed in the prompt of every later
  session unless someone edited the file by hand (#121).
- **The stream-json embedding channel answers control requests.** A client can
  send Claude Code's `interrupt`, `set_permission_mode`, `set_model` and
  `initialize` control requests; each gets a `control_response` (success or
  error) with the same `request_id`. They used to be decoded and dropped, so the
  only way to stop a turn was to kill the process. `interrupt` cancels the
  running turn, as Esc does in the TUI. `set_permission_mode` is held to the
  command line's rules (no `bypassPermissions` unless launched in it), and
  `initialize` refuses hooks and SDK MCP servers it cannot run rather than
  ignoring them. The `result` line now carries `session_id` and `duration_ms`.
  Stdin is also read without a bound on queued turns: a client that sent more
  than eight turns ahead while one waited on a `can_use_tool` answer used to
  deadlock the session.
- **Ctrl+Z, Ctrl+L and Ctrl+D do what a shell user expects.** Ctrl+Z suspends
  Klaudia to the shell (`fg` returns), Ctrl+L clears the screen while keeping
  scrollback, and Ctrl+D on an empty idle prompt quits. In raw mode none of them
  reached the terminal's own handling, so all three did nothing. `/help` lists
  them, along with Ctrl+W and Alt+←/→, which already worked but were unlisted.
- **`/help` is aligned and complete.** A usage too long for the column now gets
  its description on the next line instead of pushing it out of line, and the
  subcommands that existed without being listed are named: `/goal clear`,
  `/logs stop`, `/trust revoke all`, `/last ls`.
- **LSP `Hover`, `DocumentSymbols`, `Rename`, and `Implementation` tools, and
  source lines in code-intel results.** The language-server client only spoke
  diagnostics, definition, and references; it now also does `textDocument/hover`
  (type/signature/docs), `textDocument/documentSymbol` (a file's outline,
  flattening both the hierarchical `DocumentSymbol[]` and the flat
  `SymbolInformation[]` server replies), `textDocument/rename`, and
  `textDocument/implementation`. `Definition`, `References`, `Implementation`,
  `DocumentSymbols`, and `Hover` results now print the actual source line next
  to each `file:line:col`, so the model sees the code without a follow-up read.
  `Rename` returns a *preview* — the workspace edit grouped by file, with each
  replacement's location, source line, and new text — and does **not** apply the
  edits; the model reviews it and applies with `Edit`. Requests for a capability
  a server did not advertise at initialize now fail fast as "not supported"
  (recorded from the initialize result) rather than erroring or hanging, reusing
  the existing per-request timeout (see #109).
- **Edit/Write append fresh language-server diagnostics.** After a successful
  Edit or Write to a file whose language has a running server, the tool now
  fetches that file's diagnostics through the LSP pool and appends a compact
  `New diagnostics:` section (`path:line:col severity message`, capped) to its
  result, so the model sees errors it just introduced without a separate
  Diagnostics call — as Claude Code does. It is wired through a `Diagnostics`
  hook on `tools.Context` (backed by the pool, set by the CLI), not through the
  agent loop's dispatch. It never fails the edit: no server, an unsupported or
  disabled language, or a timeout appends nothing, and — matching the #109 fix
  intent — silence is never reported as a false "clean".
- **`/rewind [N]` drops the last N exchanges from the conversation** (default 1)
  so recent turns can be backed out without clearing everything. An *exchange* is
  a user prompt plus the assistant turn(s) and tool results it triggered, running
  up to the message before the next prompt; rewind cuts the history at a
  user-prompt boundary, which is by construction a message carrying no
  tool_result, so a dropped exchange is always removed whole and no tool_use is
  ever left without its tool_result (the corruption `sanitizeMessages` would
  otherwise have to repair). A user-role message that carries a tool_result is
  never treated as a boundary. The persisted transcript is truncated in place to
  match, so a later `--resume` sees the same conversation as the screen; N beyond
  the exchanges present rewinds all of them and says so, and N≤0 or non-numeric is
  a no-op with an error.
- **Headless `klaudia doctor [--json]` and `klaudia config show [--origin]`.**
  Two subcommands that run and exit without starting a session, for scripts and
  CI. `klaudia doctor` prints the same environment diagnostics as the
  interactive `/doctor` (platform, auth, provider/model, context window,
  sandbox, MCP, LSP, skills); `--json` emits a `{checks, ok}` object, and the
  command exits non-zero when a *critical* check fails — defined narrowly as "no
  usable credential resolved", the one condition under which Klaudia cannot
  reach a model at all, so a missing sandbox binary or absent language server
  stays advisory. `klaudia config show` prints the effective merged config as
  `key = value` lines; `--origin` annotates each with the layer it came from
  (default / home / project / env). Origins are recovered by a parallel
  introspection pass that reads the same two files `config.Load` reads but keeps
  the layers separate and replays the merge precedence per field — `Load` itself
  is unchanged. The resolved API key is never printed: the `apiKey` line reports
  only that a key is set and whether it came inline from a file or from the
  named `apiKeyEnv` variable (an `env` origin), and the key text appears
  nowhere in the output.
- **Ctrl+G opens the current prompt draft in `$EDITOR`.** Composing a long or
  structured prompt in the input box is awkward without editor motions; Ctrl+G
  writes the current draft to a temp file, opens it in your editor via Bubble
  Tea v1's `tea.ExecProcess` (which suspends the TUI, hands the child the real
  terminal, and repaints on return), and reads the edited text back into the
  box. It works while idle, while queuing a follow-up mid-turn, and while
  answering a question in your own words. Editor resolution follows `/open`'s
  `$VISUAL` → `$EDITOR` order, then falls back to `vi`/`nano` on `PATH`; with
  none of those it prints a brief hint rather than failing. An editor that exits
  with an error leaves the original draft untouched, and the temp file is always
  removed.
- **Sub-agents defined in markdown.** Only the three built-in sub-agents
  existed, so agent definitions written for Claude Code — including those in
  its own plugins — could not be used. Files in `~/.claude/agents`,
  `~/.klaudia/agents`, `.claude/agents` and `.klaudia/agents` now define
  sub-agents (name, description, tools, model; the body is the system
  prompt), a later one replacing an earlier or a built-in of the same name.
  The frontmatter is read leniently, as those files are written with prose
  that is not YAML: all 15 agents in the upstream `claude-code` plugins
  parse. `subagent_type` matching ignores case and separators (`explore`,
  `general_purpose`), and an agent's Claude `model` is ignored on an
  OpenAI-compatible provider. Upstream 2.1.140 and the plugins directory.
- **`--safe-mode`.** Starts without anything the project supplies: its
  `.klaudia/config.toml`, the servers in its `.mcp.json` files, its skills,
  its `CLAUDE.md`, memory and knowledge. Your own config, global MCP servers
  and skills, and `~/.claude/CLAUDE.md` still load, and the model is told it
  is in safe mode. For opening a repository you have not reviewed, or
  starting despite a project config that breaks startup. Upstream
  2.1.169/2.1.248.
- **MCP `headers` and `alwaysLoad`.** An HTTP or SSE server behind a bearer
  token or an access proxy could not be used: there was nowhere to put the
  header. `"headers"` on a server are sent with every request, with `${VAR}`
  expanded as in `url`, so the secret stays out of the file. `"alwaysLoad":
  true` offers a server's tools to the model from the start instead of behind
  ToolSearch, for a server used in nearly every session. Upstream
  2.1.119/2.1.121.
- **`WorkspaceSymbol` LSP tool.** Finds a symbol by name across the project
  through the language server's `workspace/symbol` request, so the agent can go
  from a name to its declaration without grepping first — the gap alongside
  `Diagnostics`, `Definition` and `References` (upstream 2.1.162). Each match is
  listed as `name  kind  path:line:col`, with its container when the server
  gives one, capped at 100. A `file` picks the language; without one, every
  language whose project file sits at the workspace root is asked, and a server
  that fails is noted beside the others' matches rather than hiding them.
- **`sandbox.memoryMax` caps the memory of each Bash command.** Opt-in, e.g.
  `memoryMax = "4G"`. The limit covers the command's whole process tree, swap
  included, so a runaway build or test is OOM-killed alone instead of dragging
  the machine into swap. Container mode passes it as `--memory`; on Linux
  otherwise commands run in a transient `systemd-run --user --scope` with
  `MemoryMax`, after a startup probe confirms the scope's `memory.max` really
  holds the limit (where the memory controller is not delegated, systemd
  accepts the setting and enforces nothing). Where it cannot be enforced — no
  user session bus, no delegation, macOS — Klaudia warns and runs commands
  without a limit.
- **Reasoning effort and thinking settings.** `effort = "low" | "medium" | "high"
  | "xhigh" | "max"` in `.klaudia/config.toml`, `--effort` per run and `/effort`
  in the TUI set `output_config.effort`; `thinking = "adaptive" | "disabled"`
  sets the thinking parameter. Unset, Klaudia sends neither — the request is
  unchanged and each model runs its own defaults. The settings follow the
  session rather than the model, so they are adjusted to what the model is
  known to accept: effort is dropped on Haiku 4.5 and Sonnet 4.5, which reject
  it; a level the model lacks (`xhigh` before Opus 4.7, `max` on Opus 4.5) is
  lowered to its highest; and `disabled` is dropped where thinking cannot be
  turned off (Fable, Opus 5.5, Opus 5 at `xhigh`/`max`). OpenAI-compatible
  endpoints receive the effort as `reasoning_effort`, with `xhigh` and `max`
  sent as `high` — the one level every such server accepts. Sub-agents keep
  the model's defaults.
- **Fallback model (`fallbackModel`, `--fallback-model`).** An overloaded or
  unknown model used to end the turn. With a fallback configured, a request
  that fails as overloaded (529, 503, or an `overloaded_error` mid-stream) is
  retried once on the fallback, and the next request tries the primary again;
  a model the provider reports as not found is replaced by the fallback for the
  rest of the session. Nothing is retried once output has been shown, or after
  an interrupt. The switch is announced in the TUI and on stderr in `-p` runs,
  and it applies to sub-agents and compaction summaries, which share the
  provider. When the fallback has a smaller known output cap, `max_tokens` is
  lowered to fit.
- **Build information in `--version`, `/doctor`, the banner and transcripts.**
  `--version` was the constant `2.1.66-klaudia (Klaudia)`, so neither a bug
  report nor its author could tell which build produced it — or that the
  `klaudia` on `$PATH` was months older than the checkout. The first line is
  unchanged (it is the reference-compatible string, and the transcript
  `version` and MCP client version stay on it); a second line now names the
  build from `runtime/debug.ReadBuildInfo`: the commit, `+dirty` for
  uncommitted changes, the commit time and the Go toolchain. `/doctor` gains a
  `version` line, the startup banner a `build` field, and transcript entries a
  `klaudiaBuild` field. Release builds can stamp `internal/version.release` and
  `internal/version.commit` with `-ldflags -X`; a build with no VCS
  information says `dev`.
- **Input history survives a restart, and `Ctrl+R` searches it.** ↑ used to
  recall nothing after quitting or `--continue`: history lived in memory only.
  The last 200 prompts are now kept per project in the sessions dir
  (`~/.klaudia/sessions/<project>/prompt-history.ndjson`, not the project's
  `.klaudia/`, which is committed), and a resumed conversation's prompts are put
  back under ↑ as well. A `!` command is stored as the typed line, never its
  output; prompts carrying a paste chip or over 8 KiB are not written to disk.
  `Ctrl+R` opens a reverse incremental search (`Ctrl+R` again for older
  matches, `Enter` to edit the match, `Esc`/`Ctrl+G` to cancel).
- **Grep: context lines, a type filter, `head_limit`, and useful multiline
  results.** `-A`/`-B`/`-C` add lines around each match in content mode (context
  lines are marked with `-`, groups separated by `--`, as in grep); `type=go`,
  `type=ts` and ~20 other names filter by extension without a glob; `head_limit`
  shows the first N results and says how many there were. Multiline mode used to
  report only that a file matched, with an empty line; it now shows each match
  with the line it starts on. Grep and Glob now take a relative `path` from the
  working directory, like Read and Edit do.
- **User-level memory at `~/.klaudia/MEMORY.md`.** Memory was per-project only, so a
  fact about *you* (rather than a project) had to be re-taught in every checkout. A
  user-level memory file, read from `$KLAUDIA_CONFIG_DIR/MEMORY.md` (default
  `~/.klaudia/MEMORY.md`, the same base as your user config), is now loaded and injected
  into the system prompt alongside project memory. The recall section labels the two —
  **User memory** (global, across all projects) then **Project memory** (this project) —
  with the project block last so its notes refine the global ones. The file is optional:
  absent = no-op. This is read-only injection; the Memory tool still *writes* only to
  project memory (`.klaudia/MEMORY.md`), so adding a user-scoped write would be a
  follow-up.
- **Skill frontmatter parity: `allowed-tools`, `argument-hint`, `model`, and
  `$1`/`$2`/`$ARGUMENTS` substitution.** Skill frontmatter now accepts the
  Claude Code keys `allowed-tools` (the older `tools` key stays as an alias),
  `argument-hint`, and `model`. Skill bodies expand positional arguments (`$1`,
  `$2`, … `$N` — whitespace-split, empty when absent) alongside the existing
  `$ARGUMENTS`, matching Claude Code semantics; a body that references neither
  still gets non-empty arguments appended, as before. The skill's base
  directory is now surfaced: `$KLAUDIA_SKILL_DIR` (and `${KLAUDIA_SKILL_DIR}`)
  expands to it in the body, and a one-line preamble names it in the result so
  the model can reach files bundled beside the skill. `argument-hint` is
  surfaced in the Skill tool's description; `allowed-tools` and `model` are
  parsed, stored and documented but not yet enforced — there is no per-skill
  tool-restriction or model-switch hook, since a skill injects instructions
  into the current turn rather than opening a scoped sub-session.
- **Attention notifications when Klaudia needs you.** When a turn finishes or a
  permission/approval prompt is waiting, the TUI can get your attention through
  the terminal: the bell (`\a`), an OSC 9 desktop notification (iTerm2, kitty,
  WezTerm) and/or an OSC 777 notification (rxvt/urxvt and others). Configure it
  with `[tui] notify` — a comma-separated list of `bell`, `osc9` and `osc777`,
  or `all` / `off`; unset defaults to `bell`, the most portable and least
  intrusive choice. Klaudia enables terminal focus reporting (a DECSET that,
  unlike alt-screen or mouse capture, leaves inline scrollback and click-drag
  selection intact), so the notification fires only while the window is
  unfocused; a terminal that never reports focus is notified regardless. The
  escape sequences are emitted through the render path like the OSC 52 clipboard
  copy, keeping them ordered with the frame rather than racing it.
- **Lifecycle hooks: `PreToolUse`, `PostToolUse`, `UserPromptSubmit`, `Stop`.**
  A `[hooks]` config section maps each event to matcher groups of shell commands,
  modeled on Claude Code. Each hook receives a JSON payload on stdin
  (`hook_event_name` plus event fields: `tool_name`/`tool_input` for PreToolUse,
  those plus `tool_response` for PostToolUse, `prompt` for UserPromptSubmit) and
  steers the loop by exit code and stdout: exit 0 with
  `{"decision":"block","reason":…}` blocks the action, exit 2 blocks with stderr
  as the reason, any other non-zero is logged and ignored. PreToolUse can deny a
  tool (an error tool_result carries the reason and the tool never runs);
  PostToolUse appends feedback to the result; UserPromptSubmit can block the turn
  or add `additionalContext` to the prompt; a blocking Stop hook re-injects its
  reason and keeps the model working (bounded, so an always-blocking hook cannot
  loop forever). Per-hook `timeout` (default 60s) is enforced by killing the
  hook's process group. Matchers apply to the tool name for the two tool events
  (empty/`*` match all; otherwise an anchored regexp, falling back to exact
  match).

  Config shape:

  ```toml
  [[hooks.PreToolUse]]
  matcher = "Bash"
  [[hooks.PreToolUse.hooks]]
  type = "command"
  command = "my-guard"
  timeout = 30
  ```

  **Limitation — project hooks are ignored.** Hooks run arbitrary commands, so a
  checked-in project `.klaudia/config.toml` would be a code-execution vector for
  anyone who can commit to the repo (the escalation the repo already gates for
  project-supplied executable config). Only user-level (`~/.klaudia`) hooks are
  honored; project-level hooks are dropped at load time. A future project-trust
  prompt could relax this.
- **MCP `tools/list_changed`, prompts as slash commands, and remote OAuth.** Three
  related MCP enhancements (#159):
  - **`tools/list_changed` (fully implemented).** A connected server that announces
    its tool list changed now has that list re-fetched and both tool registries
    rebuilt live — a tool that appears or disappears mid-session is usable (or gone)
    without a config edit or restart. The Manager installs the notification handler
    on every session it brings up, so reconnects and reload-added servers stay
    covered; the rebuild shares the reload path and is serialised against it.
  - **Prompts as slash commands (fully implemented).** `prompts/list` and
    `prompts/get` are surfaced: each server prompt becomes a `/mcp__<server>__<prompt>`
    command in the TUI (mirroring MCP tool namespacing), whose invocation fetches the
    prompt and submits its rendered text as the turn. Arguments accept `key=value`
    tokens, and bare text fills the prompt's first required argument. Prompts are
    enumerated at startup; re-registering prompts added by a *later* config reload as
    slash commands is the one deferred piece here (the prompt stays reachable through
    the MCP layer meanwhile).
  - **OAuth 2.1 for remote (HTTP/SSE) servers.** A new `oauth` block on a server
    config. The **`client_credentials`** grant is implemented end to end: Klaudia
    exchanges a client id + secret (secret named via `clientSecretEnv`, never written
    in the file) for an access token and refreshes it transparently. The
    **`authorization_code`** grant ships as a foundation: token *storage and refresh*
    work (a token is loaded, used, and re-persisted with 0600 perms as it refreshes),
    but the interactive acquisition step — opening a browser and catching the redirect
    — is **follow-up**; until then an `authorization_code` server with no stored token
    connects unauthenticated and receives the server's 401. Config is validated at
    connect and per server, so one bad OAuth block does not abort startup.
- **`--system-prompt`, `--append-system-prompt`, `--mcp-config`, `--add-dir` CLI
  flags** (Claude Code parity). `--system-prompt <text>` replaces the default
  system prompt entirely; `--append-system-prompt <text>` appends to whatever
  prompt is otherwise used (the default, or a `--system-prompt` replacement), so
  the two compose. `--mcp-config <path-or-json>` (repeatable) loads additional
  MCP servers from a file path or inline JSON in the `.mcp.json` shape and merges
  them over the configured servers — a CLI value wins a name clash. Those servers
  connect through the same path as project `.mcp.json` servers and their tools
  keep the `mcp__<server>__<tool>` names, so they are gated for trust identically
  (allowed only under an enforcing trust posture, asked interactively, refused
  where there is nobody to ask): a CLI flag buys no extra trust. `--add-dir <dir>`
  (repeatable) adds directories the agent may operate in beyond the working
  directory; they become project roots for the host gate in every mode, and seed
  the interactive session so `/add-dir` extends rather than replaces them. All
  four are optional and backward compatible.
- **Claude Code credentials on Linux, `klaudia login`, and `ANTHROPIC_BASE_URL`.**
  Three additions to the native Anthropic provider's auth.

  Credential resolution now runs, first hit wins: `ANTHROPIC_API_KEY`
  (`x-api-key`) → `ANTHROPIC_AUTH_TOKEN` (Bearer) → the `klaudia login` store
  (`~/.klaudia/credentials.json`) → a borrowed Claude Code session. The last of
  these was previously the macOS Keychain only; on Linux (and other non-darwin
  platforms) Klaudia now also reads `~/.claude/.credentials.json`, so a user
  already signed in to Claude Code need not re-enter a key. That file is parsed
  defensively (missing file/keys/shapes tolerated): an OAuth access token under
  `claudeAiOauth.accessToken` is used as a Bearer token — the header the SDK
  already uses for a Keychain OAuth session — and is refreshed and written back
  (0600, preserving unknown fields) when expired with a refresh token present;
  a top-level `apiKey` is used as `x-api-key` when no OAuth token is present.

  `klaudia login` stores an Anthropic API key to `~/.klaudia/credentials.json`
  (0600), which the resolution chain reads as a fallback below the env vars.
  It prompts without echo on a terminal, reads a piped line otherwise, and
  takes `--api-key <k>` for scripts. A full interactive OAuth device flow is not
  implemented (follow-up); Klaudia still reuses an existing Claude Code OAuth
  session automatically.

  `ANTHROPIC_BASE_URL` is now honoured for the Anthropic API base URL. Precedence:
  explicit config `baseURL` wins, then `KLAUDIA_CUSTOM_ENDPOINT`
  (`--custom-endpoint`), then `ANTHROPIC_BASE_URL`, then the production API.
- **`/compact <focus>` and `/summary [edit]`.** `/compact` now takes optional
  free text after the command — `/compact the auth refactor` — that is threaded
  into the summary request so the model keeps detail about what you named, even
  at the cost of brevity elsewhere; a plain `/compact` is byte-for-byte the old
  prompt, and autocompact (which has no user focus) is unchanged. `/summary`
  shows the current session's last compaction summary — the in-memory one from
  this session, or the persisted one that seeds `--resume` — and `/summary edit`
  opens it in `$EDITOR` (via the same temp-file round-trip as `/open`); saving
  and quitting replaces the stored summary with the edited text, so a later
  `--resume` starts from your correction. Editing revises the persisted summary
  that seeds resume, not the message already carried in the live context.
- **`@file` includes the file, and images attach as images.** An `@path` in the
  prompt is expanded at submit time: a text file is inlined behind a
  `===== path =====` delimiter, so the model reads the file instead of being
  told to go and Read it. A line range trims what is inlined —
  `@path:START-END` (1-based, inclusive) or `@path:LINE` for one line, the
  colon form stack traces already print. An `@path` that points at an image
  (png / jpeg / gif / webp) is attached as a base64 image content block on the
  user message rather than dumped as bytes; paste or type the path to attach a
  pasted screenshot. The on-screen prompt keeps the short `@path` the user
  typed (as paste chips do). Bounds: a whole-file text reference over 256 KiB,
  an image over 5 MiB, and a non-image binary are each refused with a short
  note in the terminal (never sent to the model) rather than swamping the
  request. Works on the first submit and on a message queued while Klaudia is
  working; a mid-turn steering interjection stays text-only, and true
  clipboard-image paste (image bytes straight off the system clipboard) is a
  follow-up.
- **Cost tracking: a per-model price table, `total_cost_usd`, a status-bar cost
  segment, `/stats` cost breakdown, and `--max-budget-usd`.** A new
  `internal/api/pricing.go` holds first-party Anthropic Messages API list prices
  (USD per 1M tokens: input, output, cache-read, cache-write) for the current
  Claude lineup — opus/sonnet/haiku/fable — plus a `CostUSD(model, usage)` helper
  that resolves CLI aliases the same way `MaxOutputTokens`/`ContextWindow` do. A
  model with no listed rate (an OpenAI-compatible endpoint, or one added since)
  is reported unknown and costs 0, so cost is never fabricated and a budget stop
  cannot fire on it.

  Rates are list prices captured **as of 2026-06-24** and **will drift** — update
  the table when Anthropic reprices or ships a model. Cache columns follow
  Anthropic's standard multipliers of the input rate (read 0.10×, write 1.25× for
  the 5-minute TTL), stored explicitly for auditability.

  The cumulative cost is computed once in the agent loop from the token totals it
  already tracks and surfaced as `total_cost_usd` in both the headless JSON result
  (previously a hardcoded `0` placeholder) and the stream-json result line. The
  TUI shows a compact running cost (e.g. `$0.0123`) in the status bar and a cost
  breakdown under `/stats`. `--max-budget-usd <n>` stops the run gracefully at the
  next turn boundary once cumulative cost reaches the limit — the same shape as
  `--max-turns`, with stop reason `max_budget`, a new exit code (`5`), and an
  explanatory turn note. Budget is enforced per `loop.Run` (main headless/TUI run
  and each `--loop` iteration); sub-agent spend is not folded into the parent
  budget.
- **Background sub-agents, an `/agents` status view, and worktree isolation for
  writers.** The Agent tool takes `background: true`: it launches the sub-agent,
  returns a handle immediately, and lets the parent turn continue instead of
  blocking until the child finishes. A finished child's result is delivered back
  on a later turn (a registry polled at the loop's two safe injection points, the
  same points steering uses), so a long investigation runs alongside the main
  thread rather than stalling it. Synchronous spawning stays the default, so
  nothing regresses.

  `/agents` now lists the launched background sub-agents beneath the available
  types — id, type, status (running/succeeded/failed), elapsed, and the current
  tool a running one is on — so a background launch is no longer a closed box.

  A background *writer* (a type that can call Write/Edit/Bash, i.e.
  general-purpose or any explicit mutating toolset) runs in its own detached-HEAD
  git worktree, created on launch and removed on completion, so concurrent
  writers cannot corrupt each other's tree; read-only agents (Explore, Plan)
  share the parent tree. If a worktree cannot be created — e.g. the project is not
  a git repository — the writer fails rather than silently running in the shared
  tree.

  Shared state (the background registry) is mutex-guarded and verified with
  `go test -race`. **Follow-up:** background delivery is wired for the interactive
  TUI path only — a headless single-shot run returns before a background child
  finishes, so it is not collected there; and the worktree lifecycle covers the
  common case (create/run/remove) but does not yet surface per-agent cancellation
  or worktree reclamation after a crash beyond a best-effort pre-clean.
- **Session management: titles, `/resume` picker, `/rename`, `klaudia sessions
  ls|rm`, and startup retention.** Sessions were resumable only by remembering a
  UUID and passing `--resume`; there was no way to see what was stored, name a
  session, or clear old ones, so the sessions directory grew without bound and a
  UUID told you nothing about which conversation it was.

  Each session now derives a one-line title from its first prompt, persisted in a
  `<id>.meta.json` sidecar beside the transcript (the transcript itself stays an
  append-only log, so mutable metadata lives next to it, as the compaction
  summary already does). Titles are backfilled lazily the first time a session is
  listed. `klaudia sessions ls` prints id, age, project and title (`--json` for
  the machine-readable form); `klaudia sessions rm <id>` deletes a session's
  transcript, summary and metadata. In the TUI, `/resume` opens the standard
  numbered picker over recent sessions (title + age) and switches to the chosen
  one in place — reusing a swappable recorder so subsequent turns append to the
  resumed transcript — and `/rename <title>` sets the current session's title.

  Retention prunes stale sessions once at startup, bounded by a new `[sessions]`
  config section: `retentionDays` (default 30) and `retentionMax` (default 100),
  each independent, with `-1` to disable. The active session is never pruned and
  never counts against the cap, and every delete — retention or `sessions rm` —
  is guarded to only remove files sitting directly inside the sessions store.
- **`extraHeadersEnv` for OpenAI-compatible providers.** A config map of HTTP header
  name → environment-variable NAME (never a value in the file, mirroring `apiKeyEnv`),
  applied to every request alongside `Authorization`. `provider = "openai"` is now valid
  with no `apiKey`/`apiKeyEnv` when `extraHeadersEnv` is set, so an endpoint gated only by
  non-bearer headers — e.g. a Cloudflare Access service token
  (`CF-Access-Client-Id`/`CF-Access-Client-Secret`) — is reachable; a referenced env var
  that is unset is a startup error naming the variable (not its value). Both request paths
  (`streamAttempt` and `ListModels`) authenticate through one `setAuth` helper.
- **`.mcp.json` resolves `${VAR}` and `${VAR:-default}`.** `command`, `args`,
  `env` values and `url` may reference Klaudia's environment with the syntax
  the reference MCP clients accept. The README used to say, deliberately, that
  there was no expansion and a value was used exactly as written. That held
  until a config written for the reference client arrived: its stdio server was
  spawned with `MSP_LOKI_URL='${MSP_LOKI_URL:-http://loki:3100}'` — the literal
  — connected to nothing, and every tool on it failed with the server's own
  generic error. Nothing in the output pointed at the config, because from
  Klaudia's side nothing had gone wrong.

  References are resolved at connect time, not at load, so the stored config
  stays as written and a reload can still tell an unchanged file from a changed
  one. A variable that is unset with no default is an error naming the server,
  the field and the variable, and it fails only that server — an empty string,
  which is what `sh` would substitute, would vanish into the subprocess and
  come back as the same unexplained failure this is fixing. A bare `$VAR` is
  left alone, as the reference clients leave it, so a value that merely
  contains a dollar sign is not rewritten.
- **An editor can drive Klaudia directly: the Agent Client Protocol v1, over
  stdio (`--input-format acp`).** Zed, Neovim's `acp.nvim` and the JetBrains
  plugin all speak it, so the editor keeps its own UI for messages, tool calls,
  diffs and permission prompts while Klaudia does the work. The arithmetic is
  the argument: the stream-json channel needs an adapter written once per
  editor, where ACP support is written once per editor *for every agent*, and
  the editors already shipped theirs.

  Served: `initialize`, `session/new`, `session/load`, `session/list`,
  `session/close`, `session/prompt`, `session/cancel`, `session/set_mode`, and
  `session/update` notifications for assistant text, thoughts, tool calls and
  their outcomes, plans, diffs, token usage and the available commands. The
  three permission modes are exposed as ACP session modes, so the editor's mode
  picker is Klaudia's `/mode` — per session, because a client with three threads
  open has three conversations and one process-wide mode cannot be right for all
  of them.

  `fs/read_text_file` is used when the client offers it, and that is a
  correctness fix rather than politeness: the editor hands back the user's
  *unsaved buffer*, and reading disk while someone looks at unsaved edits is how
  a model ends up reasoning about text that is no longer there.

  Declined, all deliberately:

  - **`fs/write_text_file`.** Reading through the client is safe; writing is
    not. Klaudia's `Edit` matches against what it read and `Write` is what
    `/undo` checkpoints — both of which assume the file on disk is the one they
    touched.
  - **`terminal/*`.** Routing `Bash` through the editor's terminal hands over
    execution, and with it the sandbox, the trust gate, job control and output
    clamping. A nicer widget is not worth any of those.
  - **`session/delete`.** Not advertised. A transcript is the user's record of
    what an agent did on their machine; an editor's "close tab" should not be
    able to erase it.
  - **Image and audio prompt blocks.** The loop takes a string prompt, so there
    is nowhere for the bytes to go, and claiming the capability would have the
    editor send a screenshot that Klaudia silently discarded. `embeddedContext`
    *is* claimed — a resource block carrying text is inlined, which is the whole
    point of pasting the open buffer in.
  - **MCP servers from the client.** Klaudia connects the ones in `.mcp.json`
    itself.

  Decisions worth recording:

  - **v1, not v2.** v2 is published as a Draft and changes real things —
    `tool_call` folded into an upsert-only update, a turn-lifecycle
    `state_update`, agent-owned terminals — but v1 is what every shipping client
    negotiates today. The version is agreed at `initialize`, so v2 is additive
    later: answer 2 to a client asking for 2.
  - **ACP session ids are Klaudia transcript ids.** Not a second numbering with
    a mapping table beside it, which is how `session/load` ends up loading
    something adjacent to what the user picked. `--resume`/`--continue` reaches
    ACP too: the editor's first thread *is* the session the CLI resolved, and
    keeps appending to its transcript.
  - **One transcript per session, not per process.** ACP is the first frontend
    with more than one conversation at a time, and a shared recorder had two
    threads appending to one file — so `session/load` replayed an interleaving
    that never happened.
  - **One prompt at a time, across all sessions.** A client may open several
    threads, but this process has one tool registry, one job store, one executor
    and one sandbox, so two concurrent turns would interleave. A prompt arriving
    while another runs waits, and a cancel while it waits is still honoured.
  - **`TodoWrite` is a plan update, not a tool call.** Clients render plans as a
    checklist, which is the entire point of the tool; left as a tool call the
    user sees a raw JSON array. `Edit` and `Write` carry an ACP diff, and `Edit`
    sends its own `old_string`/`new_string` rather than a reconstructed file —
    reconstructing means reimplementing the replacement rules here, and a
    preview that computes the change differently from the tool is worse than a
    narrower one, because this is the text the permission prompt asks you to
    approve.
  - **Token usage is sent per turn, not per inner call.** Klaudia's `usage`
    event is a delta; ACP's `usage_update` is "resident in the context, out of
    this many". Forwarding deltas showed a session at 300% of its window.
  - **A host-gate refusal maps to `failed`.** v1 has no fourth status, and
    "completed" would claim the tool ran. The refusal's own text goes out as the
    call's content so the user can read what actually happened.
- **Sub-agents that can write get a git checkout of their own.** Two children
  editing one working tree is not a race in the usual sense: nothing is
  corrupted and nothing errors — one writes a file, another reads a half-written
  version and reasons about it, a third rewrites the first one's edit, and every
  step *succeeds*. The alternative was running children one at a time, which is
  the thing concurrency was for. So a sub-agent holding `Write`, `Edit`,
  `NotebookEdit` or `Bash` now runs in a `git worktree` under
  `~/.klaudia/worktrees/<project>/<agent-type>-<timestamp>/`, and `Explore` and
  `Plan` keep sharing the tree — they hold Read, Glob and Grep, so there is
  nothing to isolate them from, and their whole output is paths that a checkout
  would make wrong.

  Not under the project root, because the parent's own Glob and Grep walk it:
  the model would find two of every file and read a copy of the code it is
  editing. And under `~/.klaudia` rather than `TMPDIR` because the trust
  classifier reads a bare `$HOME` path as the project zone, where a temp path
  earns a host prompt per sub-agent.

  **The checkout holds your uncommitted work, not `HEAD`.** `git worktree add
  <dir> HEAD` would hand the child the last commit — the one state nobody asked
  about — and a child told to "test the function I just wrote" would report,
  convincingly, that there is no such function. It is seeded with the tracked
  delta and the untracked non-ignored files, committed on the detached HEAD as
  the baseline the child's work is measured against, with hooks and signing
  skipped: a repository's pre-commit hook is aimed at the user's commits, and
  running their linter every time a sub-agent spawns is a side effect nobody
  asked for. Ignored files are *not* copied — build output is what makes a copy
  expensive, and a tree that builds is a different promise from a tree that
  matches. The cost is real: a child that needs an install step before it can
  test will pay for it, or fail, which is what `[subagents] worktree = false` is
  for.

  Coming back, the child's work is diffed against the baseline and applied with
  `git apply`, which touches files and not the index — same reason `/undo`
  writes loose objects instead of stashing, and your staging area is exactly as
  you left it. `git apply` is all-or-nothing, which is the behaviour worth
  having (a patch that no longer fits is one whose file moved underneath us), so
  it is retried file by file and what did not fit is named to both you and the
  model. The model is told because it asked a child to change files, and a
  silent conflict has it carry on describing work that is not there. A checkout
  survives any conflict — it holds the only copy of that version — and a failed
  or interrupted child keeps its checkout with the path in the error, because
  half a change applied to your tree is the outcome isolation exists to prevent.
  Stale checkouts are pruned after seven days.

  Adoption is serialised per repository, and that lock was measured rather than
  assumed: without it, two concurrent adoptions of one file reported success
  twice and silently kept one version — the precise failure the feature exists
  to remove.
- **Every frontend runs a turn through one contract (`agent.Turn`).** Each one
  used to declare its own `RunFunc` — the TUI's took nine positional parameters,
  stream-json's five — and the CLI wrote a closure per mode that filled in
  `Options` by hand. Two consequences, both live bugs rather than hypotheticals:
  a frontend that *omitted* a capability was indistinguishable from one that did
  not want it (which is how `AskUserQuestion` and `ExitPlanMode` came to be dead
  over stream-json), and adding a parameter meant touching every frontend, so
  nobody did and the gap between the TUI and the embedding channels only
  widened. A capability is now a field a frontend sets, the copying happens once
  in `Turn.Apply`, and a new field left zero behaves exactly as it did before it
  existed. `HostChange.Fields` is shared for the same reason: stream-json's
  `host_change` payload and ACP's `_meta` have to agree about which fields are
  omitted, and the omissions are the informative part.
- **MCP elicitation, so a server can ask the user instead of failing.** The
  client now speaks protocol **2026-07-28** (go-sdk v1.6.1 → v1.8.0) and
  advertises the elicitation capability, which is how a server obtains the one
  thing it cannot derive — a token, a branch name, a confirmation before
  something destructive. Without it the server's only options were a degraded
  path or an error that said nothing about what it actually wanted.

  The question goes to the same prompt `AskUserQuestion` uses and is labelled
  with the server that asked; "an MCP server wants your GitHub token" is not
  answerable without knowing which one. Each schema field is one question —
  booleans and enums become choices, anything else is typed — and `required`
  fields are asked first in the order the schema lists them. That part of the
  order is real, because `required` is a JSON array; the optional fields are
  sorted, because object key order does not survive decoding and the schema
  reaches us already decoded.

  Answers are free text, so a declared type is a coercion that can fail. A
  failure **cancels** rather than guessing: `cancel` is recoverable — the server
  may ask again — where an invented number is not. An answer outside a declared
  enum cancels for the same reason. `3.0` is accepted for an integer; `3.5` is
  not.

  Two things are deliberately not supported:

  - **URL-mode elicitation is declined, and not advertised.** The spec's other
    mode hands the client a link to open out of band and expects an immediate
    "accept". A terminal cannot open a browser without reaching outside the
    project, and answering yes for a link that was only printed into a
    scrollback nobody is watching is worse for the server than being told no —
    it waits on a flow that never started. Declaring form-only lets it choose
    its own fallback. Nothing on the multi round-trip path enforces the declared
    mode, so the check is made again on arrival.
  - **Headless runs advertise no elicitation at all.** There is nobody to ask, so
    a capability would only earn a decline on every request. A server that sees
    nothing takes its non-interactive path instead.

  Two findings from the SDK worth recording, because neither is guessable from
  the spec text. On 2026-07-28 a server **cannot** send `elicitation/create`
  while serving a request: it returns an `InputRequests` map in the tool result
  and the client fulfils it and retries (multi round-trip requests, SEP-2322).
  The SDK's client middleware already runs that loop, so `mcpTool.Execute` needed
  no changes — but it fulfils the requests of one round **concurrently**, and the
  frontend has a single question slot, so two prompts in flight would overwrite
  each other's options and the user would answer the wrong question. Elicitations
  are serialised.

  Capabilities are now set explicitly, which also drops the SDK's default
  `roots: {listChanged: true}`. Klaudia registers no root, so that advertised a
  feature whose only possible answer was an empty list; roots is deprecated as of
  this protocol version anyway.
- **`/doctor` warns about the deprecated HTTP+SSE transport.** `type:"sse"` still
  works and is still supported, but the spec has deprecated it and servers drop
  it on their own schedule. The symptom when one does is a connect error with no
  hint that the fix is one word in `.mcp.json`, so the check names the servers
  still on it.
- **Lifecycle hooks: four events, and a confirmation for the ones that arrive
  with a clone.** The commonest request a harness gets is "run *my* thing at
  *that* moment" — format after a write, refuse edits to generated files, paste
  the ticket into every prompt — and every one of them is a feature Klaudia
  would otherwise have to grow an opinion about. A hook is a line of shell and
  the agent stays out of it.

  Four events: `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`.
  Claude Code has grown past thirty, and the long tail of them exists to serve
  one workflow each; every event is a promise about when the loop calls out,
  which is a promise about the loop's shape. `Stop`/`SubagentStop` — "the model
  thinks it is done, make it keep going" — were left out deliberately: a hook
  that can deny completion can hang a session, and nothing in a shell command
  knows better than the loop whether the work is finished.

  **A hook is not a security boundary, and the ordering is what keeps it from
  pretending to be one.** `PreToolUse` runs *after* the host gate and the
  permission check have both allowed a call, so a hook can narrow what Klaudia
  will do and can never widen it. The alternative — a hook consulted as part of
  the decision — is a config file that can grant permission, which is a way to
  switch the gate off by writing a file.

  Which matters because a project's `.klaudia/config.toml` arrives with a clone.
  Cloning a repo and starting Klaudia in it would otherwise be enough to run
  whatever that file says, before the user has read a line of it: the same shape
  that made editors stop sourcing project-local config, and the reason `.vscode`
  tasks and `direnv` both grew an approval step. So hooks from `~/.klaudia` are
  the user's own and run unprompted — asking someone to confirm their own
  settings file is how people learn to stop reading prompts — and hooks from the
  repository are confirmed once, through the existing host-change card, with
  every command shown. The approval is recorded in `~/.klaudia/hooks.json` keyed
  by project directory.

  The unit of approval is the whole set, fingerprinted over each hook's event,
  matcher, command and timeout, in order. A set rather than a file, because a
  prompt has to show what is being agreed to and "these four commands" is
  something a person can read where "hooks: yes" is not. And a fingerprint
  rather than a flag, because the dangerous repo is not the one that asks on day
  one — it is the one that asks for something harmless and changes it in a later
  pull. Reverting to a formerly-approved set re-asks too: that is precisely the
  move available to an attacker who got a yes once. `config.merge` deliberately
  does *not* merge the two files' hook lists, because a merged slice is one in
  which a repository's command is indistinguishable from the user's.

  Details that were decided rather than inherited:

  - **Exit 2 is a verdict; any other non-zero status is a malfunction.** A hook
    that exits 127 because the formatter is not installed has made no judgement
    about the tool call, and reporting that to the model as a refusal would have
    it reason about, and route around, a broken environment. Malfunctions and
    timeouts go to the user as notices and never to the model — a broken script
    is the operator's to fix. The convention itself is Claude Code's, and so are
    the stdin field names (`tool_name`, `tool_input`, `tool_response`), because
    a hook script is the most portable artefact in the ecosystem and a
    gratuitously different payload means editing every existing one for nothing.
  - **Hook stdout is prompt, so it is bounded.** 8 KB, with the truncation
    reported. `command = "git log"` in a long-lived repo is megabytes pasted in
    front of every user message.
  - **30 seconds by default, not Claude Code's minute.** `PreToolUse` is in the
    critical path of every single tool call, so a hook that hangs is a session
    that looks frozen; the formatter and linter cases finish in under a second.
  - **Injected context is its own content block, placed before the prompt.**
    Separate, so a frontend replaying the transcript can tell what the user
    typed from what a hook added. Before, because context that follows an
    instruction reads as part of it — a hook that pastes a file listing after
    "delete the stale ones" has changed what the sentence means.
  - **`PostToolUse` output is appended to the result and labelled.** The model
    has to be able to tell the tool's own words from a hook's commentary: a
    formatter saying "reformatted 1 file" is not something `Write` printed. A
    refusal here cannot undo the call, but it does make the result arrive as an
    error with the reason attached, which is the difference between the model
    believing its write succeeded and knowing the linter rejected it.
  - **`SessionStart` fires once per session and cannot block.** Once per
    session, not per turn, and the claim lives on the runner because that is the
    only object with the session's lifetime — a `Loop` is per-process, but a
    sub-agent builds its own, which would have fired it again on the first
    `Agent` call. It cannot block because there is nothing left to block, and
    treating its exit 2 as a veto would make a config typo look like a broken
    install. Sub-agents run the two tool events and skip `UserPromptSubmit`:
    their prompt is the parent's instruction, not the user's.
  - **A bad entry is dropped and reported, not fatal and not silent.** A
    mistyped event name is the likeliest error by a distance, and
    `"SessionStarted"` quietly never firing is the worst outcome available. All
    the problems in a file are reported, not the first.
  - **Hooks are not sandboxed, whatever `[sandbox]` says.** That setting
    confines the model's commands. A hook is the user's own automation, and the
    things it exists to do — run a formatter, touch a git index, post a
    notification — are exactly what confinement forbids; one failing under
    `sandbox-exec` would fail invisibly from the config file that declared it.
  - **In a headless run, project hooks do not run.** The notice distinguishes
    that from a refusal, because nobody objecting is not the same as agreement.

  `/doctor` now lists the configured hooks in the order they will run and warns
  when a project set is still waiting on approval — the one state whose only
  symptom is silence. See `docs/hooks.md`.

- **The model is told which permission mode it is working under.** It was never
  told, and it is the one doing the work under the constraint. In plan mode it
  reasoned its way to an edit, called `Edit`, and was refused — a wasted turn,
  and worse, a plan built without knowing that running the build to check it had
  never been an option. Leaving plan mode was the mirror image: the user got a
  banner, the transcript recorded it, and the model carried on hedging about
  work it was now free to do.

  The active mode is now a system-prompt clause, rebuilt per turn, and a change
  is emitted as a `permission_mode` event for headless and stream-json
  frontends (the TUI already prints its own banner and shows the mode in the
  status bar). Per turn rather than once per run because approving a plan flips
  the mode mid-run. Doing it in the loop — the only place that holds the live
  mode function — covers the picker, `/mode`, `/plan`, plan approval and any
  future control request without any of them knowing about it.

  Two details. Autonomous adds no clause: it is what the base prompt already
  assumes, and restating the default every turn is tokens spent to change
  nothing. And the clause is a separate system block appended after the base
  prompt rather than spliced into it, so the long identical prefix stays
  byte-for-byte stable and a mode change invalidates nothing before the cache
  breakpoint.

- **AGENTS.md is read.** It is the cross-agent instruction standard — now a
  Linux Foundation project, present in several hundred thousand repositories and
  read by around twenty agents — and Klaudia read none of them. A user who had
  already written down their conventions for exactly this purpose got a model
  that had never seen them.

  Both files are now read, at three levels, generic first at each one:
  `~/.claude/CLAUDE.md`, `~/.klaudia/AGENTS.md`, then `AGENTS.md` and
  `CLAUDE.md` at the git root and again in the working directory. Both rather
  than one, because a repo may carry either or both with different content — the
  generic file for every agent and a Claude-specific refinement beside it — and
  reading one of them silently drops the other.

  Supporting both with `ln -s AGENTS.md CLAUDE.md` is the commonest arrangement
  in the wild, and a copy is the second commonest. Either would have sent the
  whole instruction block twice in every request, so identity is established
  both by resolved path (`EvalSymlinks` — `filepath.Abs` gives a symlink its own
  path) and by content hash, which is the only thing that catches the copy. The
  prompt's section header now names the files the instructions actually came
  from, so a model asked to write a convention down knows which file to put it
  in.

- **Read-only tool calls in the same batch run at the same time.** A turn's
  tool calls were dispatched strictly one after another, so the commonest shape
  in a turn — three or four `Read`s, or a `Glob` and two `Grep`s, issued
  together to orient — cost the *sum* of its round trips instead of the slowest
  one. Adjacent calls to tools that have opted in now execute concurrently, up
  to five at once.

  Opting in means implementing `tools.ConcurrencySafe`, and the marker asserts
  more than "does not write": that the tool never prompts the user, holds no
  process-wide resource assuming one caller, and does not depend on when it ran
  relative to its neighbours. `Read`, `Glob` and `Grep` are marked; everything
  else keeps the old behaviour, because adding a tool should not require
  thinking about parallelism and the cost of forgetting should be a slow batch
  rather than a race.

  Three properties are preserved rather than traded away:

  - **Order.** Only *adjacent* safe calls are grouped. A `Read`, an `Edit` and
    another `Read` stay in that order — the second `Read`'s result depends on
    the `Edit` having happened — so groups are maximal runs and nothing is ever
    lifted past a call it followed. Results are written by index, so the model
    sees them paired with the calls it made however the goroutines finished.
  - **One prompt at a time.** A call whose host-gate or permission check would
    ask is never grouped, whatever the tool reports: there is one `Approver` and
    one place to put a question. That is decided before anything starts, so
    `dispatch` does not need to know it is in a group.
  - **A readable transcript.** A group's events are delivered in call order.
    The call at the head streams live so the user sees movement immediately;
    later calls buffer until the ones before them finish. A group of one
    behaves exactly like the old serial path.

  The loop-breaker counters moved behind a mutex (`failureState`). The lock
  makes each operation atomic, not the breaker *decision* atomic with the
  update that follows it — serialising dispatch around a heuristic would undo
  the parallelism it protects. The consequence is bounded: a group can run one
  extra copy of a call that was about to be refused, and since only
  concurrency-safe tools are grouped, the extra copy is a read that reads
  again.

- **A turn's tool results are capped in aggregate, not just one by one.** The
  per-result cap cannot see this case: five results that each stop just under
  the 30 KB budget are 150 KB in a single user message, and a turn is not
  limited to five calls. Twenty well-behaved `Grep`s is most of a 200K window
  in one message, with nothing in the per-result path finding anything wrong —
  and parallel dispatch above makes that shape common rather than theoretical.

  One turn's combined tool-result text is now held to 200 KB. Shares are
  allotted by water-filling rather than an equal split: results already under
  their share keep every byte and what they do not use is redistributed to the
  large ones, because trimming a 200-byte result to an equal share of a budget
  it was never going to exhaust loses real content to no purpose. Image parts
  survive untouched — the model cannot recover vision content from a log file —
  and a result the per-result cap already spilled keeps pointing at that file
  rather than gaining a second notice naming a spill of the already-clamped
  copy. The cap runs after each call's events are emitted, so a local frontend
  still shows what the tool actually produced.

- **An elided tool result now leaves a handle, not a blank.** Microcompact
  replaced every old result with the same fixed string, so after a pass the
  conversation held several identical `[Old tool result elided to save
  context]` markers: the model could not tell which call each had been, and the
  content was gone for good. The only recoveries were re-running the tool —
  paying the latency and the tokens again, and hoping it was idempotent — or
  reasoning without it.

  The placeholder now carries the original size, its first non-empty line
  truncated to 120 bytes, and the path of a spill file holding the full text, so
  it reads `[Old tool result elided to save context; 70 bytes; began:
  ./internal/agent/loop.go:349: for _, tu := range toolUses {; full text:
  /…/elided-123.log]`. That is enough to recognise the call and recover it with
  a `Read`, for a few dozen tokens against the thousands a pass reclaims. The
  spill is injected as a `Spiller` function rather than imported, keeping the
  package dependency-free, and it is the same on-disk spill the per-result cap
  writes; with no spiller the placeholder simply promises no file.

  `Microcompact` runs in two passes for a reason: `compact()` is called at the
  top of every turn, and pricing the placeholders *before* writing anything lets
  the `MinTokensToSave` floor reject the pass without side effects. A single
  pass would have written a fresh set of spill files on every turn a
  conversation spent sitting just under that floor. The second pass spills and
  recomputes the exact saving with the real paths in place.

- **Every tool's output is capped, not just Bash's.** The clamp-and-spill
  machinery in `internal/tools/output.go` had exactly one caller — Bash — so a
  single Grep across a large repo, a verbose MCP server, or any tool added by
  someone who had not thought about the context window could put its entire
  output into the conversation. Measured on a synthetic Grep-shaped result: 980
  KB arrived at the model intact, roughly a quarter of a million tokens from one
  call.

  The agent loop now applies a backstop after collapsing a tool's results: the
  text is clamped head-and-tail to the same 30 KB budget Bash uses, the
  untruncated copy is written to `~/.klaudia/outputs/<tool>-*.log`, and a notice
  names the file so the model can read what was removed. The UI is unaffected —
  it already receives the full text out of band via `Result.Full`.

  This is a backstop, not a replacement. A tool that understands its own output
  clamps it better than a byte-level cut can: Bash keeps a tail because that is
  where the verdict is, and appends the exit annotation afterwards so it
  survives. Output already within budget passes through untouched, so
  self-clamping tools are never cut twice. Spill files are now named after the
  tool that produced them (previously all `bash-*.log`), which also meant
  sanitising MCP names — `CreateTemp` rejects a pattern containing a path
  separator outright.

- **MCP servers can be configured globally, in `~/.klaudia/.mcp.json`.** Only
  `./.mcp.json` and `./.klaudia/.mcp.json` were read, both relative to the
  project, so a server you want in *every* project had to be copied into every
  checkout. Worse, the message when none were configured — "Add them in
  `.mcp.json` or `.klaudia/.mcp.json`" — reads like one of those is a home-dir
  path, and installing to `~/.klaudia/.mcp.json` on that basis produces a file
  that is never read, no error, and no servers. Config and sessions live under
  `~/.klaudia`, and skills already load from `~/.klaudia/skills`, so the absence
  of a global MCP scope was the odd one out.

  Precedence is global → project `.mcp.json` → project `.klaudia/.mcp.json`,
  per server name, matching how `config.toml` overlays a global file with a
  project one. Parse errors now name the full path: three files share the base
  name `.mcp.json`, and "which one is broken" is the entire question when the
  answer is a file in another directory.

- **MCP config changes apply to the running session.** Any `.mcp.json` that
  applies to the project is watched, and an edit adds, drops or restarts servers
  in place — a server whose config didn't change keeps its session rather than
  being restarted along with the rest. Both the main registry and the one
  sub-agents draw from are rebuilt, along with the deferred-tool set, so a
  sub-agent spawned after a reload sees the same tools as its parent.

  The watch covers only the config files themselves, not `~/.klaudia` as a
  whole: that directory also holds `sessions/`, `jobs/` and `browser/`, which a
  live session writes to continuously, and watching it recursively would rebuild
  the MCP servers on every message typed. A config that no longer parses is left
  unapplied rather than applied empty — a half-typed file should not take
  working servers away — and both that and any server which fails to launch are
  now reported in the transcript.

- **A failed MCP reload says so.** Reload outcomes were silent, on the grounds
  that the watcher could not write to a live TUI without corrupting the render.
  That was not true: background jobs already report their exits across
  goroutines through the model's event channel, and reload notices go the same
  way. Silence was the expensive part — a typo in `.mcp.json` looked exactly
  like a clean reload, and the first sign of trouble was an unrelated-looking
  tool failure much later.

  A config that fails to parse reports the parse error *and* that the previously
  loaded servers are still running, because the natural reading of a config
  error is that MCP is now down, and the useful fact is the opposite. Servers
  that fail to launch are named, capped at three with a count of the rest, and
  point at `/mcp`. A reload that works stays silent: announcing every one would
  print a line each time an unrelated key in the file was saved.
- **`-c` for `--continue`, and shell completion in `--help`.** `-c` was an
  "unknown shorthand". `klaudia completion bash|zsh|fish|powershell` always
  worked, but cobra hides it on a command with no sub-commands; `--help` and the
  README now say it exists. `--continue`'s help no longer says "(default when
  available)", which read oddly on a flag you pass explicitly.
- **An "Environment variables" table in the README.** It lists all 16
  `KLAUDIA_*` and `ANTHROPIC_*` variables with their defaults; the README used to
  mention 8, so `KLAUDIA_MAX_RETRIES` — which the 429 error tells you to set —
  was documented nowhere. `cmd/klaudia/envdocs_test.go` fails when non-test code
  names such a variable the table lacks, or the table lists one no code reads.
  It collects every string literal that is exactly a variable name, not only
  `os.Getenv` arguments, because the browser package reads through its own
  `getenv`/`envBool` helpers.
- **(s)omething else on every permission prompt, and the prompt says Esc
  cancels the turn.** The redirect answer existed only on host-change asks; an
  ordinary tool ask offered yes / always / no. `s` now declines any ask as a
  redirect: the model is told the user is about to say what they want instead
  and to wait for it rather than route around the refusal, and what you type
  next lands before Klaudia's next step. Both prompts end with `(esc cancels
  turn)`, because Esc is not "no" — it ends the whole turn.

### Changed
- **The always-loaded toolset is trimmed: the Browser and Task tools are now
  deferred.** They join the MCP tools behind `ToolSearch` — withheld from every
  request's standing tool list and revealed on demand — instead of riding in
  eagerly. This removes eight tools (`BrowserSearch`, `BrowserFetch`,
  `BrowserNavigate`, `BrowserSnapshot`, `TaskCreate`, `TaskList`, `TaskGet`,
  `TaskUpdate`) from the request every turn carries, and its cached prompt
  prefix, on the many turns that never touch them. The Browser tools' own
  descriptions already tell the model to prefer the server-side
  `web_search`/`web_fetch` on Claude models, so they were fallbacks the request
  paid for on every turn regardless; the four-tool Task store is distinct from
  the `TodoWrite` planning checklist, which stays eager because nearly every
  non-trivial turn uses it. The tools stay registered and work the instant
  `ToolSearch` reveals them; a session with no MCP servers now trims its
  toolset too (previously `ToolSearch` was registered only when MCP tools were
  present). The common edit/read/bash/search core is untouched.

- **Auto-resume announces itself, and skips a stale session.** An interactive
  launch picks up the directory's most recent session, and the resume banner
  only rendered when there was a goal, a dirty tree or a dead job — so a
  week-old conversation on a clean tree came back with no sign, and its whole
  history was re-sent on the first turn. Auto-resume now always prints one
  line (`Resumed <id> · N messages · last active 3h ago · --new-session to
  start fresh`), and a session last active longer ago than
  `[session] autoResumeMaxAge` (default `24h`; `7d` style accepted; `0`
  disables it) is not auto-resumed: the launch starts fresh and names the old
  session and `--continue` / `-r <id>` to get it back. An explicit
  `--continue` or `-r` ignores the cutoff.

### Changed
- **`/diff` pages a long diff, in colour.** It used to print the whole diff,
  uncoloured, straight into scrollback. It now takes the same route as `/last`:
  a diff longer than two screens opens in `$PAGER` (falling back to `less`,
  then `more`), a shorter one prints inline. git is asked for
  `--color=always` unless `NO_COLOR` is set or the terminal has no colour, in
  which case it is asked for `--no-color` so a `color.ui = always` in git
  config does not override `NO_COLOR`. When `git diff` fails, the message is
  git's own rather than `exit status 128`.
- **Tab completes slash-command arguments.** Tab after `/<cmd> ` used to fall
  through to `@path` completion. It now offers what the command takes: `/theme`
  themes, `/mode` modes, `/logs`, `/restart` and `/stopjob` jobs (and the
  `/logs` flags after a `-`), `/last` result numbers and `list`, `/unpin` pinned
  files, and `/trust` subcommands and, after `revoke`, the live approval ids.
  `/model` completes from the list the last `/model` fetched and never fetches
  on Tab; before any fetch it says how to get one. One match is filled in with a
  space so the next Tab moves on; several are listed and extended to their
  common prefix, then cycled. Commands without a completer, and an argument
  typed as `@…`, keep `@path` completion. Each completer lives on its entry in
  the command table.
- **Pickers take arrow keys and a type-ahead filter, and `/model` lists every
  model.** `/model`, `/mode`, `/theme` and `/mcp` used to print numbered items
  and accept only the digits 1–9, so `/model` cut the provider's list to nine
  and an OpenAI-compatible endpoint serving dozens of models left most of them
  unreachable (`/model <id>` was the only way in). The list is now drawn in the
  live region with a highlighted row: `↑`/`↓` (and PgUp/PgDn, Home/End) move
  it, Enter picks it, typing filters the list word by word, Backspace edits the
  filter, and Esc cancels. The digits still pick directly while no filter is
  typed. A list taller than the terminal shows a scrolling window (at most 15
  rows) with the position in the footer.

### Removed
- **The per-command permission model is gone, not deprecated.** No allow/deny
  rules, no `--allowedTools`/`--disallowedTools`, no `/allow` or `/deny`, no
  `default`/`acceptEdits`/`dontAsk` modes, no "always" answer on a prompt, and
  no `[permissions] allow`/`deny` or `[trust]` config. `HostPolicy` goes with
  them: the gate has no observe or off posture, because `bypassPermissions`
  already means "check nothing" and says so.

  These were kept as a migration affordance when zones landed. Running both
  models at once turned out to be worse than either, in a way only visible in
  use: a rule in `.klaudia/config.toml` put the next session into observe,
  observe dropped its permission mode to `default`, and `default` asked before
  every edit and every command. So answering "always" — the thing offered to
  stop a prompt — was what guaranteed more prompts next session, in a project
  that would otherwise have started autonomous. Persistence was also gated on
  `.klaudia/` already existing, so a project acquired the behaviour the first
  time anything created that directory. Measured on a project where `/goal`
  wrote `.klaudia/GOAL.md`: the next "always" reached disk, and simulating the
  following startup gave `trust=observe permissionMode=default`.

  Two further dead ends went with it. `/trust upgrade` returned "Already
  enforcing" without looking at the permission mode, so a session that reached
  gate-enforcing with the mode left behind — `/plan off` left `default`, an
  approved `ExitPlanMode` left `acceptEdits`, neither touching the gate — had
  no signposted way out: the command the UI and docs pointed at reported
  success and changed nothing. Both now land on `autonomous`, and the gate has
  no posture to be out of step with.

  Approving an operation is the replacement, and it was already the documented
  one — `docs/trust.md` and `/trust` had both been claiming Klaudia no longer
  created rules while `tui.go` still did. Existing configs are not migrated:
  delete the `[permissions]` allow/deny entries and any `[trust]` section.
  `[permissions] mode` is still read and now takes
  `autonomous`/`plan`/`bypassPermissions`.

### Fixed
- **Flag mistakes exit 2, not 1.** `--new-session` with `--continue` or
  `--resume`, an unknown `--output-format`, and an unknown `--create-config`
  target were reported as run failures. The exit-code contract says an
  invocation mistake is a usage error (2), and nothing had run.
- **`--allowedTools`/`--disallowedTools` no longer switch the host guardrail to
  observe.** Starting in observe is the migration path for a *config* with
  per-command rules; the flags counted too, so `-p x --allowedTools Read
  --permission-mode autonomous` was refused as a usage error, and without
  `--permission-mode` the same flags quietly dropped the run from autonomous to
  default. Only config rules count now.
- **A language server that crashed stayed dead for the rest of the session.**
  The pool handed back the cached client without checking it; every later
  Diagnostics/Definition/References call for that language failed. A dead
  server is now reaped and replaced on the next call.
- **LSP diagnostics: a waiter leaked per cancelled or timed-out call, and a
  server that died mid-wait cost the full 10s.** The waiter is now always
  unregistered, and a server exit releases waiting calls with an error.
- **Language servers were killed before they could act on `exit`.** `Close`
  sent the exit notification and killed the process in the same breath; it now
  gives the server up to 2s to exit on its own. Found by the stress run, where
  the test for it failed 2 runs in 5 under `-race`.
- **A busy Chrome profile wasn't always retried with a temporary one.** The
  retry keyed on Chrome's "ProcessSingleton" message, but chromedp can discard
  Chrome's output when it exits quickly (it calls `cmd.Wait` concurrently with
  reading the pipe). A launch that fails with no readable output is now retried
  too, and reported as such instead of "chrome failed to start:" and nothing.
- **Auto-resume no longer picks up another folder's session.** Session
  directories are named by replacing every non-alphanumeric character with
  `-`, so `/a/my_proj` and `/a/my-proj` shared one, and auto-resume took the
  newest transcript in it. A transcript is now resumed only in the folder it
  was recorded in (older transcripts with no recorded folder still are).
  Upstream 2.1.239.
- **Resume no longer stops at a transcript line over 16 MB.** One large line —
  an image tool result is enough — ended the read, and the resumed history
  silently lost everything after it. Upstream 2.1.257/2.1.267.
- **Blank text blocks and orphan tool results are repaired before sending.** An
  empty or whitespace-only text block, or a `tool_result` whose `tool_use` was
  lost, made the API reject every later request, leaving a resumed session
  unusable. Both are dropped. Upstream 2.1.69/2.1.92/2.1.274/2.1.277.
- **A conversation that outgrows the window can be compacted again.** When a
  request was refused as too long, compaction sent the same over-limit history
  as its summary request, which was refused too; the failure was silent, the
  retry overflowed, and so did every later prompt. The summary request is now
  sanitized and, if refused as too long, shrunk and retried up to three times
  — the older half dropped at a real user turn, or, with nothing to drop, the
  longest text halved head-and-tail — and the summary says when the oldest
  part is missing. Automatic compaction failures are reported rather than
  silent, and after three in a row the threshold-driven attempt pauses until
  `/compact` succeeds (an overflow still forces one). Summaries get the
  model's output budget, 4k–16k tokens, instead of a fixed 4096. Upstream
  2.1.69/2.1.76/2.1.85/2.1.89/2.1.269/2.1.281/2.1.284.
- **A reply cut off mid-stream is no longer shown as a finished answer.** A
  stream that closed without `message_stop` (Anthropic) or without `[DONE]`
  and a `finish_reason` (OpenAI-compatible) was accepted as complete, so a
  truncated reply — or a turn with no stop reason — was recorded as the
  answer. A mid-stream `overloaded_error` or `api_error` ended the turn with a
  raw error. These, and a response that cannot be assembled, are now an
  interrupted stream: retried when nothing had been shown yet, like a stall,
  and otherwise reported as a reply that was cut off. The OpenAI-compatible
  provider now honours `Retry-After`, which its comment always claimed and
  its code never read; a wait over 60 seconds is surfaced instead of sat
  through. Upstream 2.1.94/2.1.98/2.1.199/2.1.281/2.1.284.
- **Skills with prose descriptions load.** Frontmatter was parsed as strict
  YAML, and a description such as `Use for slides. Triggers include: deck`
  is not valid YAML, so the skill was turned away with a warning. When YAML
  parsing fails, the known keys are now read as `key: rest of line` (with
  indented continuation lines and `[a, b]` tool lists); only frontmatter with
  no recognisable key is still an error. A file starting with a UTF-8
  byte-order mark had its frontmatter read as body; the mark is now skipped.
  `${CLAUDE_SKILL_DIR}` and `${KLAUDIA_SKILL_DIR}` in a skill body become the
  skill file's directory. Upstream 2.1.69/2.1.239.
- **MCP results other than text reach the model.** Only text content was
  kept: an image, an embedded resource or a structuredContent-only result
  arrived as an empty string, so a screenshot tool that worked looked like
  one that returned nothing. PNG, JPEG, GIF and WebP images now become image
  blocks (as Read's do); embedded resources contribute their text or image,
  or a line naming the binary; audio, resource links and other image types
  are named; structuredContent is shown as JSON when there is no text; and
  an empty result says so. Upstream 2.1.113/2.1.128/2.1.136/2.1.268/2.1.283.
- **Every page of an MCP server's tool list is read, and a failed list is
  reported.** One `tools/list` call read only the first page, so a server that
  paginates lost every tool after it; a failed list was skipped, which looked
  exactly like a server with no tools. All pages are now read, a failure is
  retried once, and one that persists is shown at startup and in `/mcp`
  against the server. Upstream 2.1.132/2.1.144/2.1.147/2.1.181/2.1.191.
- **Edits can no longer leave a truncated file, and Write keeps CRLF.** Write,
  Edit and NotebookEdit truncated the file and then wrote it, so a crash, a
  full disk or a killed process in between left it empty or cut short. An
  existing file is now replaced through a synced temporary file and a rename,
  keeping its permissions and writing through a symlink to its target; a new
  file is created as before, under the umask. Write also sent the model's LF
  line endings over a CRLF file, turning every line into a diff; it now
  matches the file it replaces, as Edit already did. Upstream 2.1.77/2.1.181.
- **A command that prints without end no longer grows Klaudia's memory
  without end.** Bash output was collected whole in memory and trimmed only
  afterwards, so `yes`, a verbose build or a runaway log could exhaust memory
  before the trim ran. Each of stdout and stderr now keeps its first and last
  8 MiB while the command runs, counting what it drops between them, and says
  so in the output. The model still sees the usual head and tail. Upstream
  2.1.247/2.1.252/2.1.265.
- **A sub-agent that fails keeps what it found.** An error part-way through a
  sub-agent's run — an overloaded service, a dropped stream — returned only
  "Sub-agent failed", and everything it had worked out was lost. Its last
  completed reply now comes back with the error, marked as possibly
  incomplete. Upstream 2.1.199/2.1.200/2.1.246.
- **One transient API error no longer ends a `--loop` goal run.** The loop
  returned on the first error from an iteration, so an overloaded response or
  a stalled stream hours into a run stopped it. An iteration that fails
  transiently (rate limit, 5xx or overload, stalled stream, network error) is
  now retried after 30 s, 1 min and 2 min, reporting each retry; other errors
  still stop the run at once. Upstream 2.1.269.
- **A transcript that cannot be written is reported.** Recording errors were
  discarded, so a full disk or a removed sessions directory lost the session
  without a word, and `--continue` later had nothing to resume. The first
  failure in a run now produces a warning — in the TUI, as a `warning` event
  in stream-json, on stderr for `-p` text and json. And the stream-json
  envelope is still written when the transcript beside it fails: the two were
  recorded in turn and a failure stopped both.
- **MCP recovers from the common connection failures on its own.** A server
  configured with a `url` and no `type` that speaks only legacy SSE failed
  with an HTTP error; Klaudia now falls back to SSE. A remote server that
  restarted left its tools failing on a lost session until someone ran
  `/mcp`; a call that finds its session gone now reconnects once and is
  sent again (the call had not run). `/mcp` offers "Reconnect all
  disconnected" when more than one is down. And servers that failed at
  startup are named in the system prompt, so the model tells you one is down
  instead of concluding its tools never existed. Upstream
  2.1.247/2.1.265/2.1.273/2.1.274/2.1.283/2.1.284.
- **Read copes with very long lines and checks images before sending them.**
  One line over 1 MB (minified JavaScript, a data dump) failed the whole read
  with "token too long", and the 2,000-byte cut on long lines could split a
  UTF-8 character. Lines of any length are now read, kept to 2,000 bytes on
  a character boundary, and marked with how much was cut. Images were typed
  by their extension and sent unchecked; an empty file, a mislabelled one, or
  one over the API's limits was rejected, and because it stayed in the
  conversation, rejected on every later request. The type now comes from the
  file's content (a PNG named `.jpg` is sent as a PNG), and empty files,
  non-images, and images over 5 MB or 8,000 pixels on a side are refused with
  a message saying why. Upstream 2.1.122/2.1.126/2.1.144/2.1.145/2.1.157/
  2.1.166.
- **A grep that finds nothing is not a failed command.** Every non-zero exit
  was an error result, and error results drive the loop's repeat-failure
  steering, so a model searching for something correctly absent was nudged
  as if it kept making the same mistake. Exit 1 from grep, rg, ag, diff, cmp
  and test — the last command in the line — is now an answer, marked "no
  match / differences found". Exit 2 and above are still errors. Upstream
  2.1.144.
- **Stopping a command also stops what it detached (Linux).** Commands are
  killed as a process group, and a process that leaves the group — `setsid`,
  a daemon that double-forks — escaped: it outlived Esc, `/stopjob` and the
  end of the session, often still holding a port. Each command's environment
  now carries a tag, and stopping it also stops every process still carrying
  that tag (SIGTERM, then SIGKILL after the usual grace). A command that
  finishes on its own leaves what it started alone. macOS is unchanged.
  Upstream 2.1.257.
- **A reply cut off mid-stream is kept in the history.** When the stream stalled
  or the user pressed `Esc` after some of the answer had been shown, the turn
  ended with an error and the history did not include the partial text. The
  model had no record of what the user had already read. The shown text is now
  recorded as an assistant message and in the transcript, ending with a marker
  that says whether the connection dropped or the user interrupted. An
  unfinished tool call in that reply is dropped, because it never ran. The
  OpenAI-compatible provider used to discard the partial text when the read
  failed, and now returns it as well.
- **Memory notes were lost once any detail note existed.** With a file under
  `.klaudia/memory/`, MEMORY.md ends with its `## Linked memory` section, and
  each new bullet was appended to the end of the file — inside that section —
  which the refresh after the add then rebuilt without it. Every note added
  after the first was silently dropped. New bullets now go above the linked
  section, and the add is a single write (#97).
- **An unquoted positional prompt kept only its first word.** `klaudia explain
  this code` sent the model `explain`; the positional words are now joined with
  spaces, so it sends `explain this code`, the same as the quoted form.
- **`-p` reads a piped prompt, and refuses an empty one.** `echo "q" | klaudia -p`
  ignored stdin and sent a request with no user message, exiting 0. Piped stdin
  now becomes the prompt, or is appended after a blank line to one given on the
  command line (`git diff | klaudia -p "review this"`). With a prompt already
  given, Klaudia waits at most 3s for stdin to start, so a caller that leaves a
  silent pipe open is not hung; redirect `< /dev/null` to skip the wait. No
  prompt at all is a usage error (exit 2).
- **Edit refuses an empty `old_string`.** An empty string matches between every
  character, so with `replace_all` Edit spliced `new_string` all through the
  file (`abc` became `XaXbXcX`) and reported success; without it the edit
  failed as "not unique". The call is now rejected before it runs, with a
  message pointing at Write.
- **`[input] enter = "newline"` now takes effect.** Loading the config dropped
  the whole `[input]` section when it merged the home and project files, so
  Return always sent, whichever file set it. The setting now loads like the
  others: project over home.
- **Resuming a compacted session dropped every turn since the last
  compaction.** Once a session had compacted, resume seeded only the persisted
  summary, and nothing in the transcript said where the summary ended, so the
  conversation after it was lost on the next launch — the default resume path
  for any long session. Compaction now appends a `compact_boundary` system line
  to the transcript, and resume seeds the summary followed by every message
  after the last boundary. `--full` still replays the whole transcript; a
  transcript compacted before this change has no boundary and resumes from the
  summary alone, as before.
- **Compaction on Claude Fable 5.1 and Claude Opus 5.5 no longer edits history
  that has already been sent.** These models reject a replayed thinking block
  if anything before it changed since it was produced. For accounts created on
  or after 2026-08-31, that rejection is a 400. Two edits could trigger it:
  - Microcompact trimmed old tool results. It is now skipped on these models,
    because trimming has no form the check accepts. Autocompact still keeps
    the context within the window.
  - The autocompact and `/compact` summary request replayed the conversation's
    thinking without its system prompt and tools. So on an enforced account,
    compaction would fail exactly when it was needed. The summary request now
    leaves out the thinking blocks. The history the model continues from is
    unchanged.

  Other models behave as before. The `opus` and `fable` aliases still point at
  5.0.
- **`git commit -am "msg"` was refused as having no message.** The check that
  stops a commit from opening an editor looked for the substring `-m`, which a
  short-flag cluster (`-am`, `-sm`, `-asm`) does not contain, so the model was
  told to "pass the message with -m" when it had, and retried in a loop. The
  arguments are now parsed as git parses them: clusters, attached values
  (`-mfix`, `-F/tmp/msg`, `-CHEAD`), `--opt=value`, and `--`. The same parse
  stops the opposite mistake, where `-m` inside a path (`src/my-module`),
  another option's value, or a later command in the line let a commit through
  to an editor that hung the turn. `-c`/`--reedit-message` no longer counts as
  supplying a message: git opens the editor on it.
- **`BashOutput` is capped like Bash output.** A read returned everything a
  background job wrote since the previous read, so a chatty dev server left
  alone for a few turns could flood the context in one call. A read over 30,000
  bytes now keeps its start and — with two thirds of the budget — its most
  recent output, says how many bytes were elided, and names the job's log file
  and the byte range of the read so the model can Read or grep the rest (a job
  whose log is memory-only is spilled to a file instead). A job-log read that
  returned short no longer advances the read cursor past bytes it never
  returned.
- **A picker opened during a turn could start a second, concurrent turn.**
  `/mode`, `/mcp` and `/model` open their pickers while Klaudia works, but
  choosing an item set the UI idle with the turn still running, so the next
  Enter started another agent alongside it. Esc in such a picker cancelled the
  turn instead of the picker. `/logs --errors` started its turn without the
  running state, with the same result. A picker now returns to the state it
  opened over (and stays open if the turn ends under it), Esc closes the picker
  and leaves the turn alone, a prompt from the turn (approval, question, plan)
  takes over from an open picker, and a turn refuses to start while another is
  still in flight.
- **`~/.claude/skills/synced` no longer warns at every start.** Claude Code
  keeps the skills it syncs from claude.ai there, one level deeper
  (`synced/<bucket>/<skill>/SKILL.md` beside a `manifest.json`), so the loader
  read `synced` as a skill directory missing its `SKILL.md` and said so each
  time Klaudia started. It is now skipped quietly, and the synced skills are
  not loaded: many are claude.ai-specific. A `synced` directory anywhere else
  still warns, and a skill actually named `synced` still loads. The skill
  package's tests also stop reading the real home directory, which made one of
  them fail on any machine with such a directory (#110).
- **Grep and Glob honour `.gitignore` and can see dot-directories.** Only a
  fixed list of directories (`node_modules`, `vendor`, `.git`, …) was skipped,
  so searches walked `target/`, `dist/`, `build/` and `coverage/`; and every
  hidden entry was skipped, so `Glob(".github/**/*.yml")` found nothing and
  `.eslintrc` was never searched. The walks now follow ripgrep: `.gitignore`
  and `.git/info/exclude` inside a git repository, `.ignore` anywhere, ignore
  files between the repository root and the search root included. A hidden
  or ignored entry is still searched when the search names it — as the path,
  as a pattern's literal leading segments (`.github/…`, `dist/*.js`), or as a
  dot-name in the pattern (`.env*`, `**/.eslintrc`). The `@` file completion
  shares the walk, so ignored build output no longer crowds it. Not read: the
  global `core.excludesFile`.
- **The model no longer writes `.klaudia/KNOWLEDGE.md` without asking.** The
  `Memory` tool allowed every operation, including `add scope=project` and
  `promote`, and the file it writes is injected into every later session's
  system prompt — so a prompt injection read from a web page or an MCP server
  could make itself permanent. Those two writes now ask, in autonomous mode
  too, and show the note being written; `dontAsk`, headless and plan mode
  refuse them with a message pointing at session memory and the allow rule
  `Memory(project)`. Session notes in `MEMORY.md` stay autonomous. The injected
  knowledge is now framed as project notes to weigh against the code, not
  "established facts".
- **Bash refused ordinary commands as self-backgrounded services.** The
  detector scanned the line for `&` and for service names as substrings, so
  `go run ./cmd/gen &> gen.log` was "backgrounded", `go test ./server/... &`
  matched `serve`, `vitest` matched `vite` and `repair.sh` matched `air`. The
  line is now parsed: `&>` and `&>>` are redirections, names match as whole
  words, only a command that is itself backgrounded counts, and `a & b & wait`
  is allowed because the shell waits for both.
- **"Needs a terminal" refusals rejected their own suggested command.** The
  check looked only at the program name, so `man ls | cat` was refused with
  "use `man <page> | cat`", and `crontab -l` with "use `crontab -l`". Forms that
  never touch a terminal now run: `man`, `less`, `more` and `most` with stdout
  piped or redirected, `top -b` (`top -l` on macOS), `crontab -l`/`-r`/`<file>`,
  tmux and screen commands that do not attach (`tmux ls`, `tmux new -d`,
  `screen -dmS`, `screen -ls`), `visudo -c`, and `--help`/`--version` on any of
  them. `top` in a pipe stays refused (it fails with "failed tty get"), and the
  `top` advice now names `top -b -n 1`.
- **The host guardrail missed common host changes and asked about harmless
  ones.** It now asks before `sudo pacman -S nginx` and `pacman -Syu` (and
  `rpm -ivh`, `dpkg -i`, `nix-env -i`, which had the same fault: the operation
  is a flag, so the package was read as the verb), `yarn global add`,
  `git config --global`/`--system` (docs/trust.md already called `~/.gitconfig`
  host), `sudo pip install` and `sudo make install`. It no longer asks about
  `brew update` or `apt-get update`, which refresh an index and install
  nothing, or about `ln -s $(pwd)/tool ~/.local/bin/tool`, where only the link
  is written. A write to a path held in a variable (`rm -rf "$BUILD_DIR"`) now
  says whether it writes or deletes, and the refusal suggests re-running with
  the literal path.
- **`--create-config` broke a working Anthropic setup.** The starter set
  `provider = "openai"` with a placeholder `baseURL`, so a user with only
  `ANTHROPIC_API_KEY` who ran it could no longer start (`provider "openai"
  needs apiKey…`). The starter now selects `provider = "anthropic"` and carries
  the OpenAI-compatible block commented out, with instructions for swapping it
  in. Its permissions example also lists `dontAsk`, which it had left out.
- **A negative `--max-turns` is a usage error (exit 2)** instead of being
  accepted and silently treated as unlimited.
- **The OpenAI-provider missing-key error names the variable.** With
  `apiKeyEnv = "MY_API_KEY"` set and `$MY_API_KEY` empty, it said only "needs
  apiKey or apiKeyEnv" — pointing at a config that was already right. It now
  says `$MY_API_KEY` is unset or empty.
- **A prompt that starts with a path is sent, not rejected as "Unknown command".**
  Any line starting with `/` went to slash handling, so pasting
  `/etc/nginx/nginx.conf fails to parse` answered "Unknown command"; the only
  way round it was an undocumented leading space. Now a first word that names no
  command or skill is a prompt when it holds a second `/` or has text after it,
  in the idle box and when queued mid-turn. A lone unknown `/word` still errors,
  now with a "Did you mean …?" for the nearest commands, and `//` at the start
  sends the line as a message with one slash removed.
- **Slash commands no longer ignore arguments they don't understand.** `/plan xyz`
  entered plan mode and `/errors foo` listed ten errors; both now print their
  usage and do nothing. `/add-dir` expands `~`, resolves a relative path from the
  project, and refuses a path that doesn't exist or isn't a directory — before,
  a typo went into the prompt as a directory no tool could read.
- **`/restart`, `/stopjob` and `/logs` resolve job names the same way.** With no
  argument and more than one job running, all three now list the jobs with the
  usage line (only `/logs` did); a name that matches nothing is answered with the
  jobs that exist and their state, instead of a bare "no job X". `/stopjob` on a
  job that had already exited used to report "stopped" — the store's kill is a
  no-op there — and now says it already exited, with its code.
- **`@` completion no longer litters scrollback or fires inside words.** An
  ambiguous `@path` Tab printed a permanent `candidates: …` line; the matches are
  now shown under the prompt, with the current one bracketed, only while Tab is
  cycling. The `@` must start a word, so `bob@example.com` is left alone. A
  follow-up queued during a turn is now indexed when it is sent, so `/outline`
  and `/search --mine` list it like a typed prompt.
- **A malformed tool call reached the host gate and the user before being
  rejected as invalid.** `dispatch` ran `tool.ValidateInput` only after the
  host gate and the permission/approval step, so a tool_use with bad or missing
  arguments — a call that could never run whatever anyone decided — still made
  the host gate classify it and, worse, made the user approve it before it was
  refused. Validation now runs first, right after the tool is looked up, so an
  obviously-invalid call is rejected immediately without bothering the gate or
  the user. The rejection returns the same `errResult` (with the accepted-field
  list) it did before, so the failure counters and loop-breakers behave
  identically; only the timing moved.
- **"Always allow" on a chained Bash command saved a rule for the first
  command only.** Choosing "always allow" for a compound line built the
  persisted rule from `bashparser.Prefix()` — the first command's short form
  (`git status:*`) — and saved that alone. Under the old first-command-only
  matching that silently over-permitted whatever was chained after it; once
  rules must match *every* command in a line (the every-command matching in
  PR #16, which this depends on and should merge after), the same single rule
  no longer covers the rest of the line, so the identical command is asked
  about again next time. "Always allow" now saves one short-form rule per
  distinct command in the line — `git status && go test ./...` saves
  `Bash(git status)` and `Bash(go test)` — so each command is individually
  allowed. A lone command's saved rule is unchanged. A line that cannot be
  reduced to a clean set of named commands — a parse error, a parameter
  expansion or command substitution, or an inline `bash -c`/`eval` script whose
  payload is not enumerated — falls back to the single prior specifier rather
  than guessing rules from text it could not fully read.
- **MCP, Grep and Glob output is capped like Bash's.** Only Bash output was
  clamped (head and tail kept, the whole spilled to a file the model can
  read), so one MCP call or a broad Grep could put megabytes into the
  context. All three now go through the same cap. Grep also stops collecting
  after 20,000 matches (it held every match in memory first) and says it
  stopped; Glob shows the 20,000 most recently modified files and says how
  many matched; and both stop when the turn is interrupted, where they used
  to run to the end. Upstream 2.1.91/2.1.98/2.1.105/2.1.275/2.1.277.
- **A stuck MCP server no longer holds a turn or startup forever.** Tool calls
  ran on the turn's context alone, so a server that stopped answering
  mid-call (a dropped HTTP/SSE connection) held the turn until you pressed
  Esc, and a headless run indefinitely. Calls now time out after ten minutes
  (`KLAUDIA_MCP_TOOL_TIMEOUT`, or `"timeout"` in seconds per server), with an
  error that says how to reconnect or raise it. Servers connected one after
  another with no deadline, so one that never finished its handshake blocked
  startup; they now connect in parallel, each with 30 seconds
  (`KLAUDIA_MCP_CONNECT_TIMEOUT`), and a reload uses the same deadline.
  Upstream 2.1.110/2.1.142/2.1.162/2.1.187/2.1.206/2.1.232/2.1.251.
- **A bubblewrap that cannot run no longer breaks every command.** Sandbox
  mode `os` checked only that `bwrap` was on the PATH. Where unprivileged user
  namespaces are disabled it is installed and fails to start anything, so
  every Bash call became an error. It is now tried once at startup and, if it
  cannot run, Klaudia falls back to local execution with a warning saying
  why. New `sandbox.failIfUnavailable = true` makes any unusable sandbox — a
  missing tool, bwrap that cannot run, a container mode with no image — an
  error at startup instead of a silent drop to unconfined execution.
  Upstream 2.1.83.
- **A config that did not parse was silently ignored.** A TOML syntax error in
  `~/.klaudia/config.toml` or `./.klaudia/config.toml` dropped the whole file and
  the session started on defaults — another provider, another permission mode.
  It is now a usage error (exit 2, as `exitcode.go` already documented) naming
  the file, line and column. A key the schema does not know (`modle = "x"`) was
  accepted without a word; it is now a `warning:` on stderr naming the file,
  line and key, and the rest of the file still applies, so a config written for
  a newer or older Klaudia still starts.
- **Compaction no longer pays full price for the whole conversation.** The
  summary request, for both autocompact and `/compact`, left out the system
  prompt and tools, so it shared no prefix with the conversation's cached
  requests and every token of the history — the session's largest request —
  was billed uncached. It now carries the same system prompt and tools, which
  also define the `tool_use` blocks in the history it sends, and the summary
  instruction tells the model not to call them.
- **`Diagnostics` reported "clean" when the language server never answered.**
  If the server published nothing for the file within the 10s wait — a cold
  gopls or rust-analyzer still indexing — the tool printed "No diagnostics — X
  is clean.", a false all-clear the model would trust. It now returns an error
  saying the server did not report in time and that this is not an all-clear;
  "clean" is reserved for a server that actually published an empty list (#109).
- **Read says when it stops short of the end of a file, and no longer dumps
  binary files.** A read that reached the 2,000-line default (or a given
  `limit`) ended with no sign the file went on, so the model could take the
  first window for the whole file. It now ends with
  `(showing lines A–B of N; pass offset=B+1 to continue)`. A file with a NUL
  byte in its first 8 KB (the test Grep uses) is reported as
  `<binary file, N bytes; not shown as text>` instead of as lines of noise.
- **`/clear` starts a new session; the next launch no longer brings the cleared
  conversation back.** `/clear` reset only the in-memory history: the transcript
  kept being appended to under the same id and its compaction summary stayed, so
  auto-resume restored exactly what had been cleared. It now closes the old
  transcript (kept on disk as its own session — `/clear` prints its
  `klaudia --resume <id>`) and records what follows under a fresh id with no
  summary. If you clear and quit without saying anything more, the old
  transcript is still the newest in the folder; it now ends with a
  `{"type":"system","subtype":"clear"}` line, and auto-resume starts fresh
  instead of reviving it. `--continue` and `--resume` still reopen it.
- **`/goal` survives a resume, and an unrelated `PRD.md` is no longer the loop
  spec.** The standing goal set with `/goal <text>` lived only in memory, so a
  resumed session lost it and the resume banner's Goal section could never
  appear. It is now kept in a `<session>.goal` file beside the transcript,
  restored on resume (a fork inherits it), and removed by `/goal clear`.
  Separately, the goal loop picked `./PRD.md` over `.klaudia/GOAL.md` whatever
  it contained, so a project's product requirements document became the Ralph
  spec and had wrap-up summaries written into it. `.klaudia/GOAL.md` now comes
  first, and `PRD.md` is used only when it has a goal spec's shape (a `- [ ]`
  checklist and a Verify section); otherwise `/goal` and `--loop` say it was
  passed over and why.
- **Launching from a subdirectory lost the project's memory, session and
  skills.** Memory (`.klaudia/MEMORY.md`, `KNOWLEDGE.md`), session transcripts
  and project skills were keyed by the launch directory, so starting in
  `internal/tui` gave a separate memory, no auto-resume of the repository's
  session and none of its skills — while CLAUDE.md lookup already walked to the
  git root. They are now keyed by the project root: the git top-level, or the
  launch directory outside a repository (or when the top-level is `$HOME`, a
  dotfiles repository). The launch directory's own memory, knowledge, skills
  and sessions are still read, after the root's, so nothing recorded under the
  old key is orphaned; new writes go to the root. Tools, config and the
  `Working directory` the model sees still use the launch directory.
- **`KLAUDIA_CONFIG_DIR` now moves `config.toml` and user skills too.** Sessions,
  the global `.mcp.json`, job logs and the browser profile already followed it,
  but the global `config.toml` was still read from `~/.klaudia`,
  `--create-config=global` wrote there, `/doctor` looked there for "config
  found", and user skills were read from `~/.klaudia/skills` — so a relocated
  config dir silently ran on another file's settings. Every caller now resolves
  the directory through one helper, `config.Root()` (`$KLAUDIA_CONFIG_DIR`,
  else `~/.klaudia`). If you set `KLAUDIA_CONFIG_DIR` and kept `config.toml` or
  `skills/` in `~/.klaudia`, move them into that directory.
- **An unknown model surfaced as raw JSON.** A typo'd model id ended the turn
  with `stream: openai endpoint 404: {"error":…"model_not_found"}` (or the
  Anthropic `not_found_error` equivalent). It now reads "Model not found: X
  isn't served by <endpoint>", suggests `/model` or `--model <id>`, and quotes
  the provider's own message. A 404 that does not mention a model is reported
  as a wrong path and points at the `baseURL` instead. `/model <id>` also warns
  when the id is missing from the list an earlier `/model` fetched — it never
  fetches the list itself, and still applies the id, since an endpoint can
  serve ids it does not list.
- **Claude model aliases no longer rewrite the model for the OpenAI provider.**
  With `provider = "openai"`, `--model sonnet` reached the endpoint as
  `claude-sonnet-5` with Claude's 128000-token output cap. Aliases (`opus`,
  `sonnet`, `haiku`, `fable`) and the Claude default model now resolve only for
  the Anthropic provider; any other provider is sent the model string as written,
  with a single warning when it is a bare Claude alias. The Claude context-window
  and output-cap tables apply only on Anthropic too, so an OpenAI-compatible
  model gets the unknown-model defaults (`contextWindow` / `maxTokens` still
  override). The same holds for `/model <alias>`, which also prints its
  confirmation again. An OpenAI provider with no `model` configured and no
  `--model` is now a startup error instead of silently requesting Claude's
  default model.
- **A headless run that could not start wrote nothing to stdout in JSON mode.**
  With `--output-format json` or `stream-json`, a missing credential, an
  incomplete provider config or a bad flag combination went to stderr as plain
  text and left stdout empty, so a script parsing stdout had nothing to parse.
  It now gets an `is_error: true` result line with the reason (the stderr
  message stays). The exit code is unchanged.
- **`duration_api_ms` was the wall time.** It is now the time spent waiting on
  the model — requests, retries and autocompact summaries — so it no longer
  counts tool execution. `total_cost_usd` still reads `0`: cost is not computed
  yet.
- **Some usage errors exited 1.** An invalid `--output-format`,
  `--create-config=foo` and `--new-session` with `--resume`/`--continue` now
  exit 2, like an unknown flag.
- **↑/↓ got stuck after recalling a multi-line prompt.** History was browsed
  only while the box held a single line, so once a multi-line entry was
  recalled the arrows moved inside it and could not get past. ↑ now browses
  history from the first line and ↓ from the last (readline's
  up-line-or-history), elsewhere they move the cursor. A long line that wraps
  counts by its displayed rows, so ↑ at the end of one first walks up it.
- **The Bash tool now tells the model how its shell behaves.** Its description
  says each call is a fresh shell in the project directory (`cd` and exports do
  not carry over) and names `run_in_background` for commands that never finish
  on their own. A timeout now states the limit that was hit and whether a larger
  `timeout` is still possible, rather than a bare "timed out".
- **`/compact` summarized with a stale model.** The TUI wired compaction to the
  model captured when the session was built, so a `/model` switch mid-session
  did not reach it and `/compact` kept summarizing with the original model —
  unlike every turn, which already resolves `sess.Model` fresh. Compaction now
  reads the live session model too, matching the autocompact/turn path.
- **Two more transcript-sanitiser gaps that 400 the request.** The message
  sanitiser (`internal/agent/sanitize.go`) already repaired empty content,
  orphan tool_use/tool_result and broken server-tool blocks; it now also handles
  two shapes the API rejects on send. (1) A **tool_use `name` over ~200
  characters** — a long namespaced MCP tool name is the usual source — is
  truncated to the limit. Pairing is by `tool_use_id`, which is left untouched,
  and a `tool_result` carries no name field, so the paired result round-trips
  unchanged. (2) **Thinking blocks left stale by a mid-session `/model`
  switch:** a `thinking`/`redacted_thinking` block is bound to the model that
  produced it, so after a switch the ones already in history are rejected (or
  silently dropped) by the new model. They are now dropped from every assistant
  turn *except* the in-progress one — a completed prior turn's thinking is never
  required by the API, while the current turn's may be, so it is preserved.
  Limitation: the sanitiser sees only the message list, not the active model, so
  it cannot tell a still-valid same-model prior-turn block from a stale one and
  drops both (never an API error — prior-turn thinking is optional); and if a
  switch lands mid-tool-loop the in-progress turn's thinking is kept and could
  still be stale, an edge accepted over the risk of stripping thinking the model
  needs.
- **The stream-json embedding channel emitted a different shape from
  `-p --output-format stream-json`.** Single-shot runs wrap each conversation
  message in the JS-compatible envelope (`{"type":"assistant","message":{…},
  "session_id":…}`); the `--input-format stream-json` driver wrote Klaudia's
  flat agent events instead (`{"type":"assistant","text":…}`, `{"type":
  "tool_use",…}`, `{"type":"tool_result",…}`). One binary, two answers to
  "what does an assistant message look like", and the README promised the
  first. A client written against the documented envelope — the one Claude
  Code SDKs already parse — dropped every mid-turn line and showed nothing of
  a turn until its `result`, which from the outside is indistinguishable from
  a hang. Observed in an embedding GUI whose transcripts stayed empty while
  the agent was calling tools and answering behind it.

  The driver is now the run's Recorder, so every message the loop records is
  also the message the peer is shown, in the envelope, stamped with the
  session id; the flat `assistant` / `tool_use` / `tool_result` events are no
  longer written there, since the same content would otherwise arrive twice in
  two shapes. `usage`, `tool_progress` and `compaction` still stream as flat
  events — they have no message form. A client that had adapted to the flat
  shape will need to read the envelope; the README's embedding section shows
  it.

- **A sub-agent worked without the project's context and shared the parent's
  todo list.** Its system prompt was only its type's paragraph: no working
  directory, platform or git branch, and no CLAUDE.md or
  `.klaudia/KNOWLEDGE.md`, so a general-purpose sub-agent editing code ignored
  the project's rules. It now gets the same environment block and project
  instructions as the main agent (not the parent's recalled memory). A
  general-purpose sub-agent was also handed the parent's TodoWrite, whose store
  its first call replaced, and AskUserQuestion, although the Agent tool tells
  the model a sub-agent cannot ask questions; it now has a todo list of its own
  and no AskUserQuestion. A sub-agent's result now ends with a `<usage>` block
  giving its turns, input and output tokens, and duration.
- **Memory index pointers showed `---` for notes with frontmatter.** A
  `.klaudia/memory/*.md` note's hook in MEMORY.md's `## Linked memory` section
  was its first line, which for a note with YAML frontmatter is the fence — and
  `promote` and `supersede` add frontmatter, so they turned a useful pointer
  into `- [n](memory/n.md) — ---`. The hook is now the frontmatter's
  `description` when it has one, else the first line after the frontmatter;
  `promote` and `supersede` keep a `description` when they rewrite a note. The
  hook was also cut at byte 80, which could split a multi-byte character into
  invalid UTF-8; it is now cut at 80 characters (#121).

- **The recalled memory index had no size limit.** MEMORY.md is injected into
  every request's system prompt and grows with every note. The recalled copy is
  now cut at 200 lines or 25,000 bytes, at a line boundary, ending with a line
  that says how many lines were left out and to open `.klaudia/MEMORY.md` or
  search memory for them. The file itself is not changed (#121).

- **A permission rule naming an MCP server matched nothing.** Rules were
  compared for equality against the tool's qualified name, so `mcp__loki` in
  `[permissions] allow` never matched `mcp__loki__loki_query`, and the check
  fell through to the tool's own stance as if the rule were not there. In an
  interactive session that meant being asked for a tool the config had already
  allowed; in a stream-json embedder it meant a `control_request` the client
  was not expecting (see below). `mcp__<server>` and `mcp__<server>__*` now
  cover every tool on that server, for allow and deny alike, which is the form
  the JS reference documents for MCP rules and the one people write first.

- **A stream-json `can_use_tool` request with no answer blocked the turn
  forever.** The approver waited on the context and the reply channel only.
  A client that never sent the `control_response` — because it relied on the
  config allow list and never implemented the control protocol, or because it
  stalled — left the agent wedged mid-turn with no output and no exit, and the
  only diagnosis available was the process not finishing. The wait is now
  bounded by `--ask-timeout` (default ten minutes; `0` restores the unbounded
  wait), after which the ask is denied with a tool result that names the tool,
  the timeout, and the two remedies: answer `can_use_tool`, or pre-approve the
  tool with an allow rule so it is never asked. An answer that arrives after
  the deadline is dropped rather than applied to a later request.

  Ten minutes was chosen as long enough for a person behind an editor
  integration to read a prompt and decide, and short enough that an unattended
  pipeline fails inside the run rather than at whatever outer timeout kills it.
  The JS reference waits forever; that was measured to be the wrong default
  for a channel whose peer is a program.

- **`dontAsk` was the right mode for a headless embedder, and nothing said
  so.** `--permission-mode` listed it only as "legacy", so a read-only embedder
  had no obvious way to say "run what the allow list permits, deny the rest,
  never prompt". The flag help, the config comment and the README's embedding
  section now name it and describe the `control_request` / `control_response`
  exchange a client must otherwise implement.
- **Five things the stream-json embedding channel could not do.** It is the
  channel whose entire purpose is a client with a user sitting in front of it,
  and most of what follows was a capability the transport never carried.

  - **`AskUserQuestion` and `ExitPlanMode` were dead.** The CLI passed no
    `Asker` and no `Planner` on this path, so the model called them and was told
    there was nobody to ask. Two new `control_request` subtypes, `ask_user` and
    `exit_plan`, answered on the same `request_id` channel as `can_use_tool`. A
    peer that has not implemented them should answer with an error, which reads
    as "cancelled" and leaves the model where it was — rather than the first
    option being picked and an answer the user never gave being attributed to
    them.
  - **A permission ask arrived stripped of everything but the tool name and its
    input.** `tool_use_id`, the specifier, the suggestion and the whole host
    change were dropped on the floor, so every ask looked the same. The host
    change is the damaging one: "allow Bash?" is the wrong question to put to
    someone about `systemctl restart nginx`, and a peer that cannot tell the two
    apart cannot render the right card.
  - **A refusal was reported as a successful result with no text.** `subtype:
    "success"` and an empty `result` let a pipeline treat a refusal, or a hit
    limit, as a finished task — the same bug the headless path already fixes and
    this one had not inherited. It now reports the stop reason, `is_error`, and
    the note explaining what stopped the turn.
  - **`--resume`/`--continue` was resolved and then discarded.** The driver
    started a fresh conversation while the CLI reported it had resumed one.
  - **A control response without a `subtype` was read as a failure.** Only
    `"success"` counted, so a peer that echoed a `request_id` and a payload —
    the obvious minimal implementation — had every answer treated as a denial.
- **Notices reach the operator in a headless run.** `-p` suppresses
  intermediate events on purpose — stdout is the result and nothing else,
  because it gets piped — but notices were going through the same renderer and
  being dropped with them. A notice is not a step in the work; it is the one
  class of message only the operator can act on: a hook that failed to start,
  project hooks waiting on an approval nobody is there to give, an MCP server
  that could not be reloaded. Reported but invisible is the same as unreported.

  They now go to stderr, in every headless format, so stdout's contract is
  untouched and a notice cannot land in the middle of a stream-json line.

- **The list of slash commands a skill cannot shadow is derived from the
  commands, not kept by hand.** `internal/cli` held its own copy, and it had
  drifted in both directions. It still reserved `allow` and `deny` after the
  per-command rule model was removed, so a skill could not be named either. And
  it had never learned about `/trust`, `/undo`, `/jobs`, `/pin` and fifteen
  others, so a skill named after one of those was shadowed by the built-in —
  reachable only through the `Skill` tool — with no warning at all, which is the
  exact failure the list exists to report.

  It now comes from `commandList`, the table that already drives `/help` and
  type-ahead, with the two switch-only aliases (`/?`, `/exit`) named explicitly.
  Both lists live in `internal/tui` beside the switch they describe, and a test
  parses `handleSlash` and fails if a case there is not claimed by one of them —
  measured by adding a `/bogus` case and watching it fail, rather than assumed.

- **A dead MCP server restarts itself on the next call.** A reload already
  probes liveness, but only the config changing triggers a reload — so a server
  that died mid-session with nobody editing `.mcp.json` stayed dead for the rest
  of it. A stdio server whose child process exits keeps a non-nil
  `ClientSession`, nothing nils it out, so the first sign was a tool call
  failing and the only cure was `/mcp` or a restart.

  A call that fails on the wire now probes the server and relaunches it if it
  has stopped answering. The probe decides, not the error: a bad argument or an
  unknown tool also arrives as a call error, and restarting a healthy server
  over the model's mistake would throw away whatever state it holds — for an
  editor bridge, the user's open session.

  Whether to repeat the call is a correctness question, not a performance one.
  The failure may have come on the way back from a call that already ran, and
  nothing on the wire distinguishes that from one that never arrived. So a tool
  declared read-only (`readOnlyHint`, or the per-server override) is retried,
  and anything else is reported with the ambiguity stated — the server is back,
  the call may or may not have taken effect, check before retrying. Reading an
  MCP *resource* is always retried, having no side effects to be ambiguous
  about.

  Relaunches are serialised per server and rate-limited to one per 15 seconds.
  Two tool calls, or a tool call and `/mcp`, can arrive together and would
  otherwise each spawn a child process and close the other's; and a server that
  simply cannot start would pay a full launch timeout on every call the model
  makes, turning one bad entry into a stalled session.

- **`ToolSearch` no longer loads tools on the strength of "the".** Ranking
  matches instead of demanding every term fixed descriptive queries and brought
  a cost with it: because OR-matching qualifies a tool on any single term, and a
  match *reveals* that tool, a word that appears all over the catalog spends the
  budget deferred loading exists to protect. Asked to "stop the background job
  that is running", Klaudia loaded a Godot project runner — its description
  contains "the" — and a scene manager, because "is" is a substring of "disk".

  Two rules, each aimed at one of those mechanisms. A term matching more than
  half the catalog is dropped before matches are qualified, decided against the
  loaded catalog rather than a list of English stopwords, because which words
  are uninformative depends on what is loaded — with 65 Godot tools present,
  "godot" narrows nothing either. And a term shorter than three characters
  matches whole words only, with no fuzzy tier, since document frequency cannot
  catch "is" inside "disk" when "disk" is itself a rare word.

  If every term is noise the whole query is kept: the bare "godot" case, where
  returning all 65 is the honest answer to a question that broad and returning
  nothing was the original bug. Measured on a mixed catalog, the two job queries
  above went from 7 matches to 6 and from 5 to 3, losing only tools that had
  matched on a function word.

- **A dropped web search now says so.** The repair that keeps a broken
  server-tool exchange from poisoning a session was correct and completely
  silent, and the silence turned out to be the worse half of the bug. The model
  reads a placeholder on the *next* request, so it can recover; the user saw a
  search in the transcript with no results, no error and no way to know the
  answer that followed had been reasoned without it. In the session that
  prompted this, several searches came back empty and nothing anywhere
  mentioned it.

  Each dropped exchange is now announced once, as a new `notice` event, naming
  whether a search or a fetch was lost and that it can be re-run. Once, not per
  turn: the broken exchange stays in history, so a transcript inherited from a
  resume would otherwise carry a permanent banner. Two shapes are deliberately
  *not* announced, because a false alarm is indistinguishable from a real one —
  a server tool the API withheld because the same batch held client tools (it
  runs on the next request, by design) and a paused search whose two halves are
  still in separate messages.

- **The previous frame's chrome no longer shows through tab-indented output.**
  Reading a tab-indented source file produced lines with the status bar and the
  input placeholder embedded in the indentation — `266Klaudia…` and
  `268-opus-5 · auto-e}` in a real session. The bleed runs were 8 and 16 columns
  wide, which is the signature: a literal HT advances the terminal's cursor to
  the next tab stop *without painting the cells it skips*, and Bubble Tea writes
  a queued scrollback line straight over the rows the last frame occupied,
  appending `EraseLineRight` only at the end of the line. Every cell a tab
  jumped over kept the pixels the prompt box, placeholder or status line had
  left there.

  A second defect compounded it. `fitScrollback` measures lines with
  `ansi.StringWidth`, which scores a tab as **zero** columns — HT is an
  `ExecuteAction`, not a `PrintAction` — so a tab-heavy line was judged short,
  skipped the wrap, and was handed to the terminal to wrap onto rows Klaudia
  never erases. That is the same class of bug `fitScrollback` already exists to
  prevent, arriving by a different route.

  Tabs are now expanded to real tab stops (multiples of 8, counted in columns
  with escape sequences excluded so a colour code cannot shift a later stop) in
  `fitScrollback` — the single choke point every printed block passes through,
  and before the width measurement, so one change fixes both. It is done there
  rather than in `baseStyle` deliberately: `TabWidth(NoTabConversion)` is still
  set, `m.transcript` still holds the literal tabs, and `/copy` and `/export`
  still paste `go test` and TSV output with its columns intact. lipgloss's own
  expansion was not an option — it writes four spaces per tab regardless of
  column, which is what destroys that alignment and why it was disabled in the
  first place.

- **A tool call carrying one extra property is no longer thrown away.** Tool
  input schemas are generated with `additionalProperties: false`, and that was
  enforced at dispatch, so a model that supplied every field correctly plus one
  the tool has no field for got `Input validation error` instead of a tool run.
  From a live session: four consecutive `AskUserQuestion` calls with a valid
  question and valid options, rejected for a stray top-level `description`.
  Rejecting it bought nothing — `json.Unmarshal` drops unknown fields anyway, so
  the call would have worked.

  The advertised schema still says the shape is closed, because telling the
  model the exact shape is useful; the validator compiled from it no longer
  does. Field names the tool cares about are still protected: a missing
  `required` property or a wrong type fails as before, so the case the
  field-list hint was written for (`line_start` in place of `offset`, which
  leaves `file_path` missing) still self-corrects.

- **A rejected input is no longer diagnosed as a wedged environment.** The
  second loop-breaker splits on whether the model varied its inputs across a
  run of same-shape failures: varied inputs read as the environment being
  broken, identical ones as a guessing loop. But a call refused *before* the
  tool runs — unrecognised name, input that doesn't validate, permission denied
  — produces character-identical text however much the input varies, so varying
  inputs steered straight into the environment directive. The `AskUserQuestion`
  loop above was answered with "This looks like an environment issue (shell
  wedged, leaked background process, network unreachable, filesystem broken)"
  and advised to run `KillShell`, for a schema error.

  Failures are now classified by where they came from, and only ones where the
  tool actually ran can be blamed on the environment. Pre-execution failures
  get the same-shape directive, which quotes the recurring error — the thing
  the model needed to read.

- **The repeated-failure breaker latched, and bricked Bash for the rest of the
  run.** From a live session: after two failures of the same shape, every
  subsequent Bash call — including `true`, `pwd` and `echo hello` — was refused
  in 0ms with the "environment issue" directive, long after whatever broke had
  passed. `KillShell` had nothing to kill and `Jobs` showed nothing running,
  because nothing was actually wedged.

  The streak that fires the directive was only ever cleared by a *successful*
  execution, and the directive is what prevents one — a latch, not a breaker.
  The streak is now cleared as the directive is sent, so the next call runs. The
  anti-loop property is unchanged: a tool that keeps failing rebuilds the streak
  and gets told again. Two host-gate paths already worked around the same
  deadlock locally; this fixes the general case.

- **The Bash tool now says that exit status is reported for it.** A model was
  seen appending `; echo "EXIT_STATUS: $?"` to capture a status Klaudia already
  surfaces as `[exit code N]` — which makes the shell exit 0, so the call is
  recorded as successful and the failure never reaches the loop's error
  tracking at all. The description says so, and says not to.

- **The trust posture is no longer read and written unsynchronised.**
  `HostGate.Policy` was a plain field, written by `/trust upgrade` from the
  Bubble Tea update loop and read by the gate on every tool call from the
  agent's goroutine — a data race on every session that changed posture
  mid-turn. `-race` never caught it because no test changed the policy while a
  turn was in flight. Adding the MCP trust probe gave it a third concurrent
  reader, which is what prompted looking.

  It is now an `atomic.Value` behind `Policy()`/`SetPolicy()`, so the field
  cannot be touched directly. The regression test drives all three access
  patterns at once and reports `WARNING: DATA RACE` against a plain field.

- **A paste made while Klaudia is working reaches the model.** Pasting mid-turn
  queues a steering message, and that path sent the paste *chip* rather than its
  payload: the model received the literal `[#1 pasted · 8 lines]` and nothing
  else. It then did the only thing it could and said the paste had not come
  through, which reads as a transport failure rather than a Klaudia bug — the
  reporting user and Klaudia each concluded the other end had dropped it.

  A regression, and a narrow one. Paste chips landed on 2026-08-21 with
  `promptValue()` at every submit site; mid-turn steering landed on 2026-08-24
  as a new submit path and was never taught about chips. `/goal` made it easy to
  hit rather than causing it — pasting a spec into a session that is already
  running is exactly the steering path.

  The steer box now carries both forms, as the idle prompt already did: the chip
  for the queued hint and for `↑` recall, the expansion for the agent.
  Expansion happens when the message is queued rather than when it is drained,
  because the queued branch resets the input without pushing history — the chip
  is then referenced nowhere, and the next reconcile would evict the payload out
  from under a later expansion.

  The same hole in the `!` path is fixed with it: pastes are accepted while
  idle, and a bang line submits from idle, so a pasted multi-line command was
  handed to the shell as the literal chip text.

- **Pasting into "answer in your own words" works.** The paste gate listed the
  states with an editable box, and so did `inputHeight`, and the two copies
  drifted: `stateAnsweringOther` was in one and not the other. A paste there was
  dropped entirely — no chip, no text, no message — in the one state whose
  answer is most likely to be a pasted log or diff, since it exists precisely
  because the offered options were wrong.

  Both now ask `editableInput`, and the four submit paths take their text from
  one `readInput` that returns the chip form and the expanded form together.
  Three bugs in this area were all the same mistake — a submit path reading the
  raw input and sending it — so the accessor that can give the wrong answer now
  has a single caller.

- **An enforcing session no longer prompts for every MCP call.** MCP tools were
  the one door the zone model did not reach: their intrinsic decision was `Ask`
  in every interactive mode regardless of trust, on the grounds that external
  code should never be auto-allowed by mode alone. In a session that actually
  uses MCP that produced a stream of prompts nobody could answer on the merits
  — *may `mcp__gsol__godot_game_time` run?* is not a question the user has the
  information to decide — and approving one bought a single qualified name.
  Nothing accumulated: renaming a server in `.mcp.json`, or reaching for the
  twenty-second tool on a server with sixty, started again from nothing. Rule
  matching is exact on the tool name, so `mcp__server__*` could not help.

  MCP calls now follow the trust posture, like every other tool. Enforcing acts;
  observing and off still ask. Plan mode still refuses, because that is about
  what the session is for rather than who vouches for the call, and `dontAsk`
  now proceeds when trust is enforcing instead of refusing for want of anyone to
  prompt — which is what makes MCP usable in unattended runs.

  What this gives up is documented rather than glossed: the classifier reads
  tool inputs and has no model of what an MCP server does, so an MCP call raises
  no concerns and is permitted. Following the posture means trusting the servers
  in `.mcp.json` roughly as much as the shell — the same bet configuring them
  already made. See `docs/trust.md`.

- **`ToolSearch` ranks matches instead of demanding every term.** Matching was
  a strict AND of substrings: a tool was returned only if *every* word of the
  query appeared in that one tool's name or description. Describing what you
  wanted therefore returned nothing, and the more precisely you described it the
  worse it got. With 65 Godot MCP tools loaded, `godot game time freeze runtime
  state digest` matched none of them, while the bare word `godot` matched all
  65 — including `godot_game_time`, whose own description contains *freeze*,
  *step* and *state*. The failure mode is quietly expensive: an empty result
  reads as "no such tool exists", so the obvious next move is to give up on the
  capability rather than re-word the query.

  Matching is now OR, scored and ranked, in tiers — an exact name beats a name
  substring, which beats a description substring, which beats a fuzzy
  subsequence on the name. Tools matching more of the query rank above tools
  matching it more strongly, so a specific query still lands on one tool.
  Fuzzy matching is deliberately confined to names: descriptions are long enough
  that almost any short pattern appears in them as some scattered subsequence.

  Results cap at 25, and the reply says when it truncated. Without a cap, OR
  matching would hand a broad query an entire MCP server's surface — undoing
  the context saving that deferred tools exist for.

  The ordered-subsequence scorer behind `@`-file completion moved from
  `internal/tui` to a new `internal/fuzzy` so both callers share one
  implementation, rather than the tool catalog growing a second copy. The
  domain-specific ranking stays with each caller; only the primitive is shared.

- **A reload restarts an MCP server whose process has died.** Hot reload left
  one gap: a server that crashed, or was killed from outside, was skipped by
  every subsequent reload and stayed unreachable until Klaudia restarted.
  `Reload` asked `Connected()`, which reports whether we still hold a
  `ClientSession` — and a stdio server whose child process has died keeps a
  non-nil one, because nothing nils it out. So the corpse read as "unchanged and
  running" and was left in place. Editing `.mcp.json` did nothing; the only way
  back was renaming the server's key, which made it look new rather than
  unchanged.

  Reload now probes with a 2s-bounded protocol `Ping` instead. The config
  comparison runs first, so the ping is paid only for servers that would
  otherwise have been kept, and a server that fails it is relaunched like any
  other changed one. `Connected()` keeps its cheap non-blocking meaning for
  `/mcp` and the tool wrappers, which want "is there a session" rather than "is
  the far end alive".

  Found while running two Godot MCP servers side by side, where killing a server
  by hand is routine; the regression test kills the peer and asserts the
  relaunch, and fails against the previous condition.

- **A refused host change no longer disables the tool for the rest of the turn.**
  The host gate refuses with the same text whatever the command was, so two
  refused commands looked to loop-breaker B like one error shape recurring
  across different inputs — its signature for "the environment is wedged". B
  then answers *every* later call to that tool without running it, including
  read-only ones, while reporting a shell that is perfectly healthy. Since the
  only thing that clears a streak is a successful execution, and B is what
  prevents one, the tool stayed dead until the next user message; approving the
  host change didn't help either, because approval cleared nothing.

  Gate refusals and declined asks are decisions, not malfunctions, and no longer
  feed the shape streak. Loop-breaker A now refuses with the same short-circuit
  B uses, so its own refusals can't feed B either, and a granted host change
  clears both counters for the tool.

- **A background job that exits immediately no longer races the status read.**
  `Start` called `j.status()` outside `s.mu` while the process-reaping goroutine
  wrote the same fields under it. Every other `status()` call site already held
  the lock; this one was the outlier.
- **The startup banner lists loaded skills.** Three sessions in a row could not
  establish whether skills were working, because there was no way to tell from
  outside the model: with none loaded there is no `Skill` tool to ask about, and
  with some loaded the only evidence was the model's own account of its tools.
  The banner now shows `skills: frontend-design` under the model line (first
  four names, then `+N more`), and omits the line entirely when none load —
  an empty `skills:` would read as breakage. Names come from the same list that
  backs the `/<name>` commands, so the banner cannot disagree with what will
  actually dispatch.

  Worth knowing about what is *not* automatic: a skill's name and description
  are in every request (they are the `Skill` tool's description), but its
  instructions are only loaded when the skill is invoked. That is the intended
  progressive disclosure — skill bodies run to thousands of tokens — so a model
  reporting "registered but not loaded" is describing correct behaviour.

- **Skills can be a directory: `.klaudia/skills/<name>/SKILL.md`.** The loader
  only read top-level `*.md` and skipped subdirectories before parsing
  anything, so a correctly written skill in the increasingly common
  directory-per-skill layout produced no skill, no warning, and — because the
  `Skill` tool is only registered when at least one skill loads — no tool
  either. Two sessions concluded from that that Klaudia had no skill machinery
  at all, which was the wrong lesson from true evidence.

  Both layouts now load, the directory names the skill when the frontmatter
  omits a name (`SKILL` would be useless), and a skill directory with no
  `SKILL.md` warns instead of vanishing. `skill.md` is accepted alongside
  `SKILL.md`, because a case-insensitive filesystem hides that difference until
  the skill reaches Linux.

- **Skills are read from `.claude/skills` too.** Both `~/.claude/skills` and
  `<project>/.claude/skills` are now searched, below their `.klaudia`
  equivalents, because that is where the ecosystem's installers put skills —
  the same reasoning that already has Klaudia reading `~/.claude/CLAUDE.md`.
  Precedence runs user-`.claude`, user-`.klaudia`, project-`.claude`,
  project-`.klaudia`, so a project can override an installed skill by name.

- **`/doctor` reports skills.** It lists what loaded and from which scope
  (project or user), and when nothing loaded it names the two directories to
  put skills in. This is the only place that can distinguish "no skills
  defined" from "skills are broken": with zero skills there is no `Skill` tool
  to ask about.

### Fixed
- **A long session hit `prompt is too long: 1000464 tokens > 1000000 maximum`
  and could not be resumed.** Three separate faults lined up, found from a
  four-day, 1174-record transcript in one project.

  Autocompaction never fired. Its trigger compares `EstimateTokens(messages)`
  against the window — but that only counts the message list, at ~4 chars per
  token, while the real request also carries the system prompt, the project
  instructions and every tool's JSON schema, and code tokenises closer to 3
  chars per token. For a 1M window the threshold sits at 967k, so an estimate
  running ~10% low never reaches it while the actual request sails past 1M. The
  estimate is now calibrated against the input size the API reports for each
  response — ground truth that includes the system prompt and tools — with the
  ratio only ever increasing, so a cache-heavy turn cannot undo a correction.

  The 400 was then unrecoverable: every resend is the same oversized request, so
  the session was finished. An overflow now forces a summarise-and-retry once
  per run, with the reason surfaced; a second overflow is reported rather than
  looped on.

  And the error text was unhelpful, because `isContextOverflow` matched several
  OpenAI-compatible phrasings but not Anthropic's own "prompt is too long". The
  report was a bare "Bad request (400)" with no mention of `/compact` or
  `contextWindow`.

- **A slow stream was killed as a stalled one.** Reported as a stall mid-turn,
  after text had already streamed — a healthy connection, and the second
  distinct cause behind the same message.

  The watchdog reset only when the SDK's `stream.Next()` returned a decoded
  event, and the SDK discards the API's keepalives outright (`case "ping":
  continue` in `packages/ssestream`). Anthropic sends those pings during
  exactly the pauses that matter — a large `tool_use` payload being assembled,
  extended thinking, the service under load — so a stream that was alive on the
  wire and merely slow was indistinguishable from a dead one, and got cancelled
  at 120s.

  Liveness is now measured in bytes off the socket: the transport wraps the
  response body and touches an activity tracker on every read, and the timer
  re-arms instead of firing while data is still arriving. A genuinely silent
  connection still trips it. Both are regression-tested — the ping test fails
  against the previous watchdog, and a hanging server must still stall.

  The message was misleading too: it said "auto-retried without success" even
  when no retry was attempted, which is the case whenever output has already
  been delivered (a re-issue would repeat what you just read). That path now
  says so, and points at the partial answer above it.

- **A session left idle came back with a six-minute stall.** Reconstructed from
  the transcript: 6h11m between the previous reply and the send that failed,
  then "the model connection stalled … auto-retried without success". Nothing
  was flaky, and the watchdog was doing its job — the layer below it was lying.

  The Anthropic API is HTTP/2, and Go's automatic HTTP/2 does no health
  checking by default (`http2.Transport.ReadIdleTimeout` is zero: never send a
  keepalive PING). A multiplexed connection killed by a sleep cycle, NAT or VPN
  therefore sits in the pool looking perfectly usable; the request is written
  into a black hole and the read blocks until the 120s stall watchdog fires.
  `IdleConnTimeout` does not save it, because that timer does not fire reliably
  across a sleep and the request can claim the corpse before cleanup runs. Each
  of the two retries then reused the pool and burned another 120s.

  Model calls now run on a transport with h2 keepalive pings (20s read-idle,
  10s ping timeout — both well inside the stall window, so a dead connection
  surfaces as a fast retryable error), and a stalled attempt drops idle
  connections before retrying so the next one dials.

- **An interrupted command was reported as a bare `[exit code -1]`, and got
  described back to the user as a timeout.** Interrupting a turn cancels the
  running command's context, but only `DeadlineExceeded` was recognised — a
  user cancellation fell through to a generic non-zero exit with no explanation.
  The two are indistinguishable at the shell (a killed process, no useful
  status), so the model filled the gap by guessing, and told the user a rebuild
  they had interrupted four minutes in had "hit my 15-minute tool timeout".
  Cancellation is now carried through as its own state and reported as such:
  interrupted by the user, explicitly not a timeout and not a failure of the
  command, with a note that whatever it had already done still took effect and
  the state is worth checking before re-running. Exit code 130, the convention
  for SIGINT, replaces the meaningless -1.
- **The server-tool repair deleted a web search the model was still waiting on
  (regression).** When the model calls a server tool and a client tool in the
  same batch, the API deliberately does *not* run the server tool: it returns
  `stop_reason: "tool_use"`, you return the client results, and it runs the
  search on the next request. Its `server_tool_use` therefore sits unanswered on
  purpose — and the unpaired-block repair read that as an orphan and deleted it,
  losing the search silently and breaking the request that followed. The repair
  now only treats an unanswered call as abandoned once the conversation has moved
  past that turn. Both shapes end assistant-then-user; what separates them is
  what the user message holds — only `tool_result`s means the turn is still being
  answered, ordinary text means nobody is coming back for it. Verified against
  both transcripts involved: the pending search survives untouched, and the three
  genuinely abandoned calls in the older session are still repaired.

### Added
- **A multiple-choice question always offers a way out of the multiple choice.**
  `AskUserQuestion` accepted digits `1..N` and silently swallowed every other
  key, so the only ways past a question were to pick one of its answers or kill
  the turn. That is the wrong shape for the situation the tool exists to handle:
  the *model* writes the options, so when its framing is off, a numbered list
  asks you to choose between four answers to the wrong question. The list now
  always carries one more entry the model did not write — "Something else —
  answer in your own words" — and picking it, or simply starting to type, opens
  a normal input box whose text is returned verbatim. `Esc` goes back to the
  options; the question stays live. The tool's description tells the model the
  answer may be none of its labels and to follow it as an instruction, including
  when it rejects the premise, rather than snapping it to the nearest option.
  Its result now reads "The user answered: …" instead of "chose", which is the
  truthful wording for both cases.

  Also closed while in there: answering a question whose turn had already been
  interrupted sent on a nil channel, which blocks forever and would have frozen
  the UI.
- **`input.enter` config: `Return` can insert a newline instead of sending.**
  `enter = "newline"` swaps the prompt's two chords — `Return` adds a line,
  `Alt+Return` or `Ctrl+J` sends. The default is unchanged.

  There is deliberately no `ctrl+enter` setting, because a terminal cannot
  produce one: `Return` and `Ctrl+Return` are the same byte (CR), and `Ctrl+J`
  is simply LF, which is why it has always been the newline chord. The Kitty
  keyboard protocol can disambiguate them, but Bubble Tea v1 does not parse
  those sequences — an enabled protocol would deliver `\x1b[13;5u` to the
  prompt as the literal text `13;5u`, and would also break every other `Ctrl`
  binding. So the supported route is to map the chord to `ESC` `CR` in the
  terminal, which arrives as `Alt+Return`; the README has the incantation for
  Ghostty, kitty, WezTerm, iTerm2, Windows Terminal and Apple Terminal, with
  the caveats (Windows Terminal binds `Alt+Return` to fullscreen; macOS
  Option-as-Meta costs you `é`).

  **Behaviour change:** `Alt+Return` previously sent, because the handler
  ignored the Alt modifier. It now inserts a newline in the default mode.
  `Ctrl+J` is honoured in both modes with no terminal support at all, so a
  mistyped `input.enter` (or a terminal that cannot send `Alt+Return`) can
  never leave the prompt unable to send.

  Caught while implementing: the queue-while-running path read `tea.KeyEnter`
  directly rather than going through the new decision, so in `newline` mode a
  `Return` meant to add a line queued a follow-up instead. Both paths now share
  one function, and a test asserts they agree.

- **The read-only sub-agents can reach read-only MCP tools.** `Explore` and
  `Plan` were limited to `Read`, `Glob` and `Grep`, so the two agents whose whole
  purpose is read-only fan-out research could not touch a wiki, an issue tracker
  or a chat archive. Naming MCP tools in their whitelist was never an option:
  they are discovered at connect time and differ per project.

  This is also where the context argument points. Searching four sources to
  answer one question is exactly the work that should not run in the main
  thread — a sub-agent spends its own window and hands back a summary.

  Eligibility is read from what a tool declares, not from what its agent is
  asked to do. A tool qualifies by setting the protocol's `readOnlyHint`, and a
  tool that says nothing is treated as a write, because the annotation is
  optional and the safe reading of silence is that nobody considered it. That
  matters: `mcp__gitea__delete_branch` is one connected server away from an
  agent whose only previous guarantee was a system prompt asking it not to
  write, and there is a test that fails if it ever arrives.

  For a server that annotates nothing — including one launched in its own
  read-only mode, where the operator knows something the protocol was not told —
  `"readOnly": true` on the server in `.mcp.json` says so. `"readOnly": false`
  says the opposite: `readOnlyHint` is a claim a server makes about itself and
  nothing verifies it, so an operator who does not believe a third-party server
  can decline to take its word without giving up the server, which the main
  agent keeps and still asks about before every call. Unset trusts the
  annotations. Measured against the case that prompted this: gitea-mcp annotates
  all 54 of its tools, 33 of them read-only, matching exactly what its own `-r`
  flag exposes.
- **Klaudia tells your working-tree changes apart from its own.** `/changes`
  splits them three ways: yours, Klaudia's, and *both* — a file it wrote that
  was already dirty or that you edited afterwards. Klaudia cannot merge those
  two changes, but it can refuse to pretend they are not there, which is what
  keeps undo and `/commit` honest. Startup says once how many files were already
  modified, for the case where you had forgotten.
- **`/undo`, and it cannot destroy work you did.** Before a turn writes a file,
  its contents are stored as a git blob with `git hash-object -w` — a plain
  object write that leaves your index, HEAD and `git status` untouched. A stash
  would have been simpler and is wrong: it moves the whole working tree
  including your unrelated edits. Undo shows the plan first, including the
  equivalent `git cat-file -p <sha> > path` for each file, because "undo 2
  files?" is a promise rather than an inspection. Files you also touched are
  skipped and named.
- **The completion block says what was *not* verified.** "auth tests 83/83
  passing" reads as proof until you notice the full suite was never run. A
  targeted run now names its subset and reports the gap, and files changed with
  nothing run at all says so outright.
- **`/context`, `/pin`, `/unpin`, `/forget`.** What Klaudia has read, changed
  and been working in, instead of a token percentage — with a closing line
  saying that list is what it looked at, not what the task needed. A pinned file
  is re-stated every turn, which is how it survives compaction; a file mentioned
  once forty turns ago is not really in context at all.
- **Resume reconciles the work, not just the chat.** The working tree is
  re-read, ownership is recovered from the transcript's own Write/Edit calls,
  and jobs are reported as stopped — they were children of a process that has
  exited, and the conversation you are resuming implies they are still up.
  Approvals are deliberately not restored: session-scoped means session-scoped,
  and resurrecting them would be the flaky remembered permissions the trust
  model replaced.
- **Meaningful exit codes** for headless runs: 0 done, 1 failed, 2 invoked
  wrongly, 3 hit `--max-turns`, 4 needed a host change with no way to ask, 130
  interrupted. 4 is the one worth wiring up — it distinguishes "the task needed
  a package installed" from "the model got it wrong".

### Fixed
- **A `.mcp.json` with comments loaded no servers, and said nothing.** The file
  is read with `encoding/json`, which rejects `//` and `/* */` — and this
  README documents `.mcp.json` *with* comments in two places, so the
  documented example was the broken case. Worse, `LoadConfig`'s error was
  discarded at the call site (`mcpCfg, _ := mcp.LoadConfig(cwd)`), while
  *connection* errors two lines below were reported. A stray comment therefore
  produced a session with no MCP tools, no warning, and a model that
  confidently reported the server was down — which is exactly how it was
  found, wiring a stub ledger server into a new project.

  Comments are now stripped before parsing (outside strings, so a
  `"https://…"` URL keeps its slashes), and a config that still does not parse
  is reported as `warning: mcp config: …`. Measured against a stub MCP server
  that logs every JSON-RPC request: 0 requests before the fix with a commented
  config, 3 after. Comment bytes are replaced with spaces rather than removed
  so that a parse error's offset still points at the right line.

- **A sub-agent showed nothing at all while it worked.** A twenty-minute
  research run and a hang were indistinguishable: one `⚙ Agent` line, then a
  spinner, then silence. The cause was structural rather than cosmetic — the
  child loop was started with a nil emitter (`spawner.go`), so it could not
  report even in principle, and no seam existed to carry its events out. Tools
  can now report progress through a `Progress` callback on `tools.Context`; the
  Agent tool passes it down, and the child's tool calls come back as
  `tool_progress` events rendered indented under the `Agent` line ("`↳ Read
  pricing.go`"). Only tool calls are relayed, not the child's prose — what you
  need while waiting is the shape of the work, not a second voice in the
  transcript. Progress also counts as activity, so the "quiet for…" hint no
  longer fires while a sub-agent is visibly busy.

- **A sub-agent could run unbounded, and compacted far too early.** It took
  `--max-turns` (default `0`, unlimited) — defensible for the main loop, where
  you watch each step and can interrupt, but not for a child you cannot see. It
  now defaults to a 50-turn bound and says so when it stops there, instead of
  returning a truncated answer as though it were finished. Its context window
  was also passed as `0`, falling back to the 200k compaction default, so a
  sub-agent on a 1M-token model summarised its history at a fifth of the room it
  had; it now inherits the model's real window.

- **The unpaired-server-tool repair broke valid paused searches (regression).**
  The fix below repaired a `server_tool_use` with no result, but checked each
  message on its own. A paused server tool (`stop_reason: pause_turn`) records
  its call in one assistant message and its result in the next, and the two are
  only a visible pair *after* same-role messages are merged — which happened
  later in the pipeline. So a perfectly good paused search was read as an orphan,
  its call dropped, and its result left stranded, trading one 400 for its mirror:
  ``unexpected `tool_use_id` found in `web_search_tool_result` blocks … must have
  a corresponding `server_tool_use` block before it``. The merge now runs first,
  and the pairing check is order-aware and symmetric — it drops a call with no
  result *and* a result with no preceding call, either of which the API rejects.
  Verified against the transcript that hit this: 5 unpaired blocks before, none
  after.

- **Interrupting a turn mid-web-search could brick the session.** Every
  subsequent message failed with ``messages.N: `web_search` tool use with id
  `srvtoolu_…` was found without a corresponding `web_search_tool_result`
  block``, and because the bad message stayed in history the session could be
  neither continued nor resumed. Unlike a client `tool_use` — answered by a
  `tool_result` in the *next* message, a case the sanitizer already repaired — a
  server tool's result belongs to the *same* assistant message, so an unpaired
  `server_tool_use` slipped through untouched. It arises whenever a turn is cut
  off with a search in flight, including from the `pause_turn` continuation the
  API uses for long searches, which records the paused turn before its result
  exists. The sanitizer now drops an unanswered `web_search`/`web_fetch`
  `server_tool_use` on the way out, leaving a note so the model knows the search
  was lost rather than having it vanish silently. Completed searches, and server
  tools this package doesn't model, are left untouched. Repairs already-poisoned
  transcripts on the next send, so an affected session recovers by itself.

### Changed
- **chromedp 0.15.1 → 0.16.0, cdproto 2026-04-27 → 2026-08-04.** Routine
  currency, prompted by the protocol-noise fix above: the four unhandled DOM
  events are present in every version including master, so the upgrade neither
  causes nor fixes them. Verified live on Chrome 152 — launch, navigate,
  DOM-settle wait and HTML→Markdown snapshot unchanged on two real pages, and
  chromedp logged nothing at all. The DuckDuckGo search parser returns no
  results for a headless profile with `headedFallback` off, on 0.16.0 and
  0.15.1 alike, so that is pre-existing and not from the bump.

- **Server web tools bumped to the current GA versions.** `web_search` and
  `web_fetch` now use the `20260318` tool types (were `20250305` / `20250910`),
  pinned to `allowed_callers: ["direct"]`. Direct mode is deliberate: from
  `20260209` on, the default runs search/fetch inside code execution ("dynamic
  filtering"), which nests results under a `code_execution_tool_result` — a
  different shape than the direct `web_search_result` blocks the recorder and
  citation repair handle — and 400s on pre-4.6 models. `direct` keeps the exact
  result/citation shape already round-tripped and works on every model. The
  result/citation handling is version-agnostic, so it needed no change. Dynamic
  filtering (its token-saving payoff) is a scoped follow-up: it needs
  code-execution result handling, a model-capability gate for `allowed_callers`,
  and a live smoke test. The original web-tool beta headers are kept (they are
  ignored now that the tools are GA, and dropping them is an untestable change to
  a working path).

### Fixed
- **A large tool call (e.g. writing a long file) failed with a cryptic schema
  error and then hung.** Writing a big document blew the 8192-token output cap
  mid-tool-call, so `stop_reason` was `max_tokens` and the tool's argument JSON
  arrived truncated. The stream layer patches truncated JSON to `{}` to keep the
  SDK from crashing, but the loop then dispatched that `{}` as a real call —
  `Write` with no `file_path`/`content` — surfacing "missing properties
  'file_path', 'content'" and looping as the model retried the same oversized
  write. Fixes: (1) a tool_use truncated by the output limit — `max_tokens` OR
  `model_context_window_exceeded` (Claude 4.5+ accepts input+max_tokens>context
  and stops this way instead of 400ing) — is no longer dispatched; it returns an
  actionable result telling the model its output was cut off, nothing ran, and to
  write less or continue. (2) The output cap is now the model's real maximum
  (`api.MaxOutputTokens`, doc-verified): 128000 for the 1M-context models
  (Opus 5/4.8/4.7/4.6, Sonnet 5/4.6, Fable 5), 64000 for the 200k-context models
  (Haiku 4.5, Opus 4.5, Sonnet 4.5), 8192 only for unknown/OpenAI-compatible
  models. Requesting the full cap is safe — on Claude 4.5+ an over-long request
  is accepted and stops gracefully rather than erroring. (3) A `maxTokens` config
  key overrides it (mirrors `contextWindow`), for a provider whose limit the
  table doesn't know. `turnnote` no longer points at a `--max-tokens` flag that
  never existed.

- **Chrome's protocol noise tore the frame during a browser tool.** Fetching a
  news page mid-turn printed `ERROR: unhandled node event
  *dom.EventAdRelatedStateUpdated` over the spinner row and the input box
  border, leaving the box's top edge spliced into the status line and the tool
  result. chromedp defaults its browser logger to `log.Printf`, so every message
  went to stderr — the same terminal the inline renderer is painting, with
  nothing coordinating the two.

  The first fix pointed chromedp's `WithLogf`/`WithErrorf` at a sink that
  discarded everything, which also threw away chromedp's real errors, so the
  stream is now split by cause. The noisy class is structural, not a symptom:
  chromedp routes DOM/Page events through a hand-written type switch
  (`target.go`) while cdproto's event types are generated from the protocol, so
  the switch permanently trails Chrome and everything newer than it is logged as
  an error. Against the pinned cdproto that is four DOM events —
  `AdRelatedStateUpdated`, `AdoptedStyleSheetsModified`,
  `AffectedByStartingStylesFlagUpdated`, `TopLayerElementsUpdated` — all
  carrying node state chromedp doesn't model, none actionable, and the first
  emitted once per update by any ad-carrying page. Those are dropped, matched as
  a class so the next protocol release doesn't reintroduce the tear. Everything
  else chromedp says (JSON/unmarshal failures, malformed messages, missing
  document root, executor bookkeeping) is kept in a small per-browser ring and
  attached to the error of the next browser operation that fails — `(chrome:
  …)` on the tool result, where it is worth reading. `KLAUDIA_BROWSER_LOG=<path>`
  still captures the unfiltered stream for debugging chromedp itself.

  The real fix belongs upstream, and its shape is now known: `cdp.Node` has
  `AdProvenance`, `AdoptedStyleSheets` and `AffectedByStartingStyles` fields, so
  three of the four events can be stored on the node the way the maintainer's
  own `ScrollableFlagUpdated` commit handled that one, and only
  `topLayerElementsUpdated` (no parameters, so no node) needs ignoring. A fork
  pinned by a `replace` directive was rejected because
  `go install pkg@version` ignores `replace`, so users installing from `@main`
  would silently get unpatched chromedp. Measured, not assumed: the gaps are in
  chromedp master as well as v0.15.1 and v0.16.0, against both the old and the
  current cdproto (checked by diffing cdproto's generated event types against
  the switch), so this is not us sitting on a stale dependency; no upstream
  issue or PR mentions any of the four (#1530, open since 2024, is the same
  class for an event since handled); and the unit tests build the dropped
  messages from the real cdproto types through chromedp's own format string, so
  they fail if either side changes. Not reproduced live: on Chrome
  152 none of the four fired across four ad-heavy news sites, and a killed
  Chrome logs nothing at all (chromedp only logs a read error when the JSON is
  syntactically broken) — the classification is verified from chromedp's source
  and unit tests rather than from a live capture.

  As a backstop for the next library to reach for the standard logger, the TUI
  points `log`'s output at `io.Discard` (or `KLAUDIA_LOG`) while the program
  runs, and restores the previous writer, flags and prefix on exit.

- **A web search poisoned the next turn with a 400.** After a successful search
  (the Anthropic-backend `web_search` server tool), the very next message failed
  with `citations.0.web_search_result_location.url: Value should have at least 1
  item`, and every follow-up re-sent the same bad history. Root cause was an SDK
  bug: before v1.68.0, converting a web-search citation to its request form
  copied the title and cited text but dropped the `url` and `encrypted_index`,
  both of which the API then requires back — so `ToParam()` produced an empty
  `url`. Fixed by upgrading the Anthropic SDK to v1.68.0, where the conversion
  copies both fields. A sanitize step still drops any empty-`url` web-search
  citation found in a transcript recorded by an older build (the value can't be
  recovered at send time; the annotated text is kept), so resuming an old
  session no longer 400s.

- **Typing a line that wrapped grew the input box but hid the text above it.**
  A soft-wrapping line made the box taller, yet instead of revealing the new
  wrapped row the earlier row scrolled out of view and a blank row appeared at
  the bottom. bubbles' textarea repositions its viewport during `Update` at the
  height it had *before* the box grew — and only ever scrolls toward the cursor,
  never back up — so growing the box afterwards left the view stranded past the
  top. Edits now size the textarea to its full height before the keystroke is
  applied, so a line that still fits never scrolls its top out; the box is then
  shrunk for display with the view already at the top.

- **Pasted attachments accumulated with no way to delete them.** Pasting parks
  the payload behind a short `[#N pasted · …]` chip; deleting the chip from the
  input never dropped the payload, and the counter only ever climbed, so
  repeated paste-then-delete looked like attachments piling up. The store now
  reconciles against what is actually referenced — the live input plus any
  recall-able history entry — so deleting a chip drops its attachment and the
  counter falls back to `#1` once nothing is outstanding. A chip still held by
  history is kept, so `↑` recall re-expands it exactly as before.

- **Queuing a message during a turn left a permanent hint smeared into the
  transcript.** "Klaudia will read this before its next step" was written to
  scrollback with appendLine — the immutable, terminal-owned layer — so a fresh
  copy stuck in the history every time a message was queued, on top of the live
  hint under the input that already showed the same state. Transient UI belongs
  only in the composited live region, which clears itself; it no longer touches
  scrollback. The live hint now also carries the reassurance the removed line
  had — the message is read at the next step, so it is not lost while Klaudia
  works.

- **Interrupt-to-send during a command could strand the message.** Queue a
  message, press Enter again to interrupt and send it now, and — if a tool was
  running — the message sometimes vanished: the turn cancelled but no new turn
  started, just an idle prompt. Two things drain the queue, on different
  goroutines: the agent's mid-turn steering poll and the UI's interrupt path.
  Cancelling an in-flight command completes its tool batch, which drives the
  agent straight into its post-batch poll — draining the message into the turn
  being killed, where it sat in history unanswered. Confirmed from the real
  transcript, which ended with the queued message as a trailing user turn with
  no reply. The interrupt now takes the message out of the queue before
  cancelling, so a late poll finds nothing, and the message always becomes the
  next turn.
- **Interrupting no longer loses track of running jobs.** Cancelling a turn
  kills the foreground command but leaves managed background jobs running (a dev
  server keeps its port). The resend turn now tells the model what it left up so
  it can decide whether each still matters for the new instruction — inspect,
  stop, or leave running. It does not kill them automatically; that is the
  model's call.

### Changed
- **Web tools are disambiguated and steer toward the best source.** The local
  Chrome-backed `WebSearch`/`WebFetch` are renamed `BrowserSearch`/`BrowserFetch`
  (joining `BrowserNavigate`/`BrowserSnapshot`) so they no longer shadow the
  Anthropic backend `web_search`/`web_fetch` server tools. Their descriptions now
  tell the model to prefer the built-in `web_search`/`web_fetch` when available
  (Claude models — cited, higher quality) and use the Chrome tools when it isn't
  (non-Claude providers) or when explicitly asked to drive the browser. Tool
  behaviour is unchanged; only the model-facing names and guidance moved.

- **Dependencies:** Anthropic SDK `v1.45.0 → v1.68.0` (fixes the web-search
  citation bug above; `go build`/`vet`/`test` clean, no source changes needed)
  and `invopop/jsonschema v0.13.0 → v0.14.0`. `govulncheck ./...` reports no
  vulnerabilities. `charmbracelet/bubbles`, `bubbletea`, and `lipgloss` are
  already at (or ahead of) the newest v1 releases, so there is nothing to bump on
  the v1 line. A v2 upgrade is deliberately **not** taken: v2's `textarea.
  SetHeight` calls `repositionView` (which would reintroduce the just-fixed
  wrapped-input scroll bug) and bubbletea v2 replaces the standard renderer with
  a rewrite (which would invalidate the inline reflow/last-column cursor math).
  Recorded so a future bump starts from the known incompatibilities.

- **Auto-resume skips a session that ended in a model refusal.** A refusal is
  tripped by the accumulated context, not the last message, so reviving that
  conversation makes the very next prompt — even an unrelated one — refuse too.
  Klaudia now starts fresh and says so ("Last session ended in a model refusal —
  starting fresh"), leaving the transcript on disk for an explicit --resume. Only
  the implicit pick-the-most-recent case; naming a session by --resume/--continue
  is honoured. Detected by the refusal's signature — an assistant turn with no
  content, or an older transcript's stop_reason — so it needs no new stored
  field.

### Fixed
- **A web search in the history could 400 the whole turn** with
  `web_search_tool_result.content ...: Input should be a valid array`. The
  transcript recorder marshalled the raw response message, and the SDK's
  response union types have no marshaller — so a server-tool result's content
  serialised as an object full of leaked field names
  (`OfBetaWebSearchResultBlockArray`) instead of an array. On resume that parsed
  back into an empty error block and the API rejected it. Two fixes: the
  recorder now stores the param form, which round-trips cleanly (RawJSON is not
  an option — after streaming the SDK sets it to the same leaky marshal); and
  sanitize drops an already-corrupted result and its server_tool_use before
  send, with a visible placeholder, so a transcript written before this fix
  resumes instead of failing. Verified against the real on-disk transcript that
  hit it.

- **A refused or truncated turn showed nothing but "✓ done".** Reported from a
  real session: three messages in a row got a silent completion, no answer above
  them, indistinguishable from a bug. Three stop reasons cause it — the model
  refused (`refusal`), ran out of output budget (`max_tokens`), or overflowed the
  context window — and all three rendered as dead air. Klaudia now names what
  happened and how to get out of it.

  Refusal is the one that compounds: the model's safety system can be tripped by
  earlier context rather than the latest message, so once a conversation has
  drifted into refused territory even "hi" keeps refusing — and an auto-resumed
  session brings the tainted history back with it. The note points at `/clear`
  (or `--new-session`), which is the actual way out. Headless runs no longer
  report a refusal as `success` with empty stdout: the payload carries the
  explanation and the exit code is non-zero, so a pipeline can branch on it.


### Changed
- **`/commit` stages only Klaudia-owned files.** A file it wrote that you also
  edited is left out and listed: the two changes cannot be separated without
  hunk-level surgery, and sweeping your edit into a commit describing Klaudia's
  work is the bug `/commit` already stopped doing once.

### Fixed
- **Resizing the terminal left a stack of orphaned prompt boxes.** Reported from
  a live drag-resize. Bubble Tea's inline renderer returns to the top of the live
  region with `CursorUp(linesRendered-1)`, where that count is the *logical* line
  count of the previous frame. On a resize it updates its width and calls repaint
  but does not reset the count — and the terminal has meanwhile reflowed the rows
  already on screen. A four-line region drawn at 120 columns occupies seven rows
  at 90, so the renderer moves up three when it needed six; the cursor lands
  inside the old frame, `EraseScreenBelow` clears from there, and the rows above
  survive. Every intermediate size in a drag deposits another band.

  The deficit is arithmetic, not a mystery: we rendered the previous frame, so we
  know each line's visual width, and the terminal wraps a line of width w into
  ceil(w/newWidth) rows. The next frame is now prefixed with exactly that many
  `CursorUp` plus an `EraseScreenBelow`, which puts the renderer back on the true
  top of the old frame. The escapes ride inside the frame rather than going
  straight to stdout, because the renderer owns that stream — `ansi.Truncate`
  keeps CSI sequences at zero width, which was verified rather than assumed.

  Separately, three of the four live-region lines were *exactly* the terminal
  width — measured. The renderer only appends `EraseLineRight` to lines narrower
  than the terminal, so a full-width line never erased what was to its right, and
  writing the last column parks the cursor in the pending-wrap state. The live
  region now stops one column short at every width, with a test asserting it.
- **A question could be shown with its escapes intact.** Seen in a real
  session: `On \"ask whether they want to change…\" \u2014 which did you mean?`.
  The transcript shows the model escaped its JSON twice, so one round of
  decoding — which is all that is correct — leaves backslashes as literal
  characters. Nothing was decoding it wrongly; it genuinely contained them. Short
  human-facing strings (a question, its option labels, a host-change summary) are
  now repaired on the way out, and only when the result parses cleanly as a
  quoted literal. Plans, diffs and anything multi-line or long are deliberately
  untouched: they legitimately contain backslashes, and rewriting real content
  would be worse than the bug.
- **A stale prompt box could be stranded in the middle of the conversation.**
  Seen mid-turn: box borders, "› Ask Klaudia…" and the status line sitting in
  scrollback with later tool output written across them. When `tea.Println`
  flushes new output, the renderer returns to the top of the live region with
  `CursorUp(linesRendered-1)` — a *logical* line count. Any live-region line
  that fills the terminal width is two physical rows, so the cursor lands inside
  the region and everything above it is stranded for good.

  The earlier fix stopped the input box and status line reaching the last
  column, but its test built an *idle* model — so it never rendered the two
  components that were still doing it: the streaming preview (truncated to
  exactly the width, off by one) and the approval prompts (not truncated at all;
  326 columns for a long path). The clamp now lives at one choke point covering
  the whole live region, and the test drives every state that renders something.
- **Scrollback lines longer than the terminal left residue on their last row.**
  Spotted in a screenshot of a real session: a 160-character prompt echoed at 149
  columns wrapped, and the short second row still showed "0k tokens" from the
  status line that had been there before. Bubble Tea appends EraseLineRight to a
  queued line only when it is *narrower* than the terminal, so an over-wide line
  gets none and the terminal's own wrap leaves a partial final row. Klaudia now
  wraps its own output to one column short of the terminal, making every physical
  row a line the renderer will clean up. The transcript keeps the unwrapped text,
  so /copy and /export are unchanged.
- **The status bar showed "ask" while the session was autonomous.** shortMode had
  no case for the mode added in the six-to-three collapse, so it fell through to
  the default. Of everything on that line, the mode is the one field that must
  not be wrong — it was telling the user Klaudia would stop and check while it
  was working straight through. There is now a test that every mode has its own
  label and no two share one.
- **The host gate told the model off and never told the user.** Reported from a
  live session: a blocked call printed a paragraph of policy as a red `✗`
  failure, the model quietly took another route, and the user — who might well
  have said yes — was never asked. The message instructed the model to call
  `RequestHostChange`, and the model, being a volunteer, generally didn't.

  The first instinct was to make the gate ask every time. That was wrong, and
  the sessions that prompted it prove why: both blocks were an incidental
  `2>/dev/null` and a scratch file in `/tmp`, where routing around is not a
  workaround but the correct answer. Asking would have been pure interruption,
  which is the prompt fatigue the design already avoids on purpose.

  So the split is by whether Klaudia can proceed. A block it can route around is
  a non-event: the refusal to the model is now three short sentences that prefer
  another route and mention the declaration tool second, and it draws as a muted
  `⊘ changes this machine: writes /dev/null — trying another way` rather than as
  a failure. A block it cannot route around still reaches the user, and the
  prompt grew a third answer — **(s)omething else** — because declining a host
  change usually means "not like that" rather than "give up"; it keeps the turn
  alive and the redirect lands before Klaudia's next action.

  The residual risk is the quiet one: giving up without saying so. A host change
  stopped and never approved is now named in the completion block under `Not
  done — needs your agreement`, judged at end of turn so the good path — gate
  stops it, model declares it properly, user agrees — is not misreported as
  undone.
- **`2>/dev/null` was gated as a host change.** Reported from a live session.
  Writing to a pseudo-device changes nothing about the machine, and discarding
  output is one of the commonest things a command does — a false prompt on it
  costs more than the guardrail is worth. `/dev/null`, `/dev/stdout`,
  `/dev/stderr`, `/dev/tty`, `/dev/fd/*`, `/dev/pts/*` and the random/zero
  sources are all ordinary now. Block devices deliberately are not: `dd
  of=/dev/disk2` still asks, which is the reason `/dev` is watched at all.
- **`/tmp` was a host change on macOS and not on Linux.** Found while fixing the
  above. macOS resolves `/tmp` to `/private/tmp`, and the resolved form was being
  added to the host prefixes — so writing a scratch file asked for permission on
  one machine and not the other. `/tmp` is scratch space everywhere now.
- **A dev server backgrounded with `&` bypassed the job system entirely.** Found
  by running the spec's agent-loop torture test: the model shell-backgrounded
  the server eleven times and then managed the processes by hand with `pkill`
  and `kill -9 $(lsof -ti:PORT)` — no name, no managed log, no crash detection,
  no restart. A model that already knows `&` will use `&`, and the timeout nudge
  cannot help because a self-backgrounded command returns immediately. Bash now
  refuses to detach something long-running and names `run_in_background`; the
  refusal fires only when the command both backgrounds *and* looks long-running,
  so `sleep 1 &` is nobody's business.
- **Ownership was recorded before the write, not after.** The stamp used to be
  taken from the `tool_use` event, which fires before the tool runs — so every
  file Klaudia edited looked like it had changed underneath, i.e. like *you* had
  edited it, and undo would have refused to restore its own work. A failed Write
  no longer claims a file it never wrote either.
- **Session teardown abandoned processes that were slow to stop.** KillAll sent
  SIGTERM and returned; the binary then exited, taking with it the goroutine
  that would have escalated to SIGKILL two seconds later. A server that ignores
  SIGTERM therefore kept its port forever — the exact symptom process groups
  were introduced to fix. Teardown now waits for each job to actually go. Found
  by the smoke test, which was itself checking too early.
- **A declared host change that was refused left no trace.** The guardrail only
  logged changes it *caught*, so a model that did the right thing — declared its
  intent up front and was told no — was invisible to /trust and to the exit
  code. It is recorded now, which is what makes exit 4 reliable.
- **The undo snapshot could race the write it was meant to precede.** A frontend
  learns about a tool from an event on a channel; a snapshot taken when that
  event arrives can happen after the write and capture the new contents as the
  "before". It now runs through a synchronous hook immediately before execution.

### Added
- **Long-running commands become managed jobs.** `npm run dev` used to either
  hold the agent until the timeout or vanish into an untracked process owning
  port 3000 for the afternoon. A job now has an id and a name you can say out
  loud (`npm run dev` → `dev`, derived rather than mapped from a table guessing
  at your vocabulary), a log file under `~/.klaudia/jobs/`, a port when it
  announces one, and a location — `local`, or the host it started on over ssh.
  `/jobs`, `/logs`, `/restart`, `/stopjob`, and the same reach for the model
  through `Jobs`, `BashOutput`, `RestartJob` and `KillShell`.

  **A crash is reported when it happens.** Nothing used to notice a job dying
  until something read it, so "why is the site down" got the answer "it's
  running fine".

  **Restart replaces the process in place** — same id, same name, same log, with
  a marker where the restart happened — and starting an already-running command
  hands back the existing job. Two dev servers fighting over one port is a
  confusing failure, and the loser looks like broken code.
- **`/logs` uses your pager, and follow mode cannot fight you.** `/logs <job>`
  hands `less` the real file (so `F` follows from there); `/logs -f` prints into
  the terminal's own scrollback, where scrolling up cannot be undone by new
  output arriving — there is no viewport to snap. `/logs --errors` pulls just
  the failure lines, stack traces intact, into the conversation, so a crash can
  reach the model without four thousand lines of request logging.
- **You can steer Klaudia while it works.** Typing "don't modify the API"
  mid-turn used to hold the text until the turn ended and send it afterwards, by
  which point the API had been modified. The correction now lands in the request
  that decides the next action. `/stop` asks Klaudia to finish the current step
  and report what it did — "stop after this test run" should not throw away the
  test run.
- **`!command` runs the shell without leaving the conversation**, and its output
  becomes context, so "revert that" has a referent. The input marker switches
  from `›` to `$` as soon as the line starts with `!`, so what Enter will do is
  visible before you press it.
- **A turn ends with what changed and what was verified**, kept apart on
  purpose: conflating them is how "I fixed it" comes to mean "it compiles". Only
  test runners, typecheckers, linters and vet count as verification — a passing
  `go build` is not evidence the behaviour is right. Failures stay visible with
  their count. A turn that changed nothing prints nothing.

### Changed
- **Commands run in their own process group**, and stopping one signals the
  whole group. See Fixed.
- **Children are told there is no terminal**: `GIT_TERMINAL_PROMPT=0`,
  `GIT_PAGER=cat`, `PAGER=cat`, and the real `COLUMNS`/`LINES`. Credential
  helpers are untouched. Your own `$PAGER` still drives Klaudia's long-output
  view, which has a real terminal.
- **Programs needing a terminal are refused before launch**, naming the flag
  that would have worked: `git rebase --onto`, `git add --`, `-m`,
  `ssh <host> '<command>'`. Klaudia does not allocate a PTY — one would make
  `vim` "work" in a surface the model cannot drive and you cannot see, so the
  turn would look successful while sitting in an editor forever. **Known
  limitation:** terminal resize does not reach child processes.
- **A timed-out service says so.** A bare `exit 124` reads as "the command is
  broken" and invites a retry with a longer timeout; when the command looks like
  a service, the result names `run_in_background` instead.
- **Klaudia handles SIGINT.** It had no signal handling at all, which was
  survivable only because children shared its process group. Now that they do
  not, Ctrl+C cancels the run and tears jobs down properly.

### Fixed
- **Stopping a background command left its children running.** Nothing set
  `Setpgid`, so `exec.CommandContext` signalled only the shell, not what the
  shell started — verified with a probe: a `sleep 60` grandchild survived cancel
  and Wait. That single gap is why stopping a job did not stop it, why Esc
  during a command did not end it, and why the next `npm run dev` became a
  second copy. Kill now signals the group, SIGTERM then SIGKILL after two
  seconds so a server releases its port and flushes its log rather than being
  truncated mid-sentence.
- **Background output was never written down.** It lived in a slice that only
  grew, so an afternoon's dev server held every line it ever printed in
  Klaudia's heap, none of it pageable, searchable or readable after the session.

### Added
- **Klaudia works autonomously inside the project and asks before changing your
  machine.** The old model asked per command, which produced prompt fatigue for
  ordinary work and approvals that neither covered the operation you meant nor
  stopped at its edges. Every tool call is now classified into a zone: project
  work, network fetches and work on a remote host the task calls for all proceed
  without asking — including the destructive parts, because `rm -rf ./dist` and
  `git reset --hard` are ordinary. Changes to this machine, and local credential
  material, stop.

  A few consequences are deliberate and worth knowing. Build caches under `$HOME`
  (`~/.cache`, `~/go/pkg/mod`, `~/.npm`, `~/.cargo`, `~/.m2`) are project zone,
  or every build would prompt. The rest of `$HOME` is your data and is not
  protected — this protects the machine, not the home directory — but `~/.zshrc`,
  `~/.gitconfig` and `~/Library/LaunchAgents` are host, because they configure
  your login session and persist. A project at `/opt/app` or `/usr/local/src` is
  still the project. `sudo` is not itself the trigger: `sudo -u deploy
  ./scripts/deploy.sh` in the project is project work.

  `ssh staging sudo systemctl restart nginx` is the job you asked for; the same
  line without the `ssh` is a change to the machine you are typing on. Using a
  credential (`ssh -i ~/.ssh/key`, `curl --cert`) is ordinary; printing one
  (`cat ~/.ssh/id_rsa`) is not.

  **This is a guardrail against well-intentioned mistakes, not a security
  boundary.** It reads command lines and tool inputs; it does not observe what
  programs do, so a command that computes its own target or a package's
  postinstall hook goes past it. For enforcement the kernel applies, set
  `[sandbox] mode = "os"`. See [docs/trust.md](docs/trust.md).
- **`RequestHostChange`: one approval covers a whole operation.** Klaudia
  declares what it intends to change and why, in your terms — "install nginx and
  configure it as a development proxy" rather than `sudo apt-get install -y
  nginx`. Approving it covers the package install, the config directory, the
  write, the validate and the restart. Anything outside the scope stops and says
  "this wasn't part of what you approved"; declining fails that one tool call, so
  work already done stands.

  Approving one file inside a directory covers that directory, so the second step
  of an approved change does not ask again — but approving `/etc/hosts` does not
  hand over `/etc`, a request for a whole system directory is refused before it
  reaches you, and an install grant does not authorise a removal. Approvals are
  session-scoped and never written to disk. There is no always-allow for a host
  change: a standing permission to reconfigure your machine is one you cannot see
  and did not schedule the end of.
- **`/trust`** shows the guardrail's state, the approvals live this session and
  what they reach, what the classifier has found, and any allow/deny rules
  carried over from the per-command model. `/trust revoke <id>`, `/trust revoke
  all`, `/trust upgrade`, `/trust observe`, `/trust off`.
- **`--allow-host-changes`** for unattended runs on a machine you are willing to
  have reconfigured. Without it, headless runs still do project and remote work
  and refuse host changes with a message naming the flag — rather than the old
  behaviour of denying everything that would prompt, in silence.

### Changed
- **The status line counts running jobs.** A dev server holding a port was
  invisible until the next start collided with it. Shown only when something is
  up, so it costs nothing the rest of the time — and it fills the slot the mode
  segment vacated.
- **`bypass` is styled as a warning.** In the same dim grey as the token count,
  the one mode where nothing is checking anything read as ordinary chrome.
- **The status line no longer announces the mode when nothing is unusual.** Once
  autonomous became the default it was on the line in almost every session, and
  a segment that never changes stops being read — while still outranking the
  context percentage, which is the one number there anyone acts on. `plan`,
  `bypass` and the legacy modes still appear, and still survive first when a
  narrow terminal starts dropping segments; "working normally" does not.
- **Permission modes collapse from six to three:** `autonomous` (the new
  default), `plan`, `bypassPermissions`. The old set asked you to pick a stance
  on file edits versus commands versus network, which is a question about tool
  categories and never had a good answer; zones answer it now. `default`,
  `acceptEdits` and `dontAsk` stay valid so existing configs keep working, and
  are no longer offered as a choice. `autonomous` is refused unless the host
  guardrail is enforcing — without it, it would be `bypassPermissions` under
  another name.
- **A config that already has `[permissions]` rules starts in observe mode**,
  with a one-time notice: the classifier runs and `/trust` reports what it found,
  but nothing is refused and your per-action prompts continue until you run
  `/trust upgrade`. Existing allow/deny rules keep working; Klaudia no longer
  creates new ones.
- **`--loop` no longer requires `--dangerously-skip-permissions`.** It needed it
  only because no mode would edit a file in the project without asking, so
  running unattended meant turning off every check to get past prompts about
  ordinary work. Use `--permission-mode autonomous`.
- **`/commit` stages what Klaudia changed, not everything.** It ran `git add -A`,
  which swept up the half-finished change you left open in another editor and the
  scratch file you meant to delete, into a commit whose message described
  something else. It now stages only the files this session edited, lists what it
  is leaving out, and — if you staged something yourself — commits exactly that
  and adds nothing on top.
- **Sub-agents share the parent's guardrail and its approvals.** A sub-agent must
  not be a way around the boundary, and an approval you gave the parent should
  cover the child doing the work.

### Fixed
- **The Bash parser did not see output redirections**, so `echo x > /etc/hosts`
  looked like a harmless `echo`. It also silently fabricated paths from partial
  expansions — `> "$HOME/notes.txt"` was recorded as a write to `/notes.txt`, an
  absolute path that looks real and is not — and dropped whole commands whose
  program name came from a variable (`$SUDO apt-get install`). Words now carry
  whether their text is the whole story, and anything deciding what a command
  touches has to check.
- **Tools ran in Klaudia's process directory, not the project.** `WorkingDir` was
  declared and never set, so `cmd.Dir` was never assigned and Grep/Glob rooted
  themselves wherever Klaudia happened to start.
- **Sandbox roots were compared unresolved**, so on macOS a write to `/etc/foo`
  never matched the policy prefix `/private/etc` and looked harmless.

### Changed
- **The input is drawn in a box, and the status line is its caption.** A dim
  full-width "model · mode · turns · tokens" line reads as a status bar, and
  status bars belong pinned to the bottom of a window — so inline rendering,
  which leaves it wherever the cursor is, made it look misplaced. The position
  was never the problem: the line had nothing visible to belong to. Framing the
  input gives it one. The caption also drops whole segments rather than
  characters when the terminal is narrow, so it degrades to "opus-5 · ask"
  instead of "opus-5 · ask · 0 tur", and the box gives way to a bare input below
  30 columns or 10 rows, where the border costs more than it buys.
- **The default model is now `claude-opus-5`** (was `claude-sonnet-4-6`), and
  the `opus`/`sonnet` aliases track the current lineup (`claude-opus-5` /
  `claude-sonnet-5`); `fable` is added for `claude-fable-5`.
- **The TUI renders inline instead of taking over the screen.** Klaudia used the
  alternate screen with a custom viewport, which hid the shell scrollback you
  launched from, denied the terminal's own search and selection over the
  conversation, threw the session away on exit, and — measured — performed worse
  than the pager it replaced: re-wrapping the whole transcript on every streamed
  token cost 102µs at 10 turns, 476µs at 50 and 1880µs (plus 2MB of garbage) at
  200. Finished output now goes into real scrollback via `tea.Println` and only
  the input and status bar are redrawn in place, so scrolling, drag-to-select,
  terminal search, tmux copy mode and output-survives-exit all work again.
  Per-token cost is now flat in session length and allocation-free (~130ns).
  `PgUp`/`PgDn` and mouse capture are gone rather than reimplemented; `/theme`
  applies to new output only, since printed text cannot be restyled.
- **`Ctrl+C` no longer quits on the first press.** It now does the smallest
  useful thing available — interrupt a running turn, cancel an open prompt,
  clear a non-empty draft — and quits only when pressed twice in a row.
- **`/last` takes an argument** (`/last <n>`, `/last list`) and opens long output
  in `$PAGER` rather than printing it, so paging, search and copy are the
  pager's job.

### Added
- **Model discovery in `/model`.** With no argument it now asks the provider
  which models it actually serves and offers them as a picker, instead of
  requiring you to type an exact ID from memory. Both backends answer at
  `GET /v1/models` — Anthropic via the SDK (with the OAuth beta header the rest
  of the client sends), OpenAI-compatible endpoints by convention — exposed as
  an optional `api.ModelLister` so a future backend that can't enumerate still
  satisfies `api.Provider` and simply falls back to type-the-ID. Selecting a
  model also records the context window the provider reports for it.
- **`/copy`** — put the last answer, a code block, a tool result or the whole
  conversation on the system clipboard using OSC 52, so it works over SSH and
  inside tmux. It copies from raw sources, never from rendered output.
- **`/search`, `/outline`, `/errors`, `/show`** — index the session and report
  matches. Inline rendering means the app can't move the terminal's scroll
  position, so these print results rather than jumping.
- **`/open <path:line>`** — open a reference copied from a stack trace or
  compiler error at the right line in `$EDITOR`.
- **A `light` theme**, and `NO_COLOR` support. Every previous theme assumed a
  dark background, and glamour was hardcoded to a TrueColor profile that ignored
  `NO_COLOR` entirely.

### Fixed
- **Pasting is byte-exact.** bubbles' textarea sanitises all input, replacing
  tabs with four spaces and mapping `\r` and `\n` to newlines *independently* —
  so CRLF text arrived with every line doubled and pasted Go, Python or Makefile
  source silently lost its tabs. Pastes are now intercepted before the widget
  sees them; large or tab-bearing ones are stored verbatim and shown as a chip
  (`[#1 pasted · 42 lines]`) that expands on submit, which also stops a
  thousand-line paste from swamping the six-row input box.
- **Rendered code copies as source.** Glamour padded every line to the wrap
  width and indented code blocks, so a copied snippet carried a four-space
  prefix and dozens of trailing spaces. Margins and block backgrounds are
  dropped and the residual padding is stripped from the rendered string.
- **Tabs survive.** lipgloss expanded them to four spaces everywhere, destroying
  the column structure of anything tabular a tool printed.
- **Earlier tool output is recoverable.** Only the single most recent result was
  kept, so a long build log became unreachable as soon as any later tool ran. A
  bounded ring now holds the last 200 results, and the inline preview names the
  number to ask for.
- **Tool-result previews are rune-safe.** They were truncated with a byte slice
  that cut multi-byte characters in half and printed replacement characters.
- **`@` completion finds nested files.** It was prefix-only, so `@session.ts`
  could never match `src/auth/session.ts`. It is now fuzzy, caches the repo
  listing instead of re-walking it on every Tab keystroke, and ranks files
  Klaudia recently read or wrote first — the previous code discarded
  `search.Glob`'s mtime ordering by re-sorting alphabetically.
- **The model and the UI no longer share one truncated string.** `tools.Result`
  gained a `Full` field (surfaced as `agent.Event.FullContent`) carrying the
  untruncated output for local display only. Bash clamps what the model sees to
  protect the context window; `/last` now shows everything the command actually
  printed — 245 KB where the model saw 30 KB, in the end-to-end test — and says
  so. This also removes the TUI's coupling to a magic string in the tool's own
  output: it was parsing the spill-file path back out of the notice. That notice
  remains, because it is what lets the *model* read the elided middle.
- **Bash output keeps its tail.** Output over 30 KB was truncated head-only, so
  the part people actually want — the `FAIL` summary from `go test ./...`, the
  error that stopped a build, the end of a stack trace — was thrown away, for
  the model as well as the user. The same budget is now split head+tail, cut on
  line boundaries, and the untruncated text is written to `~/.klaudia/outputs/`
  and named in the notice, so `/last` shows the complete log and the model can
  grep it. Spill files are pruned after 24 hours. The old truncation also
  sliced bytes, which could cut a multi-byte character in half.
- **Context-window reporting was understating the window ~5×.** The static
  per-model table claimed 200K for the current lineup, on the theory that 1M
  needed the `context-1m-2025-08-07` beta that `DefaultBetas` doesn't send.
  `GET /v1/models` reports 1M for those models, so the status bar's `ctx N%`
  had been overstating context pressure accordingly. The table is corrected
  against the live endpoint, and `/model` now prefers the provider's own figure
  over any table at all. `humanTokens` gained an M tier — a 1M window rendered
  as the unreadable "1000.0k".
- **`Ctrl+U` and `Ctrl+D` reach the input.** They were bound to viewport paging
  and matched before the textarea saw them, so readline's kill-line and
  delete-forward never worked.

### Added
- **Prompt caching (Anthropic).** Requests now set `cache_control` breakpoints on
  the stable prefix (tools + system prompt) and a rolling conversation
  breakpoint, so each turn no longer re-pays full price for the whole history.
  Because the system prompt and tools are byte-stable across launches, this also
  caches across `--continue`/resume (verified live: the resumed turn reads the
  whole prior prefix from cache rather than re-creating it). Cache usage is
  reported in the result envelope (`cache_read_input_tokens` /
  `cache_creation_input_tokens`). Disable with `KLAUDIA_DISABLE_PROMPT_CACHE`.

### Fixed
- **Tool order is now stable.** `Registry.Names()` iterated a map, so the tool
  list (and thus the request's cached prefix) was shuffled every turn — which by
  itself defeated prompt caching. Names are now sorted; verified live to take
  cache reads from 0 to most of the prefix.
- **Streaming no longer hangs forever.** A stalled model stream (half-open SSE
  connection) used to leave the TUI stuck on "thinking…". An idle watchdog now
  breaks a stalled turn — transparently retrying when nothing has been emitted
  yet, otherwise failing with a clear timeout. Configurable via
  `KLAUDIA_STREAM_IDLE_TIMEOUT` (seconds, default 120; `0` disables). Covers both
  the Anthropic and OpenAI-compatible providers.
- **Long-context-credits 429** now reports honestly: the *"Usage credits are
  required for long context requests"* error is a billing gate, so the message no
  longer suggests retrying — it points at adding credits or reducing context.
- **Session resume "lost memory".** Auto-resume could land on an empty transcript
  left by a launch that recorded nothing (quit before typing, or every turn
  errored on an expired token), reporting "resumed" with no context. `MostRecent`
  now skips contentless transcripts, and transcripts are created lazily on first
  write so aborted launches leave no file behind.
- **Markdown files rendered as soup.** Reading a `.md` file in the TUI ran its
  `cat -n` output through the Markdown renderer, collapsing every line into one
  reflowed block with inlined line numbers. Line-numbered tool output is now
  shown verbatim.

### Changed
- Docs: corrected the prompt-caching status in `docs/parity.md` (planned, not
  implemented) and documented the streaming/reliability knobs in the README.

package subagent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load returns the built-in types plus the sub-agents defined as markdown
// files, the way Claude Code and its plugins define them. It reads cwd only;
// a session calls LoadAll so the project root and the extra directories are
// read too.
func Load(cwd string, warn func(string)) []Type {
	return LoadAll(cwd, "", nil, nil, warn)
}

// LoadAll reads definitions in increasing precedence: ~/.claude/agents,
// ~/.klaudia/agents, the project root's .claude/agents and .klaudia/agents,
// then the same two under cwd when the session started in a subdirectory,
// then each extra directory. A later definition replaces an earlier one of
// the same name, a built-in included. root equal to cwd, or an extra equal to
// either, is read once.
//
// known is the tool names available at startup. A definition naming one that
// is not among them is warned about — the phrasing says "not available at
// startup", because an MCP tool connects later and is not a typo — but the
// name is kept, so a tool that appears after connect is still grantable. A
// nil known skips the check.
//
// A definition narrows what a sub-agent may do — a prompt and a tool
// allowlist — and runs under the parent's permissions, so project
// definitions are read like CLAUDE.md is.
func LoadAll(cwd, root string, extra, known []string, warn func(string)) []Type {
	byName := map[string]Type{}
	order := []string{}
	put := func(t Type) {
		key := normalize(t.Name)
		if _, ok := byName[key]; !ok {
			order = append(order, key)
		}
		byName[key] = t
	}
	for _, t := range Builtin() {
		put(t)
	}
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".claude", "agents"), filepath.Join(home, ".klaudia", "agents"))
	}
	add := func(base string) {
		if base == "" {
			return
		}
		dirs = append(dirs, filepath.Join(base, ".claude", "agents"), filepath.Join(base, ".klaudia", "agents"))
	}
	add(root)
	if root == "" || !samePath(root, cwd) {
		add(cwd)
	}
	for _, d := range extra {
		if samePath(d, cwd) || samePath(d, root) {
			continue
		}
		add(d)
	}
	knownSet := map[string]bool{}
	for _, k := range known {
		knownSet[k] = true
	}
	seenIgnored := map[string]bool{}
	for _, dir := range dirs {
		files, _ := filepath.Glob(filepath.Join(dir, "*.md"))
		sort.Strings(files)
		for _, f := range files {
			t, ignored, err := parseAgentFile(f)
			if err != nil {
				if warn != nil {
					warn(fmt.Sprintf("agent %s: %v", f, err))
				}
				continue
			}
			if warn != nil {
				for _, key := range ignored {
					if !seenIgnored[key] {
						seenIgnored[key] = true
						warn(fmt.Sprintf("agent frontmatter key %q is ignored", key))
					}
				}
				if known != nil {
					for _, name := range t.unknownNames(knownSet) {
						warn(fmt.Sprintf("agent %s: tool %q is not available at startup", f, name))
					}
				}
			}
			put(t)
		}
	}
	out := make([]Type, 0, len(order))
	for _, k := range order {
		out = append(out, byName[k])
	}
	return out
}

type agentFrontmatter struct {
	Name            string `yaml:"name"`
	Description     string `yaml:"description"`
	Tools           any    `yaml:"tools"`
	Model           string `yaml:"model"`
	MaxTurns        int    `yaml:"maxTurns"`
	DisallowedTools any    `yaml:"disallowedTools"`
	Isolation       string `yaml:"isolation"`
}

func parseAgentFile(path string) (Type, []string, error) {
	fail := func(err error) (Type, []string, error) { return Type{}, nil, err }
	data, err := os.ReadFile(path)
	if err != nil {
		return fail(err)
	}
	s := strings.TrimPrefix(string(data), "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return fail(fmt.Errorf("no frontmatter (want a --- block with name and description)"))
	}
	rest := s[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fail(fmt.Errorf("unterminated frontmatter"))
	}
	raw := rest[:end]
	var fm agentFrontmatter
	if err := yaml.Unmarshal([]byte(raw), &fm); err != nil {
		// Agent files are written as "key: text" with prose in the text:
		// "description: Use this agent when …" followed by unindented
		// "Context: …" / "user: …" example lines. That is not YAML, and most
		// of the agents shipped in Claude Code's own plugins are written so.
		lenient, ok := lenientAgentFrontmatter(raw)
		if !ok {
			return fail(fmt.Errorf("invalid frontmatter: %w", err))
		}
		fm = lenient
	}
	body := rest[end+len("\n---"):]
	if nl := strings.IndexByte(body, '\n'); nl >= 0 {
		body = body[nl+1:]
	} else {
		body = ""
	}
	if strings.EqualFold(strings.TrimSpace(fm.Model), "inherit") {
		fm.Model = "" // Claude Code's spelling of "the parent's model"
	}
	name := strings.TrimSpace(fm.Name)
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	if strings.TrimSpace(fm.Description) == "" {
		return fail(fmt.Errorf("missing description (the model chooses agents by it)"))
	}
	isolation, ok := parseIsolation(fm.Isolation)
	if !ok {
		return fail(fmt.Errorf("isolation %q is not one of auto, worktree, shared (none is accepted as shared)", fm.Isolation))
	}
	if fm.MaxTurns < 0 {
		return fail(fmt.Errorf("maxTurns must be positive, got %d", fm.MaxTurns))
	}
	t := Type{
		Name:            name,
		Description:     strings.TrimSpace(fm.Description),
		SystemPrompt:    strings.TrimSpace(body),
		Model:           strings.TrimSpace(fm.Model),
		Tools:           []string{"*"},
		MaxTurns:        fm.MaxTurns,
		DisallowedTools: toolList(fm.DisallowedTools),
		Isolation:       isolation,
	}
	switch v := fm.Tools.(type) {
	case nil:
	case string:
		t.Tools = splitTools(v)
	case []any:
		t.Tools = nil
		for _, x := range v {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				t.Tools = append(t.Tools, strings.TrimSpace(s))
			}
		}
	default:
		return fail(fmt.Errorf("tools must be a list or a comma-separated string"))
	}
	if len(t.Tools) == 0 {
		t.Tools = []string{"*"}
	}
	if t.SystemPrompt == "" {
		return fail(fmt.Errorf("empty body (the body is the agent's system prompt)"))
	}
	// A type whose granted tools can only read gets the MCP access the
	// built-in read-only types get. A wildcard grants whatever the session
	// connected, writers included, so it does not qualify.
	if !t.MayWrite() {
		t.ReadOnlyMCP = true
	}
	return t, ignoredKeys(raw), nil
}

// parseIsolation normalises a frontmatter isolation value. The vocabulary
// itself — which value isolates and which shares the tree — belongs to the
// spawner (chunk 12). This only rejects a value that is not in it, and folds
// the legacy "none" spelling onto "shared" so a file written either way loads.
// Empty and "auto" stay empty, which is "follow the session setting".
func parseIsolation(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "auto":
		return "", true
	case "worktree":
		return "worktree", true
	case "shared", "none":
		return "shared", true
	default:
		return "", false
	}
}

// samePath reports whether two directory paths name the same place, by a
// cleaned comparison rather than a stat, so a missing extra dir is not an error.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// honouredKeys are the frontmatter keys Klaudia reads. Anything else in a
// definition written for Claude Code is reported once and otherwise ignored.
var honouredKeys = map[string]bool{
	"name": true, "description": true, "tools": true, "disallowedtools": true,
	"model": true, "maxturns": true, "isolation": true,
	// color is decorative and has been silently skipped since agent files
	// were first read, so it stays quiet rather than warning on every file.
	"color": true,
}

// ignoredKeys lists the frontmatter keys this file sets that Klaudia does not
// read, in the order they appear, without duplicates.
func ignoredKeys(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(raw, "\n") {
		key, _, found := strings.Cut(line, ":")
		if !found || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "-") {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(key))
		if k == "" || !agentKeys[k] || honouredKeys[k] || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, strings.TrimSpace(key))
	}
	return out
}

// agentKeys are the frontmatter keys an agent file uses. A line starting with
// one begins a new field; any other line continues the field before it.
var agentKeys = map[string]bool{
	"name": true, "description": true, "tools": true, "model": true,
	"maxturns": true, "disallowedtools": true, "isolation": true,
	"color": true, "permissionmode": true, "skills": true, "hooks": true,
	"background": true, "memory": true, "effort": true,
}

// lenientAgentFrontmatter reads an agent file's frontmatter as "key: value"
// lines, joining every line that does not start a known key to the value
// before it. ok is false when no known key was found.
func lenientAgentFrontmatter(fm string) (agentFrontmatter, bool) {
	vals := map[string]string{}
	var last string
	for _, line := range strings.Split(fm, "\n") {
		if key, val, found := strings.Cut(line, ":"); found && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			k := strings.ToLower(strings.TrimSpace(key))
			if agentKeys[k] {
				last = k
				vals[last] = strings.TrimSpace(val)
				continue
			}
		}
		if last != "" {
			vals[last] += "\n" + line
		}
	}
	if len(vals) == 0 {
		return agentFrontmatter{}, false
	}
	out := agentFrontmatter{
		Name:        strings.TrimSpace(vals["name"]),
		Description: strings.TrimSpace(vals["description"]),
		Model:       strings.TrimSpace(vals["model"]),
		Isolation:   strings.TrimSpace(vals["isolation"]),
	}
	if t := strings.Trim(strings.TrimSpace(vals["tools"]), "[]"); t != "" {
		out.Tools = t
	}
	if t := strings.Trim(strings.TrimSpace(vals["disallowedtools"]), "[]"); t != "" {
		out.DisallowedTools = t
	}
	if m := strings.TrimSpace(vals["maxturns"]); m != "" {
		n, err := strconv.Atoi(m)
		if err != nil {
			return agentFrontmatter{}, false
		}
		out.MaxTurns = n
	}
	return out, true
}

func splitTools(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// normalize folds case and separators, so "explore", "Explore" and
// "general_purpose" name the same agents as "Explore" and "general-purpose".
func normalize(name string) string {
	return strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(name)))
}

// Find returns the type in types matching name, ignoring case and separators.
func Find(types []Type, name string) (Type, bool) {
	key := normalize(name)
	for _, t := range types {
		if normalize(t.Name) == key {
			return t, true
		}
	}
	return Type{}, false
}

// toolList reads a frontmatter field that may be one string or a list of them.
func toolList(v any) []string {
	switch x := v.(type) {
	case string:
		return splitTools(x)
	case []any:
		var out []string
		for _, e := range x {
			if n, ok := e.(string); ok && strings.TrimSpace(n) != "" {
				out = append(out, strings.TrimSpace(n))
			}
		}
		return out
	default:
		return nil
	}
}

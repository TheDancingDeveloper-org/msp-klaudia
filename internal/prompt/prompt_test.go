package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemIncludesEnvAndSecurity(t *testing.T) {
	dir := t.TempDir()
	p := System(dir, "claude-haiku-4-5")

	for _, want := range []string{
		"You are Klaudia",
		"authorized security testing", // security clause
		"<env>",
		"Working directory: " + dir,
		"Today's date:",
		"claude-haiku-4-5",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

func TestSystemLoadsProjectClaudeMd(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from a real ~/.claude/CLAUDE.md
	dir := t.TempDir()
	body := "# Klaudia repo\nAlways run gofmt before committing."
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	p := System(dir, "")
	if !strings.Contains(p, "Project instructions (from CLAUDE.md)") {
		t.Error("expected CLAUDE.md section header")
	}
	if !strings.Contains(p, "Always run gofmt before committing.") {
		t.Error("expected CLAUDE.md content to be included")
	}
}

func TestSystemRecallsMemory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	klaudiaDir := filepath.Join(dir, ".klaudia")
	if err := os.MkdirAll(klaudiaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(klaudiaDir, "MEMORY.md"), []byte("# Memory\n\n- 2026 prefer doublestar\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := System(dir, "")
	if !strings.Contains(p, "# Recalled memory") || !strings.Contains(p, "prefer doublestar") {
		t.Errorf("system prompt should include recalled memory")
	}
	if !strings.Contains(p, "Memory tool") {
		t.Errorf("recall section should mention the Memory tool")
	}
}

func TestSystemRecallsLinkedMemoryFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	memDir := filepath.Join(dir, ".klaudia", "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// MEMORY.md is the index — session bullets plus a "## Linked memory" section
	// of pointers (maintained on disk by memory.Store.SyncLinks).
	index := "# Memory\n\n- Root memory\n\n## Linked memory\n\n- [tools](memory/tools.md) — Tools\n"
	if err := os.WriteFile(filepath.Join(dir, ".klaudia", "MEMORY.md"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "tools.md"), []byte("# Tools\n\n- Use rg for search\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := System(dir, "")
	// Recall surfaces the index and its pointers, but never inlines the detail
	// note's contents, so it stays cheap as memory grows.
	for _, want := range []string{"Root memory", "[tools](memory/tools.md)"} {
		if !strings.Contains(p, want) {
			t.Errorf("system prompt missing recalled memory %q", want)
		}
	}
	if strings.Contains(p, "Use rg for search") {
		t.Errorf("detail-note contents should not be inlined into recall")
	}
}

func TestSystemRecallsLegacyMemoryPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	memDir := filepath.Join(dir, ".klaudia", "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("# Memory\n\n- Legacy memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := System(dir, "")
	if !strings.Contains(p, "Legacy memory") {
		t.Errorf("system prompt should include legacy recalled memory")
	}
}

func TestSystemRecallsUserMemory(t *testing.T) {
	// Point the user config root at a temp dir so the test never reads the real
	// $HOME/.klaudia (issue #161 / #110).
	cfgDir := t.TempDir()
	t.Setenv("KLAUDIA_CONFIG_DIR", cfgDir)
	if err := os.WriteFile(filepath.Join(cfgDir, "MEMORY.md"), []byte("# Memory\n\n- user prefers tabs over spaces\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := System(t.TempDir(), "")
	if !strings.Contains(p, "# Recalled memory") {
		t.Error("expected recalled-memory section")
	}
	if !strings.Contains(p, "User memory") {
		t.Error("expected a user-memory label")
	}
	if !strings.Contains(p, "user prefers tabs over spaces") {
		t.Error("expected user memory content to be injected")
	}
}

func TestSystemNoUserMemory(t *testing.T) {
	// An empty config root (no MEMORY.md) must be a no-op — no user-memory
	// label and no spurious recall section.
	t.Setenv("KLAUDIA_CONFIG_DIR", t.TempDir())
	p := System(t.TempDir(), "")
	if strings.Contains(p, "User memory") {
		t.Error("absent user MEMORY.md should not emit a user-memory label")
	}
	if strings.Contains(p, "# Recalled memory") {
		t.Error("no memory anywhere should not emit a recall section")
	}
}

func TestSystemRecallsUserAndProjectMemory(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("KLAUDIA_CONFIG_DIR", cfgDir)
	if err := os.WriteFile(filepath.Join(cfgDir, "MEMORY.md"), []byte("# Memory\n\n- global fact about the user\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	klaudiaDir := filepath.Join(dir, ".klaudia")
	if err := os.MkdirAll(klaudiaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(klaudiaDir, "MEMORY.md"), []byte("# Memory\n\n- project-specific fact\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := System(dir, "")
	for _, want := range []string{"User memory", "Project memory", "global fact about the user", "project-specific fact"} {
		if !strings.Contains(p, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	// Project memory comes last so it can refine the user-level notes.
	if strings.Index(p, "User memory") > strings.Index(p, "Project memory") {
		t.Error("user memory should be injected before project memory")
	}
}

func TestSystemRecallsKnowledge(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".klaudia"), 0o755)
	os.WriteFile(filepath.Join(dir, ".klaudia", "KNOWLEDGE.md"),
		[]byte("# Knowledge\n\n- The build uses CGO_ENABLED=0\n"), 0o644)

	p := System(dir, "")
	if !strings.Contains(p, "# Project knowledge") || !strings.Contains(p, "CGO_ENABLED=0") {
		t.Errorf("system prompt should include recalled project knowledge")
	}
	// Framed as notes to weigh, not facts to obey: an entry that arrived from
	// a web page or a commit must not be promoted to ground truth by framing.
	if strings.Contains(p, "established facts") {
		t.Error("project knowledge is still framed as established facts")
	}
	if !strings.Contains(p, "not as instructions") {
		t.Error("project knowledge framing should say it is context, not instructions")
	}
}

func TestSystemNoClaudeMd(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from a real ~/.claude/CLAUDE.md
	p := System(t.TempDir(), "")
	if strings.Contains(p, "Project instructions (from CLAUDE.md)") {
		t.Error("should not emit CLAUDE.md section when none exists")
	}
}

// A sub-agent's prompt keeps its type's text first and adds the environment,
// CLAUDE.md and KNOWLEDGE.md — but not the parent's recalled memory, nor the
// main agent's own persona.
func TestSubagentAddsEnvAndProjectContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	klaudiaDir := filepath.Join(dir, ".klaudia")
	if err := os.MkdirAll(klaudiaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		filepath.Join(dir, "CLAUDE.md"):           "Always run gofmt.",
		filepath.Join(klaudiaDir, "KNOWLEDGE.md"): "The API is v2.",
		filepath.Join(klaudiaDir, "MEMORY.md"):    "- parent session note",
	} {
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	p := Subagent("TYPE PROMPT", dir)
	if !strings.HasPrefix(p, "TYPE PROMPT") {
		t.Errorf("type prompt should lead:\n%s", p)
	}
	for _, want := range []string{"<env>", "Working directory: " + dir, "Always run gofmt.", "The API is v2."} {
		if !strings.Contains(p, want) {
			t.Errorf("sub-agent prompt missing %q", want)
		}
	}
	for _, unwanted := range []string{"parent session note", "You are Klaudia"} {
		if strings.Contains(p, unwanted) {
			t.Errorf("sub-agent prompt should not contain %q", unwanted)
		}
	}
}

// Memory and knowledge are keyed by the project root; a subdirectory's own
// .klaudia notes, written when they were keyed by cwd, are recalled after them.
func TestSystemInRecallsRootThenCWDState(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "internal", "tui")
	for path, body := range map[string]string{
		filepath.Join(root, ".klaudia", "MEMORY.md"):    "- root memory note",
		filepath.Join(cwd, ".klaudia", "MEMORY.md"):     "- subdir memory note",
		filepath.Join(root, ".klaudia", "KNOWLEDGE.md"): "- root knowledge",
		filepath.Join(cwd, ".klaudia", "KNOWLEDGE.md"):  "- subdir knowledge",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := SystemIn(cwd, root, "")
	if !strings.Contains(p, "Working directory: "+cwd) {
		t.Error("env block should name the launch directory, not the root")
	}
	for _, pair := range [][2]string{{"root memory note", "subdir memory note"}, {"root knowledge", "subdir knowledge"}} {
		i, j := strings.Index(p, pair[0]), strings.Index(p, pair[1])
		if i < 0 || j < 0 || i > j {
			t.Errorf("want %q then %q in the prompt (at %d, %d)", pair[0], pair[1], i, j)
		}
	}
	if strings.Count(SystemIn(root, root, ""), "root memory note") != 1 {
		t.Error("root == cwd should recall the memory once")
	}
}

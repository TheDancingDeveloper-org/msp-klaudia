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
}

func TestSystemNoClaudeMd(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from a real ~/.claude/CLAUDE.md
	p := System(t.TempDir(), "")
	if strings.Contains(p, "Project instructions (from CLAUDE.md)") {
		t.Error("should not emit CLAUDE.md section when none exists")
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

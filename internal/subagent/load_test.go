package subagent

import (
	"github.com/greenthread-ai/klaudia/internal/tools"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAgent(t *testing.T, dir, name, body string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAgents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	cwd := t.TempDir()
	writeAgent(t, filepath.Join(home, ".claude", "agents"), "reviewer.md",
		"---\nname: reviewer\ndescription: user-level reviewer\ntools: Read, Grep\nmodel: sonnet\n---\nReview it.")
	// The project's definition of the same name wins.
	writeAgent(t, filepath.Join(cwd, ".klaudia", "agents"), "reviewer.md",
		"---\nname: reviewer\ndescription: project reviewer\n---\nReview it the project's way.")
	// Written the way Claude Code's own plugin agents are: prose with colons
	// and unindented example lines after the description.
	writeAgent(t, filepath.Join(cwd, ".claude", "agents"), "hunter.md",
		"---\nname: silent-failure-hunter\ndescription: Use this agent when reviewing error handling. Examples:\nContext: a PR adds retries.\nuser: \"check it\"\nmodel: inherit\ncolor: yellow\n---\nFind swallowed errors.")
	writeAgent(t, filepath.Join(cwd, ".claude", "agents"), "broken.md", "no frontmatter here")

	var warns []string
	types := Load(cwd, func(m string) { warns = append(warns, m) })

	rev, ok := Find(types, "Reviewer")
	if !ok || rev.Description != "project reviewer" || len(rev.Tools) != 1 || rev.Tools[0] != "*" {
		t.Errorf("reviewer = %+v, want the project definition (all tools)", rev)
	}
	h, ok := Find(types, "silent_failure_hunter")
	if !ok || !strings.Contains(h.Description, "Context: a PR adds retries.") || h.Model != "" || h.SystemPrompt != "Find swallowed errors." {
		t.Errorf("hunter = %+v", h)
	}
	if _, ok := Find(types, "explore"); !ok {
		t.Error("built-ins should still be there, found case-insensitively")
	}
	if len(warns) == 0 || !strings.Contains(warns[0], "broken.md") {
		t.Errorf("warns = %v, want one for broken.md", warns)
	}
}

// A user definition with a built-in's name replaces it.
func TestLoadOverridesBuiltin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", "") // follow the pinned HOME (hermetic.sh sets it)
	cwd := t.TempDir()
	writeAgent(t, filepath.Join(cwd, ".klaudia", "agents"), "explore.md",
		"---\nname: Explore\ndescription: our explorer\ntools: [Read]\n---\nExplore carefully.")
	types := Load(cwd, nil)
	e, _ := Find(types, "Explore")
	if e.Description != "our explorer" || len(e.Tools) != 1 {
		t.Errorf("Explore = %+v, want the override", e)
	}
	n := 0
	for _, ty := range types {
		if strings.EqualFold(ty.Name, "explore") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d Explore types, want 1", n)
	}
}

// The newer frontmatter fields round-trip, and a name that is not a real tool
// is reported rather than silently dropped.
func TestLoadNewerFrontmatter(t *testing.T) {
	dir := t.TempDir()
	agents := filepath.Join(dir, ".klaudia", "agents")
	os.MkdirAll(agents, 0o755)
	os.WriteFile(filepath.Join(agents, "reviewer.md"), []byte("---\n"+
		"name: reviewer\n"+
		"description: reviews a change\n"+
		"tools: [\"*\"]\n"+
		"maxTurns: 4\n"+
		"disallowedTools: [\"Bash\", \"NoSuchTool\"]\n"+
		"isolation: none\n"+
		"verify: test -s made.txt\n"+
		"---\n"+
		"Review it.\n"), 0o644)
	var warned []string
	got := LoadAll(dir, "", nil, []string{"Bash"}, func(m string) { warned = append(warned, m) })
	var reviewer Type
	for _, ty := range got {
		if ty.Name == "reviewer" {
			reviewer = ty
		}
	}
	if reviewer.MaxTurns != 4 || reviewer.Isolation != "none" || len(reviewer.DisallowedTools) != 2 || reviewer.Verify != "test -s made.txt" {
		t.Fatalf("frontmatter = %+v", reviewer)
	}
	joined := strings.Join(warned, "\n")
	if !strings.Contains(joined, "NoSuchTool") || !strings.Contains(joined, "not available at startup") {
		t.Errorf("warned = %v, want the unknown tool named as not available at startup", warned)
	}
}

// An open toolset that can only read gets the MCP access the built-in
// read-only types get. An explicit list is the whole grant, so naming tools
// does not widen it, and a wildcard that can write does not qualify either.
func TestReadOnlyToolsetDerivesReadOnlyMCP(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".klaudia", "agents")
	writeAgent(t, dir, "open.md", "---\nname: open\ndescription: reads\ntools: \"*\"\ndisallowedTools: [Bash, Write, Edit, MultiEdit, NotebookEdit]\n---\nRead.")
	writeAgent(t, dir, "reader.md", "---\nname: reader\ndescription: reads\ntools: Read, Grep\n---\nRead.")
	writeAgent(t, dir, "wild.md", "---\nname: wild\ndescription: anything\ntools: \"*\"\n---\nAnything.")
	types := Load(cwd, nil)
	for _, tt := range []struct {
		name string
		want bool
	}{
		{"open", true}, {"reader", false}, {"wild", false},
	} {
		got, ok := Find(types, tt.name)
		if !ok || got.ReadOnlyMCP != tt.want {
			t.Errorf("%s ReadOnlyMCP = %v (found %v), want %v", tt.name, got.ReadOnlyMCP, ok, tt.want)
		}
	}
}

// A value outside the vocabulary fails the file, with a warning, rather than
// being stored and acted on later.
func TestUnknownIsolationFailsTheFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	cwd := t.TempDir()
	writeAgent(t, filepath.Join(cwd, ".klaudia", "agents"), "odd.md",
		"---\nname: odd\ndescription: odd\nisolation: somewhere\n---\nGo.")
	var warned []string
	types := Load(cwd, func(m string) { warned = append(warned, m) })
	if _, ok := Find(types, "odd"); ok {
		t.Fatal("a file with an unknown isolation value was loaded")
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "isolation") {
		t.Errorf("warned = %v, want one isolation warning", warned)
	}
}

// A key Klaudia does not read is reported once across every file that sets it.
func TestIgnoredKeyWarnedOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".klaudia", "agents")
	body := func(name string) string {
		return "---\nname: " + name + "\ndescription: d\npermissionMode: acceptEdits\n---\nGo."
	}
	writeAgent(t, dir, "a.md", body("alpha"))
	writeAgent(t, dir, "b.md", body("beta"))
	var warned []string
	Load(cwd, func(m string) { warned = append(warned, m) })
	n := 0
	for _, m := range warned {
		if strings.Contains(m, "permissionMode") && strings.Contains(m, "ignored") && strings.Contains(m, "a.md") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("permissionMode warned %d times (%v), want once", n, warned)
	}
}

// A misspelled key is the same class of mistake as an unknown tool name: it is
// reported, naming the file, rather than silently doing nothing.
func TestMisspelledKeyIsWarned(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	cwd := t.TempDir()
	writeAgent(t, filepath.Join(cwd, ".klaudia", "agents"), "typo.md",
		"---\nname: typo\ndescription: d\nmaxturn: 3\ntool: Read\n---\nGo.")
	var warned []string
	types := Load(cwd, func(m string) { warned = append(warned, m) })
	if _, ok := Find(types, "typo"); !ok {
		t.Fatal("a typo in a key rejected the file")
	}
	joined := strings.Join(warned, "\n")
	for _, key := range []string{"maxturn", "tool"} {
		if !strings.Contains(joined, key) || !strings.Contains(joined, "typo.md") {
			t.Errorf("warned = %v, want %q named with the file", warned, key)
		}
	}
}

// A non-integer maxTurns names the field instead of quoting the YAML parser.
func TestMaxTurnsMustBeAnInteger(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	cwd := t.TempDir()
	writeAgent(t, filepath.Join(cwd, ".klaudia", "agents"), "bad.md",
		"---\nname: bad\ndescription: d\nmaxTurns: a few\n---\nGo.")
	var warned []string
	Load(cwd, func(m string) { warned = append(warned, m) })
	if len(warned) != 1 || !strings.Contains(warned[0], "must be an integer") {
		t.Errorf("warned = %v, want a maxTurns integer error", warned)
	}
}

// The project root is read even when the session started in a subdirectory,
// and an extra directory is read too. A root equal to cwd is read once.
func TestLoadAllReadsRootAndExtra(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KLAUDIA_CONFIG_DIR", "")
	root := t.TempDir()
	cwd := filepath.Join(root, "sub")
	os.MkdirAll(cwd, 0o755)
	extra := t.TempDir()
	writeAgent(t, filepath.Join(root, ".klaudia", "agents"), "fromroot.md",
		"---\nname: fromroot\ndescription: at the root\n---\nRoot.")
	writeAgent(t, filepath.Join(cwd, ".klaudia", "agents"), "fromcwd.md",
		"---\nname: fromcwd\ndescription: in the subdir\n---\nCwd.")
	writeAgent(t, filepath.Join(extra, ".klaudia", "agents"), "fromextra.md",
		"---\nname: fromextra\ndescription: extra\n---\nExtra.")

	got := LoadAll(cwd, root, []string{extra}, nil, nil)
	for _, name := range []string{"fromroot", "fromcwd", "fromextra"} {
		if _, ok := Find(got, name); !ok {
			t.Errorf("LoadAll missed %s", name)
		}
	}
	// The project's own definition wins over an extra directory's.
	writeAgent(t, filepath.Join(extra, ".klaudia", "agents"), "fromroot.md",
		"---\nname: fromroot\ndescription: the extra one\n---\nExtra wins?")
	again := LoadAll(cwd, root, []string{extra}, nil, nil)
	if fr, ok := Find(again, "fromroot"); !ok || fr.Description != "at the root" {
		t.Errorf("fromroot = %+v, want the project's definition", fr)
	}

	writeAgent(t, filepath.Join(root, ".klaudia", "agents"), "twice.md",
		"---\nname: twice\ndescription: once\n---\nOnce.")
	var n int
	for _, ty := range LoadAll(root, root, nil, nil, nil) {
		if ty.Name == "twice" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("root equal to cwd loaded twice %d times", n)
	}
}

func TestUnknownToolsReported(t *testing.T) {
	reg := tools.NewRegistry(stubTool{name: "Read"}, stubTool{name: "Bash"})
	unknown := (Type{Tools: []string{"Read", "Grep"}, DisallowedTools: []string{"Nope"}}).UnknownTools(reg)
	if len(unknown) != 2 {
		t.Fatalf("unknown = %v, want Grep and Nope", unknown)
	}
}

package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skipRepo is the reproduction fixture from issue #283: a git repository with
// a CI workflow under .github, a gitignored build directory, and an untracked
// file. Only the marker directory .git is needed — the walk detects a
// repository by it, and nothing here asks git anything.
func skipRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		".git/HEAD":                "ref: refs/heads/main\n",
		".gitignore":               "target/\n",
		".github/workflows/ci.yml": "jobs:\n  test:\n    run: make needle-ci\n",
		".vscode/settings.json":    "{}\n",
		"target/out.txt":           "needle-built\n",
		"src/main.go":              "package main\n",
		"notes.txt":                "needle-untracked\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Every row of the issue's table, plus the opt-ins it proposed. "found" is
// whether the needle's file appears; "note" is what the result must say about
// where it did not look, and noteless rows must say nothing.
func TestGrepHiddenAndIgnoredRows(t *testing.T) {
	dir := skipRepo(t)
	g, _ := NewGrep()
	cases := []struct {
		name   string
		in     GrepInput
		found  string // the file that must be listed, or "" for no matches
		note   []string
		absent []string // what the note must not mention
		quiet  bool     // nothing the search could match was skipped: no note at all
	}{
		{
			name: "workflow content from the repo root is skipped, and says so",
			in:   GrepInput{Pattern: "needle-ci"},
			note: []string{"No matches found", "hidden: .github/, .vscode/", "ignored: target/", "hidden: true / no_ignore: true"},
		},
		{
			name:  "a glob naming .github finds it",
			in:    GrepInput{Pattern: "needle-ci", Glob: ".github/**/*.yml"},
			found: filepath.Join(".github", "workflows", "ci.yml"),
			// The glob could never match in target/ or .vscode/, so leaving
			// them out is not a gap worth reporting.
			quiet: true,
		},
		{
			name:  "a path inside .github finds it",
			in:    GrepInput{Pattern: "needle-ci", Path: ".github"},
			found: filepath.Join(".github", "workflows", "ci.yml"),
			quiet: true,
		},
		{
			name:   "hidden: true finds it from the root",
			in:     GrepInput{Pattern: "needle-ci", Hidden: true},
			found:  filepath.Join(".github", "workflows", "ci.yml"),
			note:   []string{"ignored: target/"},
			absent: []string{".github", ".vscode", "hidden: true"},
		},
		{
			name:  "an untracked file is searched",
			in:    GrepInput{Pattern: "needle-untracked"},
			found: "notes.txt",
			// A match was found, but two directories were not looked in: the
			// note names them so the model knows the result may be partial.
			note: []string{"hidden: .github/, .vscode/", "ignored: target/"},
		},
		{
			name: "a gitignored directory is skipped, and says so",
			in:   GrepInput{Pattern: "needle-built"},
			note: []string{"No matches found", "ignored: target/", "no_ignore: true"},
		},
		{
			name:   "no_ignore: true searches it",
			in:     GrepInput{Pattern: "needle-built", NoIgnore: true},
			found:  filepath.Join("target", "out.txt"),
			note:   []string{"hidden: .github/"},
			absent: []string{"target/.", "no_ignore: true"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := runTool(t, g, Context{WorkingDir: dir}, c.in)
			if res.IsError {
				t.Fatalf("error: %s", res.Content)
			}
			first, _, _ := strings.Cut(res.Content, "\n")
			if c.found != "" && first != c.found {
				t.Errorf("first line = %q, want %q\n%s", first, c.found, res.Content)
			}
			if c.found == "" && first != "No matches found" {
				t.Errorf("expected no matches, got:\n%s", res.Content)
			}
			for _, want := range c.note {
				if !strings.Contains(res.Content, want) {
					t.Errorf("result does not say %q:\n%s", want, res.Content)
				}
			}
			for _, bad := range c.absent {
				if _, note, _ := strings.Cut(res.Content, "(not searched"); strings.Contains(note, bad) {
					t.Errorf("note mentions %q, which was searched:\n%s", bad, res.Content)
				}
			}
			if c.quiet && strings.Contains(res.Content, "not searched") {
				t.Errorf("nothing relevant was skipped, but the result says so:\n%s", res.Content)
			}
			if strings.Contains(res.Content, ".git/") {
				t.Errorf(".git is version-control metadata, not a skipped search target:\n%s", res.Content)
			}
		})
	}
}

// With matches in hand, a dotfile skipped at the root is not worth a line —
// only directories are, since that is where whole classes of files go
// missing. With none, every skip is reported.
func TestGrepSkipNoteCountsFilesOnlyWhenEmpty(t *testing.T) {
	dir := skipRepo(t)
	g, _ := NewGrep()
	hit := runTool(t, g, Context{WorkingDir: dir}, GrepInput{Pattern: "needle-untracked"})
	if strings.Contains(hit.Content, "hidden file") {
		t.Errorf("a result with matches counted skipped dotfiles:\n%s", hit.Content)
	}
	miss := runTool(t, g, Context{WorkingDir: dir}, GrepInput{Pattern: "absent"})
	if !strings.Contains(miss.Content, "1 hidden file(s)") { // .gitignore
		t.Errorf("an empty result did not count the skipped dotfile:\n%s", miss.Content)
	}
}

// Asking for context or line numbers is asking to see lines. -A with no
// output_mode used to return bare paths and drop the context in silence.
func TestGrepContextFlagsSelectContentMode(t *testing.T) {
	dir := skipRepo(t)
	g, _ := NewGrep()
	ci := filepath.Join(".github", "workflows", "ci.yml")
	cases := []struct {
		name string
		in   GrepInput
		want string
	}{
		{"-A", GrepInput{Pattern: "test:", Hidden: true, After: 1}, ci + ":  test:\n" + ci + "-    run: make needle-ci"},
		{"-B", GrepInput{Pattern: "test:", Hidden: true, Before: 1}, ci + "-jobs:\n" + ci + ":  test:"},
		{"-C", GrepInput{Pattern: "test:", Hidden: true, Context: 1}, ci + "-jobs:\n" + ci + ":  test:\n" + ci + "-    run: make needle-ci"},
		{"-n", GrepInput{Pattern: "test:", Hidden: true, LineNum: true}, ci + ":2:  test:"},
		// An explicit mode still wins: the caller asked for paths.
		{"explicit mode", GrepInput{Pattern: "test:", Hidden: true, After: 1, OutputMode: "files_with_matches"}, ci},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := runTool(t, g, Context{WorkingDir: dir}, c.in)
			body, _, _ := strings.Cut(res.Content, "\n(not searched")
			if body != c.want {
				t.Errorf("got:\n%s\nwant:\n%s", body, c.want)
			}
		})
	}
}

func TestGlobHiddenAndIgnoredInputs(t *testing.T) {
	dir := skipRepo(t)
	g, _ := NewGlob()

	res := runTool(t, g, Context{WorkingDir: dir}, GlobInput{Pattern: "**/*.yml"})
	if !strings.HasPrefix(res.Content, "No files found") || !strings.Contains(res.Content, "hidden: .github/") {
		t.Errorf("default glob should miss the workflow and say where it did not look:\n%s", res.Content)
	}
	res = runTool(t, g, Context{WorkingDir: dir}, GlobInput{Pattern: "**/*.yml", Hidden: true})
	if first, _, _ := strings.Cut(res.Content, "\n"); first != filepath.Join(".github", "workflows", "ci.yml") {
		t.Errorf("hidden: true should find the workflow:\n%s", res.Content)
	}
	res = runTool(t, g, Context{WorkingDir: dir}, GlobInput{Pattern: "**/out.txt", NoIgnore: true})
	if first, _, _ := strings.Cut(res.Content, "\n"); first != filepath.Join("target", "out.txt") {
		t.Errorf("no_ignore: true should find the ignored file:\n%s", res.Content)
	}
	// .git stays out even with both opt-ins: it is never a search target.
	res = runTool(t, g, Context{WorkingDir: dir}, GlobInput{Pattern: "**/HEAD", Hidden: true, NoIgnore: true})
	if !strings.HasPrefix(res.Content, "No files found") {
		t.Errorf(".git was walked:\n%s", res.Content)
	}
}

// The description is what the model reads before its first call; it must
// state the default mode and the skip rules up front.
func TestGrepDescriptionStatesDefaultsAndSkips(t *testing.T) {
	g, _ := NewGrep()
	desc, _ := g.Description(t.Context())
	for _, want := range []string{"NOT searched", "hidden: true", "no_ignore: true", "files_with_matches\" by default", "selects content"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description does not say %q:\n%s", want, desc)
		}
	}
}

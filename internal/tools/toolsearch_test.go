package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func newToolSearch(t *testing.T) *ToolSearch {
	t.Helper()
	ts, err := NewToolSearch([]ToolInfo{
		{Name: "Grep", Description: "search file contents"},
		{Name: "Write", Description: "write a file"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestToolSearchExecute(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		want       string
		wantReveal []string
	}{
		{
			name:       "matches all terms and reveals",
			query:      "search contents",
			want:       "Loaded 1 tool(s):\n- Grep: search file contents",
			wantReveal: []string{"Grep"},
		},
		{
			name:  "no match",
			query: "nonexistent xyz",
			want:  "No tools matched nonexistent xyz.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newToolSearch(t)
			raw, _ := json.Marshal(ToolSearchInput{Query: tt.query})
			var captured []string

			res, err := ts.Execute(context.Background(), Context{
				Reveal: func(names ...string) {
					captured = append(captured, names...)
				},
			}, raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(res) != 1 || res[0].Content != tt.want {
				t.Fatalf("Execute() = %+v, want content %q", res, tt.want)
			}
			if strings.Join(captured, ",") != strings.Join(tt.wantReveal, ",") {
				t.Fatalf("Reveal captured %v, want %v", captured, tt.wantReveal)
			}
		})
	}
}

func TestToolSearchValidateInput(t *testing.T) {
	ts := newToolSearch(t)
	raw, _ := json.Marshal(ToolSearchInput{Query: "   "})
	if ts.ValidateInput(raw) == nil {
		t.Fatal("expected empty query to be rejected")
	}
}

// godotCatalog is a trimmed copy of the real MCP catalog that exposed the AND
// bug: descriptions are long and overlapping, and no single tool contains every
// word of a natural description of what you want.
func godotCatalog() []ToolInfo {
	return []ToolInfo{
		{Name: "mcp__gsol__godot_game_time", Description: "Freeze the running game, then step forward a bounded slice of game time, or step_until a condition holds."},
		{Name: "mcp__gsol__godot_runtime_state", Description: "Observe live game state as structured data. Use digest for a one-shot entity snapshot."},
		{Name: "mcp__gsol__godot_scene", Description: "Manage scenes in the editor: open a scene, save the open scene, or reload an open scene from disk."},
		{Name: "mcp__gsol__godot_docs", Description: "Fetch Godot Engine documentation. Returns clean markdown."},
		{Name: "Grep", Description: "search file contents"},
	}
}

func TestToolSearchRanksInsteadOfRequiringEveryTerm(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		wantTop  string   // the first tool revealed
		wantSome []string // tools that must appear somewhere in the results
		wantNone []string
	}{
		{
			// The exact query that returned nothing while the bare word
			// "godot" returned all 65 tools, these two among them. Which of
			// them ranks first is not the point and is genuinely arguable —
			// the query names concepts from both — so assert only that the
			// relevant tools arrive and the irrelevant one does not.
			name:     "descriptive multi-term query finds the relevant tools",
			query:    "godot game time freeze runtime state digest",
			wantSome: []string{"mcp__gsol__godot_game_time", "mcp__gsol__godot_runtime_state"},
			wantNone: []string{"Grep"},
		},
		{
			// A query aimed at one tool must put that tool first, rather than
			// merely including it somewhere in the list.
			name:    "a focused query ranks its tool first",
			query:   "freeze step game time",
			wantTop: "mcp__gsol__godot_game_time",
		},
		{
			name:    "a term no tool has does not veto the rest",
			query:   "godot freeze unicorn",
			wantTop: "mcp__gsol__godot_game_time",
		},
		{
			name:    "fuzzy name match survives a missing separator",
			query:   "gametime",
			wantTop: "mcp__gsol__godot_game_time",
		},
		{
			name:    "description-only term still finds its tool",
			query:   "markdown",
			wantTop: "mcp__gsol__godot_docs",
		},
		{
			name:     "an unrelated query matches nothing",
			query:    "kubernetes helm chart",
			wantNone: []string{"Grep", "mcp__gsol__godot_docs"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, err := NewToolSearch(godotCatalog())
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(ToolSearchInput{Query: tt.query})
			var revealed []string
			if _, err := ts.Execute(context.Background(), Context{
				Reveal: func(names ...string) { revealed = append(revealed, names...) },
			}, raw); err != nil {
				t.Fatal(err)
			}

			if tt.wantTop != "" {
				if len(revealed) == 0 {
					t.Fatalf("query %q revealed nothing, want %s first", tt.query, tt.wantTop)
				}
				if revealed[0] != tt.wantTop {
					t.Errorf("query %q ranked %s first, want %s (full order: %v)", tt.query, revealed[0], tt.wantTop, revealed)
				}
			}
			for _, want := range tt.wantSome {
				if !slices.Contains(revealed, want) {
					t.Errorf("query %q did not reveal %s; got %v", tt.query, want, revealed)
				}
			}
			for _, bad := range tt.wantNone {
				if slices.Contains(revealed, bad) {
					t.Errorf("query %q wrongly revealed %s; got %v", tt.query, bad, revealed)
				}
			}
		})
	}
}

// A broad query must not silently load an entire MCP server into the context,
// which is the cost deferred tools exist to avoid.
func TestToolSearchCapsAndReportsTruncation(t *testing.T) {
	catalog := make([]ToolInfo, 0, maxToolSearchResults*2)
	for i := 0; i < maxToolSearchResults*2; i++ {
		catalog = append(catalog, ToolInfo{
			Name:        fmt.Sprintf("mcp__godot__tool_%02d", i),
			Description: "a godot tool",
		})
	}
	ts, err := NewToolSearch(catalog)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(ToolSearchInput{Query: "godot"})
	var revealed []string
	res, err := ts.Execute(context.Background(), Context{
		Reveal: func(names ...string) { revealed = append(revealed, names...) },
	}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(revealed) != maxToolSearchResults {
		t.Errorf("revealed %d tools, want the cap of %d", len(revealed), maxToolSearchResults)
	}
	if !strings.Contains(res[0].Content, fmt.Sprintf("of %d matching", len(catalog))) {
		t.Errorf("truncation was not reported to the model: %q", res[0].Content)
	}
}

// jobCatalog mixes two unrelated families, which is what a real session looks
// like once an MCP server is loaded, and is what lets a noise term's cost be
// measured: a tool matched only by "the" or "running" is a tool revealed into
// the context for no reason.
func jobCatalog() []ToolInfo {
	return []ToolInfo{
		{Name: "KillShell", Description: "Stop a background job started by Bash with run_in_background, by id or name."},
		{Name: "Jobs", Description: "List the background jobs this session started: id, name, command, whether each is still running."},
		{Name: "RestartJob", Description: "Restart a background job by id or name: stops it and runs the same command again."},
		{Name: "BashOutput", Description: "Read new output from a background job started by Bash with run_in_background."},
		{Name: "mcp__gsol__godot_run", Description: "Run the project."},
		{Name: "mcp__gsol__godot_screenshot", Description: "Take a screenshot of the running game."},
		{Name: "mcp__gsol__godot_scene", Description: "Open a scene, save the open scene, or reload an open scene from disk."},
		{Name: "mcp__gsol__godot_docs", Description: "Fetch Godot Engine documentation. Returns clean markdown."},
	}
}

// OR-matching made descriptive queries work and brought this with it: a term
// that matches most of the catalog qualifies tools that have nothing to do with
// the request. Because a match *reveals* a tool, each one spends part of the
// budget deferred loading exists to protect — asked to stop a job, Klaudia
// loaded a project runner because its description contains "the".
func TestNoiseTermsDoNotRevealUnrelatedTools(t *testing.T) {
	ts, err := NewToolSearch(jobCatalog())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(ToolSearchInput{Query: "stop the background job that is running"})
	var revealed []string
	if _, err := ts.Execute(context.Background(), Context{
		Reveal: func(names ...string) { revealed = append(revealed, names...) },
	}, raw); err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(revealed, "KillShell") {
		t.Errorf("the tool that stops a job was not revealed: %v", revealed)
	}
	// Matched by "the" alone, and by nothing else in the query.
	if slices.Contains(revealed, "mcp__gsol__godot_run") {
		t.Errorf("a tool matched only by a noise term was revealed: %v", revealed)
	}
	if slices.Contains(revealed, "mcp__gsol__godot_scene") {
		t.Errorf("a tool matched only by a noise term was revealed: %v", revealed)
	}
}

// The fallback matters as much as the filter. A query made entirely of terms
// that match most of the catalog is a question that broad, and returning
// everything is the honest answer — returning nothing was the original bug.
func TestAQueryOfOnlyBroadTermsStillMatches(t *testing.T) {
	catalog := []ToolInfo{
		{Name: "godot_game_time", Description: "Freeze and step game time."},
		{Name: "godot_runtime_state", Description: "Observe live game state."},
		{Name: "godot_scene", Description: "Manage scenes in the editor."},
		{Name: "godot_docs", Description: "Fetch Godot documentation."},
		{Name: "godot_run", Description: "Run the godot project."},
	}
	// Both terms match every entry, so dropping them would leave nothing.
	top, total := rank(catalog, []string{"godot", "godot_"})
	if total != len(catalog) {
		t.Errorf("matched %d of %d tools; a query of broad terms must still match", total, len(catalog))
	}
	if len(top) != len(catalog) {
		t.Errorf("revealed %d tools, want %d", len(top), len(catalog))
	}
}

func TestInformativeTerms(t *testing.T) {
	catalog := jobCatalog() // 8 entries, so "more than half" is 5 or more
	tests := []struct {
		name  string
		terms []string
		want  []string
	}{
		{
			// "background" and "job" are each in 4 of 8 — exactly half, which
			// is not more than half, so both survive.
			name:  "terms matching half the catalog are kept",
			terms: []string{"background", "job"},
			want:  []string{"background", "job"},
		},
		{
			name:  "a term matching most of the catalog is dropped",
			terms: []string{"stop", "the"},
			want:  []string{"stop"},
		},
		{
			name:  "a single-term query is never filtered",
			terms: []string{"the"},
			want:  []string{"the"},
		},
		{
			name:  "all-noise queries keep every term",
			terms: []string{"the", "a"},
			want:  []string{"the", "a"},
		},
		{
			name:  "terms nothing matches are informative, and harmless",
			terms: []string{"stop", "unicorn"},
			want:  []string{"stop", "unicorn"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := informativeTerms(catalog, tt.terms)
			if !slices.Equal(got, tt.want) {
				t.Errorf("informativeTerms(%v) = %v, want %v", tt.terms, got, tt.want)
			}
		})
	}
}

// On a catalog of two or three tools, "more than half" is one entry, and the
// rule would throw away terms that are doing real work.
func TestNoiseFilterIsOffForTinyCatalogs(t *testing.T) {
	catalog := []ToolInfo{
		{Name: "Grep", Description: "search file contents"},
		{Name: "Write", Description: "write a file"},
	}
	got := informativeTerms(catalog, []string{"file", "contents"})
	if !slices.Equal(got, []string{"file", "contents"}) {
		t.Errorf("informativeTerms = %v, want both terms kept", got)
	}
}

// A one- or two-letter term is a substring of too much: "is" is inside "disk"
// and "this", so a query containing it revealed a scene tool and a job lister
// on the strength of two letters. Document frequency cannot catch this —
// "disk" and "this" are genuinely rare words — so short terms match whole
// words only.
func TestShortTermsMatchWholeWordsOnly(t *testing.T) {
	catalog := []ToolInfo{
		{Name: "godot_scene", Description: "Reload an open scene from disk."},
		{Name: "go_build", Description: "Compile a Go package."},
	}
	tests := []struct {
		name    string
		term    string
		wantAny bool
		wantTop string
	}{
		{name: "not inside a longer word", term: "is", wantAny: false},
		{name: "a whole word in the description", term: "go", wantAny: true, wantTop: "go_build"},
		{name: "a whole segment of a name", term: "go", wantAny: true, wantTop: "go_build"},
		{name: "longer terms still match as substrings", term: "scen", wantAny: true, wantTop: "godot_scene"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			top, total := rank(catalog, []string{tt.term})
			if !tt.wantAny {
				if total != 0 {
					t.Errorf("term %q matched %d tools (%v), want none", tt.term, total, top)
				}
				return
			}
			if total == 0 {
				t.Fatalf("term %q matched nothing, want %s", tt.term, tt.wantTop)
			}
			if top[0].Name != tt.wantTop {
				t.Errorf("term %q ranked %s first, want %s", tt.term, top[0].Name, tt.wantTop)
			}
		})
	}
}

func TestHasWord(t *testing.T) {
	tests := []struct {
		s, term string
		want    bool
	}{
		{"reload an open scene from disk.", "is", false},
		{"compile a go package", "go", true},
		{"godot_run", "run", true},
		{"godot_runtime", "run", false},
		{"id, name, command", "id", true},
		{"run_in_background", "in", true},
		{"background", "in", false},
		{"go", "go", true},
		{"", "go", false},
	}
	for _, tt := range tests {
		if got := hasWord(tt.s, tt.term); got != tt.want {
			t.Errorf("hasWord(%q, %q) = %v, want %v", tt.s, tt.term, got, tt.want)
		}
	}
}

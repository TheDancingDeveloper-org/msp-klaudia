package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenthread-ai/klaudia/internal/lsp"
)

// TestFormatLocationsIncludesSourceLine verifies location results carry the
// actual source line text next to their file:line:col (issue #157).
func TestFormatLocationsIncludesSourceLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc Target() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uri := path // SourceLine/URIPath accept a plain path as well as a file:// URI
	locs := []lsp.Location{{
		URI:   uri,
		Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 5}},
	}}
	out := formatLocations(locs)
	if !strings.Contains(out, ":3:6") {
		t.Errorf("expected 1-based file:line:col in %q", out)
	}
	if !strings.Contains(out, "func Target() {}") {
		t.Errorf("expected source line text in %q", out)
	}
}

func TestFormatRenamePreview(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("var Old int\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uri := path // SourceLine/URIPath accept a plain path as well as a file:// URI
	edits := []lsp.FileEdits{{
		URI: uri,
		Edits: []lsp.TextEdit{{
			Range:   lsp.Range{Start: lsp.Position{Line: 0, Character: 4}, End: lsp.Position{Line: 0, Character: 7}},
			NewText: "New",
		}},
	}}
	out := formatRename("New", edits)
	if !strings.Contains(out, "NOT applied") {
		t.Errorf("rename preview should say edits are not applied: %q", out)
	}
	if !strings.Contains(out, "1:5") || !strings.Contains(out, "var Old int") || !strings.Contains(out, `"New"`) {
		t.Errorf("rename preview missing location/source/newText: %q", out)
	}

	// No edits → a clear message, still not an error.
	if got := formatRename("New", nil); !strings.Contains(got, "no edits") {
		t.Errorf("empty rename = %q", got)
	}
}

func sym(name string, kind, line int, container string) lsp.Symbol {
	return lsp.Symbol{
		Name: name, Kind: kind, ContainerName: container,
		Location: lsp.Location{
			URI:   "file:///w/pkg/a.go",
			Range: lsp.Range{Start: lsp.Position{Line: line, Character: 5}},
		},
	}
}

func TestFormatSymbols(t *testing.T) {
	got := formatSymbols([]lsp.Symbol{sym("NewPool", 12, 23, ""), sym("Close", 6, 110, "*Pool")}, nil)
	want := "NewPool  function  /w/pkg/a.go:24:6\nClose  method  /w/pkg/a.go:111:6  (in *Pool)"
	if got != want {
		t.Errorf("formatSymbols =\n%s\nwant\n%s", got, want)
	}
}

func TestFormatSymbolsEmptyAndPartial(t *testing.T) {
	if got := formatSymbols(nil, nil); got != "No symbols matched." {
		t.Errorf("empty = %q", got)
	}
	got := formatSymbols([]lsp.Symbol{sym("A", 12, 0, "")}, errors.Join(errors.New("rust: no server"), errors.New("python: timeout")))
	if !strings.HasPrefix(got, "A  function") || !strings.Contains(got, "may be incomplete: rust: no server; python: timeout") {
		t.Errorf("partial = %q", got)
	}
}

func TestFormatSymbolsCaps(t *testing.T) {
	var syms []lsp.Symbol
	for i := 0; i < maxWorkspaceSymbols+7; i++ {
		syms = append(syms, sym(fmt.Sprintf("S%d", i), 13, i, ""))
	}
	got := formatSymbols(syms, nil)
	lines := strings.Split(got, "\n")
	if len(lines) != maxWorkspaceSymbols+1 || lines[len(lines)-1] != "... 7 more; refine the query" {
		t.Errorf("cap: %d lines, last %q", len(lines), lines[len(lines)-1])
	}
}

func TestWorkspaceSymbolValidateInput(t *testing.T) {
	tool, err := NewWorkspaceSymbol(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tool.ValidateInput(json.RawMessage(`{"query":"NewPool"}`)); err != nil {
		t.Errorf("query alone should validate: %v", err)
	}
	if err := tool.ValidateInput(json.RawMessage(`{"query":"NewPool","file":"a.go"}`)); err != nil {
		t.Errorf("query with file should validate: %v", err)
	}
	if err := tool.ValidateInput(json.RawMessage(`{"query":"  "}`)); err == nil {
		t.Error("a blank query should be rejected")
	}
	res, err := tool.Execute(context.Background(), Context{}, json.RawMessage(`{"query":"x"}`))
	if err != nil || len(res) != 1 || !res[0].IsError {
		t.Errorf("nil pool should be a tool error, got %+v, %v", res, err)
	}
}

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenthread-ai/klaudia/internal/lsp"
)

// diag is a small helper to build a one-error diagnostic at (line,col) 0-based.
func diag(line, col int, sev int, msg string) lsp.Diagnostic {
	return lsp.Diagnostic{
		Range:    lsp.Range{Start: lsp.Position{Line: line, Character: col}},
		Severity: sev,
		Message:  msg,
	}
}

func TestFormatNewDiagnostics(t *testing.T) {
	// Empty set appends nothing.
	if got := formatNewDiagnostics("/tmp/x.go", nil); got != "" {
		t.Errorf("empty diagnostics should format to %q, got %q", "", got)
	}

	// A non-empty set produces a compact 1-based section.
	got := formatNewDiagnostics("/tmp/x.go", []lsp.Diagnostic{
		diag(4, 1, 1, "undefined: foo"),
		diag(9, 6, 2, "declared and not used: bar"),
	})
	want := "\n\nNew diagnostics:" +
		"\n  /tmp/x.go:5:2 error undefined: foo" +
		"\n  /tmp/x.go:10:7 warning declared and not used: bar"
	if got != want {
		t.Errorf("format mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestFormatNewDiagnosticsCollapsesMultilineMessage(t *testing.T) {
	got := formatNewDiagnostics("/tmp/x.go", []lsp.Diagnostic{
		diag(0, 0, 1, "line one\n\tline two   line three"),
	})
	if strings.Count(got, "\n  ") != 1 {
		t.Errorf("multi-line message should stay on one entry line, got %q", got)
	}
	if !strings.Contains(got, "line one line two line three") {
		t.Errorf("message whitespace not collapsed: %q", got)
	}
}

func TestFormatNewDiagnosticsCaps(t *testing.T) {
	many := make([]lsp.Diagnostic, maxAppendedDiagnostics+5)
	for i := range many {
		many[i] = diag(i, 0, 1, "err")
	}
	got := formatNewDiagnostics("/tmp/x.go", many)
	if n := strings.Count(got, "\n  /tmp/x.go:"); n != maxAppendedDiagnostics {
		t.Errorf("expected %d shown entries, got %d", maxAppendedDiagnostics, n)
	}
	if !strings.Contains(got, "… and 5 more") {
		t.Errorf("expected overflow note, got %q", got)
	}
}

func TestAppendDiagnosticsHookBehaviour(t *testing.T) {
	// Nil hook (LSP off): no-op.
	if got := appendDiagnostics(context.Background(), Context{}, "/tmp/x.go"); got != "" {
		t.Errorf("nil hook should append nothing, got %q", got)
	}

	// Hook returns an error (no server / unsupported / timeout): append nothing,
	// never a false "clean".
	errCtx := Context{Diagnostics: func(context.Context, string) ([]lsp.Diagnostic, error) {
		return nil, errors.New("no language server")
	}}
	if got := appendDiagnostics(context.Background(), errCtx, "/tmp/x.go"); got != "" {
		t.Errorf("hook error should append nothing, got %q", got)
	}

	// Hook returns empty (server answered, no problems): append nothing (silence,
	// not a false all-clear).
	emptyCtx := Context{Diagnostics: func(context.Context, string) ([]lsp.Diagnostic, error) {
		return nil, nil
	}}
	if got := appendDiagnostics(context.Background(), emptyCtx, "/tmp/x.go"); got != "" {
		t.Errorf("empty diagnostics should append nothing, got %q", got)
	}

	// Hook returns problems: they are appended.
	okCtx := Context{Diagnostics: func(_ context.Context, path string) ([]lsp.Diagnostic, error) {
		return []lsp.Diagnostic{diag(2, 3, 1, "boom")}, nil
	}}
	got := appendDiagnostics(context.Background(), okCtx, "/tmp/x.go")
	if !strings.Contains(got, "New diagnostics:") || !strings.Contains(got, "3:4 error boom") {
		t.Errorf("expected appended diagnostics, got %q", got)
	}
}

func TestEditAppendsDiagnostics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	os.WriteFile(path, []byte("package main\n"), 0o644)

	e, _ := NewEdit()
	tctx := Context{Diagnostics: func(_ context.Context, p string) ([]lsp.Diagnostic, error) {
		if p != path {
			t.Errorf("hook called with %q, want %q", p, path)
		}
		return []lsp.Diagnostic{diag(0, 0, 1, "undefined: x")}, nil
	}}
	raw, _ := json.Marshal(EditInput{FilePath: path, OldString: "main", NewString: "other"})
	res, err := e.Execute(context.Background(), tctx, raw)
	if err != nil || res[0].IsError {
		t.Fatalf("edit failed: err=%v res=%+v", err, res[0])
	}
	if !strings.HasPrefix(res[0].Content, "Edited ") {
		t.Errorf("result should start with the edit message: %q", res[0].Content)
	}
	if !strings.Contains(res[0].Content, "New diagnostics:") {
		t.Errorf("edit result should carry diagnostics: %q", res[0].Content)
	}
}

func TestWriteAppendsDiagnosticsOnlyWhenNonEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")

	w, _ := NewWrite()
	// Empty diagnostics -> the base success message, nothing appended.
	cleanCtx := Context{Diagnostics: func(context.Context, string) ([]lsp.Diagnostic, error) {
		return nil, nil
	}}
	raw, _ := json.Marshal(WriteInput{FilePath: path, Content: "package main\n"})
	res, err := w.Execute(context.Background(), cleanCtx, raw)
	if err != nil || res[0].IsError {
		t.Fatalf("write failed: err=%v res=%+v", err, res[0])
	}
	if strings.Contains(res[0].Content, "New diagnostics:") {
		t.Errorf("clean write should not append a diagnostics section: %q", res[0].Content)
	}
}

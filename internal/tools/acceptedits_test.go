package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenthread-ai/klaudia/internal/permission"
)

// acceptEditsCtx is an acceptEdits context whose workspace is ws: a path is in
// it when it is ws or under it, as written.
func acceptEditsCtx(ws string) permission.Context {
	return permission.Context{
		Mode: permission.StaticMode(permission.ModeAcceptEdits),
		InWorkspace: func(p string) bool {
			return p == ws || strings.HasPrefix(p, ws+string(filepath.Separator))
		},
	}
}

// The mode's whole contract, per edit tool: inside the workspace runs, outside
// it asks, and a file that makes code run later asks even inside it.
func TestAcceptEditsEditDecisions(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	pctx := acceptEditsCtx(ws)
	edits := []Tool{
		must[*Write](t)(NewWrite()),
		must[*Edit](t)(NewEdit()),
		must[*NotebookEdit](t)(NewNotebookEdit()),
	}
	cases := []struct {
		name string
		path string
		want permission.Behavior
	}{
		{"inside the workspace", filepath.Join(ws, "src", "main.go"), permission.Allow},
		{"outside the workspace", filepath.Join(filepath.Dir(ws), "elsewhere", "x.go"), permission.Ask},
		{"a git hook inside the workspace", filepath.Join(ws, ".git", "hooks", "pre-commit"), permission.Ask},
	}
	for _, tool := range edits {
		for _, c := range cases {
			got := tool.CheckPermissions(pctx, pathRequest(c.path))
			if got.Behavior != c.want {
				t.Errorf("%s, %s: behavior = %q, want %q", tool.Name(), c.name, got.Behavior, c.want)
			}
			if got.Behavior == permission.Ask && got.Message == "" {
				t.Errorf("%s, %s: an ask must say why", tool.Name(), c.name)
			}
		}
	}
}

// With no workspace to check against, acceptEdits asks rather than assuming
// the edit is inside it. Autonomous does not consult the workspace at all, so
// the same edit outside it still runs there: the new mode narrows nothing else.
func TestAcceptEditsWithoutAWorkspaceAsks(t *testing.T) {
	w := must[*Write](t)(NewWrite())
	req := pathRequest("/somewhere/else.txt")
	if got := w.CheckPermissions(permission.Context{Mode: permission.StaticMode(permission.ModeAcceptEdits)}, req); got.Behavior != permission.Ask {
		t.Errorf("acceptEdits with no workspace: behavior = %q, want ask", got.Behavior)
	}
	if got := w.CheckPermissions(acceptEditsCtx("/proj"), req); got.Behavior != permission.Ask {
		t.Errorf("acceptEdits outside /proj: behavior = %q, want ask", got.Behavior)
	}
	auto := permission.Context{Mode: permission.StaticMode(permission.ModeAutonomous), InWorkspace: func(string) bool { return false }}
	if got := w.CheckPermissions(auto, req); got.Behavior != permission.Allow {
		t.Errorf("autonomous outside the workspace: behavior = %q, want allow (left to the host gate)", got.Behavior)
	}
}

// Network and browser tools ask in acceptEdits: it auto-approves edits and
// nothing else.
func TestAcceptEditsAsksBeforeNetworkTools(t *testing.T) {
	pctx := acceptEditsCtx("/proj")
	for _, tool := range []Tool{
		must[*BrowserSearch](t)(NewBrowserSearch(nil)),
		must[*BrowserFetch](t)(NewBrowserFetch(nil)),
		must[*BrowserNavigate](t)(NewBrowserNavigate(nil)),
		must[*BrowserSnapshot](t)(NewBrowserSnapshot(nil)),
	} {
		if got := tool.CheckPermissions(pctx, permission.PermissionRequest{}); got.Behavior != permission.Ask {
			t.Errorf("%s in acceptEdits: behavior = %q, want ask", tool.Name(), got.Behavior)
		}
	}
}

package trust

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCredentialGuard(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o755); err != nil {
		t.Fatal(err)
	}

	guard := CredentialGuard(home, home)
	if guard == nil {
		t.Fatal("a walk from home must be guarded")
	}
	for p, want := range map[string]bool{
		filepath.Join(home, ".aws"):                       true,
		filepath.Join(home, ".aws", "credentials"):        true,
		filepath.Join(home, ".netrc"):                     true,
		filepath.Join(home, ".ssh", "id_ed25519"):         true,
		filepath.Join(home, ".config", "gh", "hosts.yml"): true,
		filepath.Join(home, "src", "app.go"):              false,
		filepath.Join(home, ".config", "nvim"):            false,
	} {
		if got := guard(p); got != want {
			t.Errorf("guard(%s) = %v, want %v", p, got, want)
		}
	}

	// Rooted inside a location, the walk was aimed there; the root check is
	// what asks. Other locations stay guarded.
	inside := CredentialGuard(home, filepath.Join(home, ".aws"))
	if inside(filepath.Join(home, ".aws", "credentials")) {
		t.Error("a walk rooted in .aws must be able to search .aws")
	}
	if !inside(filepath.Join(home, ".ssh")) {
		t.Error("rooting in .aws must not open .ssh")
	}
}

// The other half of the guard: a search rooted in a credential location is
// classified as a sensitive read, so it asks before the walk starts.
func TestSearchRootedInCredentialsIsSensitive(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o755); err != nil {
		t.Fatal(err)
	}
	roots := NewRoots(home, project)
	for _, tool := range []string{"Grep", "Glob"} {
		in, _ := json.Marshal(map[string]string{"pattern": "x", "path": filepath.Join(home, ".aws")})
		as := ClassifyToolCall(tool, in, project, roots)
		sensitive := false
		for _, e := range as.Effects {
			sensitive = sensitive || e.Zone == ZoneSensitive
		}
		if !sensitive {
			t.Errorf("%s rooted in ~/.aws: effects %+v, want a sensitive read", tool, as.Effects)
		}
	}
}

package gitguard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBaselineRoundTripIncludesOthers(t *testing.T) {
	dir := t.TempDir()
	b := &Baseline{
		Root:      dir,
		Tracked:   []string{"keep.go"},
		Untracked: []string{"notes.txt"},
		others: map[string]*Baseline{
			"/other": {Root: "/other", Tracked: []string{"theirs.go"}},
		},
	}
	raw, err := MarshalBaseline(b)
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalBaseline(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.Root != dir || len(back.Tracked) != 1 || back.Tracked[0] != "keep.go" {
		t.Fatalf("primary baseline = %+v", back)
	}
	o := back.others["/other"]
	if o == nil || len(o.Tracked) != 1 || o.Tracked[0] != "theirs.go" {
		t.Fatalf("others = %+v", back.others)
	}
	if _, err := UnmarshalBaseline([]byte("{")); err == nil {
		t.Fatal("bad json accepted")
	}
	// The on-disk file is what a hook writes; keep the shape obvious.
	if filepath.Separator == '/' && !os.IsPathSeparator(raw[0]) && raw[0] != '{' {
		t.Fatalf("marshal = %s", raw)
	}
}

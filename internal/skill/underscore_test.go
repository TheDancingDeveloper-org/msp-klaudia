package skill

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDirSkipsUnderscoreDirsSilently(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "_adapters", "product"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: real\ndescription: a real skill\n---\nDo it.\n"
	if err := os.WriteFile(filepath.Join(dir, "real", "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	got := loadDir(dir, func(s string) { warnings = append(warnings, s) })
	if len(got) != 1 || got[0].Name != "real" {
		t.Fatalf("skills = %+v, want only \"real\"", got)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none for an underscore directory", warnings)
	}
}

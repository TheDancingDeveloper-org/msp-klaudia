package tools

import (
	"path/filepath"
	"testing"
)

func TestDisplayPathIsRelativeInsideTheWorkingDir(t *testing.T) {
	wd := filepath.FromSlash("/srv/sessions/work/abc")
	tctx := Context{WorkingDir: wd}
	cases := []struct{ in, want string }{
		{filepath.Join(wd, "customer", "main.tf"), filepath.FromSlash("customer/main.tf")},
		{filepath.FromSlash("/etc/hosts"), filepath.FromSlash("/etc/hosts")},
		{filepath.FromSlash("/srv/sessions/work/abcd/x"), filepath.FromSlash("/srv/sessions/work/abcd/x")},
		{"relative/already.tf", "relative/already.tf"},
	}
	for _, c := range cases {
		if got := displayPath(tctx, c.in); got != c.want {
			t.Errorf("displayPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := displayPath(Context{}, "/a/b"); got != "/a/b" {
		t.Errorf("no working dir: got %q", got)
	}
}

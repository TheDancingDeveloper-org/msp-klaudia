package bashparser

import (
	"slices"
	"testing"
)

func TestParsePrefix(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"git status", "git status"},
		{"git status -s", "git status"},
		{"ls", "ls"},
		{"npm run build", "npm run"},
		{"echo hi", "echo hi"},
	}
	for _, c := range cases {
		a, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if got := a.Prefix(); got != c.want {
			t.Errorf("Parse(%q).Prefix() = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParsePipe(t *testing.T) {
	a, err := Parse("cat foo | grep bar")
	if err != nil {
		t.Fatal(err)
	}
	if !a.HasPipe {
		t.Error("expected HasPipe for piped command")
	}
	if len(a.Commands) != 2 {
		t.Errorf("expected 2 commands, got %d", len(a.Commands))
	}
}

func TestParseError(t *testing.T) {
	if _, err := Parse("for do done ("); err == nil {
		t.Error("expected parse error for malformed input")
	}
}

// Adversarial structure: the permission specifier (Prefix) and command list must
// not be fooled by quoting, operators, redirections, or leading env assignments
// — these are exactly the shapes that could let a command dodge a rule.
func TestParseAdversarialStructure(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		wantName   string
		wantArgs   []string
		wantPrefix string
		wantPipe   bool
		wantCount  int
	}{
		{"single-quoted literal", "echo 'hello world'",
			"echo", []string{"hello world"}, "echo hello world", false, 1},
		{"double-quoted literal", `grep "func New" .`,
			"grep", []string{"func New", "."}, "grep func New", false, 1},
		{"&& chains two commands", "cat a && rm b",
			"cat", []string{"a"}, "cat a", false, 2},
		{"; chains two commands", "cat a; rm b",
			"cat", []string{"a"}, "cat a", false, 2},
		{"|| chains two commands", "test -f x || touch x",
			"test", []string{"-f", "x"}, "test x", false, 2},
		{"redirection is neither command nor arg", "echo hi > out.txt",
			"echo", []string{"hi"}, "echo hi", false, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := Parse(c.input)
			if err != nil {
				t.Fatalf("Parse(%q): %v", c.input, err)
			}
			if len(a.Commands) != c.wantCount {
				t.Fatalf("Commands count = %d, want %d (%+v)", len(a.Commands), c.wantCount, a.Commands)
			}
			if a.Commands[0].Name != c.wantName {
				t.Errorf("Name = %q, want %q", a.Commands[0].Name, c.wantName)
			}
			if !equal(a.Commands[0].Args, c.wantArgs) {
				t.Errorf("Args = %v, want %v", a.Commands[0].Args, c.wantArgs)
			}
			if got := a.Prefix(); got != c.wantPrefix {
				t.Errorf("Prefix() = %q, want %q", got, c.wantPrefix)
			}
			if a.HasPipe != c.wantPipe {
				t.Errorf("HasPipe = %v, want %v", a.HasPipe, c.wantPipe)
			}
		})
	}
}

// A leading env assignment must NOT become the command name — otherwise a rule
// keyed on "rm" would never match `FOO=bar rm -rf x`.
func TestParseEnvAssignmentDoesNotMaskCommand(t *testing.T) {
	a, err := Parse("FOO=bar rm -rf x")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Commands) != 1 || a.Commands[0].Name != "rm" {
		t.Fatalf("Commands = %+v, want a single rm command (assignment must not mask it)", a.Commands)
	}
	if a2, _ := Parse("FOO=bar"); len(a2.Commands) != 0 {
		t.Errorf("bare assignment yielded commands: %+v", a2.Commands)
	}
}

// Command substitution hides a second command; the parser walks into it so the
// nested command is surfaced in Commands (a permission layer can then see
// `echo $(rm -rf /)` is more than a lone echo).
func TestParseSurfacesNestedCommandSubstitution(t *testing.T) {
	a, err := Parse("echo $(rm -rf /tmp/x)")
	if err != nil {
		t.Fatal(err)
	}
	var sawRm bool
	for _, c := range a.Commands {
		if c.Name == "rm" {
			sawRm = true
		}
	}
	if !sawRm {
		t.Errorf("nested command substitution not surfaced; Commands = %+v", a.Commands)
	}
}

func TestParseEmptyHasNoCommands(t *testing.T) {
	for _, in := range []string{"", "   ", "\n"} {
		a, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q) unexpected error: %v", in, err)
		}
		if len(a.Commands) != 0 || a.Prefix() != "" {
			t.Errorf("Parse(%q) = %+v, want empty", in, a)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseBackground(t *testing.T) {
	for in, want := range map[string][]bool{
		"a &":               {true},
		"a & b":             {true, false},
		"(a &) ; b":         {true, false},
		"(a; b) &":          {true, true},
		"a | b &":           {true, true},
		"a && b":            {false, false},
		"a 2>&1":            {false},
		"a >&2":             {false},
		"a &> log":          {false},
		"a &>> log":         {false},
		"echo 'x & y'":      {false},
		`echo "x & y"`:      {false},
		"a & b & wait":      {true, true, false},
		"if a; then b & fi": {false, true},
	} {
		a, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
			continue
		}
		var got []bool
		for _, c := range a.Commands {
			got = append(got, c.Background)
		}
		if !slices.Equal(got, want) {
			t.Errorf("Parse(%q) backgrounds = %v, want %v", in, got, want)
		}
	}
}

func TestSequential(t *testing.T) {
	type cmd struct {
		chain int
		start bool
	}
	cases := []struct {
		line string
		seq  bool
		want []cmd // per command, when seq
	}{
		{"cd a && git add -A", true, []cmd{{0, true}, {0, false}}},
		{"cd a; git add -A", true, []cmd{{0, true}, {1, true}}},
		{"cd a\ngit add -A && git commit -m x", true, []cmd{{0, true}, {1, true}, {1, false}}},
		{"false && cd a; git add -A", true, []cmd{{0, true}, {0, false}, {1, true}}},
		{"cd a || exit; git add -A", false, nil},
		{"(cd a && git add -A); git add -A", false, nil},
		{"{ cd a; }; git add -A", false, nil},
		{"cd a | git add -A", false, nil},
		{"cd a & git add -A", false, nil},
		{"! cd a; git add -A", false, nil},
		{"if true; then cd a; fi; git add -A", false, nil},
		{"f() { cd a; }; git add -A", false, nil},
		{"echo $(cd a); git add -A", false, nil},
	}
	for _, tc := range cases {
		a, err := Parse(tc.line)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.line, err)
		}
		if a.Sequential != tc.seq {
			t.Errorf("Parse(%q).Sequential = %v, want %v", tc.line, a.Sequential, tc.seq)
			continue
		}
		if !tc.seq {
			continue
		}
		if len(a.Commands) != len(tc.want) {
			t.Fatalf("Parse(%q): %d commands, want %d", tc.line, len(a.Commands), len(tc.want))
		}
		for i, w := range tc.want {
			if c := a.Commands[i]; c.Chain != w.chain || c.ChainStart != w.start {
				t.Errorf("Parse(%q) command %d (%s): chain %d start %v, want %d %v", tc.line, i, c.Name, c.Chain, c.ChainStart, w.chain, w.start)
			}
		}
	}
}

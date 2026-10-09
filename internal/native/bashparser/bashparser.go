// Package bashparser parses bash command lines, absorbing the standalone
// tools/bash-parser binary in-process. It is used to derive a permission
// specifier (e.g. "git status") for a Bash invocation and to detect structure
// such as pipes and multiple commands.
package bashparser

import (
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Word is one shell word plus whether its text is the whole story.
//
// Literal is false when the word contained something that cannot be resolved by
// reading the source — a parameter expansion, command substitution, arithmetic
// or an extended glob. Text then holds only the literal fragments, which is
// useful as a hint and actively dangerous as a path: `"$HOME/notes.txt"` yields
// the text "/notes.txt", an absolute path that appears to be at the filesystem
// root and is not. Anything deciding what a command touches must branch on
// Literal before trusting Text.
type Word struct {
	Text    string
	Literal bool
}

// Command is a single simple command (a program invocation) within a line.
type Command struct {
	Name string   // the program, e.g. "git"; empty when the name is an expansion
	Args []string // literal text of the arguments following the program name

	// NameWord and ArgWords carry the same values with their Literal flags, for
	// callers that must not confuse a partial expansion for a real path.
	NameWord Word
	ArgWords []Word

	// Background reports that the command runs asynchronously: its statement,
	// or one enclosing it, ends in `&` — `a &`, `(a; b) &`, `(a &)`, `a | b &`.
	// `&&` and the redirections `&>`, `>&`, `2>&1` are not backgrounding.
	Background bool

	// Subshell reports that the command runs in a subshell of its own — one side
	// of a pipe. A cd among them never moves the shell that runs the commands
	// after it, so a reader following the working directory skips it.
	Subshell bool

	// Chain is the index of the top-level statement the command belongs to,
	// and ChainStart whether it is the first command of that statement's
	// `&&` chain. Both are meaningful only when Analysis.Sequential is set:
	// then a command that starts its chain always runs, and one later in the
	// chain runs only if every command before it in the chain succeeded.
	Chain      int
	ChainStart bool
}

// Redirect is an output redirection target. Only writing redirections are
// collected — a reader deciding what a command *changes* cares about `>`,
// `>>` and `&>`, not about where stdin came from.
type Redirect struct {
	Target  string // the file the redirection writes to
	Append  bool   // >> rather than >
	Literal bool   // false when Target came from an expansion (see Word)
}

// Analysis is the parsed structure of a command line.
type Analysis struct {
	Commands  []Command  // simple commands, in source order
	Redirects []Redirect // writing redirections, in source order
	HasPipe   bool       // whether the line contains a pipe
	// HasExpansion reports whether any word in the line was non-literal. A
	// cheap top-level "I could not read all of this" signal.
	HasExpansion bool
	// Sequential reports that the line is nothing but simple commands joined
	// by `;`, newlines and `&&` — no pipe, `||`, `&`, `!`, subshell, group,
	// compound command, function or command substitution. In such a line a
	// command runs in the shell that runs the next one, in source order, so a
	// reader can follow state like the working directory across it (see
	// Command.Chain).
	Sequential bool
}

// Parse parses a bash command line. On a parse error the returned Analysis is
// empty and err is non-nil; callers typically fall back to treating the whole
// line as opaque.
func Parse(input string) (Analysis, error) {
	parser := syntax.NewParser(
		syntax.KeepComments(false),
		syntax.Variant(syntax.LangBash),
	)
	prog, err := parser.Parse(strings.NewReader(input), "")
	if err != nil {
		return Analysis{}, err
	}
	// lastpipe makes bash run the last command of a pipe in the current shell, so
	// a cd there does move the shell that runs what follows. It can be enabled at
	// any point of the line before the pipe, and whether it is on is shell state
	// this reading does not track, so a line that mentions it is not sequential:
	// no cd on it is followed, and every git on it is judged where the line
	// started. (The same goes for `set -o lastpipe`: the mention test covers it.)
	lastpipe := strings.Contains(input, "lastpipe")

	var a Analysis
	// bg is a stack parallel to the walk: whether the node being visited sits
	// under a backgrounded statement. Walk calls f(nil) after a node's
	// children, which is where its entry is popped.
	bg := []bool{false}
	background := map[*syntax.CallExpr]bool{}
	var found []*syntax.CallExpr
	syntax.Walk(prog, func(node syntax.Node) bool {
		if node == nil {
			bg = bg[:len(bg)-1]
			return true
		}
		inBg := bg[len(bg)-1]
		if s, ok := node.(*syntax.Stmt); ok && s.Background {
			inBg = true
		}
		bg = append(bg, inBg)
		switch n := node.(type) {
		case *syntax.BinaryCmd:
			if n.Op == syntax.Pipe || n.Op == syntax.PipeAll {
				a.HasPipe = true
			}
		case *syntax.Stmt:
			// Redirections hang off the statement, not the call — without this
			// `echo x > /etc/hosts` looks like a harmless `echo`.
			for _, r := range n.Redirs {
				if r.Word == nil {
					continue
				}
				switch r.Op {
				case syntax.RdrOut, syntax.AppOut, syntax.RdrAll, syntax.AppAll,
					syntax.ClbOut, syntax.RdrInOut:
					w := word(r.Word)
					if !w.Literal {
						a.HasExpansion = true
					}
					// Emitted even when the target is unresolvable: "writes
					// somewhere I cannot determine" is a finding, and dropping
					// it would silently report the command as writing nothing.
					a.Redirects = append(a.Redirects, Redirect{
						Target:  w.Text,
						Append:  r.Op == syntax.AppOut || r.Op == syntax.AppAll,
						Literal: w.Literal,
					})
				}
			}
		case *syntax.CallExpr:
			found = append(found, n)
			background[n] = inBg
		}
		return true
	})
	// The walk visits a word's command substitution before the call the word
	// belongs to, so `git commit -m "$(cat …)"` is seen as cat and then git.
	// The shell runs them the other way round, and the reading of the working
	// directory follows the shell, so the calls are recorded in source order.
	sort.SliceStable(found, func(i, j int) bool {
		return found[i].Pos().Offset() < found[j].Pos().Offset()
	})
	calls := map[*syntax.CallExpr]int{} // recorded call → index in a.Commands
	for _, n := range found {
		if len(n.Args) == 0 {
			continue
		}
		ws := make([]Word, 0, len(n.Args))
		for _, w := range n.Args {
			expanded := word(w)
			if !expanded.Literal {
				a.HasExpansion = true
			}
			ws = append(ws, expanded)
		}
		args := make([]string, 0, len(ws)-1)
		for _, w := range ws[1:] {
			args = append(args, w.Text)
		}
		calls[n] = len(a.Commands)
		a.Commands = append(a.Commands, Command{
			Name:       ws[0].Text,
			Args:       args,
			NameWord:   ws[0],
			ArgWords:   ws[1:],
			Background: background[n],
		})
	}
	a.Sequential = sequential(prog, a.Commands, calls) && !lastpipe
	return a, nil
}

// sequential reports whether prog is a plain list of `&&` chains of simple
// commands, and if so numbers each recorded command's chain (Command.Chain).
//
// A command substitution breaks it only when its body contains a cd. A
// substitution runs in its own shell, so that cd never moves the parent, but the
// parser records it as a command of the line and would otherwise read the rest
// as running where it landed. A substitution that only reads — a heredoc fed to
// cat, redirects included — cannot do that.
//
// A pipe's own commands run in subshells, so they are not part of the chain the
// `&&` continues in, and a pipe with nothing after it is not a chain at all. A
// cd inside the pipe must not be read as moving this shell; the commands joined
// to the pipe by `&&` still are.
func sequential(prog *syntax.File, cmds []Command, calls map[*syntax.CallExpr]int) bool {
	piped := map[*syntax.Stmt]bool{}
	var mark func(syntax.Node)
	mark = func(n syntax.Node) {
		if s, ok := n.(*syntax.Stmt); ok {
			mark(s.Cmd)
			return
		}
		b, ok := n.(*syntax.BinaryCmd)
		if !ok {
			return
		}
		if b.Op == syntax.Pipe || b.Op == syntax.PipeAll {
			// Only the non-final sides are marked. lastpipe, when set, runs the
			// final side in this shell, and its state is not tracked here, so the
			// final side keeps its chain: a cd there is followed, the
			// conservative reading whatever lastpipe is.
			piped[b.X] = true
			// `a | b | c` parses as (a | b) | c, so the marked left side is
			// itself a pipe; its non-final sides are marked in turn.
			for x := b.X; ; {
				inner, ok := x.Cmd.(*syntax.BinaryCmd)
				if !ok || (inner.Op != syntax.Pipe && inner.Op != syntax.PipeAll) {
					break
				}
				piped[inner.X] = true
				x = inner.X
			}
		}
		mark(b.X)
		mark(b.Y)
	}
	for _, s := range prog.Stmts {
		mark(s)
	}
	var chain func(s *syntax.Stmt) ([]*syntax.CallExpr, bool)
	chain = func(s *syntax.Stmt) ([]*syntax.CallExpr, bool) {
		if s == nil || s.Background || s.Negated || s.Coprocess || s.Disown || substitutionMoves(s) {
			return nil, false
		}
		switch c := s.Cmd.(type) {
		case *syntax.CallExpr:
			if piped[s] {
				// A pipe's side runs in its own subshell, so its cd never moves
				// this shell; it joins no chain, and is marked so a reader of
				// Commands can tell.
				if k, ok := calls[c]; ok {
					cmds[k].Subshell = true
				}
				return nil, true
			}
			return []*syntax.CallExpr{c}, true
		case *syntax.BinaryCmd:
			if c.Op == syntax.Pipe || c.Op == syntax.PipeAll {
				_, ok := chain(c.X)
				if !ok {
					return nil, false
				}
				_, ok = chain(c.Y)
				return nil, ok
			}
			if c.Op != syntax.AndStmt {
				return nil, false
			}
			x, ok := chain(c.X)
			if !ok {
				return nil, false
			}
			y, ok := chain(c.Y)
			if !ok {
				return nil, false
			}
			return append(x, y...), true
		}
		return nil, false
	}
	seq := true
	assigned := 0
	for i, s := range prog.Stmts {
		cs, ok := chain(s)
		if !ok {
			seq = false
			break
		}
		for j, c := range cs {
			if k, ok := calls[c]; ok {
				cmds[k].Chain, cmds[k].ChainStart = i, j == 0
				assigned++
			}
		}
	}
	// A line whose every command sat in a pipe subshell has no chain of its own;
	// where it runs is not something this reading can vouch for.
	return seq && assigned > 0
}

// substitutionMoves reports whether a statement's command substitutions contain
// a cd. Such a cd is recorded as a command of the line even though it runs in
// its own shell and never moves the parent.
func substitutionMoves(s *syntax.Stmt) bool {
	moves := false
	var walk func([]*syntax.Stmt)
	walk = func(stmts []*syntax.Stmt) {
		for _, st := range stmts {
			syntax.Walk(st, func(node syntax.Node) bool {
				switch n := node.(type) {
				case *syntax.CallExpr:
					if len(n.Args) > 0 && wordText(n.Args[0]) == "cd" {
						moves = true
					}
				case *syntax.CmdSubst:
					walk(n.Stmts)
					return false
				case *syntax.ProcSubst:
					walk(n.Stmts)
					return false
				}
				return true
			})
		}
	}
	syntax.Walk(s, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.CmdSubst:
			walk(n.Stmts)
			return false
		case *syntax.ProcSubst:
			walk(n.Stmts)
			return false
		}
		return true
	})
	return moves
}

// Prefix returns a permission specifier for the first command: the program
// name plus its first non-flag argument (a subcommand), e.g. "git status".
// Returns "" if there are no commands.
//
// This is a best-effort heuristic: it does not know which flags take values, so
// for commands like "git -C /repo log" it may pick the flag's value rather than
// the subcommand. Permission rules should account for this approximation.
func (a Analysis) Prefix() string {
	if len(a.Commands) == 0 {
		return ""
	}
	return ShortForm(a.Commands[0].Name, a.Commands[0].Args)
}

// ShortForm is a command's "program subcommand" form: the name plus its first
// non-flag argument ("git status"), or the name alone when there is none. It
// is the form Prefix gives the first command, and the form older permission
// rules were written against.
func ShortForm(name string, args []string) string {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return name + " " + arg
		}
	}
	return name
}

// Base strips any directory from a program name, so /usr/bin/sudo and sudo
// are recognised alike.
func Base(name string) string { return base(name) }

// ShellPayloads returns the script text passed to an inline shell — the
// argument after -c for sh/bash/zsh/dash/ksh, and the argument to eval.
//
// The parser records that script as a single opaque word, so a caller
// reasoning about what a command line does has to re-Parse these to see
// inside. Returning them separately keeps that recursion the caller's explicit
// decision rather than something Parse does invisibly (and unboundedly).
func (a Analysis) ShellPayloads() []string {
	var out []string
	for _, c := range a.Commands {
		if p, ok := ShellPayload(c.Name, c.Args); ok {
			out = append(out, p)
		}
	}
	return out
}

// ShellPayload returns the script an inline shell or eval runs — the argument
// after -c for sh/bash/zsh/dash/ksh, the joined arguments of eval — for one
// command given as its program and arguments.
func ShellPayload(name string, args []string) (string, bool) {
	switch base(name) {
	case "sh", "bash", "zsh", "dash", "ksh":
		for i, arg := range args {
			// -c, and combined forms like -lc / -ec that end in c.
			if strings.HasPrefix(arg, "-") && strings.HasSuffix(arg, "c") && i+1 < len(args) {
				return args[i+1], true
			}
		}
	case "eval":
		// eval concatenates its arguments into one script.
		if len(args) > 0 {
			return strings.Join(args, " "), true
		}
	}
	return "", false
}

// base strips any directory from a program name, so /usr/bin/sudo and sudo
// are recognised alike.
func base(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}

// word extracts a word's literal text and reports whether that text is
// complete.
func word(w *syntax.Word) Word {
	text, literal := wordParts(w)
	return Word{Text: text, Literal: literal}
}

// wordText is the legacy literal-only accessor, kept for callers that already
// tolerate partial text.
func wordText(w *syntax.Word) string {
	t, _ := wordParts(w)
	return t
}

// wordParts joins a word's literal fragments and reports whether every part was
// literal.
func wordParts(w *syntax.Word) (string, bool) {
	var b strings.Builder
	literal := true
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, dp := range p.Parts {
				if lit, ok := dp.(*syntax.Lit); ok {
					b.WriteString(lit.Value)
					continue
				}
				// An expansion inside quotes: the surrounding literals are
				// kept as a hint, but the word is no longer trustworthy.
				literal = false
			}
		default:
			// ParamExp, CmdSubst, ArithmExp, ProcSubst, ExtGlob — nothing
			// readable, and the reader must know the text is incomplete.
			literal = false
		}
	}
	return b.String(), literal
}

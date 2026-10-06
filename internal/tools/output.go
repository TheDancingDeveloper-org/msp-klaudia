package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/greenthread-ai/klaudia/internal/session"
)

// Tool output has to be capped — a 5 MB build log would swallow the context
// window — but *where* it is cut matters more than the size of the cap.
//
// Cutting only the head is the worst choice available, because the part of a
// command's output people actually want is almost always at the end: the FAIL
// summary from `go test ./...`, the error that stopped a build, the last line
// of a stack trace. Keeping a head and a tail costs the same number of bytes
// and keeps both the invocation context and the verdict.
//
// The full text is also written to disk, so nothing is truly lost: the notice
// names the file, which means the model can grep it and /last can page it.
//
// # Two levels, deliberately
//
// A tool that knows its own output clamps itself — Bash does, because only
// Bash knows the verdict is at the end and that the exit annotation has to
// survive. Cap() is the backstop the agent loop applies to whatever is left:
// byte-level, content-blind, and the weaker of the two, but it is the only one
// that covers tools nobody tuned, including every MCP tool. The backstop is a
// no-op on output already under budget, so a tool that did the smart thing is
// never cut twice.

const (
	// maxToolOutput caps a single tool result to protect the context window.
	// One number for every tool: per-tool budgets should be set from measured
	// output sizes, not guessed, and nothing has been measured yet.
	maxToolOutput = 30000
	// The cap is split head-heavy: the beginning carries what ran and how it
	// started, but the tail is where the verdict is, so it gets a third.
	// Expressed as a fraction because the aggregate cap applies the same split
	// to a smaller budget.
	headFraction = 2
	headDivisor  = 3

	// bashMaxOutput is Bash's own budget. It matches the general cap so Bash's
	// self-clamping lands just under the backstop rather than fighting it.
	bashMaxOutput = maxToolOutput

	// spillMaxAge is how long full-output files are kept before being pruned.
	spillMaxAge = 24 * time.Hour
)

// Cap is the agent loop's backstop: it clamps content to the per-tool budget,
// writes the untruncated text to disk, and appends a notice naming the file so
// the model can read what was removed. It returns the text and whether
// anything was elided; content already within budget is returned unchanged.
func Cap(toolName, content string) (string, bool) {
	return CapTo(toolName, content, maxToolOutput)
}

// CapTo is Cap against an explicit budget, for the loop's per-message
// aggregate cap: a turn can hold many calls that each pass the per-result
// budget and still add up to more than the window can take, and the share each
// one gets then depends on how many there were.
//
// Content that already names a spill file is clamped but not spilled again.
// The tail keeps that notice, and the file it names holds the original — a
// second spill would only preserve an already-clamped copy.
func CapTo(toolName, content string, limit int) (string, bool) {
	clamped, elided := clampOutputTo(content, limit)
	if elided == 0 {
		return content, false
	}
	if strings.Contains(content, spillMarker) {
		return clamped, true
	}
	if path, ok := spillOutput(toolName, content); ok {
		clamped += "\n" + spillMarker + path + "]"
	}
	return clamped, true
}

// Spill writes content to the same on-disk store Cap uses and returns its
// path. Exported for compaction, which elides old tool results and needs
// somewhere to put the text it is removing.
func Spill(toolName, content string) (string, bool) {
	return spillOutput(toolName, content)
}

// clampOutput trims out to the default per-result budget.
func clampOutput(out string) (string, int) {
	return clampOutputTo(out, maxToolOutput)
}

// clampOutputTo trims out to a head+tail budget. It returns the trimmed text
// and the number of bytes removed from the middle (0 when nothing was cut).
func clampOutputTo(out string, limit int) (string, int) {
	if limit <= 0 || len(out) <= limit {
		return out, 0
	}
	headBytes := limit * headFraction / headDivisor
	head := cutAfterLastLine(out[:headBytes])
	tail := cutBeforeFirstLine(out[len(out)-(limit-headBytes):])
	elided := len(out) - len(head) - len(tail)
	return head + "\n" + elisionMarker(elided) + "\n" + tail, elided
}

func elisionMarker(n int) string {
	return fmt.Sprintf("... [%d bytes elided from the middle] ...", n)
}

// cutAfterLastLine trims back to the last complete line, so the head never ends
// mid-line. Newlines are ASCII and can't appear inside a multi-byte sequence, so
// this also guarantees a valid rune boundary; the fallback handles the
// pathological no-newline case.
func cutAfterLastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// cutBeforeFirstLine trims forward to the next line start, so the tail never
// begins mid-line.
func cutBeforeFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		return s[i+1:]
	}
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[1:]
	}
	return s
}

// spillDir is where full command output is kept.
func spillDir() string { return filepath.Join(session.ConfigRoot(), "outputs") }

// spillOutput writes the untruncated text to disk and returns its path, so the
// elided middle stays reachable. The file is named after the tool that
// produced it, so a directory of spills can be read without opening each one.
// Failure is not an error worth surfacing — the caller just omits the path
// from the notice.
func spillOutput(toolName, full string) (string, bool) {
	dir := spillDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false
	}
	pruneSpills(dir, spillMaxAge)

	f, err := os.CreateTemp(dir, spillPrefix(toolName)+"-*.log")
	if err != nil {
		return "", false
	}
	defer f.Close()
	if _, err := f.WriteString(full); err != nil {
		os.Remove(f.Name())
		return "", false
	}
	return f.Name(), true
}

// spillPrefix makes a tool name safe for a filename. MCP tool names carry
// separators ("mcp__server__tool"), and CreateTemp would reject or mangle a
// pattern containing a path separator.
func spillPrefix(toolName string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '-'
		}
	}, toolName)
	if clean == "" {
		return "tool"
	}
	return clean
}

// pruneSpills removes spill files older than maxAge. Called on each spill,
// which is cheap and keeps the directory from growing without bound. Only
// .log files are considered, so anything else a user has put here survives.
func pruneSpills(dir string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// spillMarker prefixes the notice naming the full-output file. It is written
// for the model to read — a path it can Read or grep to recover the elided
// middle. Local frontends don't parse it: they get the untruncated text
// directly via Result.Full.
const spillMarker = "[full output: "

package mcp

import (
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// defaultDebounce is how long the watcher waits for quiet before reporting a
// change. Editors rarely write a file once: a save can arrive as truncate then
// write, or as write-temp then rename, and reacting to each step would tear
// down and rebuild MCP servers against a half-written config.
const defaultDebounce = 300 * time.Millisecond

// ReloadEvent reports the outcome of one hot reload of the MCP config.
//
// Only failures travel: a reload that works is meant to be invisible, and
// announcing every successful one would punish the config file for being
// edited.
//
// It lives here rather than in a frontend because the watcher is wired for
// every mode. It used to be tui.MCPReloadEvent, which made the CLI import the
// TUI in order to describe an MCP failure, and meant that in headless,
// stream-json and --loop runs the events were constructed and then dropped on
// the floor — a typo in .mcp.json was indistinguishable from a clean reload
// until a tool call failed much later for an unrelated-looking reason.
type ReloadEvent struct {
	// ConfigErr is set when the config could not be read or parsed. Nothing
	// was applied in that case and the servers already running are untouched,
	// which is worth saying — the edit looks live but is not.
	ConfigErr string
	// ServerErrs are the per-server launch failures from an otherwise applied
	// reload, already formatted.
	ServerErrs []string
}

// Failed reports whether anything in the reload went wrong.
func (e ReloadEvent) Failed() bool { return e.ConfigErr != "" || len(e.ServerErrs) > 0 }

// Watch reports changes to any .mcp.json that applies to dir, calling onChange
// after a quiet period. It returns a stop function, which is safe to call more
// than once.
//
// It watches the containing directories rather than the files themselves, for
// two reasons: a config that does not exist yet still needs to be noticed when
// it appears, and an editor that saves by writing a temp file and renaming it
// over the target destroys the inode a file watch is attached to — the classic
// "it worked once and never again" watcher bug.
//
// The watch is deliberately NOT recursive. ~/.klaudia also holds sessions/,
// jobs/ and browser/, which Klaudia itself writes to constantly while running;
// watching that tree recursively would produce a continuous stream of events
// caused by nothing more than the session being alive. A non-recursive watch of
// the directory sees only its direct entries, and the filter below narrows that
// to the config files themselves.
func Watch(dir string, onChange func()) (stop func(), err error) {
	return watchWithDebounce(dir, defaultDebounce, onChange)
}

func watchWithDebounce(dir string, debounce time.Duration, onChange func()) (func(), error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	interesting := map[string]bool{}
	watched := map[string]bool{}
	for _, p := range ConfigPaths(dir) {
		interesting[p] = true
		d := filepath.Dir(p)
		if watched[d] {
			continue
		}
		// A directory that doesn't exist yet (commonly .klaudia/) simply isn't
		// watched. Creating one mid-session is rare enough to leave to the
		// next start, and failing the whole watcher over it would cost the
		// reloads that do work.
		if err := w.Add(d); err == nil {
			watched[d] = true
		}
	}
	if len(watched) == 0 {
		w.Close()
		return func() {}, nil
	}

	done := make(chan struct{})
	go func() {
		defer w.Close()
		var timer *time.Timer
		var fire <-chan time.Time
		for {
			select {
			case <-done:
				if timer != nil {
					timer.Stop()
				}
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				// Chmod alone is not a content change; ignoring it keeps
				// routine metadata churn from triggering reconnects.
				if ev.Op == fsnotify.Chmod || !interesting[filepath.Clean(ev.Name)] {
					continue
				}
				if timer != nil {
					timer.Stop()
				}
				timer = time.NewTimer(debounce)
				fire = timer.C
			case <-fire:
				timer, fire = nil, nil
				onChange()
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
			}
		}
	}()

	var once sync.Once
	return func() { once.Do(func() { close(done) }) }, nil
}

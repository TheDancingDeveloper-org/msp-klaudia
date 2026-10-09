package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/greenthread-ai/klaudia/internal/gitguard"
)

// A Codex hook blocks a tool call by exiting 2 with a non-empty reason on
// stderr. Exit 2 with empty stderr is treated as a hook failure and does not
// block, so every refusal here — including a panic, bad input, a missing
// baseline or a lock that does not come free — writes a reason first.
//
// ExitUsage is already 2, which is what Codex wants, but usageErrorf also
// prints "Error: …". The hook prints the reason itself and returns a bare
// exitError so the wrapper adds nothing.

const (
	gitguardLockWait = 8 * time.Second
	gitguardMaxStdin = 1 << 20
)

func newHookCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "hook",
		Short:         "Hooks for an embedding host (Codex)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newHookGitguardCommand())
	return cmd
}

func newHookGitguardCommand() *cobra.Command {
	return &cobra.Command{
		Use:           "gitguard",
		Short:         "Codex SessionStart / PreToolUse hook: refuse git that discards pre-existing work",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runHookGitguard,
	}
}

// hookBlock is a refusal that has already been written to stderr.
type hookBlock struct{ reason string }

func (e hookBlock) Error() string { return "" }

func (e hookBlock) Unwrap() error { return exitError{ExitUsage} }

func blockHook(w io.Writer, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "gitguard hook failed closed"
	}
	fmt.Fprintln(w, reason)
	return hookBlock{reason}
}

type hookPayload struct {
	SessionID     string          `json:"session_id"`
	Cwd           string          `json:"cwd"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	Source        string          `json:"source"`
}

func runHookGitguard(cmd *cobra.Command, _ []string) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = blockHook(cmd.ErrOrStderr(), fmt.Sprintf("gitguard hook panicked: %v", rec))
		}
	}()
	raw, rerr := io.ReadAll(io.LimitReader(cmd.InOrStdin(), gitguardMaxStdin+1))
	if rerr != nil {
		return blockHook(cmd.ErrOrStderr(), "gitguard: reading hook input: "+rerr.Error())
	}
	if len(raw) > gitguardMaxStdin {
		return blockHook(cmd.ErrOrStderr(), "gitguard: hook input exceeds 1 MiB")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return blockHook(cmd.ErrOrStderr(), "gitguard: empty hook input")
	}
	var in hookPayload
	if jerr := json.Unmarshal(raw, &in); jerr != nil {
		return blockHook(cmd.ErrOrStderr(), "gitguard: hook input is not JSON: "+jerr.Error())
	}
	if strings.TrimSpace(in.SessionID) == "" || strings.Contains(in.SessionID, "/") || strings.Contains(in.SessionID, "..") {
		return blockHook(cmd.ErrOrStderr(), "gitguard: hook input has no usable session_id")
	}
	switch in.HookEventName {
	case "SessionStart":
		return hookSessionStart(cmd, in)
	case "PreToolUse":
		return hookPreToolUse(cmd, in)
	default:
		return blockHook(cmd.ErrOrStderr(), "gitguard: unsupported hook_event_name "+in.HookEventName)
	}
}

func hookStatePath(sessionID string) (string, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		dir, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(dir, ".codex")
	}
	return filepath.Join(home, "gitguard", sessionID+".json"), nil
}

func hookSessionStart(cmd *cobra.Command, in hookPayload) error {
	// Any source may be the first time this session_id is seen. `codex fork`
	// mints a new id, and a resume of a session that predates the hook (or
	// whose CODEX_HOME moved) has none either. Keep a file that already
	// exists: recapturing over it would treat the run's own edits as the
	// user's. When there is no file, capture. That over-protects — earlier
	// edits of this run count as the user's — which is the safe direction,
	// and far better than a session that can never run a shell command.
	path, err := hookStatePath(in.SessionID)
	if err != nil {
		return blockHook(cmd.ErrOrStderr(), "gitguard: "+err.Error())
	}
	unlock, err := lockBaseline(path)
	if err != nil {
		return blockHook(cmd.ErrOrStderr(), "gitguard: "+err.Error())
	}
	defer unlock()

	if _, statErr := os.Stat(path); statErr == nil {
		return nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return blockHook(cmd.ErrOrStderr(), "gitguard: reading baseline: "+statErr.Error())
	}

	cwd := in.Cwd
	if cwd == "" {
		var wdErr error
		cwd, wdErr = os.Getwd()
		if wdErr != nil {
			return blockHook(cmd.ErrOrStderr(), "gitguard: no cwd: "+wdErr.Error())
		}
	}
	b, err := gitguard.Capture(cwd)
	if errors.Is(err, gitguard.ErrNotRepo) {
		b = &gitguard.Baseline{}
	} else if err != nil {
		return blockHook(cmd.ErrOrStderr(), "gitguard: capturing the working tree: "+err.Error())
	}
	return saveBaseline(cmd, in.SessionID, b)
}

func hookPreToolUse(cmd *cobra.Command, in hookPayload) error {
	switch in.ToolName {
	case "Bash", "apply_patch":
	default:
		return nil
	}
	path, err := hookStatePath(in.SessionID)
	if err != nil {
		return blockHook(cmd.ErrOrStderr(), "gitguard: "+err.Error())
	}
	unlock, err := lockBaseline(path)
	if err != nil {
		return blockHook(cmd.ErrOrStderr(), "gitguard: "+err.Error())
	}
	defer unlock()

	b, err := loadBaseline(path)
	if err != nil {
		return blockHook(cmd.ErrOrStderr(), "gitguard: "+err.Error())
	}
	if b == nil {
		return blockHook(cmd.ErrOrStderr(), "gitguard: no baseline for session "+in.SessionID+" (SessionStart source=startup did not run)")
	}

	cwd := in.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	var reason string
	switch in.ToolName {
	case "Bash":
		reason = checkBash(b, in.ToolInput, cwd)
	case "apply_patch":
		reason = checkApplyPatch(b, in.ToolInput, cwd)
	}
	if saveErr := saveBaseline(cmd, in.SessionID, b); saveErr != nil {
		return saveErr
	}
	if reason != "" {
		return blockHook(cmd.ErrOrStderr(), reason)
	}
	return nil
}

func checkBash(b *gitguard.Baseline, raw json.RawMessage, cwd string) string {
	var in struct {
		Command string `json:"command"`
		Workdir string `json:"workdir"`
	}
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &in) != nil {
		return "gitguard: Bash tool_input is not readable; refusing rather than guessing"
	}
	dir := cwd
	if in.Workdir != "" {
		dir = in.Workdir
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
	}
	return b.CheckCommand(in.Command, dir)
}

// checkApplyPatch treats each file the patch names as a first touch, so a
// repository the session has not seen yet gets its baseline captured before
// the patch lands. A Delete of a path protected in that baseline is refused;
// Add and Update are the session's own writes and are allowed.
func checkApplyPatch(b *gitguard.Baseline, raw json.RawMessage, cwd string) string {
	var in struct {
		Command string `json:"command"`
		Patch   string `json:"patch"`
		Input   string `json:"input"`
	}
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &in) != nil {
		return "gitguard: apply_patch tool_input is not readable; refusing rather than guessing"
	}
	body := in.Command
	if body == "" {
		body = in.Patch
	}
	if body == "" {
		body = in.Input
	}
	var reason string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		var path string
		var del bool
		switch {
		case strings.HasPrefix(line, "*** Delete File:"):
			path, del = strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File:")), true
		case strings.HasPrefix(line, "*** Add File:"):
			path = strings.TrimSpace(strings.TrimPrefix(line, "*** Add File:"))
		case strings.HasPrefix(line, "*** Update File:"):
			path = strings.TrimSpace(strings.TrimPrefix(line, "*** Update File:"))
		case strings.HasPrefix(line, "*** Move to:"):
			path = strings.TrimSpace(strings.TrimPrefix(line, "*** Move to:"))
		default:
			continue
		}
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) && cwd != "" {
			path = filepath.Join(cwd, path)
		}
		// Touch before judging the delete, so a repo seen for the first time
		// here still has its pre-existing state on record.
		if del {
			if r := b.CheckCommand("git rm -f -- "+shellQuote(path), filepath.Dir(path)); r != "" {
				reason = r
			}
		} else {
			b.CheckTool("Write", mustJSON(struct {
				FilePath string `json:"file_path"`
			}{path}), cwd)
		}
	}
	return reason
}

func shellQuote(p string) string {
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func loadBaseline(path string) (*gitguard.Baseline, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading baseline: %w", err)
	}
	b, err := gitguard.UnmarshalBaseline(data)
	if err != nil {
		return nil, fmt.Errorf("baseline %s is not readable: %w", path, err)
	}
	return b, nil
}

func saveBaseline(cmd *cobra.Command, sessionID string, b *gitguard.Baseline) error {
	path, err := hookStatePath(sessionID)
	if err != nil {
		return blockHook(cmd.ErrOrStderr(), "gitguard: "+err.Error())
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return blockHook(cmd.ErrOrStderr(), "gitguard: "+err.Error())
	}
	data, err := gitguard.MarshalBaseline(b)
	if err != nil {
		return blockHook(cmd.ErrOrStderr(), "gitguard: encoding baseline: "+err.Error())
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return blockHook(cmd.ErrOrStderr(), "gitguard: writing baseline: "+err.Error())
	}
	if err := os.Rename(tmp, path); err != nil {
		return blockHook(cmd.ErrOrStderr(), "gitguard: writing baseline: "+err.Error())
	}
	return nil
}

// lockBaseline takes an exclusive flock on a sibling of the state file and
// waits up to gitguardLockWait. The lock file outlives the state file so two
// hooks creating it for the first time still serialise.
func lockBaseline(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening baseline lock: %w", err)
	}
	deadline := time.Now().Add(gitguardLockWait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, fmt.Errorf("locking baseline: %w", err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("baseline lock held longer than %s", gitguardLockWait)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

package cli

import (
	"fmt"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/api"
	"github.com/greenthread-ai/klaudia/internal/permission"
)

// modeVar is a permission mode that can change while the session runs. Its Get
// is a permission.Context Mode function: each check reads the current value.
type modeVar struct {
	mu   sync.RWMutex
	mode permission.Mode
}

func newModeVar(m permission.Mode) *modeVar { return &modeVar{mode: m} }

// Get reports the current mode.
func (v *modeVar) Get() permission.Mode {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.mode
}

func (v *modeVar) set(m permission.Mode) {
	v.mu.Lock()
	v.mode = m
	v.mu.Unlock()
}

// streamModeSetter returns the handler for a stream-json set_permission_mode
// control request. It holds a peer to the same rules the command line is held
// to at startup, so a control request cannot reach a mode the flags could not:
//
//   - the mode must be one Klaudia knows;
//   - autonomous needs the host gate enforcing, since without it autonomous is
//     bypassPermissions under another name;
//   - bypassPermissions is refused unless the session was launched in it.
//     Claude Code refuses it the same way for a session not started with
//     --dangerously-skip-permissions: the operator who launched the process
//     decides whether checks can be switched off, not the peer driving it.
func streamModeSetter(live *modeVar, launch permission.Mode, hostEnforcing func() bool) func(string) error {
	return func(s string) error {
		m := permission.Mode(strings.TrimSpace(s))
		switch {
		case !m.Valid():
			return fmt.Errorf("invalid permission mode %q (autonomous|plan|bypassPermissions|dontAsk, or legacy default|acceptEdits)", s)
		case m == permission.ModeAutonomous && !hostEnforcing():
			return fmt.Errorf("permission mode %q needs the host guardrail enforcing, and it is not in this session", m)
		case m == permission.ModeBypassPermissions && launch != permission.ModeBypassPermissions:
			return fmt.Errorf("cannot switch to %s: the session was not launched with --dangerously-skip-permissions or --permission-mode bypassPermissions", m)
		}
		live.set(m)
		return nil
	}
}

// modelVar is the model a stream-json session runs its next turn with.
type modelVar struct {
	mu     sync.RWMutex
	launch anthropic.Model
	model  anthropic.Model
}

func newModelVar(m anthropic.Model) *modelVar { return &modelVar{launch: m, model: m} }

// Get reports the model for the next turn.
func (v *modelVar) Get() anthropic.Model {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.model
}

// Set handles a set_model control request: an alias resolves as it does on the
// command line, and an empty name restores the model the session launched
// with. It returns the model now in effect.
func (v *modelVar) Set(name string) (string, error) {
	m := v.launch
	if name = strings.TrimSpace(name); name != "" {
		m = api.ResolveModel(name)
	}
	v.mu.Lock()
	v.model = m
	v.mu.Unlock()
	return string(m), nil
}

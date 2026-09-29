package cli

import (
	"strings"
	"testing"

	"github.com/greenthread-ai/klaudia/internal/api"
	"github.com/greenthread-ai/klaudia/internal/permission"
)

// A stream-json peer is held to the rules the command line is held to at
// startup: a known mode, autonomous only with the host gate enforcing, and no
// way into bypassPermissions for a session the operator did not launch in it.
func TestStreamModeSetter(t *testing.T) {
	enforcing := true
	for _, tc := range []struct {
		name      string
		launch    permission.Mode
		enforcing bool
		set       string
		wantErr   string
		want      permission.Mode
	}{
		{"plan", permission.ModeAutonomous, true, "plan", "", permission.ModePlan},
		{"dontAsk", permission.ModeDefault, false, "dontAsk", "", permission.ModeDontAsk},
		{"legacy acceptEdits", permission.ModeDefault, false, "acceptEdits", "", permission.ModeAcceptEdits},
		{"unknown", permission.ModeAutonomous, true, "yolo", "invalid permission mode", permission.ModeAutonomous},
		{"autonomous with gate", permission.ModePlan, true, "autonomous", "", permission.ModeAutonomous},
		{"autonomous without gate", permission.ModeDefault, false, "autonomous", "host guardrail", permission.ModeDefault},
		{"bypass not launched", permission.ModeAutonomous, true, "bypassPermissions", "dangerously-skip-permissions", permission.ModeAutonomous},
		{"bypass launched", permission.ModeBypassPermissions, true, "bypassPermissions", "", permission.ModeBypassPermissions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := newModeVar(tc.launch)
			enforcing = tc.enforcing
			err := streamModeSetter(live, tc.launch, func() bool { return enforcing })(tc.set)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("set %q: %v", tc.set, err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("set %q: err = %v, want one mentioning %q", tc.set, err, tc.wantErr)
			}
			if got := live.Get(); got != tc.want {
				t.Errorf("mode after set %q = %q, want %q", tc.set, got, tc.want)
			}
		})
	}
}

// A mode change made through the setter is what a permission.Context built on
// the variable reports at its next check — the property that lets it reach a
// running turn and the sub-agents holding the same Context.
func TestModeVarIsReadLive(t *testing.T) {
	live := newModeVar(permission.ModeAutonomous)
	ctx := permission.Context{Mode: live.Get}
	if err := streamModeSetter(live, permission.ModeAutonomous, func() bool { return true })("plan"); err != nil {
		t.Fatal(err)
	}
	if got := permission.CurrentMode(ctx); got != permission.ModePlan {
		t.Errorf("context mode = %q, want plan", got)
	}
}

// set_model resolves aliases as --model does, and an empty name restores the
// launch model.
func TestModelVar(t *testing.T) {
	v := newModelVar("launch-model")
	got, err := v.Set("opus")
	if err != nil {
		t.Fatal(err)
	}
	if want := string(api.ResolveModel("opus")); got != want || string(v.Get()) != want {
		t.Errorf("set opus: returned %q, Get %q, want %q", got, v.Get(), want)
	}
	if got, _ := v.Set("  "); got != "launch-model" || v.Get() != "launch-model" {
		t.Errorf("reset: returned %q, Get %q, want launch-model", got, v.Get())
	}
}

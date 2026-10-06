package cli

import (
	"strings"
	"testing"

	"github.com/greenthread-ai/klaudia/internal/api"
	"github.com/greenthread-ai/klaudia/internal/permission"
)

// A stream-json peer is held to the rules the command line is held to at
// startup: a known mode (the retired names are aliases for autonomous), and no
// way into bypassPermissions for a session the operator did not launch in it.
// The host gate always enforces now, so autonomous needs no extra check.
func TestStreamModeSetter(t *testing.T) {
	for _, tc := range []struct {
		name    string
		launch  permission.Mode
		set     string
		wantErr string
		want    permission.Mode
	}{
		{"plan", permission.ModeAutonomous, "plan", "", permission.ModePlan},
		{"retired dontAsk is autonomous", permission.ModePlan, "dontAsk", "", permission.ModeAutonomous},
		{"retired acceptEdits is autonomous", permission.ModePlan, "acceptEdits", "", permission.ModeAutonomous},
		{"unknown", permission.ModeAutonomous, "yolo", "invalid permission mode", permission.ModeAutonomous},
		{"autonomous", permission.ModePlan, "autonomous", "", permission.ModeAutonomous},
		{"bypass not launched", permission.ModeAutonomous, "bypassPermissions", "dangerously-skip-permissions", permission.ModeAutonomous},
		{"bypass launched", permission.ModeBypassPermissions, "bypassPermissions", "", permission.ModeBypassPermissions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := newModeVar(tc.launch)
			var warn strings.Builder
			err := streamModeSetter(live, tc.launch, &warn)(tc.set)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("set %q: %v", tc.set, err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("set %q: err = %v, want one mentioning %q", tc.set, err, tc.wantErr)
			}
			if got := live.Get(); got != tc.want {
				t.Errorf("mode after set %q = %q, want %q", tc.set, got, tc.want)
			}
			if strings.HasPrefix(tc.name, "retired") && !strings.Contains(warn.String(), "retired") {
				t.Errorf("no deprecation notice for %q: %q", tc.set, warn.String())
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
	if err := streamModeSetter(live, permission.ModeAutonomous, nil)("plan"); err != nil {
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

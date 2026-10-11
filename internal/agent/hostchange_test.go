package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/permission"
	"github.com/greenthread-ai/klaudia/internal/tools"
	"github.com/greenthread-ai/klaudia/internal/trust"
)

// The scenario the whole design exists for, end to end through dispatch:
// declare the operation once, then every step inside it runs untouched.
func TestDeclareOnceThenSilence(t *testing.T) {
	g, proj := gateFixture(t)
	g.DeclareTool = "RequestHostChange"

	declare, err := tools.NewRequestHostChange()
	if err != nil {
		t.Fatal(err)
	}
	reg, bash := testRegistry(t, declare)
	l := New(nil, reg)

	asked := 0
	var card *HostChange
	opts := Options{
		WorkingDir: proj,
		Host:       g,
		Permission: permission.Context{Mode: permission.StaticMode(permission.ModeAutonomous)},
		Approver: ApproverFunc(func(ctx context.Context, req ApprovalRequest) permission.Decision {
			asked++
			card = req.HostChange
			return permission.Decision{Behavior: permission.Allow}
		}),
	}
	run := func(name string, input any) string {
		raw, _ := json.Marshal(input)
		tu := anthropic.BetaToolUseBlock{ID: "t", Name: name, Input: json.RawMessage(raw)}
		return resultText(l.dispatch(context.Background(), tu, opts, 0, nil, nil,
			newFailureState()))
	}

	out := run("RequestHostChange", map[string]any{
		"summary":  "Install nginx and configure it as a development proxy",
		"reason":   "the task asks for the app to run behind a local proxy",
		"paths":    []string{"/etc/nginx/nginx.conf"},
		"services": []string{"nginx"},
		"packages": []string{"nginx"},
	})
	if !strings.Contains(out, "Approved") {
		t.Fatalf("declaration was not approved: %s", out)
	}
	if asked != 1 {
		t.Fatalf("asked %d times for one declaration", asked)
	}
	if card == nil || !card.Declared {
		t.Fatalf("approval card was not marked as declared: %+v", card)
	}
	if card.Reason == "" {
		t.Error("the card carried no reason — the user is deciding whether it is worth it")
	}

	for _, cmd := range []string{
		"sudo apt-get install -y nginx",
		"sudo mkdir -p /etc/nginx/conf.d",
		"sudo tee /etc/nginx/conf.d/app.conf",
		"sudo nginx -t",
		"sudo systemctl restart nginx",
	} {
		if body := run("Bash", map[string]string{"command": cmd}); strings.Contains(body, "RequestHostChange") {
			t.Errorf("%q was stopped despite the declaration: %s", cmd, body)
		}
	}
	if asked != 1 {
		t.Fatalf("asked %d times; the declaration should have covered every step", asked)
	}
	if len(bash.ran) != 5 {
		t.Fatalf("ran %v", bash.ran)
	}
}

// Scope drift: the model goes outside what was approved and is told to declare
// the extra part, with the user seeing that it was not in the original.
func TestDriftAsksAgainAndSaysSo(t *testing.T) {
	g, proj := gateFixture(t)
	g.DeclareTool = "RequestHostChange"
	declare, _ := tools.NewRequestHostChange()
	reg, _ := testRegistry(t, declare)
	l := New(nil, reg)

	var cards []*HostChange
	opts := Options{
		WorkingDir: proj,
		Host:       g,
		Permission: permission.Context{Mode: permission.StaticMode(permission.ModeAutonomous)},
		Approver: ApproverFunc(func(ctx context.Context, req ApprovalRequest) permission.Decision {
			cards = append(cards, req.HostChange)
			return permission.Decision{Behavior: permission.Allow}
		}),
	}
	run := func(name string, input any) string {
		raw, _ := json.Marshal(input)
		tu := anthropic.BetaToolUseBlock{ID: "t", Name: name, Input: json.RawMessage(raw)}
		return resultText(l.dispatch(context.Background(), tu, opts, 0, nil, nil,
			newFailureState()))
	}

	run("RequestHostChange", map[string]any{
		"summary": "Configure nginx", "reason": "local proxy",
		"services": []string{"nginx"},
	})
	body := run("Bash", map[string]string{"command": "sudo systemctl restart postgresql"})
	if !strings.Contains(body, "RequestHostChange") {
		t.Fatalf("an unapproved service was not stopped: %s", body)
	}
	if !strings.Contains(body, "outside the scope") {
		t.Errorf("the refusal did not say it was drift: %s", body)
	}

	run("RequestHostChange", map[string]any{
		"summary": "Restart postgres too", "reason": "the app needs the database up",
		"services": []string{"postgresql"},
	})
	if len(cards) != 2 || !cards[1].Drift {
		t.Fatalf("the second card was not marked as drift: %+v", cards)
	}
}

// Declining fails that one tool call. Prior work stands and the model is told
// to carry on, because the alternative throws away completed work for no reason.
func TestDeclinedDeclarationDoesNotMintAGrant(t *testing.T) {
	g, proj := gateFixture(t)
	g.DeclareTool = "RequestHostChange"
	declare, _ := tools.NewRequestHostChange()
	reg, _ := testRegistry(t, declare)
	l := New(nil, reg)

	opts := Options{
		WorkingDir: proj,
		Host:       g,
		Permission: permission.Context{Mode: permission.StaticMode(permission.ModeAutonomous)},
		Approver: ApproverFunc(func(ctx context.Context, req ApprovalRequest) permission.Decision {
			return permission.Decision{Behavior: permission.Deny}
		}),
	}
	raw, _ := json.Marshal(map[string]any{
		"summary": "Install nginx", "reason": "proxy", "packages": []string{"nginx"},
	})
	body := resultText(l.dispatch(context.Background(),
		anthropic.BetaToolUseBlock{ID: "t", Name: "RequestHostChange", Input: json.RawMessage(raw)},
		opts, 0, nil, nil, newFailureState()))

	if !strings.Contains(body, "declined") {
		t.Fatalf("result did not report the decline: %s", body)
	}
	if !strings.Contains(body, "Continue with the rest of the task") {
		t.Errorf("the refusal reads as an abort rather than a skip: %s", body)
	}
	if n := len(g.Ledger.List()); n != 0 {
		t.Fatalf("a declined declaration minted %d grants", n)
	}
}

// An over-broad ask is refused before it reaches the user. Whether Klaudia gets
// all of /etc should not depend on how tired someone is at the time.
func TestOverBroadDeclarationNeverReachesTheUser(t *testing.T) {
	g, _ := gateFixture(t)
	asked := 0
	h := hostChangeApprover{
		gate: g,
		approver: ApproverFunc(func(ctx context.Context, req ApprovalRequest) permission.Decision {
			asked++
			return permission.Decision{Behavior: permission.Allow}
		}),
	}
	out, err := h.RequestHostChange(context.Background(), tools.HostChangeRequest{
		Summary: "reconfigure the system", Reason: "because", Paths: []string{"/etc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Approved {
		t.Error("a request for all of /etc was approved")
	}
	if asked != 0 {
		t.Error("an over-broad request was put to the user")
	}
	if !strings.Contains(out.Message, "Narrow the request") {
		t.Errorf("the model was not told how to fix it: %q", out.Message)
	}
}

// Validation is the model's first line of feedback, so it has to be specific
// enough to act on.
func TestDeclarationValidation(t *testing.T) {
	declare, err := tools.NewRequestHostChange()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		input map[string]any
		want  string
	}{
		{"no scope", map[string]any{"summary": "do a thing", "reason": "because"}, "at least one path"},
		{"wildcard", map[string]any{
			"summary": "s", "reason": "r", "paths": []string{"/etc/nginx/*.conf"},
		}, "pattern"},
		// The schema marks reason required, so this is caught there; the tool's
		// own check covers a reason that is present but blank.
		{"no reason", map[string]any{"summary": "s", "packages": []string{"nginx"}}, "reason"},
		{"blank reason", map[string]any{
			"summary": "s", "reason": "   ", "packages": []string{"nginx"},
		}, "reason is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.input)
			err := declare.ValidateInput(raw)
			if err == nil {
				t.Fatalf("accepted %v", tc.input)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// With no one to ask, the tool says so. A model told "approved" here would then
// be refused by the gate on every command, which is a confusing way to find out
// there is no user.
func TestNoApproverSaysSo(t *testing.T) {
	declare, _ := tools.NewRequestHostChange()
	raw, _ := json.Marshal(map[string]any{
		"summary": "s", "reason": "r", "packages": []string{"nginx"},
	})
	res, err := declare.Execute(context.Background(), tools.Context{}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || !res[0].IsError {
		t.Fatalf("expected an error result, got %+v", res)
	}
	if !strings.Contains(res[0].Content, "no one to ask") {
		t.Errorf("content = %q", res[0].Content)
	}
}

// The tool must not be usable to make a change, only to ask about one.
func TestDeclarationToolCannotActOnItsOwn(t *testing.T) {
	g, _ := gateFixture(t)
	if _, err := g.Ledger.Mint(trust.Request{
		Summary: "x", Services: []string{"nginx"},
	}); err != nil {
		t.Fatal(err)
	}
	// A grant exists, but the tool itself still has no path to execution: its
	// Execute only calls the approver. Guard the shape rather than the wiring.
	declare, _ := tools.NewRequestHostChange()
	if req := declare.PermissionRequest(nil); req.Specifier != "" {
		t.Errorf("the declaration tool carries a rule specifier %q, so a rule could pre-approve asking",
			req.Specifier)
	}
}

// A declared change that was refused has to leave a trace. Without it the
// ledger only knows about changes it caught — and a model that did the right
// thing, declared its intent and was told no, would be invisible to /trust and
// to an automation deciding what exit code to use.
func TestDeclinedDeclarationIsRecorded(t *testing.T) {
	g, _ := gateFixture(t)
	h := hostChangeApprover{
		gate: g,
		approver: ApproverFunc(func(context.Context, ApprovalRequest) permission.Decision {
			return permission.Decision{Behavior: permission.Deny}
		}),
	}
	if _, err := h.RequestHostChange(context.Background(), tools.HostChangeRequest{
		Summary: "Install nginx", Reason: "proxy", Packages: []string{"nginx"},
	}); err != nil {
		t.Fatal(err)
	}
	reports := g.Reports()
	if len(reports) != 1 {
		t.Fatalf("%d reports, want 1", len(reports))
	}
	if !reports[0].Enforced {
		t.Error("the declined declaration was not recorded as enforced")
	}
	if !strings.Contains(reports[0].Summary, "Install nginx") {
		t.Errorf("report summary = %q", reports[0].Summary)
	}
}

// An approved one leaves no "blocked" trace.
func TestApprovedDeclarationIsNotRecordedAsBlocked(t *testing.T) {
	g, _ := gateFixture(t)
	h := hostChangeApprover{
		gate: g,
		approver: ApproverFunc(func(context.Context, ApprovalRequest) permission.Decision {
			return permission.Decision{Behavior: permission.Allow}
		}),
	}
	h.RequestHostChange(context.Background(), tools.HostChangeRequest{
		Summary: "Install nginx", Reason: "proxy", Packages: []string{"nginx"},
	})
	for _, r := range g.Reports() {
		if r.Enforced {
			t.Errorf("an approved change was recorded as blocked: %+v", r)
		}
	}
}

// #305: an approved scope covers the same paths when the command reaches them
// over ssh, in a combined remote script, or through docker exec on that host —
// the in-scope delete runs, while a path outside the scope, the scope root
// itself and a bulk delete are still refused.
func TestApprovedScopeCoversRemoteSpelledPaths(t *testing.T) {
	g, proj := gateFixture(t)
	g.DeclareTool = "RequestHostChange"
	declare, err := tools.NewRequestHostChange()
	if err != nil {
		t.Fatal(err)
	}
	reg, bash := testRegistry(t, declare)
	l := New(nil, reg)

	asked := 0
	opts := Options{
		WorkingDir: proj,
		Host:       g,
		Permission: permission.Context{Mode: permission.StaticMode(permission.ModeAutonomous)},
		Approver: ApproverFunc(func(ctx context.Context, req ApprovalRequest) permission.Decision {
			asked++
			return permission.Decision{Behavior: permission.Allow}
		}),
	}
	run := func(name string, input any) string {
		raw, _ := json.Marshal(input)
		tu := anthropic.BetaToolUseBlock{ID: "t", Name: name, Input: json.RawMessage(raw)}
		return resultText(l.dispatch(context.Background(), tu, opts, 0, nil, nil,
			newFailureState()))
	}
	const inScope = "ssh root@winrarhost 'rm -f /mnt/2tnvme/docker/volumes/z2m_data/.mqttpw'"

	// Before any approval the remote delete is put to the user, and the
	// refusal names the host it lands on, not this machine.
	body := run("Bash", map[string]string{"command": inScope})
	if !strings.Contains(body, "RequestHostChange") {
		t.Fatalf("an unapproved remote delete ran: %s", body)
	}
	if !strings.Contains(body, "root@winrarhost") || strings.Contains(body, "this machine") {
		t.Errorf("refusal should name the remote host: %s", body)
	}

	out := run("RequestHostChange", map[string]any{
		"summary": "Delete two leftover MQTT password scratch files",
		"reason":  "they hold a plaintext password",
		"paths":   []string{"/mnt/2tnvme/docker/volumes/z2m_data/.mqttpw", "/tmp/z2m_mqtt_pw"},
	})
	if !strings.Contains(out, "Approved") {
		t.Fatalf("declaration was not approved: %s", out)
	}

	before := len(bash.ran)
	for _, cmd := range []string{
		inScope,
		"ssh root@winrarhost rm -f /mnt/2tnvme/docker/volumes/z2m_data/.mqttpw",
		"ssh root@winrarhost 'rm -f /mnt/2tnvme/docker/volumes/z2m_data/.mqttpw; docker exec mosquitto rm -f /tmp/z2m_mqtt_pw'",
		"ssh root@winrarhost docker exec mosquitto rm -f /tmp/z2m_mqtt_pw",
		"ssh -p 22 root@winrarhost 'rm -f /tmp/z2m_mqtt_pw && ls /mnt/2tnvme/docker/volumes/z2m_data'",
	} {
		if body := run("Bash", map[string]string{"command": cmd}); strings.Contains(body, "RequestHostChange") {
			t.Errorf("%q was refused inside the approved scope: %s", cmd, body)
		}
	}
	if got := len(bash.ran) - before; got != 5 {
		t.Fatalf("ran %d of 5 in-scope commands: %v", got, bash.ran)
	}

	for _, cmd := range []string{
		// Outside the scope.
		"ssh root@winrarhost 'rm -f /mnt/2tnvme/docker/volumes/other/x'",
		"ssh root@winrarhost 'rm -f /etc/passwd'",
		// The scope root itself: a grant changes what is inside it.
		"ssh root@winrarhost 'rm -rf /mnt/2tnvme/docker/volumes/z2m_data'",
		// Not spelled out: a variable, ~. (A relative path after a remote cd
		// produces no effect at all and is not gated, before or after #305.)
		"ssh root@winrarhost 'rm -f $F'",
		"ssh root@winrarhost 'rm -f ~/.mqttpw'",
		// In scope but bundled with an out-of-scope delete.
		"ssh root@winrarhost 'rm -f /mnt/2tnvme/docker/volumes/z2m_data/.mqttpw /var/lib/other'",
		// A local word dropped from the remote script: it runs, unread.
		`ssh root@winrarhost rm -f /mnt/2tnvme/docker/volumes/z2m_data/x "$X"`,
		"ssh root@winrarhost rm -f /mnt/2tnvme/docker/volumes/z2m_data/x $(cat /tmp/l)",
		`ssh root@winrarhost 'rm -f /mnt/2tnvme/docker/volumes/z2m_data/x' "$Y"`,
		// Brace expansion and globs the remote shell rewrites past the scope.
		"ssh root@winrarhost 'rm -f /mnt/2tnvme/docker/volumes/z2m_data/.{.,}/.{.,}/.{.,}/etc/shadow'",
		"ssh root@winrarhost 'rm -f /mnt/2tnvme/docker/volumes/z2m_data/*'",
		// This machine under another name.
		"ssh localhost 'rm -f /mnt/2tnvme/docker/volumes/z2m_data/.mqttpw'",
		"ssh root@127.0.0.1 'rm -f /tmp/z2m_mqtt_pw'",
		// A recursive delete of a path the user named.
		"ssh root@winrarhost 'rm -rf /tmp/z2m_mqtt_pw'",
		"ssh root@winrarhost 'rm -Rf /tmp/z2m_mqtt_pw'",
		"ssh root@winrarhost 'rm --rec -f /tmp/z2m_mqtt_pw'",
		"ssh root@winrarhost 'rm --recursiv /tmp/z2m_mqtt_pw'",
		// A link in the scope that points out of it.
		"ssh root@winrarhost 'ln -s / /mnt/2tnvme/docker/volumes/z2m_data/r'",
		// Arguments appended at run time.
		"ssh root@winrarhost 'xargs -a /tmp/list rm -f /mnt/2tnvme/docker/volumes/z2m_data/x'",
		"ssh root@winrarhost '/usr/bin/xargs -a /tmp/l rm -f /mnt/2tnvme/docker/volumes/z2m_data/x'",
		// Writes through a remote scope are never covered: a write can plant
		// a link that takes a later in-scope path anywhere.
		"ssh root@winrarhost 'cp -s /etc/shadow /mnt/2tnvme/docker/volumes/z2m_data/r'",
		"ssh root@winrarhost 'mv /tmp/lnk /mnt/2tnvme/docker/volumes/z2m_data/r'",
		"ssh root@winrarhost 'tee /mnt/2tnvme/docker/volumes/z2m_data/r </dev/null'",
	} {
		if body := run("Bash", map[string]string{"command": cmd}); !strings.Contains(body, "RequestHostChange") {
			t.Errorf("%q ran outside the approved scope: %s", cmd, body)
		}
	}
	if asked != 1 {
		t.Errorf("asked %d times; one declaration should have been enough", asked)
	}
}

// #305 review: approving a change on another host at the drift card must not
// mint a grant that then covers this machine, and a scope the user named for
// configuration must not authorise deleting that directory recursively.
func TestRemoteApprovalDoesNotGrantThisMachine(t *testing.T) {
	g, proj := gateFixture(t)
	approved := 0
	reg, _ := testRegistry(t)
	l := New(nil, reg)
	opts := Options{
		WorkingDir: proj,
		Host:       g,
		Permission: permission.Context{Mode: permission.StaticMode(permission.ModeAutonomous)},
		Approver: ApproverFunc(func(ctx context.Context, req ApprovalRequest) permission.Decision {
			approved++
			return permission.Decision{Behavior: permission.Allow}
		}),
	}
	run := func(cmd string) string {
		raw, _ := json.Marshal(map[string]string{"command": cmd})
		tu := anthropic.BetaToolUseBlock{ID: "t", Name: "Bash", Input: json.RawMessage(raw)}
		return resultText(l.dispatch(context.Background(), tu, opts, 0, nil, nil,
			newFailureState()))
	}
	run("ssh root@winrarhost 'rm -f /opt/stack/data/old.db'")
	if approved != 1 {
		t.Fatalf("the remote delete was not put to the user (asked %d)", approved)
	}
	if n := len(g.Ledger.List()); n != 0 {
		t.Fatalf("approving a remote delete minted %d grant(s)", n)
	}

	l2 := trust.NewLedger(trust.NewRoots(t.TempDir(), t.TempDir()))
	if _, err := l2.Mint(trust.Request{Summary: "configure nginx", Paths: []string{"/etc/nginx"}}); err != nil {
		t.Fatal(err)
	}
	rmrf := trust.Effect{Zone: trust.ZoneHost, Kind: trust.KindDelete, Recursive: true, Certain: true,
		Res: trust.Resource{Class: "path", ID: "/etc/nginx"}}
	if _, drift := l2.Cover([]trust.Effect{rmrf}); len(drift) == 0 {
		t.Error("a named scope let rm -rf delete the directory itself")
	}
}

package session

import "testing"

func TestChildrenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := []ChildRecord{{ID: "agent-1", Type: "Explore", Status: "succeeded", Background: true, Result: "found it"}}
	if err := WriteChildren(dir, "ses_1", in); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := ReadChildren(dir, "ses_1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(out) != 1 || out[0].Result != "found it" {
		t.Fatalf("got %+v", out)
	}
	if err := WriteChildren(dir, "ses_1", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if again, err := ReadChildren(dir, "ses_1"); err != nil || len(again) != 0 {
		t.Fatalf("clearing left %+v, %v", again, err)
	}
}

func TestReconcileChildrenSplitsFinishedFromRunning(t *testing.T) {
	deliver, orphaned := ReconcileChildren([]ChildRecord{
		{ID: "agent-1", Status: "succeeded", Background: true},
		{ID: "agent-2", Status: "running", Background: true, Provenance: "/repo on main at abc"},
		{ID: "agent-3", Status: "succeeded", Background: false},
	})
	if len(deliver) != 1 || deliver[0].ID != "agent-1" {
		t.Errorf("deliver = %+v", deliver)
	}
	if len(orphaned) != 1 || orphaned[0].ID != "agent-2" {
		t.Errorf("orphaned = %+v", orphaned)
	}
}

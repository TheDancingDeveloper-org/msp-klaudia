package session

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ChildRecord is one sub-agent as persisted beside its session, so a resume can
// say what was still running or finished-but-undelivered when the process
// exited. It is a snapshot, not a live handle: the child itself is gone.
type ChildRecord struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Label      string `json:"label,omitempty"`
	Status     string `json:"status"`
	Background bool   `json:"background"`
	Provenance string `json:"provenance,omitempty"`
	Result     string `json:"result,omitempty"`
	Err        string `json:"err,omitempty"`
}

// ChildrenPath is the registry file for a session.
func ChildrenPath(cwd, sessionID string) string {
	return filepath.Join(Dir(cwd), sessionID+".children.json")
}

// WriteChildren persists the session's children. An empty list removes the
// file, so a session that launched nothing leaves nothing behind.
func WriteChildren(cwd, sessionID string, children []ChildRecord) error {
	path := ChildrenPath(cwd, sessionID)
	if len(children) == 0 {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(children, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}

// ReadChildren loads the persisted children. A missing file is an empty list,
// not an error: most sessions never launched one.
func ReadChildren(cwd, sessionID string) ([]ChildRecord, error) {
	body, err := os.ReadFile(ChildrenPath(cwd, sessionID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []ChildRecord
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ReconcileChildren reports what a resumed session still owes. A child that had
// finished is delivered; one that was still running is reported as orphaned,
// since its process died with the session and its checkout, if it had one, is
// named by its provenance.
func ReconcileChildren(children []ChildRecord) (deliver, orphaned []ChildRecord) {
	for _, c := range children {
		if !c.Background {
			continue
		}
		switch c.Status {
		case "succeeded", "failed":
			deliver = append(deliver, c)
		case "running":
			orphaned = append(orphaned, c)
		}
	}
	return deliver, orphaned
}

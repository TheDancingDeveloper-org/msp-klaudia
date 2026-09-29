package session

import (
	"bufio"
	"encoding/json"
	"os"

	"github.com/google/uuid"
)

// SubtypeCompactBoundary marks the point in a transcript where the
// conversation was compacted. It is written as a "system" entry, the shape the
// reference uses for its own compact_boundary lines, so Read (which returns only
// user/assistant entries) and every caller of it skip it unchanged.
const SubtypeCompactBoundary = "compact_boundary"

// MarkCompaction appends a compaction boundary: every message recorded before
// it is covered by the session's persisted summary, every message after it is
// not. Resume seeds from the summary plus the messages after the last boundary
// (ReadSinceCompaction), which is how the turns taken since the last
// compaction survive a relaunch.
func (t *Transcript) MarkCompaction() error {
	id := uuid.NewString()
	e := Entry{
		Type:       "system",
		Subtype:    SubtypeCompactBoundary,
		UUID:       id,
		ParentUUID: t.lastUUID,
		SessionID:  t.meta.SessionID,
		Timestamp:  Now(),
		CWD:        t.meta.CWD,
		GitBranch:  t.meta.GitBranch,
		UserType:   t.meta.UserType,
		Version:    t.meta.Version,
	}
	t.lastUUID = &id
	return t.w.Append(e)
}

// ReadSinceCompaction returns the user/assistant entries recorded after the
// transcript's last compaction boundary, and whether it has one. A transcript
// written before boundaries were recorded reports false: where its compaction
// happened is not known, so the caller cannot tell which messages the summary
// already covers.
func ReadSinceCompaction(path string) ([]Entry, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	var entries []Entry
	marked := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // tolerate malformed/unknown lines, as Read does
		}
		switch {
		case e.Type == "system" && e.Subtype == SubtypeCompactBoundary:
			entries, marked = nil, true
		case e.Type == "user" || e.Type == "assistant":
			entries = append(entries, e)
		}
	}
	return entries, marked, sc.Err()
}

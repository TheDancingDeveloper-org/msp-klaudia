package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A session's human-facing name lives in a small sidecar file beside the
// transcript — <id>.meta.json — for the same reason the compaction summary does
// (summary.go): the transcript is an append-only log the resume path reads back
// verbatim, so metadata that changes (a title the user renames) does not belong
// inside it. The sidecar is derived lazily from the first prompt and can be
// overwritten by /rename, without ever touching the transcript.

// maxTitleLen caps a derived or set title so listings stay one line.
const maxTitleLen = 60

// SessionMeta is the sidecar's schema. Kept deliberately small; new fields are
// added with omitempty so an older sidecar still parses.
type SessionMeta struct {
	Title string `json:"title,omitempty"`
}

// MetaPath returns the metadata sidecar path for a session in cwd's project dir.
func MetaPath(cwd, sessionID string) string {
	return filepath.Join(Dir(cwd), sessionID+".meta.json")
}

// MetaPathFor returns the metadata sidecar that sits beside a transcript file.
func MetaPathFor(transcriptPath string) string {
	return strings.TrimSuffix(transcriptPath, ".jsonl") + ".meta.json"
}

// readMeta reads a sidecar, returning (SessionMeta{}, false) when it is missing
// or unreadable — a missing title is not an error, just an unnamed session.
func readMeta(path string) (SessionMeta, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionMeta{}, false
	}
	var m SessionMeta
	if json.Unmarshal(data, &m) != nil {
		return SessionMeta{}, false
	}
	return m, true
}

// writeMeta persists a sidecar, creating the project dir if needed.
func writeMeta(path string, m SessionMeta) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// SetTitle persists a title for a session. A session located outside cwd's
// project dir (resumed from another directory) keeps its sidecar beside its
// transcript, so the title follows the conversation rather than the directory
// it was renamed from.
func SetTitle(cwd, sessionID, title string) error {
	title = cleanTitle(title)
	path := MetaPath(cwd, sessionID)
	if tp, ok := Locate(cwd, sessionID); ok {
		path = MetaPathFor(tp)
	}
	return writeMeta(path, SessionMeta{Title: title})
}

// Title returns the persisted title for a session, or ("", false) if none is
// stored. It does not derive one — that is List's lazy backfill job, which also
// persists the result.
func Title(cwd, sessionID string) (string, bool) {
	if tp, ok := Locate(cwd, sessionID); ok {
		if m, ok := readMeta(MetaPathFor(tp)); ok && m.Title != "" {
			return m.Title, true
		}
	}
	if m, ok := readMeta(MetaPath(cwd, sessionID)); ok && m.Title != "" {
		return m.Title, true
	}
	return "", false
}

// DeriveTitle builds a short title from the first user prompt in a transcript.
// Tool-result messages (role "user", but carrying no typed text) are skipped so
// the title reflects what the person actually asked. Returns "" when nothing
// usable is found.
func DeriveTitle(entries []Entry) string {
	for _, e := range entries {
		if e.Type != "user" {
			continue
		}
		if t := cleanTitle(messageText(e.Message)); t != "" {
			return t
		}
	}
	return ""
}

// messageText extracts the first text block from a recorded message. The stored
// shape is an Anthropic message param, whose content is normally an array of
// blocks but may be a bare string; both are handled.
func messageText(raw json.RawMessage) string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil || len(m.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				return b.Text
			}
		}
	}
	return ""
}

// cleanTitle reduces a prompt to a single trimmed line capped at maxTitleLen
// runes (an ellipsis marks truncation), collapsing internal whitespace.
func cleanTitle(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			s = line
			break
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > maxTitleLen {
		return strings.TrimSpace(string(r[:maxTitleLen])) + "…"
	}
	return s
}

// SessionInfo describes one stored session for a listing.
type SessionInfo struct {
	ID       string    `json:"id"`
	Title    string    `json:"title,omitempty"`
	Modified time.Time `json:"modified"`
	Project  string    `json:"project,omitempty"` // the working directory the session ran in
	Path     string    `json:"path"`              // transcript file path
}

// scan is one transcript file found on disk, before its title/project are read.
type scan struct {
	id   string
	path string
	mod  time.Time
}

// scanSessions returns every stored transcript across the sessions root and the
// legacy projects root, de-duplicated by id (the newest copy wins, as Locate
// chooses it). Empty and unreadable files are skipped. It reads no transcript
// bodies, so it stays cheap for retention pruning.
func scanSessions() []scan {
	byID := map[string]scan{}
	for _, root := range []string{SessionsRoot(), legacyProjectsRoot()} {
		matches, _ := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
		for _, p := range matches {
			st, err := os.Stat(p)
			if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
				continue
			}
			id := strings.TrimSuffix(filepath.Base(p), ".jsonl")
			if !ValidID(id) {
				continue
			}
			if cur, ok := byID[id]; ok && !st.ModTime().After(cur.mod) {
				continue
			}
			byID[id] = scan{id: id, path: p, mod: st.ModTime()}
		}
	}
	out := make([]scan, 0, len(byID))
	for _, s := range byID {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mod.After(out[j].mod) })
	return out
}

// List returns every stored session, newest first. Titles are backfilled
// lazily: a session with no sidecar has one derived from its first prompt and
// persisted, so the derivation happens once rather than on every listing. The
// project is read from the transcript's first entry.
func List() ([]SessionInfo, error) {
	scans := scanSessions()
	out := make([]SessionInfo, 0, len(scans))
	for _, s := range scans {
		info := SessionInfo{ID: s.id, Path: s.path, Modified: s.mod}
		if m, ok := readMeta(MetaPathFor(s.path)); ok {
			info.Title = m.Title
		}
		// A missing title or project both need the transcript; read it once.
		if info.Title == "" || info.Project == "" {
			if entries, err := Read(s.path); err == nil {
				if info.Project == "" {
					for _, e := range entries {
						if e.CWD != "" {
							info.Project = e.CWD
							break
						}
					}
				}
				if info.Title == "" {
					if t := DeriveTitle(entries); t != "" {
						info.Title = t
						_ = writeMeta(MetaPathFor(s.path), SessionMeta{Title: t})
					}
				}
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// deleteSessionFiles removes a transcript and its sidecars (summary, metadata),
// but only when each path sits directly inside the sessions store — a guard so
// a caller can never be tricked into deleting outside it. Missing files are not
// an error.
func deleteSessionFiles(transcriptPath string) error {
	base := strings.TrimSuffix(transcriptPath, ".jsonl")
	var firstErr error
	for _, p := range []string{transcriptPath, base + ".summary.md", base + ".meta.json"} {
		if !withinSessionsStore(p) {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// withinSessionsStore reports whether p is a file directly under a project dir
// of the sessions root or the legacy projects root (root/<project>/<file>).
// This is the safety boundary for every delete: retention and `sessions rm`
// only ever remove files that pass it.
func withinSessionsStore(p string) bool {
	p = filepath.Clean(p)
	parent := filepath.Dir(filepath.Dir(p))
	return parent == filepath.Clean(SessionsRoot()) || parent == filepath.Clean(legacyProjectsRoot())
}

// DeleteByID removes a session (transcript + summary + metadata) wherever it is
// stored. It returns (false, nil) when no session with that id exists, so a
// caller can tell "deleted" from "nothing to delete". An invalid id is rejected
// rather than matched, since it can never name a real session file.
func DeleteByID(id string) (bool, error) {
	if !ValidID(id) {
		return false, fmt.Errorf("invalid session id %q", id)
	}
	var matches []string
	for _, root := range []string{SessionsRoot(), legacyProjectsRoot()} {
		m, _ := filepath.Glob(filepath.Join(root, "*", id+".jsonl"))
		matches = append(matches, m...)
	}
	if len(matches) == 0 {
		return false, nil
	}
	var firstErr error
	for _, tp := range matches {
		if err := deleteSessionFiles(tp); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr == nil, firstErr
}

// Retention bounds how many sessions, and how old, are kept on disk. A zero
// field is no cap for that dimension; both together are the common case.
type Retention struct {
	MaxAge   time.Duration // sessions modified longer ago than this are pruned (0 = no age cap)
	MaxCount int           // keep at most this many newest sessions (0 = no count cap)
}

// Prune deletes sessions that fall outside the retention policy, newest kept
// first. The active session is never deleted and never counts against MaxCount,
// so a long-running session can't prune itself no matter how many others exist.
// It returns the ids that were removed. A policy with neither cap set is a
// no-op.
func Prune(r Retention, activeID string) ([]string, error) {
	if r.MaxAge <= 0 && r.MaxCount <= 0 {
		return nil, nil
	}
	cutoff := time.Now().Add(-r.MaxAge)
	var removed []string
	var firstErr error
	kept := 0
	for _, s := range scanSessions() {
		if s.id == activeID {
			continue // the active session is untouchable and off the count
		}
		overCount := r.MaxCount > 0 && kept >= r.MaxCount
		overAge := r.MaxAge > 0 && s.mod.Before(cutoff)
		if overCount || overAge {
			if err := deleteSessionFiles(s.path); err != nil && firstErr == nil {
				firstErr = err
			}
			removed = append(removed, s.id)
			continue
		}
		kept++
	}
	return removed, firstErr
}

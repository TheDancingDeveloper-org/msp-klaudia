package cli

import (
	"encoding/json"
	"sync"

	"github.com/google/uuid"

	"github.com/greenthread-ai/klaudia/internal/session"
)

// sessionRecorder is the run's transcript recorder, able to move on to a new
// session mid-run. /clear uses that: the conversation it ends stays behind as
// its own resumable session, and what follows is recorded under a fresh id
// with no transcript and no compaction summary of its own yet. Resetting only
// the in-memory history, as /clear used to, kept appending to the same
// transcript, so the next launch auto-resumed exactly what had been cleared.
//
// It implements agent.Recorder. Every mode records through one; only the TUI
// ever rotates it.
type sessionRecorder struct {
	mu   sync.Mutex
	meta session.Meta
	tr   *session.Transcript // nil when the transcript could not be opened
}

// newSessionRecorder opens the transcript for meta. A transcript that cannot be
// opened is not fatal: the run goes on and records nothing, as before.
func newSessionRecorder(meta session.Meta) *sessionRecorder {
	r := &sessionRecorder{meta: meta}
	r.tr, _ = session.NewTranscript(meta)
	return r
}

// Record implements agent.Recorder against the current session's transcript.
func (r *sessionRecorder) Record(role string, message json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tr == nil {
		return nil
	}
	return r.tr.Record(role, message)
}

// ID is the session id being recorded now.
func (r *sessionRecorder) ID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.meta.SessionID
}

// SummaryPath is where the current session's compaction summary belongs:
// beside its transcript, which for a session resumed from another directory is
// the file it was found in.
func (r *sessionRecorder) SummaryPath() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.meta.Path
	if p == "" {
		p = session.Path(r.meta.CWD, r.meta.SessionID)
	}
	return session.SummaryPathFor(p)
}

// Rotate ends the current session and starts recording a new one under a
// freshly minted id, in the working directory's own session dir. It returns
// the new id, and the previous id when that session recorded something (""
// when it did not, since there is then nothing to resume).
//
// The previous transcript is marked cleared and closed, never deleted. Its
// summary, if any, stays beside it: the new session starts without one.
func (r *sessionRecorder) Rotate() (newID, prevID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tr != nil {
		if recorded, _ := r.tr.MarkCleared(); recorded {
			prevID = r.meta.SessionID
		}
		_ = r.tr.Close()
	}
	r.meta.SessionID = uuid.NewString()
	r.meta.Path = ""
	r.tr, _ = session.NewTranscript(r.meta)
	return r.meta.SessionID, prevID
}

// MarkCompaction records a compaction boundary in the current transcript, so a
// resume can seed from the summary and replay the messages recorded after it.
func (r *sessionRecorder) MarkCompaction() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tr == nil {
		return nil
	}
	return r.tr.MarkCompaction()
}

// DropLastMessages drops the last n messages from the current transcript (the
// TUI's rewind). It is a no-op when no transcript is open.
func (r *sessionRecorder) DropLastMessages(n int) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tr == nil {
		return 0, nil
	}
	return r.tr.DropLastMessages(n)
}

// HasTranscript reports whether a transcript is open, so a caller can wire
// rewind only when there is something on disk to rewind.
func (r *sessionRecorder) HasTranscript() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tr != nil
}

// Close closes the current transcript.
func (r *sessionRecorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tr == nil {
		return nil
	}
	return r.tr.Close()
}

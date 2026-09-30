package cli

import (
	"encoding/json"
	"io"
	"sync"

	"github.com/greenthread-ai/klaudia/internal/agent"
)

// swapRecorder is an agent.Recorder whose target transcript can be repointed at
// runtime. The interactive /resume picker uses it: selecting another session
// closes the current transcript and swaps in the resumed one, so subsequent
// turns append to the session the user picked rather than the one Klaudia
// launched in. Every method is safe to call from the UI goroutine (which swaps)
// and the agent goroutine (which records) at once.
type swapRecorder struct {
	mu    sync.Mutex
	inner agent.Recorder // may be nil when the launch transcript failed to open
}

func newSwapRecorder(inner agent.Recorder) *swapRecorder {
	return &swapRecorder{inner: inner}
}

// Record forwards to the current inner recorder, or is a no-op when there is
// none (a transcript that failed to open must not fail the run).
func (s *swapRecorder) Record(role string, message json.RawMessage) error {
	s.mu.Lock()
	r := s.inner
	s.mu.Unlock()
	if r == nil {
		return nil
	}
	return r.Record(role, message)
}

// swap replaces the inner recorder, returning the previous one (may be nil) so
// the caller can close it.
func (s *swapRecorder) swap(next agent.Recorder) agent.Recorder {
	s.mu.Lock()
	prev := s.inner
	s.inner = next
	s.mu.Unlock()
	return prev
}

// current returns the inner recorder without changing it.
func (s *swapRecorder) current() agent.Recorder {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner
}

// closeRecorder closes r if it holds a closable transcript. Nil and
// non-closable recorders are ignored, so callers can hand it whatever swap
// returned without a type check.
func closeRecorder(r agent.Recorder) {
	if c, ok := r.(io.Closer); ok {
		_ = c.Close()
	}
}

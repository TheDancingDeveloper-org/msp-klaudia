package tui

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/api"
)

// A dependency logging to the standard logger is what corrupted a frame in a
// real session (chromedp's default browser logger is log.Printf, so a burst of
// "unhandled node event" CDP noise landed on stderr mid-repaint, tearing the
// spinner line and the input box border).
func TestQuietStandardLoggerSwallowsDependencyOutput(t *testing.T) {
	var seen bytes.Buffer
	log.SetOutput(&seen)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	restore := quietStandardLogger()
	log.Printf("ERROR: unhandled node event %s", "*dom.EventAdRelatedStateUpdated")
	if seen.Len() != 0 {
		t.Fatalf("dependency log reached the terminal writer: %q", seen.String())
	}

	restore()
	log.Printf("after restore")
	if !strings.Contains(seen.String(), "after restore") {
		t.Fatalf("restore did not put the previous writer back, got %q", seen.String())
	}
}

func TestQuietStandardLoggerKeepsOutputWhenLogFileSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "klaudia.log")
	t.Setenv("KLAUDIA_LOG", path)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	restore := quietStandardLogger()
	log.Printf("ERROR: unhandled node event")
	restore()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "unhandled node event") {
		t.Fatalf("log file missing the message, got %q", data)
	}
}

// failModelCall makes one model call that the endpoint rejects, so the
// provider writes its per-attempt line wherever the model log points.
func failModelCall(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "no such route")
	}))
	t.Cleanup(srv.Close)
	p := api.NewOpenAIProvider(srv.URL+"/v1", "k", nil, nil)
	_, _ = p.StreamTurn(context.Background(), anthropic.BetaMessageNewParams{
		Model:     "unit-model",
		MaxTokens: 16,
	}, api.StreamSink{})
}

// The provider's model-call lines go to stderr — the terminal the TUI renders
// inline on — so while the TUI runs they are redirected like the standard
// logger, and come back once it exits.
func TestQuietStandardLoggerRedirectsModelCallLines(t *testing.T) {
	var terminal bytes.Buffer
	t.Cleanup(api.SetModelLog(&terminal))

	restore := quietStandardLogger()
	failModelCall(t)
	if terminal.Len() != 0 {
		t.Fatalf("model-call line reached the terminal during the TUI: %q", terminal.String())
	}
	restore()

	failModelCall(t)
	if !strings.Contains(terminal.String(), `"model_call"`) {
		t.Fatalf("restore did not put the model log back, got %q", terminal.String())
	}
}

func TestQuietStandardLoggerSendsModelCallLinesToLogFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "klaudia.log")
	t.Setenv("KLAUDIA_LOG", path)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	var terminal bytes.Buffer
	t.Cleanup(api.SetModelLog(&terminal))

	restore := quietStandardLogger()
	failModelCall(t)
	restore()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), `"model_call"`) || terminal.Len() != 0 {
		t.Fatalf("log file = %q, terminal = %q; want the line in the file only", data, terminal.String())
	}
}

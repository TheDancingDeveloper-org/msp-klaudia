package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// scriptServer answers each request with the next scripted status and body,
// repeating the last entry once the script runs out.
func scriptServer(t *testing.T, script []struct {
	status int
	body   string
}) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		if i >= len(script) {
			i = len(script) - 1
		}
		w.WriteHeader(script[i].status)
		_, _ = io.WriteString(w, script[i].body)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func noWait(t *testing.T) {
	t.Helper()
	prev := backoffUnit
	backoffUnit = 0
	t.Cleanup(func() { backoffUnit = prev })
}

func doOnce(t *testing.T, p *OpenAIProvider) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, p.baseURL+"/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	return p.doWithRetry(req, []byte(`{"model":"m"}`))
}

// An opaque 400 — a bare "invalid request" with no error envelope — is retried,
// and the retry succeeding is what the caller sees.
func TestOpaque400RetriedThenSucceeds(t *testing.T) {
	noWait(t)
	t.Setenv("KLAUDIA_MAX_RETRIES", "0") // the opaque budget stands on its own
	srv, n := scriptServer(t, []struct {
		status int
		body   string
	}{
		{400, "invalid request"},
		{200, "ok"},
	})
	p := NewOpenAIProvider(srv.URL+"/v1", "k", nil, nil)

	resp, err := doOnce(t, p)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 after retry", resp.StatusCode)
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

// A 400 that names a real client error fails immediately, with no retry.
func TestDetailed400FailsImmediately(t *testing.T) {
	t.Setenv("KLAUDIA_MAX_RETRIES", "5")
	body := `{"error":{"message":"model 'nope' does not exist","type":"invalid_request_error"}}`
	srv, n := scriptServer(t, []struct {
		status int
		body   string
	}{{400, body}})
	p := NewOpenAIProvider(srv.URL+"/v1", "k", nil, nil)

	start := time.Now()
	resp, err := doOnce(t, p)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("attempts = %d, want exactly 1 (no retry)", got)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("took %s — a detailed 400 must not wait out a backoff", time.Since(start))
	}
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), "does not exist") {
		t.Fatalf("body = %q, want the original error intact for the caller", got)
	}
}

// An opaque 400 that never clears stops after the bounded budget rather than
// looping, even with a large ordinary retry budget.
func TestOpaque400RetryIsBounded(t *testing.T) {
	noWait(t)
	t.Setenv("KLAUDIA_MAX_RETRIES", "20")
	srv, n := scriptServer(t, []struct {
		status int
		body   string
	}{{400, ""}})
	p := NewOpenAIProvider(srv.URL+"/v1", "k", nil, nil)

	resp, err := doOnce(t, p)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want the final 400", resp.StatusCode)
	}
	// The original attempt plus opaque400Retries, and no more.
	if got, want := int(n.Load()), 1+opaque400Retries; got != want {
		t.Fatalf("attempts = %d, want %d", got, want)
	}
}

// A plain-text 400 that is more than the bare phrase is a real error.
func TestOpaque400OnlyMatchesTheBarePhrase(t *testing.T) {
	t.Setenv("KLAUDIA_MAX_RETRIES", "5")
	srv, n := scriptServer(t, []struct {
		status int
		body   string
	}{{400, "invalid request: messages[2].role must be 'tool'"}})
	p := NewOpenAIProvider(srv.URL+"/v1", "k", nil, nil)

	resp, err := doOnce(t, p)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := n.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 — a specific message is not opaque", got)
	}
}

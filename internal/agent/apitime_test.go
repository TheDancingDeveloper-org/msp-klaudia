package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/api"
	"github.com/greenthread-ai/klaudia/internal/tools"
)

// slowProvider takes a fixed time to answer each request, then answers like
// recordingProvider, or fails with err when it is set.
type slowProvider struct {
	recordingProvider
	delay time.Duration
	err   error
}

func (p *slowProvider) StreamTurn(ctx context.Context, params anthropic.BetaMessageNewParams, sink api.StreamSink) (anthropic.BetaMessage, error) {
	time.Sleep(p.delay)
	if p.err != nil {
		return anthropic.BetaMessage{}, p.err
	}
	return p.recordingProvider.StreamTurn(ctx, params, sink)
}

// duration_api_ms used to be the wall time. It has to leave the tools out, or
// it cannot say whether a slow run was the model or the commands it ran.
func TestAPIDurationCountsRequestsNotTools(t *testing.T) {
	const apiDelay, toolDelay = 30 * time.Millisecond, 300 * time.Millisecond
	rec := &recordingTool{onExec: func(string) { time.Sleep(toolDelay) }}
	provider := &slowProvider{delay: apiDelay, recordingProvider: recordingProvider{turns: []anthropic.BetaMessage{
		toolUseTurn(t, "t1", "Recorder", map[string]any{"note": "slow"}),
	}}}

	start := time.Now()
	res, err := New(provider, tools.NewRegistry(rec)).Run(context.Background(), Options{
		Prompt: "go", Permission: bypassPerm(),
	}, nil)
	wall := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(provider.requests))
	}
	if res.APIDuration < 2*apiDelay {
		t.Errorf("APIDuration = %v, want at least the two requests' %v", res.APIDuration, 2*apiDelay)
	}
	// The tool and the requests do not overlap, so whatever the scheduler
	// does, the wall time left over once the API time is taken out holds the
	// tool's sleep in full.
	if wall-res.APIDuration < toolDelay {
		t.Errorf("APIDuration = %v of %v wall: the %v tool call was counted as API time", res.APIDuration, wall, toolDelay)
	}
}

// A request that fails still took the provider's time.
func TestAPIDurationCountsAFailedRequest(t *testing.T) {
	const apiDelay = 30 * time.Millisecond
	provider := &slowProvider{delay: apiDelay, err: errors.New("boom")}
	res, err := New(provider, tools.NewRegistry()).Run(context.Background(), Options{Prompt: "go", Permission: bypassPerm()}, nil)
	if err == nil {
		t.Fatal("want the provider's error")
	}
	if res.APIDuration < apiDelay {
		t.Errorf("APIDuration = %v, want at least %v", res.APIDuration, apiDelay)
	}
}

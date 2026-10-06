package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// A server behind a bearer token could not be reached: there was nowhere to
// put the header. It now comes from "headers", with ${VAR} kept out of the file.
func TestHTTPServerHeaders(t *testing.T) {
	isolateConfigRoot(t)
	t.Setenv("MCP_TEST_BEARER", "tok-123")
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "s", Version: "0"}, nil)
	srv.AddTool(&mcpsdk.Tool{Name: "t", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{}, nil
		})
	h := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	defer hs.Close()

	m, errs := Connect(context.Background(), Config{MCPServers: map[string]ServerConfig{
		"auth": {URL: hs.URL, Headers: map[string]string{"Authorization": "Bearer ${MCP_TEST_BEARER}"}, AlwaysLoad: true},
	}}, nil)
	defer m.Close()
	if len(errs) > 0 {
		t.Fatalf("connect with the header: %v", errs)
	}
	if len(m.Tools(context.Background())) != 1 {
		t.Error("expected the server's tool")
	}
	if !m.AlwaysLoad("auth") || m.AlwaysLoad("other") {
		t.Error("AlwaysLoad should reflect the server's config")
	}

	m2, errs := Connect(context.Background(), Config{MCPServers: map[string]ServerConfig{"auth": {URL: hs.URL}}}, nil)
	defer m2.Close()
	if len(errs) == 0 {
		t.Error("without the header the server should refuse the connection")
	}
}

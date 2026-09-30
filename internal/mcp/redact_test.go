package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A token placed in a URL by ${VAR} reached the connect error, and from there
// the terminal, the TUI and the model.
func TestConnectErrorRedactsResolvedValues(t *testing.T) {
	isolateConfigRoot(t)
	t.Setenv("MCP_TEST_TOKEN", "sk-secret-4f9a")
	_, err := connectServer(context.Background(), "remote", ServerConfig{
		URL: "http://127.0.0.1:1/mcp?key=${MCP_TEST_TOKEN}",
	}, nil)
	if err == nil {
		t.Fatal("want a connect error (nothing listens on port 1)")
	}
	if strings.Contains(err.Error(), "sk-secret-4f9a") {
		t.Errorf("error leaks the token: %v", err)
	}
	if !strings.Contains(err.Error(), "${MCP_TEST_TOKEN}") {
		t.Logf("error did not mention the URL at all: %v", err)
	}
}

func TestRedactValues(t *testing.T) {
	base := errors.New("dial https://h/x?k=abcd1234&u=abcd: refused (id 7)")
	got := redactValues(base, map[string]string{"KEY": "abcd1234", "USER": "abcd", "N": "7"})
	if got.Error() != "dial https://h/x?k=${KEY}&u=${USER}: refused (id 7)" {
		t.Errorf("redacted = %q", got.Error())
	}
	if !errors.Is(got, base) {
		t.Error("the original error should stay reachable")
	}
	if redactValues(base, nil) != base {
		t.Error("nothing to redact should return the error unchanged")
	}
}

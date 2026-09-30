package mcp

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// noEnv is a lookup that resolves nothing; withEnv resolves a fixed map.
func noEnv(string) (string, bool) { return "", false }
func withEnv(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// A nil OAuth block means "no OAuth": the transport gets no custom client.
func TestOAuthNilConfigMeansNoClient(t *testing.T) {
	c, err := oauthHTTPClient(context.Background(), "srv", nil, noEnv)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if c != nil {
		t.Errorf("client = %v, want nil (transport default)", c)
	}
}

func TestOAuthConfigValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		oc   *OAuthConfig
	}{
		{"missing grant", &OAuthConfig{TokenURL: "https://x/token"}},
		{"unknown grant", &OAuthConfig{Grant: "implicit", TokenURL: "https://x/token"}},
		{"cc missing tokenURL", &OAuthConfig{Grant: "client_credentials", ClientID: "id"}},
		{"cc missing clientID", &OAuthConfig{Grant: "client_credentials", TokenURL: "https://x/token"}},
		{"cc unset secret env", &OAuthConfig{Grant: "client_credentials", TokenURL: "https://x/token", ClientID: "id", ClientSecretEnv: "UNSET_XYZ"}},
		{"cc unset id env", &OAuthConfig{Grant: "client_credentials", TokenURL: "https://x/token", ClientIDEnv: "UNSET_XYZ"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := oauthHTTPClient(context.Background(), "srv", tc.oc, noEnv); err == nil {
				t.Errorf("want error for %s", tc.name)
			}
		})
	}
}

// End-to-end client_credentials: the client Klaudia builds fetches a token from
// the token endpoint and attaches it as a bearer to the actual request.
func TestClientCredentialsAttachesBearer(t *testing.T) {
	var seenAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "client_credentials" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"tok-123","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/resource", func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	oc := &OAuthConfig{
		Grant:           "client_credentials",
		TokenURL:        ts.URL + "/token",
		ClientID:        "client-a",
		ClientSecretEnv: "MCP_TEST_SECRET",
		Scopes:          []string{"mcp"},
	}
	client, err := oauthHTTPClient(context.Background(), "srv", oc, withEnv(map[string]string{"MCP_TEST_SECRET": "s3cret"}))
	if err != nil {
		t.Fatalf("oauthHTTPClient: %v", err)
	}
	if client == nil {
		t.Fatal("client is nil")
	}
	resp, err := client.Get(ts.URL + "/resource")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if seenAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want %q", seenAuth, "Bearer tok-123")
	}
}

// authorization_code with no stored token connects unauthenticated (nil client):
// the interactive acquisition is the deferred step.
func TestAuthorizationCodeWithoutTokenIsUnauthenticated(t *testing.T) {
	isolateConfigRoot(t)
	oc := &OAuthConfig{
		Grant:    "authorization_code",
		TokenURL: "https://example.com/token",
		AuthURL:  "https://example.com/authorize",
		ClientID: "id",
	}
	c, err := oauthHTTPClient(context.Background(), "srv", oc, noEnv)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if c != nil {
		t.Errorf("client = %v, want nil until a token is acquired", c)
	}
}

func TestTokenStoreRoundTrip(t *testing.T) {
	isolateConfigRoot(t)
	store := newTokenStore("srv")

	if tok, err := store.load(); err != nil || tok != nil {
		t.Fatalf("empty store: tok=%v err=%v, want nil,nil", tok, err)
	}

	want := &oauth2.Token{
		AccessToken:  "acc",
		RefreshToken: "ref",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour).Round(time.Second),
	}
	if err := store.save(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := store.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Errorf("loaded %+v, want %+v", got, want)
	}

	info, err := os.Stat(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != fs.FileMode(0o600) {
		t.Errorf("token file perms = %o, want 600 (it holds a bearer credential)", perm)
	}
}

// A refreshed token is written back to the store; an unchanged token is not
// re-fetched-and-persisted redundantly (verified by the store reflecting the
// latest token after each call).
func TestPersistingTokenSourceWritesRefreshedTokens(t *testing.T) {
	store := &tokenStore{path: filepath.Join(t.TempDir(), "srv.json")}
	src := &stepSource{toks: []*oauth2.Token{
		{AccessToken: "A", Expiry: time.Now().Add(time.Hour)},
		{AccessToken: "B", Expiry: time.Now().Add(2 * time.Hour)},
	}}
	ps := newPersistingTokenSource(src, store)

	if _, err := ps.Token(); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.load(); got == nil || got.AccessToken != "A" {
		t.Fatalf("after first Token: stored = %v, want A", got)
	}
	if _, err := ps.Token(); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.load(); got == nil || got.AccessToken != "B" {
		t.Fatalf("after refresh: stored = %v, want B", got)
	}
}

// stepSource returns each token in turn, then repeats the last.
type stepSource struct {
	toks []*oauth2.Token
	i    int
}

func (s *stepSource) Token() (*oauth2.Token, error) {
	tok := s.toks[s.i]
	if s.i < len(s.toks)-1 {
		s.i++
	}
	return tok, nil
}

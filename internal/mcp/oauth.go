package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/greenthread-ai/klaudia/internal/session"
)

// OAuthConfig configures OAuth 2.1 authorization for a remote (HTTP/SSE) MCP
// server. It hangs off ServerConfig.OAuth and applies only when a URL is set.
//
// Two grants are recognised:
//
//	client_credentials  RFC 6749 §4.4. A fully non-interactive machine-to-machine
//	                    flow: Klaudia exchanges a client id + secret for an access
//	                    token at TokenURL and refreshes it transparently. This is
//	                    implemented end to end.
//	authorization_code  RFC 6749 §4.1 / OAuth 2.1. The interactive user flow.
//	                    Token *storage and refresh* are implemented (a token
//	                    obtained out of band is loaded, used, and re-persisted as
//	                    it refreshes); the interactive acquisition step — opening a
//	                    browser and catching the redirect — is left as follow-up
//	                    (see CHANGELOG). Without a stored token the server is
//	                    reached unauthenticated and will answer 401.
//
// Secrets are never written in the config file. ClientSecretEnv (and the
// optional ClientIDEnv) name an environment variable, mirroring the apiKeyEnv /
// extraHeadersEnv convention used elsewhere: the file carries the NAME, the
// value stays in the environment.
type OAuthConfig struct {
	// Grant is "client_credentials" or "authorization_code".
	Grant string `json:"grant,omitempty"`
	// TokenURL is the token endpoint (required for both grants).
	TokenURL string `json:"tokenUrl,omitempty"`
	// AuthURL is the authorization endpoint (authorization_code only).
	AuthURL string `json:"authUrl,omitempty"`
	// RedirectURL is the OAuth redirect (authorization_code only).
	RedirectURL string `json:"redirectUrl,omitempty"`
	// ClientID is the OAuth client id. ClientIDEnv, when set, overrides it from
	// the environment (for a client id an operator would rather not commit).
	ClientID    string `json:"clientId,omitempty"`
	ClientIDEnv string `json:"clientIdEnv,omitempty"`
	// ClientSecretEnv names the environment variable holding the client secret.
	// The secret itself is never read from the config file.
	ClientSecretEnv string `json:"clientSecretEnv,omitempty"`
	// Scopes are the requested OAuth scopes.
	Scopes []string `json:"scopes,omitempty"`
}

// oauthHTTPClient builds the *http.Client a remote server's transport should use
// to authenticate, or (nil, nil) when the server has no OAuth block (the
// transport then uses its default client and sends no Authorization header).
//
// lookup is os.LookupEnv in production; tests pass a fake so no real environment
// or token endpoint is touched.
func oauthHTTPClient(ctx context.Context, server string, oc *OAuthConfig, lookup func(string) (string, bool)) (*http.Client, error) {
	if oc == nil {
		return nil, nil
	}
	grant := strings.ToLower(strings.TrimSpace(oc.Grant))
	switch grant {
	case "client_credentials":
		return clientCredentialsClient(ctx, server, oc, lookup)
	case "authorization_code":
		return authorizationCodeClient(ctx, server, oc, lookup)
	case "":
		return nil, fmt.Errorf("mcp %q: oauth.grant is required (client_credentials or authorization_code)", server)
	default:
		return nil, fmt.Errorf("mcp %q: unknown oauth.grant %q (want client_credentials or authorization_code)", server, oc.Grant)
	}
}

// clientID resolves the client id, preferring ClientIDEnv over the inline
// ClientID. A named-but-unset variable is an error, matching how a missing
// secret is reported rather than silently becoming "".
func (oc *OAuthConfig) clientID(lookup func(string) (string, bool)) (string, error) {
	if oc.ClientIDEnv != "" {
		v, ok := lookup(oc.ClientIDEnv)
		if !ok {
			return "", fmt.Errorf("oauth.clientIdEnv names %s, which is not set", oc.ClientIDEnv)
		}
		return v, nil
	}
	return oc.ClientID, nil
}

// clientSecret resolves the client secret from ClientSecretEnv. An unset
// variable is an error naming it — never an empty secret sent to the server.
func (oc *OAuthConfig) clientSecret(lookup func(string) (string, bool)) (string, error) {
	if oc.ClientSecretEnv == "" {
		return "", nil
	}
	v, ok := lookup(oc.ClientSecretEnv)
	if !ok {
		return "", fmt.Errorf("oauth.clientSecretEnv names %s, which is not set", oc.ClientSecretEnv)
	}
	return v, nil
}

func clientCredentialsClient(ctx context.Context, server string, oc *OAuthConfig, lookup func(string) (string, bool)) (*http.Client, error) {
	if strings.TrimSpace(oc.TokenURL) == "" {
		return nil, fmt.Errorf("mcp %q: oauth.tokenUrl is required for client_credentials", server)
	}
	id, err := oc.clientID(lookup)
	if err != nil {
		return nil, fmt.Errorf("mcp %q: %w", server, err)
	}
	if id == "" {
		return nil, fmt.Errorf("mcp %q: oauth.clientId (or clientIdEnv) is required for client_credentials", server)
	}
	secret, err := oc.clientSecret(lookup)
	if err != nil {
		return nil, fmt.Errorf("mcp %q: %w", server, err)
	}
	cc := &clientcredentials.Config{
		ClientID:     id,
		ClientSecret: secret,
		TokenURL:     oc.TokenURL,
		Scopes:       oc.Scopes,
	}
	// The returned client's TokenSource fetches on first use and refreshes as
	// the token expires; the token is not persisted because it can always be
	// re-minted from the client credentials.
	return cc.Client(ctx), nil
}

func authorizationCodeClient(ctx context.Context, server string, oc *OAuthConfig, lookup func(string) (string, bool)) (*http.Client, error) {
	if strings.TrimSpace(oc.TokenURL) == "" {
		return nil, fmt.Errorf("mcp %q: oauth.tokenUrl is required for authorization_code", server)
	}
	id, err := oc.clientID(lookup)
	if err != nil {
		return nil, fmt.Errorf("mcp %q: %w", server, err)
	}
	secret, err := oc.clientSecret(lookup)
	if err != nil {
		return nil, fmt.Errorf("mcp %q: %w", server, err)
	}
	store := newTokenStore(server)
	tok, err := store.load()
	if err != nil {
		return nil, fmt.Errorf("mcp %q: oauth token store: %w", server, err)
	}
	if tok == nil {
		// No token yet. The interactive acquisition (browser + redirect capture)
		// is the deferred step; connect unauthenticated so the failure is the
		// server's honest 401 rather than a confusing client-side error.
		return nil, nil
	}
	conf := &oauth2.Config{
		ClientID:     id,
		ClientSecret: secret,
		Scopes:       oc.Scopes,
		RedirectURL:  oc.RedirectURL,
		Endpoint:     oauth2.Endpoint{AuthURL: oc.AuthURL, TokenURL: oc.TokenURL},
	}
	// conf.TokenSource refreshes with the refresh_token as the access token
	// expires; the persisting wrapper writes each new token back to the store so
	// a later session starts from the refreshed one.
	src := newPersistingTokenSource(conf.TokenSource(ctx, tok), store)
	return oauth2.NewClient(ctx, src), nil
}

// --- token storage -------------------------------------------------------

// tokenStore persists a single server's OAuth token as JSON under the config
// root, so a refreshed token survives across sessions.
type tokenStore struct {
	path string
}

// newTokenStore returns the store for a server. Tokens live in
// <config>/mcp-oauth/<server>.json (honouring KLAUDIA_CONFIG_DIR).
func newTokenStore(server string) *tokenStore {
	return &tokenStore{path: filepath.Join(session.ConfigRoot(), "mcp-oauth", server+".json")}
}

// load returns the stored token, or (nil, nil) when none is stored.
func (s *tokenStore) load() (*oauth2.Token, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

// save writes the token with 0600 perms — it carries a bearer credential and
// must not be world-readable.
func (s *tokenStore) save(tok *oauth2.Token) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

// persistingTokenSource wraps an oauth2.TokenSource and writes the token back to
// a store whenever it changes (i.e. after a refresh). It is safe for concurrent
// use: the transport may fetch a token from more than one goroutine.
type persistingTokenSource struct {
	src   oauth2.TokenSource
	store *tokenStore

	mu   sync.Mutex
	last *oauth2.Token
}

func newPersistingTokenSource(src oauth2.TokenSource, store *tokenStore) *persistingTokenSource {
	return &persistingTokenSource{src: src, store: store}
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.src.Token()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	changed := p.last == nil || p.last.AccessToken != tok.AccessToken ||
		p.last.RefreshToken != tok.RefreshToken || !p.last.Expiry.Equal(tok.Expiry)
	if changed {
		// Best effort: a failed write must not fail the request that is
		// otherwise fully authorized. A subsequent refresh will try again.
		_ = p.store.save(tok)
		p.last = tok
	}
	p.mu.Unlock()
	return tok, nil
}

package app

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

type mcpOAuthConfig struct {
	ClientID            string   `json:"client_id,omitempty"`
	ClientSecretEnv     string   `json:"client_secret_env,omitempty"`
	Issuer              string   `json:"issuer,omitempty"`
	ClientIDMetadataURL string   `json:"client_id_metadata_url,omitempty"`
	Scopes              []string `json:"scopes,omitempty"`
	CallbackPort        int      `json:"callback_port,omitzero"`
}

func validateMCPOAuthConfig(config mcpServerConfig) error {
	o := config.OAuth
	if o == nil {
		return nil
	}
	if config.URL == "" || config.BearerTokenEnv != "" {
		return fmt.Errorf("oauth requires url and cannot be combined with bearer_token_env")
	}
	if o.CallbackPort < 0 || o.CallbackPort > 65535 {
		return fmt.Errorf("oauth callback_port must be between 0 and 65535")
	}
	if o.ClientSecretEnv != "" && (o.ClientID == "" || !mcpEnvName.MatchString(o.ClientSecretEnv) || o.Issuer == "") {
		return fmt.Errorf("oauth client_secret_env requires client_id and issuer")
	}
	if o.ClientID != "" && o.ClientIDMetadataURL != "" {
		return fmt.Errorf("oauth requires only one client registration method")
	}
	for _, value := range []string{o.Issuer, o.ClientIDMetadataURL} {
		if value != "" {
			if err := validateProviderBaseURL(value, true); err != nil {
				return fmt.Errorf("oauth metadata and issuer require HTTPS or loopback HTTP URLs")
			}
		}
	}
	if strings.ContainsAny(o.ClientID, "\r\n") || len(o.Scopes) > 64 {
		return fmt.Errorf("invalid oauth client or scopes")
	}
	for _, scope := range o.Scopes {
		if scope == "" || strings.ContainsAny(scope, " \t\r\n") {
			return fmt.Errorf("invalid oauth scope")
		}
	}
	return nil
}

func mcpOAuthPath(paths ConfigPaths, name string) string {
	return filepath.Join(paths.Home, "mcp-oauth-"+name+".json")
}
func mcpOAuthBinding(config mcpServerConfig) string {
	data, _ := json.Marshal(struct {
		URL   string
		OAuth *mcpOAuthConfig
	}{config.URL, config.OAuth})
	return digest(data)
}

type mcpOAuthSession struct {
	Generation string        `json:"generation"`
	Binding    string        `json:"binding"`
	Config     oauth2.Config `json:"config"`
	Token      oauth2.Token  `json:"token"`
}

const maxMCPOAuthFile = 1 << 20

func marshalMCPOAuthSession(session mcpOAuthSession) ([]byte, error) {
	data, err := json.Marshal(session)
	if err != nil {
		return nil, err
	}
	if len(data) > maxMCPOAuthFile {
		return nil, fmt.Errorf("OAuth cache exceeds 1 MiB")
	}
	return data, nil
}

type mcpOAuthTransport struct{ allowLoopback bool }

func (t mcpOAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clean := *r.URL
	clean.RawQuery = ""
	clean.Fragment = ""
	if err := validateProviderBaseURL(clean.String(), t.allowLoopback); err != nil {
		return nil, fmt.Errorf("unsafe OAuth endpoint")
	}
	response, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > maxMCPMessage {
		response.Body.Close()
		return nil, fmt.Errorf("OAuth response exceeds 16 MiB")
	}
	response.Body = http.MaxBytesReader(nil, response.Body, maxMCPMessage)
	return response, nil
}
func mcpOAuthHTTPClient(config mcpServerConfig) *http.Client {
	endpoint, _ := url.Parse(config.URL)
	return &http.Client{Transport: mcpOAuthTransport{allowLoopback: endpoint.Scheme == "http"}, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("OAuth redirects are disabled") }}
}

// The SDK asks for tokens with its detached connection context. Defer token
// lookup to RoundTrip, where the actual request deadline includes cancellation.
type mcpOAuthSDKHandler struct{ auth.OAuthHandler }

func (mcpOAuthSDKHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	return nil, nil
}

type mcpOAuthBearerTransport struct {
	http.RoundTripper
	handler auth.OAuthHandler
}

func (t mcpOAuthBearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	source, err := t.handler.TokenSource(request.Context())
	if err != nil {
		return nil, err
	}
	if source != nil {
		if cached, ok := source.(*mcpOAuthTokenSource); ok {
			cached.allowRefresh = request.Method != http.MethodDelete
			if request.GetBody != nil {
				body, err := request.GetBody()
				if err != nil {
					return nil, err
				}
				var message struct {
					ID json.RawMessage `json:"id"`
				}
				err = json.NewDecoder(io.LimitReader(body, maxMCPMessage)).Decode(&message)
				body.Close()
				if err != nil {
					return nil, err
				}
				cached.allowRefresh = cached.allowRefresh && len(message.ID) != 0
			}
		}
		token, err := source.Token()
		if err != nil {
			return nil, err
		}
		request = request.Clone(request.Context())
		request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	}
	return t.RoundTripper.RoundTrip(request)
}

func mcpOAuthMCPClient(config mcpServerConfig, handler auth.OAuthHandler) *http.Client {
	client := mcpOAuthHTTPClient(config)
	client.Timeout = 0 // Startup and tool contexts have different deadlines; token requests keep their own limit.
	client.Transport = mcpOAuthBearerTransport{RoundTripper: client.Transport, handler: handler}
	return client
}

type mcpOAuthHandler struct {
	mu         sync.Mutex
	base       *auth.AuthorizationCodeHandler
	paths      ConfigPaths
	name       string
	config     mcpServerConfig
	path       string
	generation string
	login      bool
	pending    *mcpOAuthSession // Kept in memory until the MCP connection succeeds.
}

func (h *mcpOAuthHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	if !h.login {
		_ = resp.Body.Close()
		return fmt.Errorf("OAuth login required; run meldra mcp login %s", h.name)
	}
	return h.base.Authorize(ctx, req, resp)
}

// TokenSource reads the latest cache while holding the cross-process lock. This
// makes logout/relogin and refresh rotation serialize across Meldra processes;
// the request context also bounds a refresh HTTP call.
func (h *mcpOAuthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	if h.login {
		source, err := h.base.TokenSource(ctx)
		if source == nil || err != nil {
			return nil, err
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending != nil {
		return &mcpOAuthPendingTokenSource{ctx: ctx, paths: h.paths, name: h.name, config: h.config, generation: h.generation, token: h.pending.Token}, nil
	}
	return &mcpOAuthTokenSource{ctx: ctx, paths: h.paths, name: h.name, config: h.config, path: h.path, generation: h.generation, allowRefresh: true}, nil
}

type mcpOAuthPendingTokenSource struct {
	ctx        context.Context
	paths      ConfigPaths
	name       string
	config     mcpServerConfig
	generation string
	token      oauth2.Token
}

func (s *mcpOAuthPendingTokenSource) Token() (*oauth2.Token, error) {
	if _, err := verifyConfigHome(s.paths); err != nil {
		return nil, err
	}
	lock, err := acquireMCPOAuthLock(s.ctx, mcpOAuthLockPath(s.paths, s.name))
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := checkMCPOAuthBaseline(s.paths, s.name, s.generation, s.config); err != nil {
		return nil, err
	}
	if !s.token.Valid() {
		return nil, fmt.Errorf("OAuth login token expired; run meldra mcp login %s", s.name)
	}
	return &s.token, nil
}

func (h *mcpOAuthHandler) commit(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending == nil {
		return nil // A public endpoint did not require new credentials.
	}
	source, err := saveMCPToken(ctx, h.paths, h.name, h.generation, h.config, h.pending.Config, &h.pending.Token)
	if err != nil {
		return err
	}
	h.generation = source.generation
	h.pending = nil
	return nil
}

type mcpOAuthTokenSource struct {
	ctx          context.Context
	paths        ConfigPaths
	name         string
	config       mcpServerConfig
	path         string
	generation   string
	allowRefresh bool
}

func readMCPOAuthFile(path string) ([]byte, bool, error) {
	if info, err := os.Lstat(path); err == nil && info.Size() > maxMCPOAuthFile {
		return nil, false, fmt.Errorf("OAuth cache exceeds 1 MiB")
	}
	return readPrivateFile(path)
}

func checkMCPOAuthGeneration(paths ConfigPaths, name, generation string, config mcpServerConfig) error {
	if generation == "" {
		return fmt.Errorf("OAuth session changed or was logged out; run meldra mcp login %s", name)
	}
	return checkMCPOAuthBaseline(paths, name, generation, config)
}

// An empty baseline permits the first login only while no generation exists.
// Logout or another successful login invalidates every in-flight attempt.
func checkMCPOAuthBaseline(paths ConfigPaths, name, generation string, config mcpServerConfig) error {
	data, found, err := readMCPOAuthFile(mcpOAuthPath(paths, name) + ".generation")
	if err != nil {
		return err
	}
	if found && (generation == "" || string(data) != generation) || !found && generation != "" {
		return fmt.Errorf("OAuth session changed or was logged out; run meldra mcp login %s", name)
	}
	servers, err := loadMCPConfig(paths)
	if err != nil {
		return err
	}
	current, found := servers[name]
	if !found || current.Disabled || current.OAuth == nil || mcpOAuthBinding(current) != mcpOAuthBinding(config) {
		return fmt.Errorf("OAuth configuration changed; restart after meldra mcp login %s", name)
	}
	return nil
}

func (s *mcpOAuthTokenSource) Token() (*oauth2.Token, error) {
	if _, err := verifyConfigHome(s.paths); err != nil {
		return nil, err
	}
	lock, err := acquireMCPOAuthLock(s.ctx, mcpOAuthLockPath(s.paths, s.name))
	if err != nil {
		return nil, fmt.Errorf("OAuth cache lock: %w", err)
	}
	defer lock.Close()
	if err := checkMCPOAuthGeneration(s.paths, s.name, s.generation, s.config); err != nil {
		return nil, err
	}
	data, found, err := readMCPOAuthFile(s.path)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("OAuth session was removed; run meldra mcp login %s", s.name)
	}
	var saved mcpOAuthSession
	if json.Unmarshal(data, &saved) != nil || saved.Generation != s.generation || saved.Binding != mcpOAuthBinding(s.config) {
		return nil, fmt.Errorf("OAuth session changed; run meldra mcp login %s", s.name)
	}
	if !s.allowRefresh && !saved.Token.Valid() {
		return nil, fmt.Errorf("OAuth notification requires a valid cached token")
	}
	refreshConfig := saved.Config
	scrubbedSecret := false
	if key := s.config.OAuth.ClientSecretEnv; key != "" {
		refreshConfig.ClientSecret = os.Getenv(key)
		if refreshConfig.ClientSecret == "" {
			return nil, fmt.Errorf("OAuth client secret environment variable is missing")
		}
		scrubbedSecret = saved.Config.ClientSecret != ""
		saved.Config.ClientSecret = ""
	}
	refreshContext := context.WithValue(s.ctx, oauth2.HTTPClient, mcpOAuthHTTPClient(s.config))
	token, err := refreshConfig.TokenSource(refreshContext, &saved.Token).Token()
	if err != nil {
		if s.ctx.Err() != nil {
			return nil, s.ctx.Err()
		}
		return nil, fmt.Errorf("OAuth token refresh failed; run meldra mcp login again")
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if scrubbedSecret || token.AccessToken != saved.Token.AccessToken || token.RefreshToken != saved.Token.RefreshToken || !token.Expiry.Equal(saved.Token.Expiry) {
		saved.Token = *token
		data, err = marshalMCPOAuthSession(saved)
		if err != nil {
			return nil, err
		}
		if err := atomicWriteFile(s.path, data, 0o600); err != nil {
			return nil, fmt.Errorf("save refreshed OAuth session: %w", err)
		}
		if err := syncDirectory(s.paths.Home); err != nil {
			return nil, err
		}
	}
	return token, nil
}

func saveMCPToken(ctx context.Context, paths ConfigPaths, name, generation string, config mcpServerConfig, cfg oauth2.Config, token *oauth2.Token) (*mcpOAuthTokenSource, error) {
	if _, err := verifyConfigHome(paths); err != nil {
		return nil, err
	}
	lock, err := acquireMCPOAuthLock(ctx, mcpOAuthLockPath(paths, name))
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := checkMCPOAuthBaseline(paths, name, generation, config); err != nil {
		return nil, err
	}
	path := mcpOAuthPath(paths, name)
	previous, found, err := readMCPOAuthFile(path)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config.OAuth.ClientSecretEnv != "" {
		cfg.ClientSecret = ""
	}
	generation = rand.Text()
	session := mcpOAuthSession{Generation: generation, Binding: mcpOAuthBinding(config), Config: cfg, Token: *token}
	encoded, err := marshalMCPOAuthSession(session)
	if err != nil {
		return nil, err
	}
	if err := atomicWriteFile(path, encoded, 0o600); err != nil {
		return nil, err
	}
	if err := atomicWriteFile(path+".generation", []byte(generation), 0o600); err != nil {
		var restoreErr error
		if found {
			restoreErr = atomicWriteFile(path, previous, 0o600)
		} else {
			restoreErr = os.Remove(path)
		}
		return nil, errors.Join(err, restoreErr, syncDirectory(paths.Home))
	}
	if err := syncDirectory(paths.Home); err != nil {
		return nil, err
	}
	return &mcpOAuthTokenSource{ctx: ctx, paths: paths, name: name, config: config, path: mcpOAuthPath(paths, name), generation: generation, allowRefresh: true}, nil
}

func newMCPOAuth(ctx context.Context, paths ConfigPaths, name string, config mcpServerConfig, output io.Writer, login bool) (handler auth.OAuthHandler, cleanup func(), err error) {
	cleanup = func() {}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	if config.OAuth == nil {
		return nil, cleanup, nil
	}
	if err := validateMCPOAuthConfig(config); err != nil {
		return nil, cleanup, err
	}
	var clientSecret string
	if key := config.OAuth.ClientSecretEnv; key != "" {
		clientSecret = os.Getenv(key)
		if clientSecret == "" {
			return nil, cleanup, fmt.Errorf("OAuth client secret environment variable is missing")
		}
	}
	if _, err := verifyConfigHome(paths); err != nil {
		return nil, cleanup, err
	}
	redirect := "http://127.0.0.1:1/callback"
	var listener net.Listener
	if login {
		listener, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", config.OAuth.CallbackPort))
		if err != nil {
			return nil, cleanup, err
		}
		cleanup = func() { _ = listener.Close() }
		redirect = "http://" + listener.Addr().String() + "/callback"
	}
	path := mcpOAuthPath(paths, name)
	lock, err := acquireMCPOAuthLock(ctx, mcpOAuthLockPath(paths, name))
	if err != nil {
		return nil, cleanup, err
	}
	data, found, err := readMCPOAuthFile(path)
	if err != nil {
		lock.Close()
		return nil, cleanup, err
	}
	var generation string
	if login {
		var data []byte
		data, _, err = readMCPOAuthFile(path + ".generation")
		if err == nil {
			generation = string(data)
		}
	} else {
		var saved mcpOAuthSession
		if !found {
			err = fmt.Errorf("OAuth login required; run meldra mcp login %s", name)
		} else if json.Unmarshal(data, &saved) != nil || saved.Binding != mcpOAuthBinding(config) {
			err = fmt.Errorf("OAuth configuration changed; run meldra mcp login %s", name)
		} else {
			generation = saved.Generation
			err = checkMCPOAuthGeneration(paths, name, generation, config)
		}
	}
	lock.Close()
	if err != nil {
		return nil, cleanup, err
	}
	h := &mcpOAuthHandler{paths: paths, name: name, config: config, path: path, generation: generation, login: login}
	client := mcpOAuthHTTPClient(config)
	loginAttempted := false
	options := &auth.AuthorizationCodeHandlerConfig{RedirectURL: redirect, Client: client, RequestRefreshToken: true,
		AuthorizationCodeFetcher: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			if !login || loginAttempted {
				return nil, fmt.Errorf("OAuth login required; run meldra mcp login %s", name)
			}
			loginAttempted = true
			authURL, err := url.Parse(args.URL)
			if err != nil {
				return nil, fmt.Errorf("invalid OAuth authorization URL")
			}
			clean := *authURL
			clean.RawQuery = ""
			if err := validateProviderBaseURL(clean.String(), strings.HasPrefix(config.URL, "http://")); err != nil {
				return nil, fmt.Errorf("unsafe OAuth authorization URL")
			}
			state := authURL.Query().Get("state")
			if state == "" {
				return nil, fmt.Errorf("missing OAuth state")
			}
			results := make(chan *auth.AuthorizationResult, 1)
			failures := make(chan error, 1)
			mux := http.NewServeMux()
			mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(state)) != 1 {
					http.Error(w, "Invalid OAuth state", http.StatusBadRequest)
					return
				}
				if r.URL.Query().Get("error") != "" {
					select {
					case failures <- fmt.Errorf("OAuth authorization declined"):
					default:
					}
					http.Error(w, "Authorization declined", http.StatusBadRequest)
					return
				}
				code := r.URL.Query().Get("code")
				if code == "" {
					http.Error(w, "Missing code", http.StatusBadRequest)
					return
				}
				fmt.Fprint(w, "Authorization received. Return to Meldra.")
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				select {
				case results <- &auth.AuthorizationResult{Code: code, State: state, Iss: r.URL.Query().Get("iss")}:
				default:
				}
			})
			server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
			defer func() {
				// Finish the callback response before closing active connections.
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := server.Shutdown(shutdownCtx); err != nil {
					_ = server.Close()
				}
			}()
			go func() {
				if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
					select {
					case failures <- err:
					default:
					}
				}
			}()
			fmt.Fprintf(output, "MCP OAuth login for %s. Open this URL in your browser:\n%s\n", name, sanitizeTerminalText(args.URL))
			select {
			case result := <-results:
				return result, nil
			case err := <-failures:
				return nil, err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		NewTokenSource: func(_ context.Context, cfg *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			h.mu.Lock()
			h.pending = &mcpOAuthSession{Config: *cfg, Token: *token}
			h.mu.Unlock()
			return oauth2.StaticTokenSource(token), nil
		}}
	o := config.OAuth
	if len(o.Scopes) > 0 {
		options.ScopeFilter = func([]string) []string { return append([]string(nil), o.Scopes...) }
	}
	if o.ClientID != "" {
		options.PreregisteredClient = &oauthex.ClientCredentials{ClientID: o.ClientID, Issuer: o.Issuer}
		if o.ClientSecretEnv != "" {
			options.PreregisteredClient.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: clientSecret}
		}
	} else if o.ClientIDMetadataURL != "" {
		options.ClientIDMetadataDocumentConfig = &auth.ClientIDMetadataDocumentConfig{URL: o.ClientIDMetadataURL}
	} else {
		options.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{ClientName: "Meldra", RedirectURIs: []string{redirect}, GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: "none"}}
	}
	base, err := auth.NewAuthorizationCodeHandler(options)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	h.base = base
	return h, cleanup, nil
}

func runMCPLogin(ctx context.Context, paths ConfigPaths, name string, output io.Writer) error {
	servers, err := loadMCPConfig(paths)
	if err != nil {
		return err
	}
	config, ok := servers[name]
	if !ok || config.OAuth == nil {
		return fmt.Errorf("MCP server %s has no OAuth configuration", name)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	handler, cleanup, err := newMCPOAuth(ctx, paths, name, config, output, true)
	if err != nil {
		return err
	}
	defer cleanup()
	client := mcp.NewClient(&mcp.Implementation{Name: "meldra", Version: version}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: config.URL, OAuthHandler: mcpOAuthSDKHandler{handler}, HTTPClient: mcpOAuthMCPClient(config, handler), DisableStandaloneSSE: true, MaxRetries: -1, MaxEventSize: maxMCPMessage}, nil)
	if err != nil {
		return fmt.Errorf("MCP OAuth login or connection failed: %s", mcpErrorKind(err))
	}
	defer session.Close()
	if err := handler.(*mcpOAuthHandler).commit(ctx); err != nil {
		return fmt.Errorf("save MCP OAuth login: %w", err)
	}
	fmt.Fprintf(output, "MCP server %s connected; OAuth session saved when authorization was required.\n", name)
	return nil
}
func runMCPLogout(ctx context.Context, paths ConfigPaths, name string) error {
	if name == "" || len(name) > 64 || !mcpNamePart.MatchString(name) {
		return fmt.Errorf("invalid MCP server name")
	}
	if exists, err := verifyConfigHome(paths); err != nil || !exists {
		return err
	}
	lock, err := acquireMCPOAuthLock(ctx, mcpOAuthLockPath(paths, name))
	if err != nil {
		return err
	}
	defer lock.Close()
	path := mcpOAuthPath(paths, name)
	if _, _, err := readMCPOAuthFile(path); err != nil {
		return err
	}
	if _, _, err := readMCPOAuthFile(path + ".generation"); err != nil {
		return err
	}
	if err := atomicWriteFile(path+".generation", []byte(rand.Text()), 0o600); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(paths.Home)
}

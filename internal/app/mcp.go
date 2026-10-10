package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"meldra/internal/tool"
)

const maxMCPMessage = 16 << 20

var mcpNamePart = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
var mcpEnvName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

type mcpServerConfig struct {
	OAuth          *mcpOAuthConfig   `json:"oauth,omitempty"`
	Sampling       bool              `json:"sampling,omitzero"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	EnvVars        []string          `json:"env_vars,omitempty"`
	URL            string            `json:"url,omitempty"`
	BearerTokenEnv string            `json:"bearer_token_env,omitempty"`
	Disabled       bool              `json:"disabled,omitzero"`
	StartupTimeout *int              `json:"startup_timeout_sec,omitempty"`
	ToolTimeout    *int              `json:"tool_timeout_sec,omitempty"`
}

type mcpConnections struct {
	sessions []*mcp.ClientSession
	tools    []ToolDefinition
	warnings []string
}

func (c *mcpConnections) close() {
	for _, session := range c.sessions {
		_ = session.Close()
	}
}

func loadMCPConfig(paths ConfigPaths) (map[string]mcpServerConfig, error) {
	if _, err := verifyConfigHome(paths); err != nil {
		return nil, err
	}
	path := filepath.Join(paths.Home, "mcp.json")
	if info, err := os.Lstat(path); err == nil && info.Size() > 1<<20 {
		return nil, fmt.Errorf("MCP configuration exceeds 1 MiB")
	}
	data, found, err := readPrivateFile(path)
	if err != nil || !found {
		return nil, err
	}
	var config struct {
		Servers map[string]mcpServerConfig `json:"mcpServers"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("invalid mcp.json configuration")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("mcp.json must contain one JSON object")
	}
	if config.Servers == nil || len(config.Servers) > 16 {
		return nil, fmt.Errorf("mcp.json requires mcpServers with at most 16 servers")
	}
	for name, server := range config.Servers {
		if len(name) > 64 || !mcpNamePart.MatchString(name) {
			return nil, fmt.Errorf("invalid MCP server name")
		}
		if (server.Command == "") == (server.URL == "") {
			return nil, fmt.Errorf("MCP server %s requires exactly one of command or url", name)
		}
		if server.URL != "" {
			if err := validateProviderBaseURL(server.URL, true); err != nil {
				return nil, fmt.Errorf("MCP server %s: url requires HTTPS or loopback HTTP, without credentials, query or fragment", name)
			}
			if len(server.Args)+len(server.Env)+len(server.EnvVars) != 0 {
				return nil, fmt.Errorf("MCP server %s: args/env/env_vars require command", name)
			}
		} else if server.BearerTokenEnv != "" {
			return nil, fmt.Errorf("MCP server %s: bearer_token_env requires url", name)
		}
		if err := validateMCPOAuthConfig(server); err != nil {
			return nil, fmt.Errorf("MCP server %s: %w", name, err)
		}
		for _, seconds := range []*int{server.StartupTimeout, server.ToolTimeout} {
			if seconds != nil && (*seconds < 1 || *seconds > 300) {
				return nil, fmt.Errorf("MCP server %s: timeouts must be between 1 and 300 seconds", name)
			}
		}
		keys := append(slices.Collect(maps.Keys(server.Env)), server.EnvVars...)
		if server.BearerTokenEnv != "" {
			keys = append(keys, server.BearerTokenEnv)
		}
		for _, key := range keys {
			if !mcpEnvName.MatchString(key) {
				return nil, fmt.Errorf("MCP server %s: invalid environment variable name", name)
			}
		}
	}
	return config.Servers, nil
}

func mcpTimeout(value *int, fallback int) time.Duration {
	if value != nil {
		fallback = *value
	}
	return time.Duration(fallback) * time.Second
}

// Count the encoded provider definition, including JSON escaping and metadata.
func mcpDefinitionSize(definition ToolDefinition) (int, error) {
	data, err := json.Marshal(struct {
		Type        string         `json:"type"`
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
		Strict      bool           `json:"strict"`
	}{"function", definition.Name, definition.Description, definition.Parameters, !definition.NonStrict})
	return len(data), err
}

func connectMCP(ctx context.Context, paths ConfigPaths, workspace *Workspace, selected string, localToolCounts ...int) (*mcpConnections, error) {
	limit := 112
	if len(localToolCounts) > 0 {
		limit = 128 - localToolCounts[0]
	}
	if limit < 0 {
		return nil, fmt.Errorf("local tool catalog exceeds 128 tools")
	}
	servers, err := loadMCPConfig(paths)
	if err != nil {
		return nil, err
	}
	connections := &mcpConnections{}
	catalogBytes := 2 // Array brackets; each entry also reserves its separator.
	names := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		if selected != "" && name != selected {
			continue
		}
		server := servers[name]
		if server.Disabled {
			continue
		}
		if err := ctx.Err(); err != nil {
			connections.close()
			return nil, err
		}
		session, definitions, err := connectMCPServer(ctx, paths, name, server, workspace)
		candidateBytes := catalogBytes
		// Leave room for the built-in tools in providers with a 128-tool limit.
		if err == nil && len(connections.tools)+len(definitions) > limit {
			err = fmt.Errorf("combined MCP catalog exceeds %d tools", limit)
		}
		if err == nil {
			for _, definition := range definitions {
				size, marshalErr := mcpDefinitionSize(definition)
				candidateBytes += size + 1
				if marshalErr != nil || candidateBytes > 1<<20 {
					err = fmt.Errorf("combined MCP catalog exceeds 1 MiB or has invalid schema")
					break
				}
				if names[definition.Name] {
					err = fmt.Errorf("duplicate advertised tool name")
					break
				}
			}
		}
		if err != nil {
			if session != nil {
				_ = session.Close()
			}
			connections.warnings = append(connections.warnings, fmt.Sprintf("Warning: MCP server %s unavailable: %s", name, sanitizeTerminalText(err.Error())))
			continue
		}
		for _, definition := range definitions {
			names[definition.Name] = true
		}
		catalogBytes = candidateBytes
		connections.sessions = append(connections.sessions, session)
		connections.tools = append(connections.tools, workspace.guardedDefinitions(definitions)...)
	}
	return connections, nil
}

func connectMCPServer(ctx context.Context, paths ConfigPaths, name string, config mcpServerConfig, workspace *Workspace) (*mcp.ClientSession, []ToolDefinition, error) {
	startupCtx, cancel := context.WithTimeout(ctx, mcpTimeout(config.StartupTimeout, 10))
	defer cancel()
	var transport mcp.Transport
	var oauthReady atomic.Bool
	if config.Command != "" {
		transport = mcpCommandTransport{command: newMCPCommand(config, workspace)}
	} else {
		token := ""
		if config.BearerTokenEnv != "" {
			token = os.Getenv(config.BearerTokenEnv)
			if token == "" || strings.ContainsAny(token, "\r\n") {
				return nil, nil, fmt.Errorf("bearer token environment variable is missing or invalid")
			}
		}
		transport = &mcp.StreamableClientTransport{
			Endpoint: config.URL, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: maxMCPMessage,
			HTTPClient: &http.Client{Transport: mcpHTTPTransport{token: token},
				CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("MCP redirects are disabled") }},
		}
		if config.OAuth != nil {
			handler, cleanup, err := newMCPOAuth(startupCtx, paths, name, config, workspace.output, false)
			if err != nil {
				return nil, nil, err
			}
			defer cleanup()
			httpTransport := transport.(*mcp.StreamableClientTransport)
			httpTransport.OAuthHandler = mcpOAuthSDKHandler{handler}
			httpTransport.HTTPClient = mcpOAuthMCPClient(config, handler)
			bearer := httpTransport.HTTPClient.Transport.(mcpOAuthBearerTransport)
			bearer.refreshAllowed = func() bool { return oauthReady.Load() || startupCtx.Err() == nil }
			httpTransport.HTTPClient.Transport = bearer
		}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "meldra", Version: version}, mcpClientOptions(workspace, name, config))
	session, err := client.Connect(startupCtx, transport, nil)
	if err != nil && config.Command != "" && startupCtx.Err() == nil && errors.Is(err, mcp.ErrConnectionClosed) {
		// Some deployed MCP servers only implement initialize and exit when they
		// receive the modern discovery probe. Restart the stdio process before the
		// legacy handshake; a consumed process cannot be safely reused.
		legacy := mcpCommandTransport{command: newMCPCommand(config, workspace)}
		session, err = client.Connect(startupCtx, legacy, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	}
	if err != nil {
		return nil, nil, fmt.Errorf("connection or initialization failed (%s)", mcpErrorKind(err))
	}
	fail := func(err error) (*mcp.ClientSession, []ToolDefinition, error) {
		_ = session.Close()
		return nil, nil, err
	}
	var definitions []ToolDefinition
	seen := map[string]bool{}
	schemaBytes := 2
	if caps := session.InitializeResult().Capabilities; caps != nil && caps.Tools != nil {
		for remote, err := range session.Tools(startupCtx, nil) {
			if err != nil {
				return fail(fmt.Errorf("tool discovery failed (%s)", mcpErrorKind(err)))
			}
			if remote == nil || remote.Name == "" || len(definitions) >= 128 {
				return fail(fmt.Errorf("invalid or excessive tool catalog (maximum 128)"))
			}
			nameOnWire := mcpToolName(name, remote.Name)
			if seen[nameOnWire] {
				return fail(fmt.Errorf("duplicate tool name"))
			}
			seen[nameOnWire] = true
			data, err := json.Marshal(remote.InputSchema)
			if err != nil {
				return fail(fmt.Errorf("invalid tool schema"))
			}
			var parameters map[string]any
			if json.Unmarshal(data, &parameters) != nil || parameters["type"] != "object" {
				return fail(fmt.Errorf("tool schema must be an object"))
			}
			var schema jsonschema.Schema
			if json.Unmarshal(data, &schema) != nil {
				return fail(fmt.Errorf("unsupported tool schema"))
			}
			resolved, err := schema.Resolve(nil) // Remote $ref fetching is intentionally disabled.
			if err != nil {
				return fail(fmt.Errorf("tool schema has invalid or external references"))
			}
			definition := ToolDefinition{Name: nameOnWire, Description: "External MCP tool from " + name + "/" + remote.Name + ". " + remote.Description,
				Parameters: parameters, NonStrict: true, Function: workspace.bindTool(func(input json.RawMessage) (string, error) {
					return workspace.callMCP(session, name, remote.Name, config.ToolTimeout, resolved, input)
				})}
			size, err := mcpDefinitionSize(definition)
			schemaBytes += size + 1
			if err != nil || schemaBytes > 1<<20 {
				return fail(fmt.Errorf("tool catalog exceeds 1 MiB or has invalid schema"))
			}
			definitions = append(definitions, definition)
		}
	}
	definitions = append(definitions, mcpCatalogTools(session, name, config, workspace)...)
	oauthReady.Store(true)
	return session, definitions, nil
}

func newMCPCommand(config mcpServerConfig, workspace *Workspace) *exec.Cmd {
	// The user-owned config grants startup trust. No shell or project config is loaded.
	command := exec.Command(config.Command, config.Args...)
	command.Dir = workspace.root
	command.WaitDelay = time.Second
	if configuredPath, ok := config.Env["PATH"]; ok && filepath.Base(config.Command) == config.Command && configuredPath != os.Getenv("PATH") {
		command.Err = fmt.Errorf("MCP commands with a configured PATH must use an absolute command path")
	}
	command.Env = []string{}
	for _, key := range append([]string{"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "TEMP", "TMP", "SystemRoot", "SYSTEMROOT"}, config.EnvVars...) {
		if value, ok := os.LookupEnv(key); ok {
			command.Env = append(command.Env, key+"="+value)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(config.Env)) {
		command.Env = append(command.Env, key+"="+config.Env[key])
	}
	return command
}

func mcpToolName(server, name string) string {
	combined := "mcp__" + server + "__" + name
	if len(combined) <= 64 && mcpNamePart.MatchString(name) {
		return combined
	}
	// Preserve readable names when possible; a digest avoids lossy sanitization collisions.
	return "mcp__" + server[:min(len(server), 20)] + "__" + digest([]byte(combined))[:32]
}

func (w *Workspace) callMCP(session *mcp.ClientSession, server, name string, timeout *int, schema *jsonschema.Resolved, input json.RawMessage) (string, error) {
	arguments, decodeErr := decodeMCPArguments(input)
	if decodeErr != nil {
		return "", fmt.Errorf("MCP arguments must be a JSON object within 1 MiB")
	}
	if err := schema.Validate(arguments); err != nil {
		return "", fmt.Errorf("MCP arguments do not match the tool schema: %w", err)
	}
	detail := fmt.Sprintf("Server: %s\nTool: %s\nArguments: %s\nExternal tools use server privileges and are not confined to this workspace.", server, name, input)
	if !w.requestApproval(ApprovalRequest{Kind: ApprovalCommand, Title: "Call MCP tool " + server + "/" + name, Detail: detail, Prompt: detail + "\nAllow MCP call? [y/N] "}) {
		return "Declined; MCP tool was not called.", nil
	}
	if err := w.ctx.Err(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(w.ctx, mcpTimeout(timeout, 60))
	defer cancel()
	tool.Observe(w.ctx, func(o *tool.Observation) { o.Started = true })
	defer w.beginMCPInteraction(ctx, server)()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(input)})
	if err != nil {
		return "", fmt.Errorf("MCP %s/%s: %s after dispatch; outcome unknown", server, name, mcpErrorKind(err))
	}
	if result == nil {
		return "", fmt.Errorf("MCP returned no result; outcome unknown")
	}
	if len(result.InputRequests) > 0 {
		return "", fmt.Errorf("MCP input requests are unsupported; inspect external outcome")
	}
	data, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	truncated := len(data) > maxMCPMessage
	if truncated {
		data = []byte(truncateUTF8(string(data), maxMCPMessage, "\n[artifact truncated]"))
	}
	output := capText(string(data))
	tool.Observe(w.ctx, func(o *tool.Observation) {
		o.Result.Status = tool.Succeeded
		if result.IsError {
			o.Result.Status = tool.Failed
		}
		o.Result.Truncated = truncated || len(data) > maxToolOutput
		o.Result.Attachments = []tool.OutputArtifact{{Name: "mcp-result.json", Content: data, Truncated: truncated}}
	})
	if result.IsError {
		return output, fmt.Errorf("MCP tool failed: %s", output)
	}
	return output, nil
}

// Transport diagnostics may contain credentials, URLs, or server-controlled text.
func mcpErrorKind(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, errMCPAuth) {
		return errMCPAuth.Error()
	}
	if errors.Is(err, mcp.ErrConnectionClosed) {
		return "connection closed"
	}
	return "protocol or transport error"
}

var errMCPAuth = errors.New("authentication rejected; configure bearer_token_env or OAuth login")

type mcpHTTPTransport struct{ token string }

// Close must stop descendants before waiting for protocol readers: a child can
// inherit stdout and keep the SDK reader alive after its launcher exits.
type mcpCommandTransport struct{ command *exec.Cmd }

func (t mcpCommandTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	prepareMCPProcess(t.command)
	transport := &mcp.CommandTransport{Command: t.command, TerminateDuration: time.Second}
	connection, err := transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &mcpCommandConnection{Connection: connection, close: sync.OnceValue(func() error {
		stopMCPProcess(t.command)
		return connection.Close()
	})}, nil
}

type mcpCommandConnection struct {
	mcp.Connection
	close func() error
}

func (c *mcpCommandConnection) Close() error { return c.close() }

func (t mcpHTTPTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	if t.token != "" {
		request.Header.Set("Authorization", "Bearer "+t.token)
	}
	response, err := http.DefaultTransport.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		_ = response.Body.Close()
		return nil, errMCPAuth
	}
	if response.ContentLength > maxMCPMessage {
		_ = response.Body.Close()
		return nil, fmt.Errorf("MCP response exceeds 16 MiB")
	}
	response.Body = http.MaxBytesReader(nil, response.Body, maxMCPMessage)
	return response, nil
}

// Reject numeric tokens whose value would change during schema validation.
func decodeMCPArguments(input json.RawMessage) (map[string]any, error) {
	if len(input) > maxApprovalPreviewBytes {
		return nil, fmt.Errorf("arguments exceed 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var arguments map[string]any
	if err := decoder.Decode(&arguments); err != nil {
		return nil, err
	}
	if arguments == nil {
		return nil, fmt.Errorf("arguments must be an object")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("arguments contain trailing data")
	}
	var convert func(any) (any, error)
	convert = func(value any) (any, error) {
		switch v := value.(type) {
		case json.Number:
			f, err := v.Float64()
			original, valid := new(big.Rat).SetString(string(v))
			encoded, exact := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
			if err != nil || !valid || !exact || original.Cmp(encoded) != 0 {
				return nil, fmt.Errorf("numeric argument loses precision")
			}
			return f, nil
		case map[string]any:
			for key, item := range v {
				next, err := convert(item)
				if err != nil {
					return nil, err
				}
				v[key] = next
			}
		case []any:
			for i, item := range v {
				next, err := convert(item)
				if err != nil {
					return nil, err
				}
				v[i] = next
			}
		}
		return value, nil
	}
	_, err := convert(arguments)
	return arguments, err
}

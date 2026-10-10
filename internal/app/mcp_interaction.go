package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/big"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/muesli/cancelreader"
	"meldra/internal/provider"
)

func mcpClientOptions(w *Workspace, name string, config mcpServerConfig) *mcp.ClientOptions {
	options := &mcp.ClientOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Capabilities: &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}, URL: &mcp.URLElicitationCapabilities{}}}}
	options.ElicitationHandler = func(ctx context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		active := w.mcpActiveServer.Load()
		if active == nil || active.server != name {
			return nil, fmt.Errorf("unsolicited MCP interaction rejected")
		}
		ctx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(active.ctx, cancel)
		defer stop()
		defer cancel()
		w.mcpInteractionMu.Lock()
		defer w.mcpInteractionMu.Unlock()
		if w.mcpActiveServer.Load() != active || active.ctx.Err() != nil {
			return nil, fmt.Errorf("unsolicited MCP interaction rejected")
		}
		return w.elicitMCP(ctx, name, r.Params)
	}
	if config.Sampling {
		options.CreateMessageHandler = func(ctx context.Context, r *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
			active := w.mcpActiveServer.Load()
			if active == nil || active.server != name {
				return nil, fmt.Errorf("unsolicited MCP sampling rejected")
			}
			ctx, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(active.ctx, cancel)
			defer stop()
			defer cancel()
			w.mcpInteractionMu.Lock()
			defer w.mcpInteractionMu.Unlock()
			if w.mcpActiveServer.Load() != active || active.ctx.Err() != nil {
				return nil, fmt.Errorf("unsolicited MCP sampling rejected")
			}
			return w.sampleMCP(ctx, name, r.Params)
		}
	}
	return options
}

func (w *Workspace) mcpHumanInput(ctx context.Context, prompt string) (string, bool) {
	if ctx.Err() != nil {
		return "", false
	}
	prompt = sanitizeTerminalText(prompt)
	if w.mcpInput != nil {
		return w.mcpInput(ctx, prompt)
	}
	if w.approve != nil {
		return "", false
	} // A UI must own its input; never compete for stdin.
	fmt.Fprintln(w.output, prompt)
	line, err := w.input.ReadString('\n')
	if ctx.Err() != nil || (err != nil && (err != io.EOF || line == "")) {
		return "", false
	}
	return strings.TrimSpace(line), true
}
func (w *Workspace) mcpExplicitApproval(ctx context.Context, title, detail string) bool {
	request := ApprovalRequest{Kind: ApprovalCommand, Title: sanitizeTerminalText(title), Detail: sanitizeTerminalText(detail), Prompt: sanitizeTerminalText(detail + "\nAllow? [y/N] ")}
	if ctx.Err() != nil {
		return false
	}
	if w.approve != nil {
		return w.approve(ctx, request)
	}
	answer, ok := w.mcpHumanInput(ctx, request.Title+"\n"+request.Prompt)
	return ok && (strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes"))
}
func (w *Workspace) elicitMCP(ctx context.Context, server string, p *mcp.ElicitParams) (*mcp.ElicitResult, error) {
	if p == nil || len(p.Message) > 64<<10 {
		return nil, fmt.Errorf("invalid MCP elicitation")
	}
	if p.Mode == "url" {
		u, err := url.Parse(p.URL)
		if err != nil {
			return nil, fmt.Errorf("invalid elicitation URL")
		}
		clean := *u
		clean.RawQuery = ""
		clean.Fragment = ""
		if err := validateProviderBaseURL(clean.String(), true); err != nil {
			return nil, fmt.Errorf("elicitation URL requires HTTPS or loopback HTTP")
		}
		detail := fmt.Sprintf("MCP server: %s\n%s\nURL: %s\nOpen this URL yourself in your browser. Approve only after completing the external interaction. Meldra will not open or inspect it.", server, p.Message, p.URL)
		action := "decline"
		if w.mcpExplicitApproval(ctx, "MCP URL interaction", detail) {
			action = "accept"
		}
		if ctx.Err() != nil {
			action = "cancel"
		}
		return &mcp.ElicitResult{Action: action}, nil
	}
	if p.Mode != "" && p.Mode != "form" {
		return nil, fmt.Errorf("unsupported elicitation mode")
	}
	data, err := json.Marshal(p.RequestedSchema)
	if err != nil || len(data) > 64<<10 {
		return nil, fmt.Errorf("invalid elicitation schema")
	}
	if err := validateMCPFormShape(data); err != nil {
		return nil, err
	}
	var schema jsonschema.Schema
	if json.Unmarshal(data, &schema) != nil || schema.Type != "object" || len(schema.Properties) > 32 {
		return nil, fmt.Errorf("elicitation requires an object with at most 32 fields")
	}
	for name, field := range schema.Properties {
		if field == nil || (field.Type != "string" && field.Type != "integer" && field.Type != "number" && field.Type != "boolean" && field.Type != "array") {
			return nil, fmt.Errorf("unsupported elicitation field")
		}
		if field.Type == "array" && (field.Items == nil || len(field.Items.Enum) == 0 && len(field.Items.AnyOf) == 0) {
			return nil, fmt.Errorf("elicitation arrays must contain enumerated strings")
		}
		lower := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(name + " " + field.Title + " " + field.Description + " " + field.Format))
		for _, secret := range []string{"password", "secret", "apikey", "token", "credential", "creditcard"} {
			if strings.Contains(lower, secret) {
				return nil, fmt.Errorf("sensitive elicitation requires URL mode")
			}
		}
	}
	// jsonschema-go recognizes json.Number for numeric bounds, but classifies
	// its Go string kind as a string for type checks. Check numeric types below
	// and let the resolver validate their remaining constraints without rounding.
	validationSchema := schema
	validationSchema.Properties = maps.Clone(schema.Properties)
	for name, field := range schema.Properties {
		if field.Type == "number" || field.Type == "integer" {
			copy := *field
			copy.Type = ""
			copy.MinLength, copy.MaxLength = nil, nil
			validationSchema.Properties[name] = &copy
		}
	}
	resolved, err := validationSchema.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("invalid or externally referenced elicitation schema")
	}
	prompt := fmt.Sprintf("MCP server %s requests information:\n%s\nSchema: %s\nEnter one JSON object, 'decline', or 'cancel':", server, p.Message, data)
	originalPrompt := prompt
	for range 3 {
		answer, ok := w.mcpHumanInput(ctx, prompt)
		if !ok || strings.EqualFold(answer, "cancel") {
			return &mcp.ElicitResult{Action: "cancel"}, nil
		}
		if answer == "" || strings.EqualFold(answer, "decline") {
			return &mcp.ElicitResult{Action: "decline"}, nil
		}
		var content map[string]any
		decoder := json.NewDecoder(strings.NewReader(answer))
		decoder.UseNumber()
		if len(answer) <= 64<<10 && decoder.Decode(&content) == nil && decoder.Decode(new(any)) == io.EOF && content != nil {
			declared := true
			for key, value := range content {
				field, ok := schema.Properties[key]
				if !ok {
					declared = false
					break
				}
				switch field.Type {
				case "string":
					_, declared = value.(string)
				case "boolean":
					_, declared = value.(bool)
				case "array":
					items, ok := value.([]any)
					declared = ok
					for _, item := range items {
						if _, ok := item.(string); !ok {
							declared = false
							break
						}
					}
				}
				if !declared {
					break
				}
				if field.Type == "number" || field.Type == "integer" {
					number, ok := value.(json.Number)
					rational, valid := new(big.Rat).SetString(string(number))
					if !ok || !valid || field.Type == "integer" && !rational.IsInt() {
						declared = false
						break
					}
					if field.Type == "integer" {
						// The MCP SDK validates results using Go's native integer
						// kinds. Convert without passing through float64 so values
						// above JavaScript's safe-integer range remain exact on wire.
						integer := rational.Num()
						switch {
						case integer.IsInt64():
							content[key] = integer.Int64()
						case integer.IsUint64():
							content[key] = integer.Uint64()
						default:
							declared = false
						}
					} else {
						float, err := number.Float64()
						if err != nil {
							declared = false
						} else {
							content[key] = float
						}
					}
				}
			}
			if declared && resolved.Validate(content) == nil {
				return &mcp.ElicitResult{Action: "accept", Content: content}, nil
			}
		}
		prompt = "Invalid form response: values must match the shown schema.\n" + originalPrompt
	}
	return nil, fmt.Errorf("elicitation response failed validation three times")
}

// Forms support the protocol's primitive fields and titled enums, not arbitrary JSON Schema composition.
func validateMCPFormShape(data []byte) error {
	var root map[string]any
	if json.Unmarshal(data, &root) != nil || root == nil {
		return fmt.Errorf("invalid flat elicitation schema")
	}
	for key, value := range root {
		switch key {
		case "type", "properties", "required", "title", "description", "$schema":
		case "additionalProperties":
			if value != false {
				return fmt.Errorf("flat elicitation cannot allow undeclared properties")
			}
		default:
			return fmt.Errorf("unsupported flat elicitation keyword %q", key)
		}
	}
	fields, ok := root["properties"].(map[string]any)
	if !ok {
		return fmt.Errorf("flat elicitation requires declared properties")
	}
	var checkField func(map[string]any, bool) error
	checkField = func(field map[string]any, item bool) error {
		for key, value := range field {
			switch key {
			case "type", "title", "description", "default", "enum", "enumNames", "minLength", "maxLength", "format", "minimum", "maximum", "minItems", "maxItems":
			case "items":
				schema, ok := value.(map[string]any)
				if !ok || item {
					return fmt.Errorf("flat elicitation disallows nested arrays")
				}
				if err := checkField(schema, true); err != nil {
					return err
				}
			case "oneOf", "anyOf":
				entries, ok := value.([]any)
				if !ok || len(entries) == 0 || key == "anyOf" && !item || key == "oneOf" && item {
					return fmt.Errorf("flat elicitation only supports titled enum choices")
				}
				for _, value := range entries {
					entry, ok := value.(map[string]any)
					if !ok || len(entry) != 2 {
						return fmt.Errorf("flat elicitation requires const/title enum entries")
					}
					constant, valueOK := entry["const"].(string)
					title, titleOK := entry["title"].(string)
					if !valueOK || !titleOK || constant == "" || title == "" {
						return fmt.Errorf("invalid flat elicitation enum")
					}
				}
			default:
				return fmt.Errorf("unsupported flat elicitation field keyword %q", key)
			}
		}
		return nil
	}
	for _, value := range fields {
		field, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid flat elicitation field")
		}
		if err := checkField(field, false); err != nil {
			return err
		}
	}
	return nil
}
func (w *Workspace) sampleMCP(ctx context.Context, server string, p *mcp.CreateMessageParams) (*mcp.CreateMessageResult, error) {
	if p == nil || p.MaxTokens < 1 || len(p.Messages) == 0 || len(p.Messages) > 64 {
		return nil, fmt.Errorf("invalid MCP sampling request")
	}
	data, err := json.Marshal(p)
	if err != nil || len(data) > 1<<20 {
		return nil, fmt.Errorf("MCP sampling request exceeds 1 MiB")
	}
	var items provider.Items
	for _, message := range p.Messages {
		if message == nil || (message.Role != "user" && message.Role != "assistant") {
			return nil, fmt.Errorf("invalid sampling role")
		}
		text, ok := message.Content.(*mcp.TextContent)
		if !ok {
			return nil, fmt.Errorf("Meldra sampling currently accepts text content only")
		}
		items = append(items, provider.MessageItems(string(message.Role), text.Text)...)
	}
	detail := fmt.Sprintf("Server: %s\nThis makes a separate paid model request and shares its result with that MCP server. No Meldra conversation or workspace context is included.\nRequest: %s", server, data)
	if !w.mcpExplicitApproval(ctx, "MCP sampling request", detail) {
		return nil, fmt.Errorf("MCP sampling declined")
	}
	paths, err := ResolveConfigPaths()
	if err != nil {
		return nil, err
	}
	settings, err := LoadSettings(paths)
	if err != nil {
		return nil, err
	}
	settings, err = effectiveSettings(settings)
	if err != nil {
		return nil, err
	}
	if settings.APIKey == "" {
		return nil, fmt.Errorf("sampling provider is not configured")
	}
	backend := provider.Connect(provider.Connection{APIKey: settings.APIKey, BaseURL: settings.BaseURL})
	inferenceCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	result, err := backend.Infer(inferenceCtx, provider.Request{Model: settings.Model, Instructions: p.SystemPrompt, Input: provider.ItemsInput(items), MaxOutputTokens: min(p.MaxTokens, 8192)}, provider.Options{CustomProvider: isCustomBaseURL(settings.BaseURL), MaxResponseBytes: 1 << 20}, provider.Observer{})
	cancel()
	if err != nil {
		return nil, fmt.Errorf("MCP sampling provider failed")
	}
	stop := "endTurn"
	if result.Response != nil && result.Response.Status == "incomplete" && result.Response.IncompleteReason == "max_output_tokens" {
		stop = "maxTokens"
	} else if err := provider.Validate(result.Response); err != nil {
		return nil, fmt.Errorf("MCP sampling returned an invalid response")
	}
	text := result.Response.OutputText()
	if !w.mcpExplicitApproval(ctx, "Share MCP sampling result", fmt.Sprintf("Server: %s\nModel: %s\nResult: %s", server, settings.Model, text)) {
		return nil, fmt.Errorf("MCP sampling result sharing declined")
	}
	return &mcp.CreateMessageResult{Role: "assistant", Model: settings.Model, Content: &mcp.TextContent{Text: text}, StopReason: stop}, nil
}

type mcpInteraction struct {
	server string
	ctx    context.Context
}

func (w *Workspace) beginMCPInteraction(ctx context.Context, server string) func() {
	ctx, cancel := context.WithCancel(ctx)
	w.mcpActiveServer.Store(&mcpInteraction{server: server, ctx: ctx})
	return func() {
		w.mcpActiveServer.Store(nil)
		cancel()
		// Join callbacks before the chat reads stdin or replaces workspace state.
		w.mcpInteractionMu.Lock()
		w.mcpInteractionMu.Unlock()
	}
}

type mcpCLIInput struct {
	*bufio.Reader
	interrupt cancelreader.CancelReader
	source    io.Reader
}

// cancelreader calls Fd during reads; cache it while SyscallConn protects against signal-time Close.
type mcpInputFile struct {
	*os.File
	descriptor uintptr
}

func (f mcpInputFile) Fd() uintptr { return f.descriptor }

func newMCPCLIInput(input io.Reader) (*mcpCLIInput, func(), error) {
	if existing, ok := input.(*mcpCLIInput); ok {
		return existing, func() {}, nil
	}
	if _, ok := input.(cancelreader.File); !ok {
		return &mcpCLIInput{Reader: bufferedInput(input)}, func() {}, nil
	}
	if file, ok := input.(*os.File); ok {
		raw, err := file.SyscallConn()
		if err != nil {
			return nil, nil, err
		}
		var descriptor uintptr
		if err := raw.Control(func(fd uintptr) { descriptor = fd }); err != nil {
			return nil, nil, err
		}
		info, err := file.Stat()
		if err != nil {
			return nil, nil, err
		}
		// epoll cannot watch regular files or /dev/null.
		if info.Mode().IsRegular() {
			return &mcpCLIInput{Reader: bufferedInput(input)}, func() {}, nil
		}
		if info.Mode()&os.ModeCharDevice != 0 && !term.IsTerminal(descriptor) {
			nullInfo, err := os.Stat(os.DevNull)
			if err != nil {
				return nil, nil, err
			}
			if os.SameFile(info, nullInfo) {
				return &mcpCLIInput{Reader: bufferedInput(input)}, func() {}, nil
			}
			return nil, nil, fmt.Errorf("unsupported nonterminal character device for stdin")
		}
		input = mcpInputFile{File: file, descriptor: descriptor}
	}
	reader, err := cancelreader.NewReader(input)
	if err != nil {
		return nil, nil, err
	}
	result := &mcpCLIInput{Reader: bufio.NewReader(reader), interrupt: reader, source: input}
	return result, func() {
		if result.interrupt != nil {
			_ = result.interrupt.Close()
		}
	}, nil
}

func (input *mcpCLIInput) humanInput(output io.Writer) func(context.Context, string) (string, bool) {
	return func(ctx context.Context, prompt string) (string, bool) {
		if ctx.Err() != nil {
			return "", false
		}
		if input.interrupt != nil {
			done := make(chan struct{})
			stop := context.AfterFunc(ctx, func() { input.interrupt.Cancel(); close(done) })
			defer func() {
				if !stop() {
					<-done
					// CancelReader cancellation is permanent; preserve read-ahead for the next interaction.
					pending, _ := input.Peek(input.Buffered())
					pending = bytes.Clone(pending)
					_ = input.interrupt.Close()
					reader, err := cancelreader.NewReader(input.source)
					if err == nil {
						input.interrupt = reader
						input.Reader.Reset(io.MultiReader(bytes.NewReader(pending), reader))
					} else {
						input.interrupt = nil
						input.Reader.Reset(strings.NewReader(""))
					}
				}
			}()
		}
		fmt.Fprintln(output, sanitizeTerminalText(prompt))
		line, err := input.ReadString('\n')
		return strings.TrimSpace(line), ctx.Err() == nil && (err == nil || err == io.EOF && line != "")
	}
}

package app

import (
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"meldra/internal/provider"
)

type inferenceFunc func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error)

func (f inferenceFunc) Infer(ctx context.Context, request provider.Request, options provider.Options, observer provider.Observer) (provider.Result, error) {
	return f(ctx, request, options, observer)
}

func TestHeadlessTurnsPreserveContinuationWithoutReadingInput(t *testing.T) {
	requests := 0
	agent := NewAgent(inferenceFunc(func(ctx context.Context, request provider.Request, options provider.Options, observer provider.Observer) (provider.Result, error) {
		requests++
		if requests == 1 && request.PreviousResponseID != "" {
			t.Fatal("unexpected first response ID")
		}
		if requests == 2 && request.PreviousResponseID != "response-1" {
			t.Fatalf("lost continuation: %q", request.PreviousResponseID)
		}
		if request.Model != defaultModel || request.Instructions == "" {
			t.Fatal("missing model or instructions")
		}
		return provider.Result{Response: &provider.Response{ID: "response-1", Status: "completed", Text: "done"}}, nil
	}), func() (string, bool) { t.Fatal("headless turn read interactive input"); return "", false }, nil)
	agent.output = io.Discard
	for _, input := range []string{"first", "second"} {
		if err := agent.RunTurn(t.Context(), input); err != nil {
			t.Fatal(err)
		}
	}
	if requests != 2 {
		t.Fatalf("requests = %d", requests)
	}
}

func TestHeadlessTurnRejectsConcurrentExecution(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	agent := NewAgent(inferenceFunc(func(ctx context.Context, _ provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return provider.Result{}, ctx.Err()
		}
		return provider.Result{Response: &provider.Response{Status: "completed", Text: "done"}}, nil
	}), nil, nil)
	agent.output = io.Discard
	done := make(chan error, 1)
	go func() { done <- agent.RunTurn(t.Context(), "first") }()
	<-entered
	if err := agent.RunTurn(t.Context(), "second"); !errors.Is(err, ErrAgentBusy) {
		t.Errorf("second turn = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestHeadlessTurnCancelsActiveTool(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	agent := NewAgent(inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		return provider.Result{Response: &provider.Response{ID: "call", Status: "completed", Output: []provider.OutputItem{{Type: "function_call", Name: "wait", CallID: "one", Arguments: "{}"}}}}, nil
	}), nil, []ToolDefinition{{Name: "wait", Function: func(got context.Context, _ json.RawMessage) (string, error) {
		if got.Done() == nil {
			t.Error("tool did not receive cancellable turn context")
		}
		close(started)
		<-got.Done()
		return "", got.Err()
	}}})
	agent.output = io.Discard
	done := make(chan error, 1)
	go func() { done <- agent.RunTurn(ctx, "wait") }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled turn = %v", err)
	}
}

func TestWorkspaceToolsUseInvocationContextWithoutLeakingIt(t *testing.T) {
	workspace, _ := testWorkspace(t, t.TempDir(), "", true)
	base := workspace.ctx
	ctx, cancel := context.WithCancel(t.Context())
	handler := workspace.bindTool(func(json.RawMessage) (string, error) {
		if workspace.ctx != ctx {
			t.Error("workspace did not receive invocation context")
		}
		cancel()
		return "", workspace.contextErr()
	})
	if _, err := handler(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("tool cancellation = %v", err)
	}
	if workspace.ctx != base {
		t.Fatal("invocation context leaked to subsequent tools")
	}
	if _, err := workspace.bindTool(func(json.RawMessage) (string, error) { return "ok", workspace.contextErr() })(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
}

// Keep SDK adaptation out of application logic as new features are added.
func TestApplicationDoesNotImportProviderSDK(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(path, "github.com/openai/") {
				t.Errorf("%s imports SDK %s; use provider boundary", name, path)
			}
		}
	}
}

type saveSessionFunc func(*Session) error

func (f saveSessionFunc) Save(session *Session) error { return f(session) }

func TestHeadlessTurnDoesNotInferAfterPersistenceFailure(t *testing.T) {
	sentinel := errors.New("storage unavailable")
	agent := NewAgent(inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		t.Fatal("inference ran without saving the user request")
		return provider.Result{}, nil
	}), nil, nil)
	agent.output = io.Discard
	agent.session = &Session{ID: "test"}
	agent.store = saveSessionFunc(func(session *Session) error {
		if len(session.Messages) != 1 || session.Messages[0].Content != "request" {
			t.Error("wrong session state")
		}
		return sentinel
	})
	if err := agent.RunTurn(t.Context(), "request"); !errors.Is(err, sentinel) {
		t.Fatalf("save error = %v", err)
	}
}

func TestInteractiveRunSkipsBlankLine(t *testing.T) {
	requests := 0
	agent := NewAgent(inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		requests++
		return provider.Result{Response: &provider.Response{Status: "completed", Text: "done"}}, nil
	}), userMessages("", "  \t", "reply"), nil)
	agent.output = io.Discard
	if err := agent.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("blank input triggered model request: %d", requests)
	}
}

func TestWorkspaceRejectsOverlappingRegisteredTools(t *testing.T) {
	workspace, _ := testWorkspace(t, t.TempDir(), "", true)
	entered := make(chan struct{})
	release := make(chan struct{})
	handler := workspace.bindTool(func(json.RawMessage) (string, error) {
		close(entered)
		<-release
		return "done", nil
	})
	done := make(chan error, 1)
	go func() { _, err := handler(t.Context(), nil); done <- err }()
	<-entered
	_, err := workspace.bindTool(func(json.RawMessage) (string, error) {
		t.Error("overlapping handler ran")
		return "", nil
	})(t.Context(), nil)
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Errorf("overlapping tool = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

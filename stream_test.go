package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

type scriptedResponseStream struct {
	events []responses.ResponseStreamEventUnion
	index  int
	err    error
	closed bool
}

func (s *scriptedResponseStream) Next() bool {
	if s.index >= len(s.events) {
		return false
	}
	s.index++
	return true
}

func (s *scriptedResponseStream) Current() responses.ResponseStreamEventUnion {
	return s.events[s.index-1]
}

func (s *scriptedResponseStream) Err() error {
	return s.err
}

func (s *scriptedResponseStream) Close() error {
	s.closed = true
	return nil
}

type stallingResponseStream struct {
	event     responses.ResponseStreamEventUnion
	sent      bool
	released  chan struct{}
	closeOnce sync.Once
}

func newStallingResponseStream(event responses.ResponseStreamEventUnion) *stallingResponseStream {
	return &stallingResponseStream{
		event:    event,
		released: make(chan struct{}),
	}
}

func (s *stallingResponseStream) Next() bool {
	if !s.sent {
		s.sent = true
		return true
	}
	<-s.released
	return false
}

func (s *stallingResponseStream) Current() responses.ResponseStreamEventUnion {
	return s.event
}

func (s *stallingResponseStream) Err() error { return nil }

func (s *stallingResponseStream) Close() error {
	s.closeOnce.Do(func() { close(s.released) })
	return nil
}

type delayedResponseStream struct {
	events    []responses.ResponseStreamEventUnion
	delays    []time.Duration
	index     int
	closed    chan struct{}
	closeOnce sync.Once
}

func (s *delayedResponseStream) Next() bool {
	if s.index >= len(s.events) {
		return false
	}
	if delay := s.delays[s.index]; delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-s.closed:
			return false
		}
	}
	select {
	case <-s.closed:
		return false
	default:
	}
	s.index++
	return true
}

func (s *delayedResponseStream) Current() responses.ResponseStreamEventUnion {
	return s.events[s.index-1]
}

func (s *delayedResponseStream) Err() error { return nil }

func (s *delayedResponseStream) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func streamedCompletedResponse(t *testing.T, text string) responses.Response {
	t.Helper()
	escapedText, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	return *responseFromJSON(t, `{
		"id":"resp_stream",
		"status":"completed",
		"output":[{
			"type":"message",
			"id":"msg_stream",
			"status":"completed",
			"role":"assistant",
			"content":[{"type":"output_text","text":`+string(escapedText)+`,"annotations":[]}]
		}]
	}`)
}

func TestAgentStreamsDeltasToTerminalWithoutRepeatingText(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", defaultBaseURL)
	stream := &scriptedResponseStream{events: []responses.ResponseStreamEventUnion{
		{Type: "response.created"},
		{Type: "response.output_text.delta", Delta: "Hello"},
		{Type: "response.output_text.delta", Delta: ", world!"},
		{Type: "response.completed", Response: streamedCompletedResponse(t, "Hello, world!")},
	}}
	var output bytes.Buffer
	agent := Agent{
		getUserMessage: userMessages("say hello"),
		output:         &output,
		createStream: func(context.Context, responses.ResponseNewParams) responseStream {
			return stream
		},
		createResponse: func(context.Context, responses.ResponseNewParams) (*responses.Response, error) {
			t.Fatal("non-streaming response path was used")
			return nil, nil
		},
	}

	if err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !stream.closed {
		t.Fatal("stream was not closed")
	}
	if got := output.String(); !strings.Contains(got, "\u001b[93mMeldra\u001b[0m: Hello, world!\n") {
		t.Fatalf("terminal stream output = %q", got)
	} else if strings.Count(got, "Hello, world!") != 1 {
		t.Fatalf("streamed text was repeated: %q", got)
	}
}

func TestAgentStreamsDeltasToUIEvents(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", defaultBaseURL)
	stream := &scriptedResponseStream{events: []responses.ResponseStreamEventUnion{
		{Type: "response.output_text.delta", Delta: "streamed "},
		{Type: "response.output_text.delta", Delta: "reply"},
		{Type: "response.completed", Response: streamedCompletedResponse(t, "streamed reply")},
	}}
	var events []UIEvent
	var output bytes.Buffer
	agent := Agent{
		getUserMessage: userMessages("reply"),
		output:         &output,
		events: UIEventSinkFunc(func(event UIEvent) {
			events = append(events, event)
		}),
		createStream: func(context.Context, responses.ResponseNewParams) responseStream {
			return stream
		},
	}

	if err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var deltas strings.Builder
	done := 0
	for _, event := range events {
		if event.Kind == UIEventAssistantDelta {
			deltas.WriteString(event.Text)
		}
		if event.Kind == UIEventAssistantDone {
			done++
		}
	}
	if got := deltas.String(); got != "streamed reply" {
		t.Fatalf("streamed deltas = %q", got)
	}
	if done != 1 {
		t.Fatalf("assistant completion events = %d, want 1", done)
	}
	if got := output.String(); got != "" {
		t.Fatalf("TUI stream unexpectedly wrote to terminal: %q", got)
	}
}

func TestAgentUsesOutputTextDoneWhenGatewayOmitsDeltasAndOutput(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "https://provider.example/v1")
	stream := &scriptedResponseStream{events: []responses.ResponseStreamEventUnion{
		{Type: "response.output_text.done", Text: "final reply"},
		{Type: "response.completed", Response: responses.Response{ID: "resp_done", Status: responses.ResponseStatusCompleted}},
	}}
	var events []UIEvent
	agent := Agent{
		getUserMessage: userMessages("reply"),
		events: UIEventSinkFunc(func(event UIEvent) {
			events = append(events, event)
		}),
		createStream: func(context.Context, responses.ResponseNewParams) responseStream {
			return stream
		},
	}

	if err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	done := 0
	for _, event := range events {
		if event.Kind == UIEventAssistantDelta {
			text.WriteString(event.Text)
		}
		if event.Kind == UIEventAssistantDone {
			done++
		}
	}
	if got := text.String(); got != "final reply" {
		t.Fatalf("streamed output_text.done = %q", got)
	}
	if done != 1 {
		t.Fatalf("assistant completion events = %d, want 1", done)
	}
}

func TestAgentReconstructsGatewayOutputItemsAfterToolCall(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "https://provider.example/v1")
	first := responseFromJSON(t, `{
		"id":"resp_call",
		"status":"completed",
		"output":[{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{}"}]
	}`)
	var finalItem responses.ResponseOutputItemUnion
	if err := json.Unmarshal([]byte(`{
		"type":"message",
		"id":"msg_final",
		"status":"completed",
		"role":"assistant",
		"content":[{"type":"text","text":"tool result received"}]
	}`), &finalItem); err != nil {
		t.Fatal(err)
	}
	streams := []*scriptedResponseStream{
		{events: []responses.ResponseStreamEventUnion{
			{Type: "response.completed", Response: *first},
		}},
		{events: []responses.ResponseStreamEventUnion{
			{Type: "response.output_item.done", Item: finalItem},
			{Type: "response.completed", Response: *responseFromJSON(t, `{
				"id":"resp_final",
				"status":"completed",
				"output":[{"type":"reasoning","id":"reason_final","status":"completed","summary":[{"type":"summary_text","text":"checked"}]}]
			}`)},
		}},
	}
	streamIndex := 0
	toolCalls := 0
	var events []UIEvent
	agent := Agent{
		getUserMessage: userMessages("use the tool"),
		tools: []ToolDefinition{{Name: "echo", Function: func(json.RawMessage) (string, error) {
			toolCalls++
			return "ok", nil
		}}},
		events: UIEventSinkFunc(func(event UIEvent) {
			events = append(events, event)
		}),
		createStream: func(context.Context, responses.ResponseNewParams) responseStream {
			if streamIndex >= len(streams) {
				t.Fatal("unexpected additional inference request")
			}
			stream := streams[streamIndex]
			streamIndex++
			return stream
		},
	}

	if err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if toolCalls != 1 || streamIndex != 2 {
		t.Fatalf("tool calls = %d, streams = %d", toolCalls, streamIndex)
	}
	var replies []string
	for _, event := range events {
		if event.Kind == UIEventAssistantMessage {
			replies = append(replies, event.Text)
		}
	}
	if got := strings.Join(replies, ""); got != "tool result received" {
		t.Fatalf("reconstructed reply = %q", got)
	}
}

func TestAgentReplaysFullToolContextForCustomBaseURL(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "https://provider.example/v1")
	first := responseFromJSON(t, `{
		"id":"resp_call",
		"status":"completed",
		"output":[{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{}"}]
	}`)
	streams := []*scriptedResponseStream{
		{events: []responses.ResponseStreamEventUnion{{Type: "response.completed", Response: *first}}},
		{events: []responses.ResponseStreamEventUnion{{Type: "response.completed", Response: streamedCompletedResponse(t, "tool result received")}}},
	}
	var params []responses.ResponseNewParams
	agent := Agent{
		getUserMessage: userMessages("use the tool"),
		tools: []ToolDefinition{{Name: "echo", Function: func(json.RawMessage) (string, error) {
			return "ok", nil
		}}},
		createStream: func(_ context.Context, request responses.ResponseNewParams) responseStream {
			params = append(params, request)
			stream := streams[len(params)-1]
			return stream
		},
	}

	if err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(params) != 2 {
		t.Fatalf("requests = %d, want 2", len(params))
	}
	encoded, err := json.Marshal(params[1])
	if err != nil {
		t.Fatal(err)
	}
	var followUp struct {
		PreviousResponseID string `json:"previous_response_id"`
		Input              []struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			CallID  string          `json:"call_id"`
			Content json.RawMessage `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(encoded, &followUp); err != nil {
		t.Fatal(err)
	}
	if followUp.PreviousResponseID != "" {
		t.Fatalf("custom tool follow-up sent previous_response_id: %s", encoded)
	}
	if len(followUp.Input) != 3 || followUp.Input[0].Role != "user" || !bytes.Contains(followUp.Input[0].Content, []byte("use the tool")) || followUp.Input[1].Type != "function_call" || followUp.Input[2].Type != "function_call_output" || followUp.Input[2].CallID != "call_1" {
		t.Fatalf("custom tool follow-up did not replay the full context: %s", encoded)
	}
}

func TestAgentRejectsEmptyCustomToolFollowUpWithoutRetry(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "https://provider.example/v1")
	first := responseFromJSON(t, `{
		"id":"resp_call",
		"status":"completed",
		"output":[{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{}"}]
	}`)
	empty := responses.Response{ID: "resp_empty", Status: responses.ResponseStatusCompleted}
	streams := []*scriptedResponseStream{
		{events: []responses.ResponseStreamEventUnion{{Type: "response.completed", Response: *first}}},
		{events: []responses.ResponseStreamEventUnion{{Type: "response.completed", Response: empty}}},
	}
	toolCalls := 0
	var params []responses.ResponseNewParams
	agent := Agent{
		getUserMessage: userMessages("use the tool"),
		tools: []ToolDefinition{{Name: "echo", Function: func(json.RawMessage) (string, error) {
			toolCalls++
			return "ok", nil
		}}},
		createStream: func(_ context.Context, request responses.ResponseNewParams) responseStream {
			params = append(params, request)
			stream := streams[len(params)-1]
			return stream
		},
	}

	err := agent.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "without assistant output or tool call") {
		t.Fatalf("empty custom follow-up error = %v", err)
	}
	if toolCalls != 1 || len(params) != 2 {
		t.Fatalf("tool calls = %d, requests = %d; want 1 and 2", toolCalls, len(params))
	}
	encoded, err := json.Marshal(params[1])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"previous_response_id"`)) {
		t.Fatalf("custom tool follow-up sent previous_response_id: %s", encoded)
	}
}

func TestAgentRejectsEmptyCompletedResponse(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", defaultBaseURL)
	agent := Agent{
		getUserMessage: userMessages("reply"),
		createStream: func(context.Context, responses.ResponseNewParams) responseStream {
			return &scriptedResponseStream{events: []responses.ResponseStreamEventUnion{
				{Type: "response.completed", Response: responses.Response{ID: "resp_empty", Status: responses.ResponseStatusCompleted}},
			}}
		},
	}

	err := agent.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "without assistant output or tool call") {
		t.Fatalf("empty completed response error = %v", err)
	}
}

func TestAgentFinishesPartialStreamBeforeReportingError(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", defaultBaseURL)
	stream := &scriptedResponseStream{events: []responses.ResponseStreamEventUnion{
		{Type: "response.output_text.delta", Delta: "partial"},
		{Type: "error", Message: "connection dropped"},
	}}
	var events []UIEvent
	agent := Agent{
		getUserMessage: userMessages("reply"),
		events: UIEventSinkFunc(func(event UIEvent) {
			events = append(events, event)
		}),
		createStream: func(context.Context, responses.ResponseNewParams) responseStream {
			return stream
		},
	}

	err := agent.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "connection dropped") {
		t.Fatalf("stream error = %v", err)
	}
	if !stream.closed {
		t.Fatal("stream was not closed")
	}
	if len(events) < 2 || events[len(events)-1].Kind != UIEventAssistantDone {
		t.Fatalf("events did not finish the partial response: %#v", events)
	}
}

func TestAgentStreamsForCustomBaseURL(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "https://provider.example/v1")
	streamCalled := false
	stream := &scriptedResponseStream{events: []responses.ResponseStreamEventUnion{
		{Type: "response.output_text.delta", Delta: "streamed "},
		{Type: "response.output_text.delta", Delta: "reply"},
		{Type: "response.completed", Response: streamedCompletedResponse(t, "streamed reply")},
	}}
	agent := Agent{
		getUserMessage: userMessages("reply"),
		output:         &bytes.Buffer{},
		createStream: func(context.Context, responses.ResponseNewParams) responseStream {
			streamCalled = true
			return stream
		},
		createResponse: func(context.Context, responses.ResponseNewParams) (*responses.Response, error) {
			t.Fatal("non-streaming response path was used")
			return nil, nil
		},
	}

	if err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !streamCalled {
		t.Fatal("streaming path was not used")
	}
}

func TestAgentStreamsFromCustomResponseSSEEndpoint(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		if incoming.Method != http.MethodPost || incoming.URL.Path != "/responses" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if got := incoming.Header.Get("Accept"); got != "text/event-stream" {
			http.Error(writer, "streaming requests must accept text/event-stream", http.StatusNotAcceptable)
			return
		}
		defer incoming.Body.Close()
		if err := json.NewDecoder(incoming.Body).Decode(&request); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"streamed \"}\n\n")
		_, _ = fmt.Fprint(writer, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"reply\"}\n\n")
		_, _ = fmt.Fprint(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_sse\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_sse\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"streamed reply\",\"annotations\":[]}]}]}}\n\n")
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL)

	client := openai.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(server.URL))
	var output bytes.Buffer
	agent := NewAgent(&client, userMessages("reply"), nil)
	agent.output = &output
	if err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := request["stream"].(bool); !ok || !got {
		t.Fatalf("stream request flag = %#v", request["stream"])
	}
	if got := output.String(); !strings.Contains(got, "Meldra\u001b[0m: streamed reply\n") {
		t.Fatalf("SSE output = %q", got)
	}
}

func TestAgentStreamsFromCustomSSEEndpointWithoutContentType(t *testing.T) {
	firstEventWritten := make(chan struct{})
	releaseStream := make(chan struct{})
	completed := streamedCompletedResponse(t, "first token")
	completedEvent, err := json.Marshal(struct {
		Type     string             `json:"type"`
		Response responses.Response `json:"response"`
	}{
		Type:     "response.completed",
		Response: completed,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		if incoming.Method != http.MethodPost || incoming.URL.Path != "/responses" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		// Deliberately omit Content-Type: some compatible gateways do this even
		// though their body is valid SSE.
		_, _ = fmt.Fprint(writer, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"first token\"}\n\n")
		writer.(http.Flusher).Flush()
		close(firstEventWritten)
		<-releaseStream
		_, _ = fmt.Fprintf(writer, "event: response.completed\ndata: %s\n\n", completedEvent)
		writer.(http.Flusher).Flush()
	}))
	defer server.Close()

	t.Setenv("OPENAI_BASE_URL", server.URL)
	client := openai.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(server.URL))
	firstDelta := make(chan struct{}, 1)
	agent := NewAgent(&client, userMessages("reply"), nil)
	agent.events = UIEventSinkFunc(func(event UIEvent) {
		if event.Kind == UIEventAssistantDelta {
			select {
			case firstDelta <- struct{}{}:
			default:
			}
		}
	})
	runDone := make(chan error, 1)
	go func() { runDone <- agent.Run(context.Background()) }()

	select {
	case <-firstEventWritten:
	case <-time.After(time.Second):
		t.Fatal("server did not write the first SSE event")
	}

	select {
	case <-firstDelta:
		// The first delta must be available before the server closes the stream.
	case <-time.After(time.Second):
		close(releaseStream)
		<-runDone
		t.Fatal("first SSE delta was buffered until the response finished")
	}
	close(releaseStream)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("agent did not finish after the terminal SSE event")
	}
}

func TestAgentFallsBackForCustomBaseURLWhenStreamingIsUnsupported(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "https://provider.example/v1")
	response := streamedCompletedResponse(t, "fallback reply")
	streamCalled := 0
	responseCalled := 0
	stream := &scriptedResponseStream{err: &openai.Error{StatusCode: http.StatusBadRequest, Message: "streaming is not supported"}}
	agent := Agent{
		createStream: func(context.Context, responses.ResponseNewParams) responseStream {
			streamCalled++
			return stream
		},
		createResponse: func(context.Context, responses.ResponseNewParams) (*responses.Response, error) {
			if !stream.closed {
				t.Fatal("stream was not closed before non-streaming fallback")
			}
			responseCalled++
			return &response, nil
		},
	}

	for range 2 {
		result, err := agent.runInference(context.Background(), responses.ResponseNewParamsInputUnion{}, "")
		if err != nil {
			t.Fatal(err)
		}
		if result.response == nil || result.response.OutputText() != "fallback reply" {
			t.Fatalf("fallback response = %#v", result.response)
		}
	}
	if streamCalled != 1 || responseCalled != 2 {
		t.Fatalf("stream calls = %d, response calls = %d", streamCalled, responseCalled)
	}
	if !agent.streamUnsupported {
		t.Fatal("unsupported stream capability was not remembered")
	}
}

func TestAgentUsesOneRequestWhenCustomEndpointReturnsJSONForStream(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		requests++
		if incoming.Method != http.MethodPost || incoming.URL.Path != "/responses" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, `{"id":"resp_json","status":"completed","output":[{"type":"message","id":"msg_json","status":"completed","role":"assistant","content":[{"type":"output_text","text":"complete reply","annotations":[]}]}]}`)
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL)

	client := openai.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(server.URL))
	var output bytes.Buffer
	agent := NewAgent(&client, userMessages("reply"), nil)
	agent.output = &output
	if err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("custom endpoint requests = %d, want 1", requests)
	}
	if got := output.String(); !strings.Contains(got, "Meldra\u001b[0m: complete reply\n") {
		t.Fatalf("JSON response output = %q", got)
	}
}

func TestAgentDoesNotFallbackForTransientCustomStreamError(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "https://provider.example/v1")
	want := context.DeadlineExceeded
	stream := &scriptedResponseStream{err: want}
	agent := Agent{
		createStream: func(context.Context, responses.ResponseNewParams) responseStream {
			return stream
		},
		createResponse: func(context.Context, responses.ResponseNewParams) (*responses.Response, error) {
			t.Fatal("transient stream error must not be retried")
			return nil, nil
		},
	}

	_, err := agent.runInference(context.Background(), responses.ResponseNewParamsInputUnion{}, "")
	if !errors.Is(err, want) {
		t.Fatalf("stream error = %v, want %v", err, want)
	}
	if agent.streamUnsupported {
		t.Fatal("transient stream error disabled streaming")
	}
}

func TestUnsupportedStreamErrorDetection(t *testing.T) {
	unsupported := &openai.Error{StatusCode: http.StatusBadRequest, Message: "streaming is not supported"}
	if !isUnsupportedStreamError(unsupported) {
		t.Fatal("unsupported stream API error was not recognized")
	}
	for _, err := range []error{
		context.Canceled,
		&openai.Error{StatusCode: http.StatusBadRequest, Message: "invalid model"},
		&openai.Error{StatusCode: http.StatusServiceUnavailable, Message: "streaming is not supported"},
	} {
		if isUnsupportedStreamError(err) {
			t.Fatalf("error was incorrectly treated as stream unsupported: %#v", err)
		}
	}
}

func TestAgentDoesNotFallbackAfterCustomStreamBegins(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "https://provider.example/v1")
	stream := &scriptedResponseStream{events: []responses.ResponseStreamEventUnion{
		{Type: "response.output_text.delta", Delta: "partial"},
	}, err: errors.New("connection dropped")}
	agent := Agent{
		getUserMessage: userMessages("reply"),
		createStream: func(context.Context, responses.ResponseNewParams) responseStream {
			return stream
		},
		createResponse: func(context.Context, responses.ResponseNewParams) (*responses.Response, error) {
			t.Fatal("stream failure after output must not be retried")
			return nil, nil
		},
	}

	err := agent.Run(context.Background())
	if !errors.Is(err, stream.err) {
		t.Fatalf("stream error = %v, want %v", err, stream.err)
	}
}

func TestRunInferenceReturnsStreamReadError(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", defaultBaseURL)
	want := errors.New("read failure")
	stream := &scriptedResponseStream{err: want}
	agent := Agent{createStream: func(context.Context, responses.ResponseNewParams) responseStream {
		return stream
	}}

	_, err := agent.runInference(context.Background(), responses.ResponseNewParamsInputUnion{}, "")
	if !errors.Is(err, want) {
		t.Fatalf("stream error = %v, want %v", err, want)
	}
	if !stream.closed {
		t.Fatal("stream was not closed")
	}
}

func TestRunInferenceTimesOutAnIdleStreamAfterPartialOutput(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", defaultBaseURL)
	stream := newStallingResponseStream(responses.ResponseStreamEventUnion{
		Type:  "response.output_text.delta",
		Delta: "partial",
	})
	agent := Agent{
		streamIdleTimeout: 20 * time.Millisecond,
		createStream: func(context.Context, responses.ResponseNewParams) responseStream {
			return stream
		},
	}

	started := time.Now()
	result, err := agent.runInference(context.Background(), responses.ResponseNewParamsInputUnion{}, "")
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("idle timeout took too long: %s", elapsed)
	}
	var timeoutErr *responseStreamIdleTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("idle stream error = %v, want responseStreamIdleTimeoutError", err)
	}
	if result.streamedText != "partial" || !result.streamedTextShown {
		t.Fatalf("partial stream result = %#v", result)
	}
	select {
	case <-stream.released:
		// The watchdog closed the blocked stream rather than leaving Next stuck.
	default:
		t.Fatal("idle watchdog did not close the blocked stream")
	}
}

func TestRunInferenceResetsIdleTimeoutAfterEachSSEEvent(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", defaultBaseURL)
	stream := &delayedResponseStream{
		events: []responses.ResponseStreamEventUnion{
			{Type: "response.created"},
			{Type: "response.output_text.delta", Delta: "still streaming"},
			{Type: "response.completed", Response: streamedCompletedResponse(t, "still streaming")},
		},
		delays: []time.Duration{0, 100 * time.Millisecond, 100 * time.Millisecond},
		closed: make(chan struct{}),
	}
	agent := Agent{
		streamIdleTimeout: 150 * time.Millisecond,
		createStream: func(context.Context, responses.ResponseNewParams) responseStream {
			return stream
		},
	}

	result, err := agent.runInference(context.Background(), responses.ResponseNewParamsInputUnion{}, "")
	if err != nil {
		t.Fatalf("stream with regular events timed out: %v", err)
	}
	if result.response == nil || result.response.OutputText() != "still streaming" {
		t.Fatalf("stream response = %#v", result.response)
	}
}

func TestRunInferenceTimesOutBeforeStreamingResponseArrives(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", defaultBaseURL)
	started := make(chan struct{})
	agent := Agent{
		streamIdleTimeout: 20 * time.Millisecond,
		createStream: func(ctx context.Context, _ responses.ResponseNewParams) responseStream {
			close(started)
			<-ctx.Done()
			return &scriptedResponseStream{err: ctx.Err()}
		},
	}

	done := make(chan error, 1)
	go func() {
		_, err := agent.runInference(context.Background(), responses.ResponseNewParamsInputUnion{}, "")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream creation did not start")
	}
	select {
	case err := <-done:
		var timeoutErr *responseStreamIdleTimeoutError
		if !errors.As(err, &timeoutErr) {
			t.Fatalf("pre-stream timeout error = %v, want responseStreamIdleTimeoutError", err)
		}
	case <-time.After(time.Second):
		t.Fatal("idle watchdog did not cancel a stalled stream creation")
	}
}

func TestAgentDoesNotPersistCancelledInput(t *testing.T) {
	store := NewSessionStore(mustConfigPaths(t))
	session, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	agent := Agent{
		getUserMessage: func() (string, bool) {
			cancel()
			return "discard this", true
		},
		session: session,
		store:   store,
		createResponse: func(context.Context, responses.ResponseNewParams) (*responses.Response, error) {
			t.Fatal("cancelled input reached inference")
			return nil, nil
		},
	}

	if err := agent.Run(ctx); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 0 {
		t.Fatalf("cancelled input was persisted: %#v", loaded.Messages)
	}
}

func TestAgentPersistsPartialStreamAfterFailure(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", defaultBaseURL)
	store := NewSessionStore(mustConfigPaths(t))
	session, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stream := &scriptedResponseStream{events: []responses.ResponseStreamEventUnion{
		{Type: "response.output_text.delta", Delta: "partial answer"},
		{Type: "error", Message: "connection dropped"},
	}}
	agent := Agent{
		getUserMessage: userMessages("reply"),
		session:        session,
		store:          store,
		createStream: func(context.Context, responses.ResponseNewParams) responseStream {
			return stream
		},
	}

	if err := agent.Run(context.Background()); err == nil {
		t.Fatal("stream error was not returned")
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 {
		t.Fatalf("saved messages = %#v", loaded.Messages)
	}
	if got := loaded.Messages[1]; got.Role != "assistant" || !strings.Contains(got.Content, "partial answer") || !strings.Contains(got.Content, "Streaming interrupted") {
		t.Fatalf("partial response = %#v", got)
	}
	if !loaded.resumed || loaded.PreviousResponseID != "" {
		t.Fatalf("session continuation state = %#v", loaded)
	}
}

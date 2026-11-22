package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

const (
	defaultStreamIdleTimeout                 = 90 * time.Second
	defaultProviderResponseBytes       int64 = 32 << 20
	defaultProviderResponsePrefixBytes int64 = 64 << 10
)

type Stream interface {
	Next() bool
	Current() responses.ResponseStreamEventUnion
	Err() error
	Close() error
}
type responseStream = Stream

type wireResult struct {
	Response          *responses.Response
	StreamedText      string
	StreamedTextShown bool
	ReceivedTextDelta bool
	StreamHadEvent    bool
}

type Result struct {
	Response          *Response
	StreamedText      string
	StreamedTextShown bool
	ReceivedTextDelta bool
	StreamHadEvent    bool
}

type Inference interface {
	Infer(context.Context, Request, Options, Observer) (Result, error)
}
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
	NonStrict   bool
}
type Request struct {
	Model              string
	Instructions       string
	Input              Input
	PreviousResponseID string
	MaxOutputTokens    int64
	Tools              []Tool
}

type Options struct {
	CustomProvider   bool
	IdleTimeout      time.Duration
	MaxResponseBytes int64
}

type Observer struct {
	Status     func(string)
	Text       func(string, bool)
	FilterText func(string) string
}

type Client struct {
	sdk               *openai.Client
	CreateResponse    func(context.Context, responses.ResponseNewParams) (*responses.Response, error)
	CreateStream      func(context.Context, responses.ResponseNewParams) Stream
	streamUnsupported bool
}

type Connection struct {
	APIKey  string
	BaseURL string
}

func Connect(connection Connection) *Client {
	client := openai.NewClient(option.WithAPIKey(connection.APIKey), option.WithBaseURL(connection.BaseURL))
	return New(&client)
}

func New(client *openai.Client) *Client      { return &Client{sdk: client} }
func (c *Client) StreamingUnsupported() bool { return c.streamUnsupported }

func (c *Client) Infer(ctx context.Context, request Request, options Options, observer Observer) (Result, error) {
	params := responses.ResponseNewParams{Model: request.Model, Instructions: openai.String(request.Instructions), Input: request.Input.value}
	if request.MaxOutputTokens > 0 {
		params.MaxOutputTokens = openai.Int(request.MaxOutputTokens)
	}
	if request.PreviousResponseID != "" {
		params.PreviousResponseID = openai.String(request.PreviousResponseID)
	}
	for _, tool := range request.Tools {
		params.Tools = append(params.Tools, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
			Name: tool.Name, Description: openai.String(tool.Description), Parameters: tool.Parameters, Strict: openai.Bool(!tool.NonStrict),
		}})
	}

	a := invocation{client: c, customProvider: options.CustomProvider, streamIdleTimeout: options.IdleTimeout,
		maxProviderResponseBytes: options.MaxResponseBytes, createResponse: c.CreateResponse, createStream: c.CreateStream, observer: observer}
	if c.sdk != nil {
		a.createResponse = func(ctx context.Context, params responses.ResponseNewParams) (*responses.Response, error) {
			return c.sdk.Responses.New(ctx, params, option.WithMiddleware(limitProviderResponseWithLimit(a.providerResponseLimit())))
		}
		a.createStream = func(ctx context.Context, params responses.ResponseNewParams) Stream {
			options := []option.RequestOption{option.WithHeader("Accept", "text/event-stream")}
			if a.customProvider {
				options = append(options, option.WithMiddleware(normalizeNonSSEStreamingResponseWithLimit(a.providerResponseLimit())))
			}
			options = append(options, option.WithMiddleware(limitProviderResponseWithLimit(a.providerResponseLimit())))
			return c.sdk.Responses.NewStreaming(ctx, params, options...)
		}
	}
	result, err := a.run(ctx, params)
	return Result{Response: domainResponse(result.Response), StreamedText: result.StreamedText,
		StreamedTextShown: result.StreamedTextShown, ReceivedTextDelta: result.ReceivedTextDelta, StreamHadEvent: result.StreamHadEvent}, err
}

type invocation struct {
	client                   *Client
	customProvider           bool
	streamIdleTimeout        time.Duration
	maxProviderResponseBytes int64
	createResponse           func(context.Context, responses.ResponseNewParams) (*responses.Response, error)
	createStream             func(context.Context, responses.ResponseNewParams) Stream
	observer                 Observer
}

func (a *invocation) status(value string) {
	if a.observer.Status != nil {
		a.observer.Status(value)
	}
}
func (a *invocation) text(value string, first bool) {
	if a.observer.Text != nil {
		a.observer.Text(value, first)
	}
}
func (a *invocation) filterText(value string) string {
	if a.observer.FilterText != nil {
		return a.observer.FilterText(value)
	}
	return value
}

type IdleTimeoutError struct {
	Timeout time.Duration
}

func (e *IdleTimeoutError) Error() string {
	return fmt.Sprintf("response stream was idle for %s without a terminal event; the API gateway did not finish the response. Retry, or use a gateway with Responses streaming support", e.Timeout)
}

type responseStreamCloser struct {
	mu             sync.Mutex
	stream         responseStream
	closeRequested bool
}

func (c *responseStreamCloser) set(stream responseStream) {
	c.mu.Lock()
	c.stream = stream
	shouldClose := c.closeRequested
	c.mu.Unlock()
	if shouldClose && stream != nil {
		_ = stream.Close()
	}
}

func (c *responseStreamCloser) close() {
	c.mu.Lock()
	if c.closeRequested {
		c.mu.Unlock()
		return
	}
	c.closeRequested = true
	stream := c.stream
	c.mu.Unlock()
	if stream != nil {
		_ = stream.Close()
	}
}

type responseStreamIdleWatchdog struct {
	timeout   time.Duration
	onTimeout func()

	mu         sync.Mutex
	timer      *time.Timer
	generation uint64
	stopped    bool
	timedOut   bool
}

func newResponseStreamIdleWatchdog(timeout time.Duration, onTimeout func()) *responseStreamIdleWatchdog {
	if timeout <= 0 {
		return nil
	}
	watchdog := &responseStreamIdleWatchdog{
		timeout:   timeout,
		onTimeout: onTimeout,
	}
	watchdog.resetLocked()
	return watchdog
}

func (w *responseStreamIdleWatchdog) noteEvent() bool {
	if w == nil {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || w.timedOut {
		return false
	}
	w.resetLocked()
	return true
}

func (w *responseStreamIdleWatchdog) resetLocked() {
	w.generation++
	generation := w.generation
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(w.timeout, func() {
		w.expire(generation)
	})
}

func (w *responseStreamIdleWatchdog) expire(generation uint64) {
	w.mu.Lock()
	if w.stopped || w.timedOut || generation != w.generation {
		w.mu.Unlock()
		return
	}
	w.timedOut = true
	onTimeout := w.onTimeout
	w.mu.Unlock()
	if onTimeout != nil {
		onTimeout()
	}
}

func (w *responseStreamIdleWatchdog) stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.stopped = true
	if w.timer != nil {
		w.timer.Stop()
	}
	w.mu.Unlock()
}

func (w *responseStreamIdleWatchdog) expired() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.timedOut
}

func (a *invocation) effectiveStreamIdleTimeout() time.Duration {
	if a.streamIdleTimeout != 0 {
		return a.streamIdleTimeout
	}
	return defaultStreamIdleTimeout
}

func (a *invocation) providerResponseLimit() int64 {
	if a.maxProviderResponseBytes > 0 {
		return a.maxProviderResponseBytes
	}
	return defaultProviderResponseBytes
}

func (a *invocation) run(ctx context.Context, params responses.ResponseNewParams) (wireResult, error) {

	if a.createStream == nil || a.client.streamUnsupported {
		if a.createResponse == nil {
			return wireResult{}, fmt.Errorf("response client is not configured")
		}
		response, err := a.createResponse(ctx, params)
		return wireResult{Response: response}, err
	}

	idleTimeout := a.effectiveStreamIdleTimeout()
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	streamCloser := &responseStreamCloser{}
	idleWatchdog := newResponseStreamIdleWatchdog(idleTimeout, func() {
		cancelStream()
		streamCloser.close()
	})
	defer idleWatchdog.stop()
	stream := a.createStream(streamCtx, params)
	streamCloser.set(stream)
	if idleWatchdog.expired() {
		return wireResult{}, &IdleTimeoutError{Timeout: idleTimeout}
	}
	if stream == nil {
		return a.fallbackFromUnsupportedStream(ctx, params, wireResult{}, fmt.Errorf("response stream is not configured"))
	}
	closeStream := streamCloser.close
	defer closeStream()

	var result wireResult
	textBytes := 0
	parts := make(map[[2]int64]*strings.Builder)
	var partOrder [][2]int64
	partFor := func(event responses.ResponseStreamEventUnion) *strings.Builder {
		key := [2]int64{event.OutputIndex, event.ContentIndex}
		if part := parts[key]; part != nil {
			return part
		}
		part := new(strings.Builder)
		parts[key] = part
		partOrder = append(partOrder, key)
		return part
	}
	currentText := func() string {
		var joined strings.Builder
		joined.Grow(textBytes)
		for _, key := range partOrder {
			joined.WriteString(parts[key].String())
		}
		return joined.String()
	}
	var completedOutput []responses.ResponseOutputItemUnion
	var completedIndices []int64
	responseLimit := a.providerResponseLimit()
	for stream.Next() {
		if !idleWatchdog.noteEvent() {
			result.StreamedText = currentText()
			return result, &IdleTimeoutError{Timeout: idleTimeout}
		}
		result.StreamHadEvent = true
		event := stream.Current()
		switch event.Type {
		case "response.created", "response.in_progress":
			a.status("Thinking")
		case "response.output_text.delta":
			if event.Delta != "" {
				if exceedsProviderResponseLimit(textBytes, len(event.Delta), responseLimit) {
					result.StreamedText = currentText()
					return result, &ResponseLimitError{Limit: responseLimit}
				}
				result.ReceivedTextDelta = true
				partFor(event).WriteString(event.Delta)
				textBytes += len(event.Delta)
				if delta := a.filterText(event.Delta); delta != "" {
					firstDelta := !result.StreamedTextShown
					result.StreamedTextShown = true
					a.text(delta, firstDelta)
				}
			}
		case "response.output_item.added":
			if event.Item.Type == "function_call" && event.Item.Name != "" {
				a.status("Preparing " + event.Item.Name)
			}
		case "response.output_text.done":
			if event.Text != "" {
				part := partFor(event)
				previous := part.String()
				if exceedsProviderResponseLimit(textBytes-len(previous), len(event.Text), responseLimit) {
					result.StreamedText = currentText()
					return result, &ResponseLimitError{Limit: responseLimit}
				}
				part.Reset()
				part.WriteString(event.Text)
				textBytes += len(event.Text) - len(previous)
				result.ReceivedTextDelta = true
				finalText := event.Text
				first := !result.StreamedTextShown
				if strings.HasPrefix(event.Text, previous) {
					finalText = strings.TrimPrefix(event.Text, previous)
				} else if previous != "" && result.StreamedTextShown {
					finalText = "\n" + event.Text
				}
				if finalText = a.filterText(finalText); finalText != "" {
					result.StreamedTextShown = true
					a.text(finalText, first)
				}
			}
		case "response.output_item.done":
			if event.Item.Type != "" {
				completedOutput = append(completedOutput, event.Item)
				completedIndices = append(completedIndices, event.OutputIndex)
			}
		case "response.completed", "response.failed", "response.incomplete":
			response := event.Response
			merged, err := mergeCompletedStreamOutput(response.Output, completedOutput, completedIndices)
			if err != nil {
				result.StreamedText = currentText()
				return result, err
			}
			response.Output = merged
			if OutputText(&response) == "" && textBytes > 0 {
				raw, _ := json.Marshal(map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []map[string]any{{"type": "output_text", "text": currentText(), "annotations": []any{}}}})
				var message responses.ResponseOutputItemUnion
				if err := json.Unmarshal(raw, &message); err != nil {
					return result, err
				}
				response.Output = append([]responses.ResponseOutputItemUnion{message}, response.Output...)
			}
			result.Response = &response
			result.StreamedText = currentText()
			return result, nil
		case "error":
			result.StreamedText = currentText()
			if event.Message != "" {
				return result, fmt.Errorf("response stream: %s", event.Message)
			}
			return result, fmt.Errorf("response stream failed")
		}
	}
	result.StreamedText = currentText()
	if idleWatchdog.expired() {
		return result, &IdleTimeoutError{Timeout: idleTimeout}
	}
	if err := stream.Err(); err != nil {
		if !result.StreamHadEvent && isUnsupportedStreamError(err) {
			closeStream()
			return a.fallbackFromUnsupportedStream(ctx, params, result, err)
		}
		return result, err
	}
	err := fmt.Errorf("response stream ended without a terminal response")
	return result, err
}

func mergeCompletedStreamOutput(output, completed []responses.ResponseOutputItemUnion, positions ...[]int64) ([]responses.ResponseOutputItemUnion, error) {
	if len(completed) == 0 {
		return output, nil
	}
	if len(output) == 0 {
		return append([]responses.ResponseOutputItemUnion(nil), completed...), nil
	}

	merged := append([]responses.ResponseOutputItemUnion(nil), output...)
	for completedIndex, completedItem := range completed {
		match := -1
		for index, existing := range merged {
			if existing.ID != "" && existing.ID == completedItem.ID {
				match = index
				break
			}
			if existing.Type == "function_call" && completedItem.Type == "function_call" && existing.CallID != "" && existing.CallID == completedItem.CallID {
				match = index
				break
			}
		}
		if match < 0 && completedItem.Type == "function_call" && len(positions) > 0 && completedIndex < len(positions[0]) {
			position := positions[0][completedIndex]
			if position >= 0 && position < int64(len(output)) && output[position].Type == "function_call" {
				original := output[position]
				for index, existing := range merged {
					if existing.Type == "function_call" && (original.ID != "" && original.ID == existing.ID || original.CallID != "" && original.CallID == existing.CallID) {
						match = index
						break
					}
				}
			}
		}
		if match >= 0 {
			if merged[match].Type != completedItem.Type {
				return nil, errors.New("conflicting response item types")
			}
			if completedItem.Type == "function_call" {
				item, err := mergeFunctionCall(merged[match], completedItem)
				if err != nil {
					return nil, err
				}
				merged[match] = item
				continue
			}
			merged[match] = completedItem
			continue
		}
		if completedItem.Type == "message" {
			text := OutputText(&responses.Response{Output: []responses.ResponseOutputItemUnion{completedItem}})
			duplicate := false
			for _, existing := range merged {
				if existing.Type == "message" && (existing.ID == "" || completedItem.ID == "") && text != "" && text == OutputText(&responses.Response{Output: []responses.ResponseOutputItemUnion{existing}}) {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
		}
		if completedItem.Type == "message" && len(positions) > 0 && completedIndex < len(positions[0]) {
			position := positions[0][completedIndex]
			if position >= 0 && position <= int64(len(merged)) {
				merged = slices.Insert(merged, int(position), completedItem)
				continue
			}
		}
		merged = append(merged, completedItem)
	}
	return merged, nil
}

func mergeFunctionCall(terminal, streamed responses.ResponseOutputItemUnion) (responses.ResponseOutputItemUnion, error) {
	for _, pair := range [][2]string{{terminal.ID, streamed.ID}, {terminal.CallID, streamed.CallID}, {terminal.Name, streamed.Name}} {
		if pair[0] != "" && pair[1] != "" && pair[0] != pair[1] {
			return terminal, errors.New("conflicting streamed and terminal tool call")
		}
	}
	fields := func(item responses.ResponseOutputItemUnion) (map[string]json.RawMessage, error) {
		raw := []byte(item.RawJSON())
		if len(raw) == 0 {
			var err error
			raw, err = json.Marshal(item)
			if err != nil {
				return nil, err
			}
		}
		var result map[string]json.RawMessage
		err := json.Unmarshal(raw, &result)
		return result, err
	}
	left, err := fields(terminal)
	if err != nil {
		return terminal, err
	}
	right, err := fields(streamed)
	if err != nil {
		return terminal, err
	}
	terminalArgs, terminalValid := normalizedArguments(terminal.Arguments.OfString)
	streamedArgs, streamedValid := normalizedArguments(streamed.Arguments.OfString)
	if terminalValid && streamedValid && !reflect.DeepEqual(terminalArgs, streamedArgs) {
		return terminal, errors.New("conflicting streamed and terminal tool arguments")
	}
	if terminalValid {
		right["arguments"] = left["arguments"]
	}
	for key, value := range left {
		if old := right[key]; len(old) == 0 || string(old) == `""` || string(old) == "null" {
			right[key] = value
		}
	}
	raw, err := json.Marshal(right)
	if err != nil {
		return terminal, err
	}
	var merged responses.ResponseOutputItemUnion
	err = json.Unmarshal(raw, &merged)
	return merged, err
}

type exactJSONNumber struct{ Value string }

func normalizedArguments(raw string) (any, bool) {
	if !json.Valid([]byte(raw)) {
		return nil, false
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	return normalizeJSONNumbers(value), true
}

func normalizeJSONNumbers(value any) any {
	switch v := value.(type) {
	case json.Number:
		if len(v) > 4096 {
			return v
		}
		if index := strings.IndexAny(string(v), "eE"); index >= 0 {
			exponent, err := strconv.ParseInt(string(v)[index+1:], 10, 32)
			if err != nil || exponent < -4096 || exponent > 4096 {
				return v
			}
		}
		if number, ok := new(big.Rat).SetString(string(v)); ok {
			return exactJSONNumber{number.RatString()}
		}
		return v
	case []any:
		for i := range v {
			v[i] = normalizeJSONNumbers(v[i])
		}
	case map[string]any:
		for key, item := range v {
			v[key] = normalizeJSONNumbers(item)
		}
	}
	return value
}

func (a *invocation) fallbackFromUnsupportedStream(ctx context.Context, params responses.ResponseNewParams, result wireResult, streamErr error) (wireResult, error) {
	if !a.customProvider || result.StreamedTextShown || a.createResponse == nil {
		return result, streamErr
	}
	response, err := a.createResponse(ctx, params)
	if err != nil {
		return result, err
	}
	a.client.streamUnsupported = true
	return wireResult{Response: response}, nil
}

func isUnsupportedStreamError(err error) bool {
	apiErr, ok := errors.AsType[*openai.Error](err)
	if !ok || apiErr.StatusCode < 400 || apiErr.StatusCode >= 500 {
		return false
	}
	message := strings.ToLower(apiErr.Message + " " + apiErr.RawJSON())
	return strings.Contains(message, "stream") && (strings.Contains(message, "unsupported") || strings.Contains(message, "not supported") || strings.Contains(message, "not allowed") || strings.Contains(message, "not available"))
}

type ResponseLimitError struct {
	Limit int64
}

func (e *ResponseLimitError) Error() string {
	return fmt.Sprintf("provider response exceeded the configured %s limit", formatResponseByteLimit(e.Limit))
}

type PrefixLimitError struct {
	Limit int64
}

func (e *PrefixLimitError) Error() string {
	return fmt.Sprintf("provider response prefix exceeded the configured %s limit", formatResponseByteLimit(e.Limit))
}

func formatResponseByteLimit(limit int64) string {
	switch {
	case limit%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", limit/(1<<20))
	case limit%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", limit/(1<<10))
	case limit == 1:
		return "1 byte"
	default:
		return fmt.Sprintf("%d bytes", limit)
	}
}

func exceedsProviderResponseLimit(current, additional int, limit int64) bool {
	return int64(current) > limit || int64(additional) > limit-int64(current)
}

func limitProviderResponseWithLimit(limit int64) option.Middleware {
	return func(request *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		response, err := next(request)
		if err != nil || response == nil || response.Body == nil {
			return response, err
		}
		if response.ContentLength > limit {
			_ = response.Body.Close()
			return response, &ResponseLimitError{Limit: limit}
		}
		response.Body = &providerResponseLimitReadCloser{
			ReadCloser: response.Body,
			remaining:  limit,
			limit:      limit,
		}
		return response, nil
	}
}

type providerResponseLimitReadCloser struct {
	io.ReadCloser
	remaining int64
	limit     int64
}

func (r *providerResponseLimitReadCloser) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return r.ReadCloser.Read(buffer)
	}
	if r.remaining == 0 {
		var probe [1]byte
		count, err := r.ReadCloser.Read(probe[:])
		if count > 0 {
			return 0, &ResponseLimitError{Limit: r.limit}
		}
		return count, err
	}
	if int64(len(buffer)) > r.remaining {
		buffer = buffer[:int(r.remaining)]
	}
	count, err := r.ReadCloser.Read(buffer)
	r.remaining -= int64(count)
	return count, err
}

func normalizeNonSSEStreamingResponseWithLimit(limit int64) option.Middleware {
	return func(request *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		return normalizeNonSSEStreamingResponseForLimit(request, next, limit)
	}
}

func normalizeNonSSEStreamingResponseForLimit(request *http.Request, next option.MiddlewareNext, limit int64) (*http.Response, error) {
	response, err := next(request)
	if err != nil || response == nil || response.Body == nil || response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return response, err
	}
	if response.ContentLength > limit {
		_ = response.Body.Close()
		return response, &ResponseLimitError{Limit: limit}
	}

	prefixLimit := int64(defaultProviderResponsePrefixBytes)
	if limit < prefixLimit {
		prefixLimit = limit
	}
	isJSON, restoredBody, inspectErr := responseBodyStartsWithJSONWithLimit(response.Body, prefixLimit)
	if inspectErr != nil {
		_ = response.Body.Close()
		return response, inspectErr
	}
	response.Body = restoredBody
	if !isJSON {
		return response, nil
	}

	responseBody, readErr := readProviderResponse(response.Body, limit)
	closeErr := response.Body.Close()
	if readErr != nil {
		return response, readErr
	}
	if closeErr != nil {
		return response, closeErr
	}

	trimmed := bytes.TrimSpace(responseBody)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		response.Body = io.NopCloser(bytes.NewReader(responseBody))
		return response, nil
	}
	response.Body = io.NopCloser(io.MultiReader(
		strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"),
		&jsonCompactReader{source: bytes.NewReader(trimmed)},
		strings.NewReader("}\n\n"),
	))
	response.Header.Set("Content-Type", "text/event-stream")
	response.ContentLength = -1
	return response, nil
}

func readProviderResponse(body io.Reader, limit int64) ([]byte, error) {
	responseBody, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(responseBody)) > limit {
		return nil, &ResponseLimitError{Limit: limit}
	}
	return responseBody, nil
}

func responseBodyStartsWithJSONWithLimit(body io.ReadCloser, limit int64) (bool, io.ReadCloser, error) {
	reader := bufio.NewReader(body)
	var prefix bytes.Buffer
	for {
		if int64(prefix.Len()) >= limit {
			return false, nil, &PrefixLimitError{Limit: limit}
		}
		byteValue, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, &prefixedReadCloser{Reader: bytes.NewReader(prefix.Bytes()), Closer: body}, nil
			}
			return false, nil, err
		}
		if err := prefix.WriteByte(byteValue); err != nil {
			return false, nil, err
		}
		if byteValue == ' ' || byteValue == '\n' || byteValue == '\r' || byteValue == '\t' {
			continue
		}
		return byteValue == '{' || byteValue == '[', &prefixedReadCloser{
			Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), reader),
			Closer: body,
		}, nil
	}
}

type prefixedReadCloser struct {
	io.Reader
	io.Closer
}

type jsonCompactReader struct {
	source      io.Reader
	buffer      [32 << 10]byte
	unread      []byte
	sourceError error
	inString    bool
	escaped     bool
}

func (r *jsonCompactReader) Read(destination []byte) (int, error) {
	written := 0
	for len(destination) > 0 {
		if len(r.unread) == 0 {
			if r.sourceError != nil {
				if written > 0 {
					return written, nil
				}
				return 0, r.sourceError
			}
			count, err := r.source.Read(r.buffer[:])
			if count > 0 {
				r.unread = r.buffer[:count]
			}
			if err != nil {
				r.sourceError = err
			}
			if count == 0 {
				if written > 0 {
					return written, nil
				}
				return 0, err
			}
		}

		value := r.unread[0]
		r.unread = r.unread[1:]
		if !r.inString && (value == ' ' || value == '\n' || value == '\r' || value == '\t') {
			continue
		}
		destination[0] = value
		destination = destination[1:]
		written++
		if r.inString {
			if r.escaped {
				r.escaped = false
			} else if value == '\\' {
				r.escaped = true
			} else if value == '"' {
				r.inString = false
			}
		} else if value == '"' {
			r.inString = true
		}
	}
	return written, nil
}

func OutputText(response *responses.Response) string {
	if response == nil {
		return ""
	}
	if text := response.OutputText(); text != "" {
		return text
	}

	var text strings.Builder
	for _, item := range response.Output {
		if item.Type != "message" || (item.Role != "" && item.Role != "assistant") {
			continue
		}
		for _, content := range item.Content {
			if content.Text != "" {
				text.WriteString(content.Text)
			}
		}
	}
	return text.String()
}

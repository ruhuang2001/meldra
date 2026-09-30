package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
)

func TestJSONCompactReaderPreservesStringsAcrossShortReads(t *testing.T) {
	reader := &jsonCompactReader{source: strings.NewReader(`{
  "id": "response text",
  "quote": "a \"quoted\" value"
}`)}
	var got strings.Builder
	var buffer [1]byte
	for {
		count, err := reader.Read(buffer[:])
		if count > 0 {
			got.Write(buffer[:count])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	const want = `{"id":"response text","quote":"a \"quoted\" value"}`
	if got.String() != want {
		t.Fatalf("compacted JSON = %q, want %q", got.String(), want)
	}
}
func TestNormalizeNonSSEStreamingResponseRejectsOversizedJSON(t *testing.T) {
	const limit int64 = 8
	request := httptest.NewRequest(http.MethodPost, "https://provider.example/v1/responses", nil)
	response, err := normalizeNonSSEStreamingResponseWithLimit(limit)(request, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"text":"too long"}`))}, nil
	})
	if response == nil {
		t.Fatal("oversized JSON response was discarded")
	}
	var limitErr *ResponseLimitError
	if !errors.As(err, &limitErr) || limitErr.Limit != limit {
		t.Fatalf("oversized JSON error = %v, want provider response limit %d", err, limit)
	}
}
func TestResponseBodyStartsWithJSONRejectsOversizedPrefix(t *testing.T) {
	const limit int64 = 8
	isJSON, restored, err := responseBodyStartsWithJSONWithLimit(io.NopCloser(strings.NewReader(strings.Repeat(" ", int(limit)+1)+`{"id":"response"}`)), limit)
	if isJSON || restored != nil {
		t.Fatalf("oversized prefix result = isJSON:%t body:%#v", isJSON, restored)
	}
	var prefixErr *PrefixLimitError
	if !errors.As(err, &prefixErr) || prefixErr.Limit != limit {
		t.Fatalf("oversized prefix error = %v, want provider response prefix limit %d", err, limit)
	}
}
func TestProviderResponseBodyLimit(t *testing.T) {
	const limit int64 = 8
	request := httptest.NewRequest(http.MethodPost, "https://provider.example/v1/responses", nil)
	response, err := limitProviderResponseWithLimit(limit)(request, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("123456789"))}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	got, readErr := io.ReadAll(response.Body)
	if string(got) != "12345678" {
		t.Fatalf("limited response body = %q", got)
	}
	var limitErr *ResponseLimitError
	if !errors.As(readErr, &limitErr) || limitErr.Limit != limit {
		t.Fatalf("limited response read error = %v, want provider response limit %d", readErr, limit)
	}
	if got := (&ResponseLimitError{Limit: defaultProviderResponseBytes}).Error(); got != "provider response exceeded the configured 32 MiB limit" {
		t.Fatalf("default provider response limit error = %q", got)
	}
}
func TestUnsupportedStreamErrorDetection(t *testing.T) {
	unsupported := &openai.Error{StatusCode: http.StatusBadRequest, Message: "streaming is not supported"}
	if !isUnsupportedStreamError(unsupported) {
		t.Fatal("unsupported stream API error was not recognized")
	}
	for _, err := range []error{context.Canceled, &openai.Error{StatusCode: http.StatusBadRequest, Message: "invalid model"}, &openai.Error{StatusCode: http.StatusServiceUnavailable, Message: "streaming is not supported"}} {
		if isUnsupportedStreamError(err) {
			t.Fatalf("error was incorrectly treated as stream unsupported: %#v", err)
		}
	}
}

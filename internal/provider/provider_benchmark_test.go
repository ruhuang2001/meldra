package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/responses"
)

type benchmarkStream struct {
	remaining int
	current   responses.ResponseStreamEventUnion
}

func (s *benchmarkStream) Next() bool {
	if s.remaining < 0 {
		return false
	}
	if s.remaining == 0 {
		s.current = responses.ResponseStreamEventUnion{Type: "response.completed", Response: responses.Response{ID: "done", Status: "completed"}}
	} else {
		s.current = responses.ResponseStreamEventUnion{Type: "response.output_text.delta", Delta: "token "}
	}
	s.remaining--
	return true
}
func (s *benchmarkStream) Current() responses.ResponseStreamEventUnion { return s.current }
func (s *benchmarkStream) Err() error                                  { return nil }
func (s *benchmarkStream) Close() error                                { return nil }

func BenchmarkProviderStream(b *testing.B) {
	for _, deltas := range []int{32, 512, 4096} {
		b.Run(fmt.Sprintf("deltas_%d", deltas), func(b *testing.B) {
			client := &Client{CreateStream: func(context.Context, responses.ResponseNewParams) Stream { return &benchmarkStream{remaining: deltas} }}
			b.SetBytes(int64(deltas * len("token ")))
			b.ReportAllocs()
			for b.Loop() {
				result, err := client.Infer(b.Context(), Request{Model: "offline", Input: UserInput("hello")}, Options{}, Observer{Text: func(string, bool) {}})
				if err != nil || len(result.StreamedText) != deltas*6 {
					b.Fatalf("stream = %d bytes, %v", len(result.StreamedText), err)
				}
			}
		})
	}
}

func BenchmarkProviderReplayCompaction(b *testing.B) {
	for _, outputs := range []int{16, 64, 256} {
		for _, bounded := range []bool{false, true} {
			b.Run(fmt.Sprintf("outputs_%d/compact_%t", outputs, bounded), func(b *testing.B) {
				input := UserItems("original task")
				for i := range outputs {
					input = append(input, ToolOutput(fmt.Sprint(i), strings.Repeat("x", 4096)))
				}
				limit := outputs * 8192
				if bounded {
					limit = outputs * 1024
				}
				b.ReportAllocs()
				for b.Loop() {
					_, size, compacted, err := BoundInput(input, limit)
					if err != nil || size > limit || compacted != bounded {
						b.Fatalf("compaction: size=%d compacted=%t err=%v", size, compacted, err)
					}
				}
			})
		}
	}
}

func BenchmarkProviderJSONNormalization(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("bytes_%d", size), func(b *testing.B) {
			body := `{"id":"r","status":"completed","output":[],"extra":"` + strings.Repeat("<", size) + `"}`
			request := httptest.NewRequest(http.MethodPost, "https://offline.invalid/responses", nil)
			middleware := normalizeNonSSEStreamingResponseWithLimit(int64(len(body)))
			next := func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
			}
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				response, err := middleware(request, next)
				if err != nil {
					b.Fatal(err)
				}
				_, err = io.Copy(io.Discard, response.Body)
				closeErr := response.Body.Close()
				if err != nil || closeErr != nil {
					b.Fatalf("read: %v close: %v", err, closeErr)
				}
			}
		})
	}
}

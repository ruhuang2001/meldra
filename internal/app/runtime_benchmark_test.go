package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meldra/internal/provider"
)

// These measure Meldra runtime overhead with zero model/network latency. Each
// operation includes a fresh runner and a complete turn, including tool dispatch.
func BenchmarkAgentTurn(b *testing.B) {
	for _, calls := range []int{0, 1, 8, 32} {
		b.Run(fmt.Sprintf("tools_%d", calls), func(b *testing.B) {
			output := make([]provider.OutputItem, calls)
			for i := range output {
				output[i] = provider.OutputItem{Type: "function_call", CallID: fmt.Sprint(i), Name: "echo", Arguments: `{"value":"ok"}`}
			}
			tools := []ToolDefinition{{Name: "echo", Function: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}}
			b.ReportAllocs()
			for b.Loop() {
				requests := 0
				backend := inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
					requests++
					if calls > 0 && requests == 1 {
						return provider.Result{Response: &provider.Response{ID: "tools", Status: "completed", Output: output}}, nil
					}
					return provider.Result{Response: &provider.Response{ID: "done", Status: "completed", Text: "done"}}, nil
				})
				agent := NewAgent(backend, nil, tools)
				agent.output = io.Discard
				if err := agent.RunTurn(b.Context(), "benchmark"); err != nil {
					b.Fatal(err)
				}
				want := 1
				if calls > 0 {
					want = 2
				}
				if requests != want {
					b.Fatalf("requests = %d, want %d", requests, want)
				}
			}
			b.ReportMetric(float64(calls), "tools/turn")
		})
	}
}

func benchmarkWorkspace(b *testing.B, count int) *Workspace {
	b.Helper()
	root := b.TempDir()
	for i := range count {
		path := filepath.Join(root, fmt.Sprintf("file-%04d.txt", i))
		if err := os.WriteFile(path, []byte(strings.Repeat("ordinary source line\n", 32)+"unique needle\n"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	workspace, err := NewWorkspace(root, bufio.NewReader(strings.NewReader("")), io.Discard, true)
	if err != nil {
		b.Fatal(err)
	}
	workspace.SetContext(b.Context())
	return workspace
}

func BenchmarkWorkspaceDiscovery(b *testing.B) {
	for _, count := range []int{100, 1000} {
		for _, operation := range []string{"list", "search"} {
			b.Run(fmt.Sprintf("%s/files_%d", operation, count), func(b *testing.B) {
				workspace := benchmarkWorkspace(b, count)
				b.ReportAllocs()
				for b.Loop() {
					var result string
					var err error
					if operation == "list" {
						result, err = workspace.listFiles(json.RawMessage(`{}`))
					} else {
						// A no-match scan measures the full tree, not the first result cap.
						result, err = workspace.searchFiles(json.RawMessage(`{"query":"absent marker"}`))
					}
					if err != nil {
						b.Fatal(err)
					}
					if operation == "list" && !strings.Contains(result, "file-0000.txt") {
						b.Fatal("incomplete list")
					}
				}
				b.ReportMetric(float64(count), "files/op")
			})
		}
	}
}

func BenchmarkWorkspaceRead(b *testing.B) {
	for _, size := range []int{4 << 10, 256 << 10, 4 << 20} {
		b.Run(fmt.Sprintf("bytes_%d", size), func(b *testing.B) {
			workspace := benchmarkWorkspace(b, 0)
			data := strings.Repeat("source line\n", size/12+1)
			if err := os.WriteFile(filepath.Join(workspace.root, "source.txt"), []byte(data), 0o644); err != nil {
				b.Fatal(err)
			}
			// Read near the end to include line-skip cost at different file sizes.
			args, _ := json.Marshal(map[string]any{"path": "source.txt", "offset": max(1, strings.Count(data, "\n")-100), "limit": 100})
			b.ReportAllocs()
			for b.Loop() {
				if _, err := workspace.readFile(args); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkWorkspacePrepareChanges(b *testing.B) {
	for _, count := range []int{1, 8, 32} {
		b.Run(fmt.Sprintf("files_%d", count), func(b *testing.B) {
			workspace := benchmarkWorkspace(b, count)
			inputs := make([]changeInput, count)
			for i := range inputs {
				inputs[i] = changeInput{Path: fmt.Sprintf("file-%04d.txt", i), OldStr: "unique needle", NewStr: "updated value"}
			}
			b.ReportAllocs()
			for b.Loop() {
				changes, err := workspace.prepare(inputs)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := workspace.diff(changes, false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkWorkspaceDurableEditUndo(b *testing.B) {
	workspace := benchmarkWorkspace(b, 1)
	input := []changeInput{{Path: "file-0000.txt", OldStr: "unique needle", NewStr: "updated value"}}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := workspace.applyInputs(input); err != nil {
			b.Fatal(err)
		}
		if _, err := workspace.undo(json.RawMessage(`{}`)); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(2, "writes/op")
}

func BenchmarkSessionPersistence(b *testing.B) {
	for _, messages := range []int{10, 100} {
		for _, operation := range []string{"save", "load"} {
			b.Run(fmt.Sprintf("%s/messages_%d", operation, messages), func(b *testing.B) {
				paths, err := ConfigPathsForHome(filepath.Join(b.TempDir(), "home"))
				if err != nil {
					b.Fatal(err)
				}
				store := NewSessionStore(paths)
				session, err := store.New(b.TempDir())
				if err != nil {
					b.Fatal(err)
				}
				for range messages {
					session.appendMessage("user", strings.Repeat("context ", 128))
				}
				if err := store.Save(session); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				for b.Loop() {
					if operation == "save" {
						err = store.Save(session)
					} else {
						_, err = store.Load(session.ID)
					}
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

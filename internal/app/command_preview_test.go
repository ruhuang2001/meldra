package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"meldra/internal/tool"
)

func TestCommandPreviewHelper(t *testing.T) {
	index := slices.Index(os.Args, "--command-preview-helper")
	if index < 0 {
		return
	}
	data, err := os.ReadFile(os.Args[index+1])
	if err != nil {
		os.Exit(2)
	}
	if _, err := os.Stdout.Write(data); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestCommandPreviewPreservesUTF8AndRawArtifact(t *testing.T) {
	for _, test := range []struct {
		name      string
		raw       string
		want      string
		truncated bool
	}{
		{"short unicode", "中文🎉", "中文🎉", false},
		{"split Chinese", strings.Repeat("a", maxToolOutput-1) + "中tail", strings.Repeat("a", maxToolOutput-1), true},
		{"split emoji", strings.Repeat("a", maxToolOutput-2) + "🎉tail", strings.Repeat("a", maxToolOutput-2), true},
		{"complete boundary", strings.Repeat("a", maxToolOutput-3) + "中tail", strings.Repeat("a", maxToolOutput-3) + "中", true},
		{"invalid bytes", "a\xff\xfe中", "a�中", false},
		{"replacement exceeds budget", strings.Repeat("a", maxToolOutput-1) + "\xff", strings.Repeat("a", maxToolOutput-1), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			w, _ := testWorkspace(t, t.TempDir(), "", true)
			w.policy, _ = newRuntimePolicy(ModeBuild, PermissionInteractive)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if err := w.SetCommandExecutables([]string{executable}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(w.root, "output.bin"), []byte(test.raw), 0600); err != nil {
				t.Fatal(err)
			}
			registry, err := tool.New(w.ToolDefinitions())
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(map[string]any{"command": executable, "args": []string{"-test.run=^TestCommandPreviewHelper$", "--", "--command-preview-helper", "output.bin"}, "timeout": 10})
			if err != nil {
				t.Fatal(err)
			}
			result, err := registry.Invoke(t.Context(), "run_command", raw)
			if err != nil || result.Status != tool.Succeeded {
				t.Fatalf("command failed: status=%s err=%v", result.Status, err)
			}
			_, preview, ok := strings.Cut(result.Output, "\nstatus: 0\n")
			if !ok {
				t.Fatalf("missing command status: %.200q", result.Output)
			}
			if test.truncated {
				preview, ok = strings.CutSuffix(preview, outputTruncationSuffix)
				if !ok {
					t.Fatal("truncated preview lacks marker")
				}
			}
			if !utf8.ValidString(preview) || len(preview) > maxToolOutput || preview != test.want {
				t.Fatalf("preview valid=%t bytes=%d suffix=%.30q", utf8.ValidString(preview), len(preview), preview[max(0, len(preview)-30):])
			}
			if result.Truncated != test.truncated || len(result.Attachments) != 1 || string(result.Attachments[0].Content) != test.raw {
				t.Fatalf("raw evidence or truncation changed: truncated=%t attachments=%d", result.Truncated, len(result.Attachments))
			}
		})
	}
}

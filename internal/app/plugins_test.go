package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"meldra/internal/tool"
)

const testPluginStdioServerScript = `#!/usr/bin/env python3
import json, os, sys

def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    req = json.loads(line)
    method = req.get("method")
    req_id = req.get("id")
    if method == "initialize":
        send({
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {
                "protocolVersion": req.get("params", {}).get("protocolVersion", "2025-11-25"),
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "plugin-stdio-fixture", "version": "1.0.0"}
            }
        })
    elif method == "notifications/initialized":
        continue
    elif method == "tools/list":
        send({
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {
                "tools": [{
                    "name": "env_probe",
                    "description": "Report subprocess cwd, args, env, and write marker to PLUGIN_DATA.",
                    "inputSchema": {"type": "object", "properties": {}}
                }]
            }
        })
    elif method == "tools/call":
        data_dir = os.environ.get("PLUGIN_DATA", "")
        marker = ""
        if data_dir:
            marker_path = os.path.join(data_dir, "probe-marker.txt")
            with open(marker_path, "w", encoding="utf-8") as f:
                f.write("persisted")
            marker = marker_path
        payload = {
            "cwd": os.getcwd(),
            "argv": sys.argv[1:],
            "plugin_root": os.environ.get("PLUGIN_ROOT", ""),
            "plugin_data": data_dir,
            "custom": os.environ.get("CUSTOM_VAR", ""),
            "config_path": os.environ.get("CONFIG_PATH", ""),
            "literal_unknown": os.environ.get("LITERAL_UNKNOWN", ""),
            "path_env": os.environ.get("PATH", ""),
            "has_openai_key": "OPENAI_API_KEY" in os.environ,
            "marker": marker,
        }
        send({
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {
                "content": [{"type": "text", "text": json.dumps(payload)}]
            }
        })
    elif req_id is not None:
        send({"jsonrpc": "2.0", "id": req_id, "result": {}})
`

func writeFileMode(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func testPluginPaths(t *testing.T) ConfigPaths {
	t.Helper()
	paths, err := ConfigPathsForHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := InitializeConfig(paths); err != nil {
		t.Fatal(err)
	}
	t.Setenv(MeldraHomeEnv, paths.Home)
	return paths
}

func TestPluginManifestValidationAndConformance(t *testing.T) {
	paths := testPluginPaths(t)

	t.Run("minimal valid and metadata non-format strictness", func(t *testing.T) {
		dir := t.TempDir()
		manifest := `{
			"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
			"name": "acme.tools-1",
			"version": "not-semver-at-all",
			"description": "Portable plugin",
			"author": {"name": "Alice", "email": "not-an-email", "url": "not-a-url"},
			"homepage": "not a url",
			"repository": "also not a url",
			"license": "Proprietary Custom",
			"keywords": ["alpha", "beta"],
			"extensions": {
				"com.other.client": {"arbitrary": [1, 2, true]},
				"org.example.unvalidated": "ignored-without-validation"
			},
			"unknownTopField": {"nested": 1},
			"anotherUnknown": 42
		}`
		writeFileMode(t, filepath.Join(dir, "plugin.json"), manifest, 0o600)
		writeFileMode(t, filepath.Join(dir, "skills", "greet", "SKILL.md"), "---\nname: greet\ndescription: Say hello\n---\nHello!\n", 0o600)

		loaded, err := loadPluginFromDir(t.Context(), paths, dir, nil)
		if err != nil {
			t.Fatalf("loadPluginFromDir failed: %v", err)
		}
		if loaded.Manifest.Name != "acme.tools-1" || loaded.Manifest.Version != "not-semver-at-all" {
			t.Fatalf("manifest=%+v", loaded.Manifest)
		}
		if len(loaded.Skills) != 1 || loaded.Skills[0].Name != "greet" {
			t.Fatalf("skills=%+v", loaded.Skills)
		}
		if len(loaded.Manifest.Extensions) != 2 || loaded.Manifest.Extensions[0] != "com.other.client" {
			t.Fatalf("extensions=%v", loaded.Manifest.Extensions)
		}
		if len(loaded.Warnings) != 2 {
			t.Fatalf("expected 2 unknown-field warnings, got %v", loaded.Warnings)
		}
	})

	t.Run("non-object extensions is non-fatal", func(t *testing.T) {
		dir := t.TempDir()
		writeFileMode(t, filepath.Join(dir, "plugin.json"), `{
			"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
			"name": "a",
			"extensions": ["not", "an", "object"]
		}`, 0o600)
		writeFileMode(t, filepath.Join(dir, "skills", "ok", "SKILL.md"), "---\nname: ok\ndescription: works\n---\nBody\n", 0o600)

		loaded, err := loadPluginFromDir(t.Context(), paths, dir, nil)
		if err != nil {
			t.Fatalf("expected non-object extensions to be non-fatal: %v", err)
		}
		if len(loaded.Skills) != 1 || len(loaded.Warnings) != 1 || !strings.Contains(loaded.Warnings[0], "extensions") {
			t.Fatalf("loaded=%+v", loaded)
		}
	})

	fatalCases := []struct {
		name     string
		manifest string
	}{
		{"missing schema", `{"name":"demo"}`},
		{"unsupported schema", `{"$schema":"https://agent-plugins.org/schemas/2.0.0/plugin.schema.json","name":"demo"}`},
		{"null schema", `{"$schema":null,"name":"demo"}`},
		{"missing name", `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"}`},
		{"uppercase name", `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"My-Plugin"}`},
		{"leading hyphen", `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"-start"}`},
		{"trailing period", `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"end."}`},
		{"double hyphen", `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"has--double"}`},
		{"double period", `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"too..dots"}`},
		{"too long name", fmt.Sprintf(`{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"%s"}`, strings.Repeat("a", 65))},
		{"null version", `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"demo","version":null}`},
		{"wrong keywords type", `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"demo","keywords":["ok",123]}`},
		{"author unknown field", `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"demo","author":{"name":"a","extra":"x"}}`},
		{"author non-object", `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"demo","author":"alice"}`},
		{"invalid UTF-8", fmt.Sprintf(`{"$schema":%q,"name":"demo","description":"%s"}`, agentPluginsManifestSchema, "\xff")},
		{"trailing json", `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"demo"}{}`},
		{"not object", `[]`},
	}
	for _, tc := range fatalCases {
		t.Run("fatal/"+tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFileMode(t, filepath.Join(dir, "plugin.json"), tc.manifest, 0o600)
			if _, err := loadPluginFromDir(t.Context(), paths, dir, nil); err == nil {
				t.Fatalf("expected fatal error for %s", tc.name)
			}
		})
	}
}

func TestPluginContainmentAndFailureIsolation(t *testing.T) {
	paths := testPluginPaths(t)

	t.Run("escaping plugin.json rejects entire plugin", func(t *testing.T) {
		outside := t.TempDir()
		outsideManifest := filepath.Join(outside, "plugin.json")
		writeFileMode(t, outsideManifest, `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"escape"}`, 0o600)
		dir := t.TempDir()
		if err := os.Symlink(outsideManifest, filepath.Join(dir, "plugin.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := loadPluginFromDir(t.Context(), paths, dir, nil); err == nil {
			t.Fatal("expected escaping plugin.json to be rejected")
		}
	})

	t.Run("invalid skills location does not disable valid mcp.json", func(t *testing.T) {
		dir := t.TempDir()
		writeFileMode(t, filepath.Join(dir, "plugin.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"mixed-skills-file"}`, 0o600)
		writeFileMode(t, filepath.Join(dir, "skills"), "not a directory", 0o600)
		writeFileMode(t, filepath.Join(dir, "mcp.json"), `{
			"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
			"mcpServers": {
				"remote": {"type": "streamable-http", "url": "https://mcp.example.com/mcp"}
			}
		}`, 0o600)

		loaded, err := loadPluginFromDir(t.Context(), paths, dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !loaded.SkillsDisabled || loaded.MCPDisabled || len(loaded.MCPServers) != 1 {
			t.Fatalf("loaded=%+v", loaded)
		}
	})

	t.Run("invalid mcp.json top-level does not disable valid skills", func(t *testing.T) {
		dir := t.TempDir()
		writeFileMode(t, filepath.Join(dir, "plugin.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"mixed-bad-mcp"}`, 0o600)
		writeFileMode(t, filepath.Join(dir, "skills", "good", "SKILL.md"), "---\nname: good\ndescription: valid skill\n---\nBody\n", 0o600)
		writeFileMode(t, filepath.Join(dir, "mcp.json"), `{
			"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
			"mcpServers": {},
			"unexpectedTop": true
		}`, 0o600)

		loaded, err := loadPluginFromDir(t.Context(), paths, dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !loaded.MCPDisabled || loaded.SkillsDisabled || len(loaded.Skills) != 1 || loaded.Skills[0].Name != "good" {
			t.Fatalf("loaded=%+v", loaded)
		}
	})

	t.Run("skill isolation, no recursion, internal symlink allowed, escaping symlink rejected", func(t *testing.T) {
		outside := t.TempDir()
		outsideSkill := filepath.Join(outside, "SKILL.md")
		writeFileMode(t, outsideSkill, "---\nname: escaped\ndescription: bad\n---\nSecret\n", 0o600)
		outsideRef := filepath.Join(outside, "secret.txt")
		writeFileMode(t, outsideRef, "TOP SECRET", 0o600)

		dir := t.TempDir()
		writeFileMode(t, filepath.Join(dir, "plugin.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"skill-bounds"}`, 0o600)
		writeFileMode(t, filepath.Join(dir, "shared", "internal.md"), "---\nname: linked\ndescription: internal link\n---\nLinked body\n", 0o600)
		writeFileMode(t, filepath.Join(dir, "shared", "guide.txt"), "INTERNAL GUIDE", 0o600)
		writeFileMode(t, filepath.Join(dir, "skills", "good", "SKILL.md"), "---\nname: good\ndescription: valid\n---\nGood body\n", 0o600)
		writeFileMode(t, filepath.Join(dir, "skills", "bad-name", "SKILL.md"), "---\nname: wrong\ndescription: invalid\n---\nBad\n", 0o600)
		writeFileMode(t, filepath.Join(dir, "skills", "nested", "deep", "SKILL.md"), "---\nname: deep\ndescription: should not recurse\n---\nDeep\n", 0o600)
		if err := os.MkdirAll(filepath.Join(dir, "skills", "linked", "references"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..", "..", "shared", "internal.md"), filepath.Join(dir, "skills", "linked", "SKILL.md")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..", "..", "..", "shared", "guide.txt"), filepath.Join(dir, "skills", "linked", "references", "guide.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideRef, filepath.Join(dir, "skills", "linked", "references", "escape.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "skills", "escaped"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideSkill, filepath.Join(dir, "skills", "escaped", "SKILL.md")); err != nil {
			t.Fatal(err)
		}

		var out bytes.Buffer
		if err := runPluginsCommand(t.Context(), []string{"enable", dir}, &out); err != nil {
			t.Fatalf("enable failed: %v", err)
		}
		w, _ := testWorkspace(t, t.TempDir(), "", false)
		if err := protectRuntimePaths(w, paths); err != nil {
			t.Fatal(err)
		}
		catalog, err := discoverSkills(t.Context(), w, paths)
		if err != nil {
			t.Fatal(err)
		}
		if len(catalog.Skills) != 2 || catalog.Skills[0].Name != "good" || catalog.Skills[1].Name != "linked" {
			t.Fatalf("skills=%+v warnings=%v", catalog.Skills, catalog.Warnings)
		}
		if catalog.Skills[0].Plugin != "skill-bounds" {
			t.Fatalf("expected plugin attribution, got %+v", catalog.Skills[0])
		}
		got, err := catalog.read(t.Context(), toolInput(t, map[string]any{"name": "linked", "path": "references/guide.txt"}))
		if err != nil || !strings.Contains(got, "INTERNAL GUIDE") {
			t.Fatalf("internal symlink resource failed: got=%q err=%v", got, err)
		}
		if _, err := catalog.read(t.Context(), toolInput(t, map[string]any{"name": "linked", "path": "references/escape.txt"})); err == nil {
			t.Fatal("escaping symlink resource was unexpectedly allowed")
		}
		if err := os.Link(outsideRef, filepath.Join(dir, "skills", "linked", "references", "alias.txt")); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.read(t.Context(), toolInput(t, map[string]any{"name": "linked", "path": "references/alias.txt"})); err == nil {
			t.Fatal("hard-linked resource was unexpectedly allowed")
		}
		if _, err := catalog.read(t.Context(), toolInput(t, map[string]any{"name": "linked", "path": "../good/SKILL.md"})); err == nil {
			t.Fatal("relative traversal resource was unexpectedly allowed")
		}
	})
}

func TestPluginMCPConfigTwoStageValidationAndExpansion(t *testing.T) {
	paths := testPluginPaths(t)
	dir := t.TempDir()
	outsideBin := filepath.Join(t.TempDir(), "outside-bin")
	writeFileMode(t, outsideBin, "#!/bin/sh\n", 0o700)

	writeFileMode(t, filepath.Join(dir, "plugin.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"mcp-matrix"}`, 0o600)
	writeFileMode(t, filepath.Join(dir, "bin", "valid-server"), "#!/bin/sh\n", 0o700)
	writeFileMode(t, filepath.Join(dir, "subdir", "keep"), "ok", 0o600)
	if err := os.Symlink(outsideBin, filepath.Join(dir, "bin", "escaped-server")); err != nil {
		t.Fatal(err)
	}
	mcpJSON := `{
		"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
		"mcpServers": {
			"good-stdio": {
				"type": "stdio",
				"command": "./bin/valid-server",
				"args": ["--root", "${PLUGIN_ROOT}/cfg", "--data", "${PLUGIN_DATA}/db", "--keep", "${UNKNOWN}", "../opaque-arg"],
				"env": {
					"A": "${PLUGIN_ROOT}/a",
					"B": "${PLUGIN_DATA}/b",
					"LITERAL": "${HOME}",
					"OPAQUE": "../outside"
				},
				"cwd": "./subdir"
			},
			"good-data-cwd": {
				"type": "stdio",
				"command": "python3",
				"cwd": "${PLUGIN_DATA}"
			},
			"good-http": {
				"type": "streamable-http",
				"url": "https://mcp.example.com/mcp?tenant=${PLUGIN_ROOT}",
				"headers": {"X-Tenant": "${PLUGIN_ROOT}", "X-Mode": "public"}
			},
			"good-sse": {
				"type": "sse",
				"url": "http://127.0.0.1:9999/sse"
			},
			"bad-escape-cmd": {
				"type": "stdio",
				"command": "./bin/escaped-server"
			},
			"bad-parent-cmd": {
				"type": "stdio",
				"command": "../bin/server"
			},
			"bad-placeholder-cmd": {
				"type": "stdio",
				"command": "${PLUGIN_ROOT}/bin/valid-server"
			},
			"bad-abs-cmd": {
				"type": "stdio",
				"command": "/bin/sh"
			},
			"bad-reserved-env": {
				"type": "stdio",
				"command": "python3",
				"env": {"PLUGIN_ROOT": "/override"}
			},
			"bad-reserved-data": {
				"type": "stdio",
				"command": "python3",
				"env": {"PLUGIN_DATA": "/override"}
			},
			"bad-cwd-bare": {
				"type": "stdio",
				"command": "python3",
				"cwd": "subdir"
			},
			"bad-cwd-escape": {
				"type": "stdio",
				"command": "python3",
				"cwd": "${PLUGIN_ROOT}/../outside"
			},
			"bad-cwd-data-escape": {
				"type": "stdio",
				"command": "python3",
				"cwd": "${PLUGIN_DATA}/../outside"
			},
			"bad-http-insecure": {
				"type": "streamable-http",
				"url": "http://example.com/mcp"
			},
			"bad-http-userinfo": {
				"type": "streamable-http",
				"url": "https://user:pass@example.com/mcp"
			},
			"bad-http-fragment": {
				"type": "streamable-http",
				"url": "https://example.com/mcp#frag"
			},
			"bad-http-dup-header": {
				"type": "streamable-http",
				"url": "https://example.com/mcp",
				"headers": {"X-Tenant": "a", "x-tenant": "b"}
			},
			"bad-unknown-type": {
				"type": "websocket",
				"url": "wss://example.com/ws"
			},
			"bad-cross-field": {
				"type": "stdio",
				"command": "python3",
				"url": "https://example.com/mcp"
			}
		}
	}`
	writeFileMode(t, filepath.Join(dir, "mcp.json"), mcpJSON, 0o600)

	loaded, err := loadPluginFromDir(t.Context(), paths, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.MCPDisabled {
		t.Fatalf("expected per-server isolation, not full MCP disable: %v", loaded.Warnings)
	}
	if len(loaded.MCPServers) != 4 {
		t.Fatalf("expected 4 valid servers, got %d: %+v warnings=%v", len(loaded.MCPServers), loaded.MCPServers, loaded.Warnings)
	}
	if len(loaded.Warnings) != 15 {
		t.Fatalf("expected 15 skipped-server warnings, got %d: %v", len(loaded.Warnings), loaded.Warnings)
	}
	byName := map[string]pluginMCPServer{}
	for _, s := range loaded.MCPServers {
		byName[s.Name] = s
	}
	stdio := byName["good-stdio"]
	if stdio.Command != filepath.Join(loaded.Root, "bin", "valid-server") {
		t.Fatalf("resolved command = %q", stdio.Command)
	}
	if stdio.CWD != filepath.Join(loaded.Root, "subdir") {
		t.Fatalf("cwd = %q", stdio.CWD)
	}
	if stdio.Args[1] != loaded.Root+"/cfg" || stdio.Args[3] != loaded.DataDir+"/db" || stdio.Args[5] != "${UNKNOWN}" || stdio.Args[6] != "../opaque-arg" {
		t.Fatalf("args = %v", stdio.Args)
	}
	if stdio.Env["A"] != loaded.Root+"/a" || stdio.Env["B"] != loaded.DataDir+"/b" || stdio.Env["LITERAL"] != "${HOME}" || stdio.Env["OPAQUE"] != "../outside" {
		t.Fatalf("env = %v", stdio.Env)
	}
	httpServer := byName["good-http"]
	if httpServer.URL != "https://mcp.example.com/mcp?tenant=${PLUGIN_ROOT}" || httpServer.Headers["X-Tenant"] != "${PLUGIN_ROOT}" {
		t.Fatalf("url/headers must NOT expand placeholders: %+v", httpServer)
	}
	if byName["good-data-cwd"].CWD != loaded.DataDir {
		t.Fatalf("data cwd = %q, want %q", byName["good-data-cwd"].CWD, loaded.DataDir)
	}
}

func TestPluginNonRecursivePlaceholderExpansion(t *testing.T) {
	got := expandPluginPlaceholders("${PLUGIN_ROOT}/file", "/tmp/${PLUGIN_DATA}", "/var/data")
	if got != "/tmp/${PLUGIN_DATA}/file" {
		t.Fatalf("placeholder expansion was recursive: got %q", got)
	}
}

func TestPluginSkillFrontmatterAndFilesystemResolution(t *testing.T) {
	paths := testPluginPaths(t)
	dir := t.TempDir()
	writeFileMode(t, filepath.Join(dir, "plugin.json"), fmt.Sprintf(`{"$schema":%q,"name":"portable"}`, agentPluginsManifestSchema), 0o600)
	for name, extra := range map[string]string{"指南": "metadata: {lang: zh}", "bad": "metadata: {version: 1}", "too-long": "compatibility: " + strings.Repeat("a", 501)} {
		writeFileMode(t, filepath.Join(dir, "skills", name, "SKILL.md"), "---\nname: "+name+"\ndescription: guide\n"+extra+"\n---\nBody\n", 0o600)
	}
	if err := os.MkdirAll(filepath.Join(dir, "tools", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tools/sub", filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	writeFileMode(t, filepath.Join(dir, "mcp.json"), fmt.Sprintf(`{"$schema":%q,"mcpServers":{"run":{"type":"stdio","command":"./alias/../server","cwd":"${PLUGIN_ROOT}/alias/.."}}}`, agentPluginsMCPSchema), 0o600)
	loaded, err := loadPluginFromDir(t.Context(), paths, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Warnings) != 2 || len(loaded.Skills) != 1 || loaded.Skills[0].Name != "指南" {
		t.Fatalf("skills=%v warnings=%v", loaded.Skills, loaded.Warnings)
	}
	if len(loaded.MCPServers) != 1 || loaded.MCPServers[0].CWD != filepath.Join(loaded.Root, "tools") || loaded.MCPServers[0].Command != filepath.Join(loaded.Root, "tools", "server") {
		t.Fatalf("paths no longer follow filesystem resolution: %+v", loaded.MCPServers)
	}
}

func TestPluginEndToEndStdioRuntimeApprovalAndPersistence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX script fixture")
	}
	paths := testPluginPaths(t)
	t.Setenv("OPENAI_API_KEY", "sk-secret-must-not-leak")

	dir := t.TempDir()
	writeFileMode(t, filepath.Join(dir, "plugin.json"), `{
		"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
		"name": "stdio.fixture",
		"version": "1.0.0"
	}`, 0o600)
	scriptPath := filepath.Join(dir, "bin", "server")
	writeFileMode(t, scriptPath, testPluginStdioServerScript, 0o700)
	writeFileMode(t, filepath.Join(dir, "mcp.json"), `{
		"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
		"mcpServers": {
			"local": {
				"type": "stdio",
				"command": "./bin/server",
				"args": ["--root=${PLUGIN_ROOT}", "--data=${PLUGIN_DATA}", "--keep=${OTHER}"],
				"env": {
					"CUSTOM_VAR": "from-plugin",
					"CONFIG_PATH": "${PLUGIN_ROOT}/config.json",
					"LITERAL_UNKNOWN": "${HOME}"
				}
			},
			"broken-sibling": {
				"type": "stdio",
				"command": "./bin/does-not-exist"
			}
		}
	}`, 0o600)

	var inspectOut bytes.Buffer
	if err := runPluginsCommand(t.Context(), []string{"inspect", dir, "--json"}, &inspectOut); err != nil {
		t.Fatalf("inspect failed: %v", err)
	}
	var inspected loadedPlugin
	if err := json.Unmarshal(inspectOut.Bytes(), &inspected); err != nil {
		t.Fatalf("inspect json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.Home, pluginsRegistryFile)); !os.IsNotExist(err) {
		t.Fatal("inspect must not write plugins.json")
	}
	if _, err := os.Stat(inspected.DataDir); !os.IsNotExist(err) {
		t.Fatal("inspect must not create PLUGIN_DATA")
	}

	var out bytes.Buffer
	if err := runPluginsCommand(t.Context(), []string{"enable", dir}, &out); err != nil {
		t.Fatalf("enable failed: %v", err)
	}
	if !strings.Contains(out.String(), "not-approved") {
		t.Fatalf("enable output = %q", out.String())
	}
	w, _ := testWorkspace(t, t.TempDir(), "", true)
	connections, err := connectMCP(t.Context(), paths, w, "")
	if err != nil {
		t.Fatal(err)
	}
	defer connections.close()
	if len(connections.tools) != 0 {
		t.Fatalf("unapproved plugin MCP server started before allow-mcp: %+v", connections.tools)
	}

	out.Reset()
	if err := runPluginsCommand(t.Context(), []string{"allow-mcp", "stdio.fixture"}, &out); err != nil {
		t.Fatalf("allow-mcp failed: %v", err)
	}

	approvedConn, err := connectMCP(t.Context(), paths, w, "")
	if err != nil {
		t.Fatal(err)
	}
	defer approvedConn.close()
	if len(approvedConn.tools) != 1 {
		t.Fatalf("expected 1 tool from valid server despite broken sibling, got %d; warnings=%v", len(approvedConn.tools), approvedConn.warnings)
	}
	if len(approvedConn.warnings) == 0 {
		t.Fatal("expected warning for broken-sibling server")
	}
	reg, err := tool.New(approvedConn.tools)
	if err != nil {
		t.Fatal(err)
	}
	res, err := reg.Invoke(t.Context(), approvedConn.tools[0].Name, json.RawMessage(`{}`))
	if err != nil || res.Status != tool.Succeeded {
		t.Fatalf("invoke failed: res=%+v err=%v", res, err)
	}
	var envelope struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(res.Output), &envelope); err != nil || len(envelope.Content) == 0 {
		t.Fatalf("unmarshal envelope %q: %v", res.Output, err)
	}
	var probe struct {
		CWD            string   `json:"cwd"`
		Argv           []string `json:"argv"`
		PluginRoot     string   `json:"plugin_root"`
		PluginData     string   `json:"plugin_data"`
		Custom         string   `json:"custom"`
		ConfigPath     string   `json:"config_path"`
		LiteralUnknown string   `json:"literal_unknown"`
		HasOpenAIKey   bool     `json:"has_openai_key"`
		Marker         string   `json:"marker"`
	}
	if err := json.Unmarshal([]byte(envelope.Content[0].Text), &probe); err != nil {
		t.Fatalf("unmarshal probe %q: %v", envelope.Content[0].Text, err)
	}
	if probe.CWD != inspected.Root {
		t.Fatalf("default cwd = %q, want plugin root %q", probe.CWD, inspected.Root)
	}
	if probe.PluginRoot != inspected.Root || probe.PluginData != inspected.DataDir {
		t.Fatalf("plugin env mismatch: %+v", probe)
	}
	if probe.Custom != "from-plugin" || probe.ConfigPath != inspected.Root+"/config.json" || probe.LiteralUnknown != "${HOME}" {
		t.Fatalf("env expansion mismatch: %+v", probe)
	}
	if len(probe.Argv) != 3 || probe.Argv[0] != "--root="+inspected.Root || probe.Argv[1] != "--data="+inspected.DataDir || probe.Argv[2] != "--keep=${OTHER}" {
		t.Fatalf("args mismatch: %v", probe.Argv)
	}
	if probe.HasOpenAIKey {
		t.Fatal("OPENAI_API_KEY leaked to plugin stdio subprocess")
	}
	if _, err := os.Stat(probe.Marker); err != nil {
		t.Fatalf("PLUGIN_DATA marker missing: %v", err)
	}

	cached, _, err := loadAllMCPConfigs(t.Context(), paths)
	if err != nil {
		t.Fatal(err)
	}
	id := pluginMCPServerID("stdio.fixture", "local")
	writeFileMode(t, scriptPath, testPluginStdioServerScript+"\n# modified\n", 0o700)
	if session, _, err := connectMCPServer(t.Context(), paths, id, cached[id], w); err == nil {
		session.Close()
		t.Fatal("a cached config started a changed executable")
	}
	tamperedConn, err := connectMCP(t.Context(), paths, w, "")
	if err != nil {
		t.Fatal(err)
	}
	defer tamperedConn.close()
	if len(tamperedConn.tools) != 0 {
		t.Fatal("modified plugin executable was allowed to run without re-approval")
	}
	if !strings.Contains(strings.Join(tamperedConn.warnings, "\n"), "changed since approval") {
		t.Fatalf("expected stale-approval warning, got %v", tamperedConn.warnings)
	}

	out.Reset()
	if err := runPluginsCommand(t.Context(), []string{"disable", "stdio.fixture"}, &out); err != nil {
		t.Fatalf("disable failed: %v", err)
	}
	if _, err := os.Stat(probe.Marker); err != nil {
		t.Fatalf("disable without --purge-data deleted PLUGIN_DATA: %v", err)
	}

	out.Reset()
	if err := runPluginsCommand(t.Context(), []string{"enable", dir, "--allow-mcp"}, &out); err != nil {
		t.Fatalf("re-enable failed: %v", err)
	}
	out.Reset()
	if err := runPluginsCommand(t.Context(), []string{"disable", "stdio.fixture", "--purge-data"}, &out); err != nil {
		t.Fatalf("disable --purge-data failed: %v", err)
	}
	if _, err := os.Stat(inspected.DataDir); !os.IsNotExist(err) {
		t.Fatalf("disable --purge-data did not remove PLUGIN_DATA")
	}
}

func TestPluginRemoteTransportsStreamableHTTPAndSSEAndHeaderOriginGuards(t *testing.T) {
	paths := testPluginPaths(t)

	var mu sync.Mutex
	var streamHeaders http.Header
	streamServer := mcp.NewServer(&mcp.Implementation{Name: "stream-fixture", Version: "1"}, nil)
	streamServer.AddTool(&mcp.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "stream-pong"}}}, nil
	})
	streamHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return streamServer }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	streamEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		streamHeaders = r.Header.Clone()
		mu.Unlock()
		streamHandler.ServeHTTP(w, r)
	}))
	defer streamEndpoint.Close()

	var sseHeaders http.Header
	sseServer := mcp.NewServer(&mcp.Implementation{Name: "sse-fixture", Version: "1"}, nil)
	sseServer.AddTool(&mcp.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "sse-pong"}}}, nil
	})
	sseHandler := mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return sseServer }, nil)
	sseEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if sseHeaders == nil {
			sseHeaders = r.Header.Clone()
		}
		mu.Unlock()
		sseHandler.ServeHTTP(w, r)
	}))
	defer sseEndpoint.Close()

	var crossOriginReceivedHeader string
	crossOriginTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		crossOriginReceivedHeader = r.Header.Get("X-Tenant")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer crossOriginTarget.Close()

	redirectEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, crossOriginTarget.URL+"/mcp", http.StatusFound)
	}))
	defer redirectEndpoint.Close()

	dir := t.TempDir()
	writeFileMode(t, filepath.Join(dir, "plugin.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"remote-net"}`, 0o600)
	mcpJSON := fmt.Sprintf(`{
		"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
		"mcpServers": {
			"stream": {
				"type": "streamable-http",
				"url": %q,
				"headers": {
					"X-Tenant": "acme-public",
					"Accept": "text/should-not-override"
				}
			},
			"legacy": {
				"type": "sse",
				"url": %q,
				"headers": {"X-SSE-Tag": "v1"}
			},
			"redirected": {
				"type": "streamable-http",
				"url": %q,
				"headers": {"X-Tenant": "must-not-forward"}
			}
		}
	}`, streamEndpoint.URL, sseEndpoint.URL, redirectEndpoint.URL)
	writeFileMode(t, filepath.Join(dir, "mcp.json"), mcpJSON, 0o600)

	var out bytes.Buffer
	if err := runPluginsCommand(t.Context(), []string{"enable", dir, "--allow-mcp"}, &out); err != nil {
		t.Fatalf("enable --allow-mcp failed: %v", err)
	}
	w, _ := testWorkspace(t, t.TempDir(), "", true)
	connections, err := connectMCP(t.Context(), paths, w, "")
	if err != nil {
		t.Fatal(err)
	}
	defer connections.close()
	if len(connections.tools) != 2 {
		t.Fatalf("expected 2 tools (stream + sse), got %d; warnings=%v", len(connections.tools), connections.warnings)
	}
	reg, err := tool.New(connections.tools)
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]bool{}
	for _, def := range connections.tools {
		res, err := reg.Invoke(t.Context(), def.Name, json.RawMessage(`{}`))
		if err != nil || res.Status != tool.Succeeded {
			t.Fatalf("tool %s failed: %+v %v", def.Name, res, err)
		}
		var envelope struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(res.Output), &envelope); err != nil || len(envelope.Content) == 0 {
			t.Fatalf("tool %s invalid output %q: %v", def.Name, res.Output, err)
		}
		results[envelope.Content[0].Text] = true
	}
	if !results["stream-pong"] || !results["sse-pong"] {
		t.Fatalf("results=%v", results)
	}
	mu.Lock()
	defer mu.Unlock()
	if streamHeaders.Get("X-Tenant") != "acme-public" {
		t.Fatalf("missing configured header on streamable-http: %v", streamHeaders)
	}
	if strings.Contains(streamHeaders.Get("Accept"), "should-not-override") {
		t.Fatalf("configured header overrode client-generated Accept header: %v", streamHeaders.Values("Accept"))
	}
	if sseHeaders.Get("X-Sse-Tag") != "v1" {
		t.Fatalf("missing configured header on sse: %v", sseHeaders)
	}
	if crossOriginReceivedHeader != "" {
		t.Fatalf("redirect forwarded configured header to another origin: %q", crossOriginReceivedHeader)
	}
}

func TestPluginHTTPTransportStripsHeadersOnCrossOriginRequest(t *testing.T) {
	var seenHeader string
	otherOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeader = r.Header.Get("X-Plugin-Header")
		w.WriteHeader(http.StatusOK)
	}))
	defer otherOrigin.Close()

	origURL, err := url.Parse("http://127.0.0.1:1/sse")
	if err != nil {
		t.Fatal(err)
	}
	transport := mcpHTTPTransport{
		originURL: origURL,
		headers:   map[string]string{"X-Plugin-Header": "public-value"},
		streamSSE: true,
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, otherOrigin.URL+"/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if seenHeader != "" {
		t.Fatalf("cross-origin request leaked configured header: %q", seenHeader)
	}
}

func TestPluginCLIListRevokeAndProtectedHomeRejection(t *testing.T) {
	paths := testPluginPaths(t)

	var out bytes.Buffer
	if err := runPluginsCommand(t.Context(), nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No plugins configured.") {
		t.Fatalf("empty list output = %q", out.String())
	}

	writeFileMode(t, filepath.Join(paths.Home, "plugin.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"bad-home"}`, 0o600)
	if err := runPluginsCommand(t.Context(), []string{"enable", paths.Home}, &out); err == nil {
		t.Fatal("expected registering MELDRA_HOME itself as a plugin to fail")
	}
	ancestor := filepath.Dir(paths.Home)
	writeFileMode(t, filepath.Join(ancestor, "plugin.json"), fmt.Sprintf(`{"$schema":%q,"name":"overlap"}`, agentPluginsManifestSchema), 0o600)
	if err := runPluginsCommand(t.Context(), []string{"enable", ancestor}, &out); err == nil {
		t.Fatal("registered a plugin containing private state")
	}

	allowedDir := filepath.Join(paths.Home, "plugins", "local-pack")
	writeFileMode(t, filepath.Join(allowedDir, "plugin.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"local-pack","version":"0.1.0"}`, 0o600)
	writeFileMode(t, filepath.Join(allowedDir, "skills", "hello", "SKILL.md"), "---\nname: hello\ndescription: say hi\n---\nhi\n", 0o600)
	writeFileMode(t, filepath.Join(allowedDir, "mcp.json"), `{
		"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
		"mcpServers": {"api": {"type": "streamable-http", "url": "https://mcp.example.com/mcp"}}
	}`, 0o600)

	out.Reset()
	if err := runPluginsCommand(t.Context(), []string{"enable", allowedDir, "--allow-mcp"}, &out); err != nil {
		t.Fatalf("expected plugin under MELDRA_HOME/plugins to succeed: %v", err)
	}

	out.Reset()
	if err := runPluginsCommand(t.Context(), []string{"list", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var list pluginListOutput
	if err := json.Unmarshal(out.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Plugins) != 1 || list.Plugins[0].MCPStatus != "approved" || list.Plugins[0].SkillsCount != 1 {
		t.Fatalf("list=%+v", list)
	}

	out.Reset()
	if err := runPluginsCommand(t.Context(), []string{"revoke-mcp", "local-pack"}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runPluginsCommand(t.Context(), []string{"list", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Plugins[0].MCPStatus != "not-approved" {
		t.Fatalf("expected not-approved after revoke-mcp, got %+v", list.Plugins[0])
	}

	w, _ := testWorkspace(t, t.TempDir(), "", false)
	if err := protectRuntimePaths(w, paths); err != nil {
		t.Fatal(err)
	}
	catalog, err := discoverSkills(t.Context(), w, paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 1 || catalog.Skills[0].Name != "hello" {
		t.Fatalf("expected skill under MELDRA_HOME/plugins to be readable: %+v warnings=%v", catalog.Skills, catalog.Warnings)
	}
	got, err := catalog.read(t.Context(), toolInput(t, map[string]any{"name": "hello", "path": nil}))
	if err != nil || !strings.Contains(got, "hi") {
		t.Fatalf("read_skill under MELDRA_HOME/plugins failed: got=%q err=%v", got, err)
	}
}

func TestPluginCatalogCLIAndToolNames(t *testing.T) {
	testPluginPaths(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "catalog-fixture", Version: "1"}, nil)
	server.AddResource(&mcp.Resource{URI: "plugin://doc/1", Name: "doc1", MIMEType: "text/plain"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "plugin://doc/1", Text: "hello resource"}}}, nil
	})
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer endpoint.Close()

	dir := t.TempDir()
	writeFileMode(t, filepath.Join(dir, "plugin.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"acme.docs"}`, 0o600)
	writeFileMode(t, filepath.Join(dir, "mcp.json"), fmt.Sprintf(`{
		"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
		"mcpServers": {"docs.api": {"type": "streamable-http", "url": %q}}
	}`, endpoint.URL), 0o600)

	var out bytes.Buffer
	if err := runPluginsCommand(t.Context(), []string{"enable", dir, "--allow-mcp"}, &out); err != nil {
		t.Fatal(err)
	}
	serverID := pluginMCPServerID("acme.docs", "docs.api")
	if got := mcpToolName(serverID, "search"); len(got) > 64 || !mcpNamePart.MatchString(got) {
		t.Fatalf("invalid wire tool name %q", got)
	}
	if got := mcpCatalogName("resources", serverID); len(got) > 64 || !mcpNamePart.MatchString(got) {
		t.Fatalf("invalid catalog tool name %q", got)
	}
	if mcpToolName(serverID, "search__all") == mcpToolName(serverID+"__search", "all") {
		t.Fatal("distinct server/tool pairs have the same wire name")
	}

	out.Reset()
	ws := t.TempDir()
	if err := runCLIContext(t.Context(), []string{"mcp", "resources", serverID, "--workspace", ws}, strings.NewReader("y\n"), &out); err != nil {
		t.Fatalf("meldra mcp resources for plugin server failed: %v", err)
	}
	if !strings.Contains(out.String(), "plugin://doc/1") {
		t.Fatalf("catalog CLI output = %q", out.String())
	}
}

func TestPluginRegistryConcurrentUpdates(t *testing.T) {
	paths := testPluginPaths(t)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 8 {
		dir := t.TempDir()
		writeFileMode(t, filepath.Join(dir, "plugin.json"), fmt.Sprintf(`{"$schema":%q,"name":"pack-%d"}`, agentPluginsManifestSchema, i), 0o600)
		wg.Go(func() {
			<-start
			if err := runPluginsCommand(t.Context(), []string{"enable", dir}, io.Discard); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	reg, err := loadPluginRegistry(paths)
	if err != nil || len(reg.Plugins) != 8 {
		t.Fatalf("lost a concurrent registration: got %d plugins, error %v", len(reg.Plugins), err)
	}
}

func TestPluginDataPurgeRemainsInPrivateStorage(t *testing.T) {
	paths := testPluginPaths(t)
	dir := t.TempDir()
	writeFileMode(t, filepath.Join(dir, "plugin.json"), fmt.Sprintf(`{"$schema":%q,"name":"demo"}`, agentPluginsManifestSchema), 0o600)
	if err := runPluginsCommand(t.Context(), []string{"enable", dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.Symlink(other, filepath.Join(paths.Home, pluginDataDirName)); err != nil {
		t.Fatal(err)
	}
	if _, err := ensurePluginDataDir(paths, "demo"); err == nil {
		t.Fatal("accepted a redirected data root")
	}
	marker := filepath.Join(other, "demo", "unrelated")
	writeFileMode(t, marker, "keep", 0o600)
	if err := runPluginsCommand(t.Context(), []string{"disable", "demo", "--purge-data"}, io.Discard); err == nil {
		t.Fatal("purge accepted a redirected data root")
	}
	if text, err := os.ReadFile(marker); err != nil || string(text) != "keep" {
		t.Fatalf("unrelated file changed: %q %v", text, err)
	}
	if err := runPluginsCommand(t.Context(), []string{"disable", "demo"}, io.Discard); err != nil {
		t.Fatalf("registration was lost after failed purge: %v", err)
	}
}

func TestPluginNativeMCPCompatibility(t *testing.T) {
	paths := testPluginPaths(t)
	writeFileMode(t, filepath.Join(paths.Home, "mcp.json"), `{"mcpServers":{"native":{"url":"https://example.test/mcp"}}}`, 0o600)
	writeFileMode(t, filepath.Join(paths.Home, pluginsRegistryFile), `not json`, 0o600)
	servers, warnings, err := loadAllMCPConfigs(t.Context(), paths)
	if err != nil || len(warnings) == 0 || len(servers) != 1 || servers["native"].URL == "" {
		t.Fatalf("native MCP lost: servers=%v warnings=%v err=%v", servers, warnings, err)
	}
	const beforePlugins = `{"Config":{"url":"https://example.test/mcp"},"Executable":"","ExecutableDigest":"","Environment":null,"OAuthGeneration":""}`
	binding, err := mcpConnectionBinding(t.Context(), paths, "native", servers["native"], nil)
	if err != nil || binding != digest([]byte(beforePlugins)) {
		t.Fatalf("native connection no longer matches its pending approval: %s, %v", binding, err)
	}
}

func TestPluginCLIModesShadowingAndStdioDataCWD(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX script fixture")
	}
	paths := testPluginPaths(t)

	dir := t.TempDir()
	writeFileMode(t, filepath.Join(dir, "plugin.json"), `{
		"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
		"name": "data-cwd",
		"version": "1.2.3",
		"description": "Data cwd plugin",
		"extensions": {"com.example.other": {"enabled": true}},
		"extraKey": "warn-me"
	}`, 0o600)
	writeFileMode(t, filepath.Join(dir, "skills", "shared", "SKILL.md"), "---\nname: shared\ndescription: plugin copy\n---\nPlugin body\n", 0o600)
	writeFileMode(t, filepath.Join(dir, "skills", "unique", "SKILL.md"), "---\nname: unique\ndescription: unique plugin skill\n---\nUnique body\n", 0o600)
	scriptPath := filepath.Join(dir, "server.py")
	writeFileMode(t, scriptPath, testPluginStdioServerScript, 0o700)
	writeFileMode(t, filepath.Join(dir, "mcp.json"), `{
		"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
		"mcpServers": {
			"bare": {
				"type": "stdio",
				"command": "python3",
				"args": ["${PLUGIN_ROOT}/server.py"],
				"cwd": "${PLUGIN_DATA}",
				"env": {"CUSTOM_VAR": "overlay", "PATH": "/custom/bin"}
			}
		}
	}`, 0o600)

	var out bytes.Buffer
	if err := runCLIContext(t.Context(), []string{"plugins", "help"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "meldra plugins inspect") {
		t.Fatalf("help output = %q", out.String())
	}

	out.Reset()
	if err := runPluginsCommand(t.Context(), []string{"inspect", dir}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Plugin: data-cwd (1.2.3)", "Extensions (ignored): com.example.other", "Skills (2):", "MCP servers (1):", "Warning:"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("inspect text missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := runPluginsCommand(t.Context(), []string{"enable", dir, "--allow-mcp"}, &out); err != nil {
		t.Fatal(err)
	}

	otherDir := t.TempDir()
	writeFileMode(t, filepath.Join(otherDir, "plugin.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"data-cwd"}`, 0o600)
	if err := runPluginsCommand(t.Context(), []string{"enable", otherDir}, &out); err == nil {
		t.Fatal("expected duplicate plugin name from different path to fail")
	}

	out.Reset()
	if err := runPluginsCommand(t.Context(), []string{"list"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "data-cwd 1.2.3") || !strings.Contains(out.String(), "approved") {
		t.Fatalf("list text = %q", out.String())
	}

	wsDir := t.TempDir()
	writeFileMode(t, filepath.Join(wsDir, ".agents", "skills", "shared", "SKILL.md"), "---\nname: shared\ndescription: workspace copy\n---\nWorkspace body\n", 0o600)
	out.Reset()
	if err := runCLIContext(t.Context(), []string{"skills", "--workspace", wsDir}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "unique [plugin:data-cwd]") || !strings.Contains(out.String(), "workspace copy") || !strings.Contains(out.String(), "shadowed") {
		t.Fatalf("meldra skills output = %q", out.String())
	}

	w, _ := testWorkspace(t, wsDir, "", true)
	connections, err := connectMCP(t.Context(), paths, w, "")
	if err != nil {
		t.Fatal(err)
	}
	defer connections.close()
	if len(connections.tools) != 1 {
		t.Fatalf("expected 1 tool from bare python3 server, got %d; warnings=%v", len(connections.tools), connections.warnings)
	}
	reg, err := tool.New(connections.tools)
	if err != nil {
		t.Fatal(err)
	}
	res, err := reg.Invoke(t.Context(), connections.tools[0].Name, json.RawMessage(`{}`))
	if err != nil || res.Status != tool.Succeeded {
		t.Fatalf("bare stdio invoke failed: %+v %v", res, err)
	}
	var envelope struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(res.Output), &envelope); err != nil || len(envelope.Content) == 0 {
		t.Fatalf("envelope %q: %v", res.Output, err)
	}
	var probe struct {
		CWD        string `json:"cwd"`
		PluginData string `json:"plugin_data"`
		PathEnv    string `json:"path_env"`
	}
	if err := json.Unmarshal([]byte(envelope.Content[0].Text), &probe); err != nil {
		t.Fatal(err)
	}
	if probe.CWD != probe.PluginData {
		t.Fatalf("expected cwd == PLUGIN_DATA (%q), got %q", probe.PluginData, probe.CWD)
	}
	if probe.PathEnv != "/custom/bin" {
		t.Fatalf("expected configured PATH overlay /custom/bin, got %q", probe.PathEnv)
	}

	cfgA := mcpServerConfig{URL: "https://mcp.example.com/mcp", transport: "streamable-http", headers: map[string]string{"X-Tenant": "a"}, fromPlugin: true}
	cfgB := mcpServerConfig{URL: "https://mcp.example.com/mcp", transport: "streamable-http", headers: map[string]string{"X-Tenant": "b"}, fromPlugin: true}
	bindA, err := mcpConnectionBinding(t.Context(), paths, "plugin/data-cwd/api", cfgA, w)
	if err != nil {
		t.Fatal(err)
	}
	bindB, err := mcpConnectionBinding(t.Context(), paths, "plugin/data-cwd/api", cfgB, w)
	if err != nil {
		t.Fatal(err)
	}
	if bindA == bindB {
		t.Fatal("changing plugin HTTP headers did not change MCP connection binding")
	}
}

func testPluginProviderConfig(t *testing.T, paths ConfigPaths, baseURL string) {
	t.Helper()
	writeFileMode(t, paths.ConfigFile, fmt.Sprintf("model = \"gpt-test\"\nbase_url = %q\nallow_insecure_base_url = true\n", baseURL), 0o600)
	t.Setenv("OPENAI_API_KEY", "sk-plugin-test-key")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_MODEL", "")
}

func startScriptedResponsesServer(t *testing.T, handler func(step int, body map[string]any) (responseID string, output []map[string]any)) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	step := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		current := step
		step++
		mu.Unlock()
		respID, outItems := handler(current, body)
		payload, err := json.Marshal(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     respID,
				"status": "completed",
				"output": outItems,
			},
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", payload)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestPluginPlanModeBlocksMCPStartWhileAllowingPluginSkill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX script fixture")
	}
	paths := testPluginPaths(t)
	wsDir := t.TempDir()
	pluginDir := t.TempDir()
	startedMarker := filepath.Join(t.TempDir(), "mcp-started.marker")

	writeFileMode(t, filepath.Join(pluginDir, "plugin.json"), `{
		"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
		"name": "plan-guard"
	}`, 0o600)
	writeFileMode(t, filepath.Join(pluginDir, "skills", "checklist", "SKILL.md"), "---\nname: checklist\ndescription: Plan review checklist\n---\nPLAN_CHECKLIST_SECRET_BODY\n", 0o600)
	sentinelScript := "#!/usr/bin/env python3\nimport os\nopen(os.environ['START_MARKER'], 'w').write('started')\n" + strings.TrimPrefix(testPluginStdioServerScript, "#!/usr/bin/env python3\n")
	writeFileMode(t, filepath.Join(pluginDir, "bin", "sentinel"), sentinelScript, 0o700)
	writeFileMode(t, filepath.Join(pluginDir, "mcp.json"), fmt.Sprintf(`{
		"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
		"mcpServers": {
			"local": {
				"type": "stdio",
				"command": "./bin/sentinel",
				"env": {"START_MARKER": %q}
			}
		}
	}`, startedMarker), 0o600)

	var out bytes.Buffer
	if err := runPluginsCommand(t.Context(), []string{"enable", pluginDir, "--allow-mcp"}, &out); err != nil {
		t.Fatalf("enable --allow-mcp failed: %v", err)
	}

	serverID := pluginMCPServerID("plan-guard", "local")
	mcpTool := mcpToolName(serverID, "env_probe")
	sawSkillBody := false
	sawMCPToolInPlan := false
	sawMCPOutputInBuild := false

	providerServer := startScriptedResponsesServer(t, func(step int, body map[string]any) (string, []map[string]any) {
		rawBody, _ := json.Marshal(body)
		switch step {
		case 0:
			if strings.Contains(string(rawBody), mcpTool) {
				sawMCPToolInPlan = true
			}
			return "resp_plan_1", []map[string]any{{
				"type":      "function_call",
				"call_id":   "call_plan_skill",
				"name":      "read_skill",
				"arguments": `{"name":"checklist","path":null}`,
			}}
		case 1:
			if strings.Contains(string(rawBody), "PLAN_CHECKLIST_SECRET_BODY") {
				sawSkillBody = true
			}
			return "resp_plan_2", []map[string]any{{
				"type":    "message",
				"id":      "msg_plan",
				"status":  "completed",
				"role":    "assistant",
				"content": []map[string]any{{"type": "output_text", "text": "plan complete", "annotations": []any{}}},
			}}
		case 2:
			return "resp_build_1", []map[string]any{{
				"type":      "function_call",
				"call_id":   "call_build_mcp",
				"name":      mcpTool,
				"arguments": `{}`,
			}}
		default:
			if strings.Contains(string(rawBody), "probe-marker.txt") {
				sawMCPOutputInBuild = true
			}
			return "resp_build_2", []map[string]any{{
				"type":    "message",
				"id":      "msg_build",
				"status":  "completed",
				"role":    "assistant",
				"content": []map[string]any{{"type": "output_text", "text": "build complete", "annotations": []any{}}},
			}}
		}
	})
	testPluginProviderConfig(t, paths, providerServer.URL)

	out.Reset()
	if err := runCLIContext(t.Context(), []string{"--mode", "plan", "--workspace", wsDir, "--output-format", "stream-json", "--prompt", "inspect checklist"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("plan turn failed: %v\noutput=%s", err, out.String())
	}
	if !sawSkillBody {
		t.Fatal("Plan mode did not return plugin skill body")
	}
	if sawMCPToolInPlan {
		t.Fatal("Plan mode unexpectedly advertised plugin MCP tool")
	}
	if _, err := os.Stat(startedMarker); !os.IsNotExist(err) {
		t.Fatal("Plan mode unexpectedly launched approved plugin MCP subprocess")
	}

	out.Reset()
	if err := runCLIContext(t.Context(), []string{"--mode", "build", "--auto-approve", "--workspace", wsDir, "--output-format", "stream-json", "--prompt", "run probe"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("build turn failed: %v\noutput=%s", err, out.String())
	}
	if _, err := os.Stat(startedMarker); err != nil {
		t.Fatalf("Build mode did not start plugin MCP subprocess: %v", err)
	}
	if !sawMCPOutputInBuild {
		t.Fatal("Build mode did not receive plugin MCP tool output")
	}
}

func TestPluginStreamJSONSuspendResumeAndTamperRejection(t *testing.T) {
	paths := testPluginPaths(t)
	wsDir := t.TempDir()
	pluginDir := t.TempDir()

	var dispatches atomic.Int64
	remoteServer := mcp.NewServer(&mcp.Implementation{Name: "deploy-fixture", Version: "1"}, nil)
	remoteServer.AddTool(&mcp.Tool{Name: "deploy_check", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		dispatches.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "deploy-ok"}}}, nil
	})
	remoteEndpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remoteServer }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer remoteEndpoint.Close()

	writeMCPConfig := func(tenant string) {
		writeFileMode(t, filepath.Join(pluginDir, "mcp.json"), fmt.Sprintf(`{
			"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
			"mcpServers": {
				"api": {
					"type": "streamable-http",
					"url": %q,
					"headers": {"X-Tenant": %q}
				}
			}
		}`, remoteEndpoint.URL, tenant), 0o600)
	}
	writeFileMode(t, filepath.Join(pluginDir, "plugin.json"), `{
		"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
		"name": "resume-guard"
	}`, 0o600)
	writeMCPConfig("v1")

	var out bytes.Buffer
	if err := runPluginsCommand(t.Context(), []string{"enable", pluginDir, "--allow-mcp"}, &out); err != nil {
		t.Fatal(err)
	}

	serverID := pluginMCPServerID("resume-guard", "api")
	mcpTool := mcpToolName(serverID, "deploy_check")
	providerServer := startScriptedResponsesServer(t, func(step int, body map[string]any) (string, []map[string]any) {
		if step == 0 {
			return "resp_suspend", []map[string]any{{
				"type":      "function_call",
				"call_id":   "call_deploy_1",
				"name":      mcpTool,
				"arguments": `{}`,
			}}
		}
		return "resp_resumed", []map[string]any{{
			"type":    "message",
			"id":      "msg_resumed",
			"status":  "completed",
			"role":    "assistant",
			"content": []map[string]any{{"type": "output_text", "text": "resumed-ok", "annotations": []any{}}},
		}}
	})
	testPluginProviderConfig(t, paths, providerServer.URL)

	out.Reset()
	suspendErr := runCLIContext(t.Context(), []string{"--mode", "build", "--workspace", wsDir, "--output-format", "stream-json", "--prompt", "check deploy"}, strings.NewReader(""), &out)
	if cliExitCode(suspendErr) != 3 {
		t.Fatalf("expected exit code 3 (approval required), got err=%v\noutput=%s", suspendErr, out.String())
	}
	if dispatches.Load() != 0 {
		t.Fatalf("tool dispatched before approval: %d", dispatches.Load())
	}

	var taskID, detailHash string
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		var env struct {
			Kind   string `json:"kind"`
			TaskID string `json:"task_id"`
			Data   struct {
				DetailHash string `json:"detail_hash"`
				Detail     string `json:"detail"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &env); err == nil && env.Kind == "approval_required" {
			taskID = env.TaskID
			detailHash = env.Data.DetailHash
			if !strings.Contains(env.Data.Detail, serverID) {
				t.Fatalf("approval detail missing plugin server ID %q: %s", serverID, env.Data.Detail)
			}
		}
	}
	if taskID == "" || detailHash == "" {
		t.Fatalf("missing task_id/detail_hash in stream-json output:\n%s", out.String())
	}

	writeMCPConfig("v2")
	out.Reset()
	if err := runCLIContext(t.Context(), []string{"task", "resume", taskID, "--approve-detail-hash", detailHash}, strings.NewReader(""), &out); err == nil {
		t.Fatal("expected resume to fail after tampering mcp.json without re-approval")
	}
	if dispatches.Load() != 0 {
		t.Fatalf("tool dispatched despite stale plugin approval: %d", dispatches.Load())
	}

	out.Reset()
	if err := runPluginsCommand(t.Context(), []string{"allow-mcp", "resume-guard"}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runCLIContext(t.Context(), []string{"task", "resume", taskID, "--approve-detail-hash", detailHash}, strings.NewReader(""), &out); err == nil {
		t.Fatal("expected resume to fail when plugin header changed relative to approved connection binding")
	}
	if dispatches.Load() != 0 {
		t.Fatalf("tool dispatched despite changed connection binding: %d", dispatches.Load())
	}

	writeMCPConfig("v1")
	out.Reset()
	if err := runPluginsCommand(t.Context(), []string{"allow-mcp", "resume-guard"}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runCLIContext(t.Context(), []string{"task", "resume", taskID, "--approve-detail-hash", detailHash}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("expected resume with matching plugin config to succeed: %v\noutput=%s", err, out.String())
	}
	if dispatches.Load() != 1 {
		t.Fatalf("expected exactly 1 tool dispatch after valid resume, got %d", dispatches.Load())
	}
	if !strings.Contains(out.String(), "resumed-ok") {
		t.Fatalf("resumed output missing completion message:\n%s", out.String())
	}
}

func TestPluginChatTurnCombinesReadSkillAndMCPWithApproval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX script fixture")
	}
	paths := testPluginPaths(t)
	wsDir := t.TempDir()
	pluginDir := t.TempDir()

	writeFileMode(t, filepath.Join(pluginDir, "plugin.json"), `{
		"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
		"name": "combo"
	}`, 0o600)
	writeFileMode(t, filepath.Join(pluginDir, "skills", "ops", "SKILL.md"), "---\nname: ops\ndescription: Ops runbook\n---\nRead references/steps.txt\n", 0o600)
	writeFileMode(t, filepath.Join(pluginDir, "skills", "ops", "references", "steps.txt"), "STEP_ONE_VERIFY_PLUGIN\n", 0o600)
	writeFileMode(t, filepath.Join(pluginDir, "bin", "server"), testPluginStdioServerScript, 0o700)
	writeFileMode(t, filepath.Join(pluginDir, "mcp.json"), `{
		"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
		"mcpServers": {
			"local": {
				"type": "stdio",
				"command": "./bin/server",
				"env": {"CUSTOM_VAR": "combo-ok"}
			}
		}
	}`, 0o600)

	var out bytes.Buffer
	if err := runPluginsCommand(t.Context(), []string{"enable", pluginDir, "--allow-mcp"}, &out); err != nil {
		t.Fatal(err)
	}

	serverID := pluginMCPServerID("combo", "local")
	mcpTool := mcpToolName(serverID, "env_probe")
	sawRef := false
	sawProbe := false

	providerServer := startScriptedResponsesServer(t, func(step int, body map[string]any) (string, []map[string]any) {
		rawBody, _ := json.Marshal(body)
		switch step {
		case 0:
			return "resp_combo_1", []map[string]any{{
				"type":      "function_call",
				"call_id":   "call_skill_ref",
				"name":      "read_skill",
				"arguments": `{"name":"ops","path":"references/steps.txt"}`,
			}}
		case 1:
			if strings.Contains(string(rawBody), "STEP_ONE_VERIFY_PLUGIN") {
				sawRef = true
			}
			return "resp_combo_2", []map[string]any{{
				"type":      "function_call",
				"call_id":   "call_mcp_probe",
				"name":      mcpTool,
				"arguments": `{}`,
			}}
		default:
			if strings.Contains(string(rawBody), "combo-ok") {
				sawProbe = true
			}
			return "resp_combo_3", []map[string]any{{
				"type":    "message",
				"id":      "msg_combo",
				"status":  "completed",
				"role":    "assistant",
				"content": []map[string]any{{"type": "output_text", "text": "combo complete", "annotations": []any{}}},
			}}
		}
	})
	testPluginProviderConfig(t, paths, providerServer.URL)

	out.Reset()
	if err := runCLIContext(t.Context(), []string{"--mode", "build", "--workspace", wsDir, "--prompt", "use $ops and probe"}, strings.NewReader("y\n"), &out); err != nil {
		t.Fatalf("combo chat failed: %v\noutput=%s", err, out.String())
	}
	if !sawRef || !sawProbe {
		t.Fatalf("expected both skill ref and MCP probe output: sawRef=%v sawProbe=%v", sawRef, sawProbe)
	}
	if !strings.Contains(out.String(), serverID) || !strings.Contains(out.String(), "combo complete") {
		t.Fatalf("chat output missing approval server ID or final answer:\n%s", out.String())
	}

	var tasksOut bytes.Buffer
	if err := runCLIContext(t.Context(), []string{"tasks", "--json"}, strings.NewReader(""), &tasksOut); err != nil {
		t.Fatal(err)
	}
	var listed []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(tasksOut.Bytes(), &listed); err != nil || len(listed) == 0 {
		t.Fatalf("tasks json=%q err=%v", tasksOut.String(), err)
	}
	var showOut bytes.Buffer
	if err := runCLIContext(t.Context(), []string{"task", "show", listed[0].ID, "--json"}, strings.NewReader(""), &showOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(showOut.String(), "read_skill") || !strings.Contains(showOut.String(), mcpTool) {
		t.Fatalf("task record missing read_skill or plugin MCP tool:\n%s", showOut.String())
	}
}

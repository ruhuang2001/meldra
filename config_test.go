package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveConfigPaths(t *testing.T) {
	t.Run("MELDRA_HOME override", func(t *testing.T) {
		override := filepath.Join(t.TempDir(), "custom")
		t.Setenv(MeldraHomeEnv, override)

		paths, err := ResolveConfigPaths()
		if err != nil {
			t.Fatal(err)
		}
		if paths.Home != override {
			t.Fatalf("Home = %q, want %q", paths.Home, override)
		}
	})

	t.Run("default user home", func(t *testing.T) {
		userHome := t.TempDir()
		t.Setenv(MeldraHomeEnv, "")
		t.Setenv("HOME", userHome)

		paths, err := ResolveConfigPaths()
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(userHome, ".meldra")
		if paths.Home != want {
			t.Fatalf("Home = %q, want %q", paths.Home, want)
		}
	})
}

func TestInitializeConfigPreservesFilesAndSecuresPermissions(t *testing.T) {
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "meldra-home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Home, 0o755); err != nil {
		t.Fatal(err)
	}
	const existing = "model = \"existing-model\"\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := InitializeConfig(paths); err != nil {
		t.Fatal(err)
	}
	if err := InitializeConfig(paths); err != nil {
		t.Fatalf("second init failed: %v", err)
	}

	contents, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != existing {
		t.Fatalf("init overwrote config: %q", contents)
	}
	for path, want := range map[string]os.FileMode{
		paths.Home:            0o700,
		paths.ConfigFile:      0o600,
		paths.CredentialsFile: 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("permissions for %s = %04o, want %04o", path, got, want)
		}
	}
}

func TestLoadConfigRejectsInvalidTOML(t *testing.T) {
	paths, err := ConfigPathsForHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := InitializeConfig(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("model = not-quoted\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = LoadConfig(paths)
	if err == nil || !strings.Contains(err.Error(), "value must be a quoted string") {
		t.Fatalf("LoadConfig error = %v, want quoted-string parse error", err)
	}
}

func TestConfigTOMLSupportedSyntaxAndValidation(t *testing.T) {
	config, err := parseConfigTOML("model = 'model#name' # comment\nbase_url = \"https://example.test/v1#fragment\"\nfuture_key = \"ignored\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if config.Model != "model#name" || config.BaseURL != "https://example.test/v1#fragment" {
		t.Fatalf("parsed config = %#v", config)
	}
	for _, contents := range []string{
		"[provider]\nmodel = \"x\"\n",
		"model = \"one\"\nmodel = \"two\"\n",
		"model\n",
		"model = \"unterminated\n",
	} {
		if _, err := parseConfigTOML(contents); err == nil {
			t.Errorf("accepted invalid config %q", contents)
		}
	}
	if err := SaveConfig(mustConfigPaths(t), Config{Model: "bad\nmodel"}); err == nil {
		t.Fatal("SaveConfig accepted a model containing a newline")
	}
	if err := SaveAPIKey(mustConfigPaths(t), "bad\nkey"); err == nil {
		t.Fatal("SaveAPIKey accepted a key containing a newline")
	}
}

func mustConfigPaths(t *testing.T) ConfigPaths {
	t.Helper()
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "meldra-home"))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestConfigRejectsSymlinksAndInsecurePermissions(t *testing.T) {
	paths := mustConfigPaths(t)
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("model = \"outside\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, paths.ConfigFile); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(paths); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink config error = %v", err)
	}
	if err := SaveConfig(paths, Config{Model: "safe"}); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink save error = %v", err)
	}
	if err := os.Remove(paths.ConfigFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("model = \"unsafe\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(paths); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("insecure config error = %v", err)
	}

	linkedHome := filepath.Join(t.TempDir(), "linked-home")
	if err := os.Symlink(paths.Home, linkedHome); err != nil {
		t.Fatal(err)
	}
	linkedPaths, err := ConfigPathsForHome(linkedHome)
	if err != nil {
		t.Fatal(err)
	}
	if err := InitializeConfig(linkedPaths); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink home error = %v", err)
	}
}

func TestMalformedCredentialsErrorDoesNotLeakSecret(t *testing.T) {
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "meldra-home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := InitializeConfig(paths); err != nil {
		t.Fatal(err)
	}
	const secret = "sk-secret-that-must-not-leak"
	if err := os.WriteFile(paths.CredentialsFile, []byte("OPENAI_API_KEY=\""+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = LoadCredentials(paths)
	if err == nil || !strings.Contains(err.Error(), "invalid credentials syntax") {
		t.Fatalf("LoadCredentials error = %v, want generic syntax error", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("LoadCredentials error leaked API key: %v", err)
	}
}

func TestEnvironmentOverridesFilesAndConfigShowRedactsKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv(MeldraHomeEnv, home)
	paths, err := ResolveConfigPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(paths, Config{Model: "file-model", BaseURL: "https://file.example/v1"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveAPIKey(paths, "file-secret-key"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_MODEL", "env-model")
	t.Setenv("OPENAI_BASE_URL", "https://env.example/v1")
	t.Setenv("OPENAI_API_KEY", "env-secret-key")

	var output bytes.Buffer
	if err := runCLI([]string{"config", "show"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{home, "model=env-model", "base_url=https://env.example/v1", "openai_api_key=********"} {
		if !strings.Contains(got, want) {
			t.Errorf("config show output does not contain %q:\n%s", want, got)
		}
	}
	for _, secret := range []string{"file-secret-key", "env-secret-key"} {
		if strings.Contains(got, secret) {
			t.Errorf("config show leaked %q:\n%s", secret, got)
		}
	}
}

func TestFileSettingsReachChatRequest(t *testing.T) {
	const configuredModel = "file-configured-model"
	var request struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		if incoming.Method != http.MethodPost || incoming.URL.Path != "/responses" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if incoming.Header.Get("Authorization") != "Bearer file-api-key" {
			http.Error(writer, "unexpected authorization", http.StatusUnauthorized)
			return
		}
		defer func() { _ = incoming.Body.Close() }()
		if err := json.NewDecoder(incoming.Body).Decode(&request); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"configured reply\"}\n\n")
		_, _ = fmt.Fprint(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_config\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_config\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"configured reply\",\"annotations\":[]}]}]}}\n\n")
	}))
	defer server.Close()

	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "meldra-home"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(MeldraHomeEnv, paths.Home)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	if err := SaveConfig(paths, Config{Model: configuredModel, BaseURL: server.URL}); err != nil {
		t.Fatal(err)
	}
	if err := SaveAPIKey(paths, "file-api-key"); err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	var output bytes.Buffer
	if err := runChat(context.Background(), strings.NewReader("hello\n"), &output, ChatOptions{Workspace: workspace, workspaceExplicit: true}); err != nil {
		t.Fatal(err)
	}
	if request.Model != configuredModel || !request.Stream {
		t.Fatalf("request settings = model %q, stream %t", request.Model, request.Stream)
	}
	if !strings.Contains(output.String(), "configured reply") {
		t.Fatalf("chat output = %q", output.String())
	}
	sessions, err := NewSessionStore(paths).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || len(sessions[0].Messages) != 2 {
		t.Fatalf("saved sessions = %#v", sessions)
	}
}

func TestConfigShowIncludesBuiltInDefaults(t *testing.T) {
	t.Setenv(MeldraHomeEnv, filepath.Join(t.TempDir(), "meldra-home"))
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")

	var output bytes.Buffer
	if err := runCLI([]string{"config", "show"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "model="+defaultModel) || !strings.Contains(got, "base_url="+defaultBaseURL) {
		t.Fatalf("config show does not include effective defaults:\n%s", got)
	}
}

func TestMeldraNeverLoadsWorkingDirectoryDotEnv(t *testing.T) {
	workingDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(workingDirectory, ".env"), []byte("OPENAI_API_KEY=project-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workingDirectory)
	t.Setenv(MeldraHomeEnv, filepath.Join(t.TempDir(), "meldra-home"))
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("OPENAI_BASE_URL", "")

	err := runCLI(nil, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY is not configured") {
		t.Fatalf("runCLI error = %v, want missing-key error", err)
	}
	if got := os.Getenv("OPENAI_API_KEY"); got != "" {
		t.Fatalf("working-directory .env was loaded: OPENAI_API_KEY=%q", got)
	}
}

func TestCLIHelpVersionAndErrors(t *testing.T) {
	t.Setenv(MeldraHomeEnv, filepath.Join(t.TempDir(), "meldra-home"))
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("OPENAI_BASE_URL", "")

	t.Run("help", func(t *testing.T) {
		var output bytes.Buffer
		if err := runCLI([]string{"--help"}, strings.NewReader(""), &output); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "meldra config init") {
			t.Fatalf("unexpected help output: %s", output.String())
		}
	})

	t.Run("config help", func(t *testing.T) {
		var output bytes.Buffer
		if err := runCLI([]string{"config", "--help"}, strings.NewReader(""), &output); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "meldra config [show]") || strings.Contains(output.String(), "meldra config path") {
			t.Fatalf("unexpected config help output: %s", output.String())
		}
	})

	t.Run("version", func(t *testing.T) {
		originalVersion := version
		version = "v1.2.3"
		t.Cleanup(func() { version = originalVersion })
		var output bytes.Buffer
		if err := runCLI([]string{"version"}, strings.NewReader(""), &output); err != nil {
			t.Fatal(err)
		}
		if output.String() != "v1.2.3\n" {
			t.Fatalf("version output = %q", output.String())
		}
	})

	t.Run("unknown command", func(t *testing.T) {
		err := runCLI([]string{"unknown"}, strings.NewReader(""), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), `unknown command "unknown"`) {
			t.Fatalf("runCLI error = %v", err)
		}
	})

	t.Run("missing API key", func(t *testing.T) {
		err := runCLI(nil, strings.NewReader(""), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "meldra config init") || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
			t.Fatalf("runCLI error = %v", err)
		}
	})
}

func TestCLIConfigInitAndArgumentValidation(t *testing.T) {
	home := filepath.Join(t.TempDir(), "meldra-home")
	t.Setenv(MeldraHomeEnv, home)
	var output bytes.Buffer
	if err := runCLI([]string{"config", "init"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), home) || !strings.Contains(output.String(), "credentials.env") {
		t.Fatalf("config init output = %q", output.String())
	}
	output.Reset()
	if err := runCLI([]string{"config"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{home, filepath.Join(home, "config.toml"), filepath.Join(home, "credentials.env")} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("config output missing %q: %s", want, output.String())
		}
	}
	if err := runCLI([]string{"config", "path"}, strings.NewReader(""), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), `unknown config command "path"`) {
		t.Errorf("config path error = %v, want unknown-command error", err)
	}

	invalid := [][]string{
		{"version", "extra"},
		{"sessions", "extra"},
		{"resume", "one", "two"},
		{"config", "show", "extra"},
		{"config", "init", "extra"},
		{"config", "unknown"},
		{"--workspace="},
		{"--resume"},
		{"--resume="},
	}
	for _, args := range invalid {
		if err := runCLI(args, strings.NewReader(""), &bytes.Buffer{}); err == nil {
			t.Errorf("runCLI accepted invalid arguments %#v", args)
		}
	}
}

func TestCLIWorkspaceSessionsAndResume(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "meldra-home")
	workspace := t.TempDir()
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(MeldraHomeEnv, configHome)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("OPENAI_BASE_URL", "")

	var output bytes.Buffer
	if err := runCLI([]string{"--workspace", workspace}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Workspace: "+canonicalWorkspace) || !strings.Contains(output.String(), "Session: ") {
		t.Fatalf("chat startup output = %q", output.String())
	}

	paths, err := ResolveConfigPaths()
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := NewSessionStore(paths).List()
	if err != nil || len(sessions) != 0 {
		t.Fatalf("empty chat saved sessions = %#v, error = %v", sessions, err)
	}
	store := NewSessionStore(paths)
	session, err := store.Create(canonicalWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	session.appendMessage("user", "continue")
	if err := store.Save(session); err != nil {
		t.Fatal(err)
	}
	sessions, err = store.List()
	if err != nil || len(sessions) != 1 || sessions[0].ID != session.ID {
		t.Fatalf("sessions = %#v, error = %v", sessions, err)
	}

	output.Reset()
	if err := runCLI([]string{"sessions"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), sessions[0].ID) || !strings.Contains(output.String(), canonicalWorkspace) {
		t.Fatalf("sessions output = %q", output.String())
	}

	output.Reset()
	if err := runCLI([]string{"--resume", sessions[0].ID}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Session: "+sessions[0].ID) || !strings.Contains(output.String(), "Workspace: "+canonicalWorkspace) {
		t.Fatalf("resume output = %q", output.String())
	}

	otherWorkspace := t.TempDir()
	err = runCLI([]string{"--resume", sessions[0].ID, "--workspace", otherWorkspace}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "belongs to workspace") {
		t.Fatalf("mismatched resume error = %v", err)
	}
}

func TestParseChatOptions(t *testing.T) {
	options, err := parseChatOptions([]string{"--workspace=./project", "--resume", "latest", "--yes"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Workspace != "./project" || options.Resume != "latest" || !options.AutoApprove || !options.workspaceExplicit {
		t.Fatalf("options = %#v", options)
	}
	if _, err := parseChatOptions([]string{"--workspace"}); err == nil {
		t.Fatal("accepted --workspace without a path")
	}
}

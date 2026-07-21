package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// version is set by GoReleaser for tagged builds.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = os.Stdin.Close()
	}()
	if err := runCLIContext(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func runCLI(args []string, stdin io.Reader, stdout io.Writer) error {
	return runCLIContext(context.Background(), args, stdin, stdout)
}

func runCLIContext(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(args) > 0 {
		switch args[0] {
		case "config":
			return runConfigCommand(args[1:], stdout)
		case "version", "--version", "-v":
			if len(args) != 1 {
				return fmt.Errorf("version does not accept arguments")
			}
			_, err := fmt.Fprintln(stdout, version)
			return err
		case "help", "--help", "-h":
			_, err := fmt.Fprint(stdout, usageText)
			return err
		case "sessions":
			if len(args) != 1 {
				return fmt.Errorf("sessions does not accept arguments")
			}
			return runSessionsCommand(stdout)
		case "resume":
			if len(args) > 2 {
				return fmt.Errorf("resume accepts at most one session ID")
			}
			id := "latest"
			if len(args) == 2 {
				id = args[1]
			}
			return runChat(ctx, stdin, stdout, ChatOptions{Resume: id})
		default:
			if !strings.HasPrefix(args[0], "-") {
				return fmt.Errorf("unknown command %q\n\n%s", args[0], usageText)
			}
			options, err := parseChatOptions(args)
			if err != nil {
				return err
			}
			return runChat(ctx, stdin, stdout, options)
		}
	}

	return runChat(ctx, stdin, stdout, ChatOptions{})
}

func runConfigCommand(args []string, stdout io.Writer) error {
	paths, err := ResolveConfigPaths()
	if err != nil {
		return err
	}
	if len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprint(stdout, configUsageText)
		return err
	}

	if len(args) == 0 || args[0] == "show" {
		if len(args) > 1 {
			return fmt.Errorf("config show does not accept arguments")
		}

		settings, err := LoadSettings(paths)
		if err != nil {
			return err
		}
		settings = effectiveSettings(settings)
		_, err = fmt.Fprint(stdout, FormatConfigShow(ConfigShowData(paths, settings)))
		return err
	}

	switch args[0] {
	case "init":
		if len(args) != 1 {
			return fmt.Errorf("config init does not accept arguments")
		}
		if err := InitializeConfig(paths); err != nil {
			return err
		}
		_, err := fmt.Fprintf(stdout, "Initialized Meldra configuration in %s\nEdit %s to add your OPENAI_API_KEY.\n", paths.Home, paths.CredentialsFile)
		return err
	case "path":
		if len(args) != 1 {
			return fmt.Errorf("config path does not accept arguments")
		}
		_, err := fmt.Fprintf(stdout, "Home: %s\nConfig: %s\nCredentials: %s\n", paths.Home, paths.ConfigFile, paths.CredentialsFile)
		return err
	default:
		return fmt.Errorf("unknown config command %q\n\n%s", args[0], configUsageText)
	}
}

type ChatOptions struct {
	Workspace         string
	Resume            string
	AutoApprove       bool
	workspaceExplicit bool
}

func parseChatOptions(args []string) (ChatOptions, error) {
	var options ChatOptions
	for index := 0; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--yes":
			options.AutoApprove = true
		case argument == "--workspace":
			index++
			if index >= len(args) || args[index] == "" {
				return ChatOptions{}, fmt.Errorf("--workspace requires a path")
			}
			options.Workspace = args[index]
			options.workspaceExplicit = true
		case strings.HasPrefix(argument, "--workspace="):
			options.Workspace = strings.TrimPrefix(argument, "--workspace=")
			if options.Workspace == "" {
				return ChatOptions{}, fmt.Errorf("--workspace requires a path")
			}
			options.workspaceExplicit = true
		case argument == "--resume":
			index++
			if index >= len(args) || args[index] == "" {
				return ChatOptions{}, fmt.Errorf("--resume requires a session ID or latest")
			}
			options.Resume = args[index]
		case strings.HasPrefix(argument, "--resume="):
			options.Resume = strings.TrimPrefix(argument, "--resume=")
			if options.Resume == "" {
				return ChatOptions{}, fmt.Errorf("--resume requires a session ID or latest")
			}
		default:
			return ChatOptions{}, fmt.Errorf("unknown command or option %q\n\n%s", argument, usageText)
		}
	}
	return options, nil
}

func runSessionsCommand(stdout io.Writer) error {
	paths, err := ResolveConfigPaths()
	if err != nil {
		return err
	}
	sessions, err := NewSessionStore(paths).List()
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		_, err = fmt.Fprintln(stdout, "No saved sessions.")
		return err
	}
	for _, session := range sessions {
		summary := session.Summary
		if summary == "" {
			summary = "<no summary>"
		}
		summary = strings.ReplaceAll(summary, "\n", " ")
		if len(summary) > 80 {
			summary = summary[:80] + "..."
		}
		if _, err := fmt.Fprintf(stdout, "%s  %s  %s  %s\n", session.ID, session.UpdatedAt.Format(time.RFC3339), session.Workspace, summary); err != nil {
			return err
		}
	}
	return nil
}

func runChat(ctx context.Context, stdin io.Reader, stdout io.Writer, options ChatOptions) error {
	paths, err := ResolveConfigPaths()
	if err != nil {
		return err
	}
	settings, err := LoadSettings(paths)
	if err != nil {
		return err
	}
	settings = effectiveSettings(settings)
	applySettings(settings)
	if settings.APIKey == "" {
		return fmt.Errorf("OPENAI_API_KEY is not configured; run \"meldra config init\" and add it to %s, or set OPENAI_API_KEY", paths.CredentialsFile)
	}

	reader := bufio.NewReader(stdin)
	store := NewSessionStore(paths)
	var session *Session
	if options.Resume != "" {
		session, err = store.Load(options.Resume)
		if err != nil {
			return err
		}
		if !options.workspaceExplicit {
			options.Workspace = session.Workspace
		}
	}
	if options.Workspace == "" {
		options.Workspace, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve current workspace: %w", err)
		}
	}
	workspace, err := NewWorkspace(options.Workspace, reader, stdout, options.AutoApprove)
	if err != nil {
		return err
	}
	if err := workspace.ProtectPath(paths.Home); err != nil {
		return err
	}
	workspace.SetContext(ctx)
	if session != nil && session.Workspace != workspace.root {
		return fmt.Errorf("session %s belongs to workspace %s, not %s", session.ID, session.Workspace, workspace.root)
	}
	if session == nil {
		session, err = store.New(workspace.root)
		if err != nil {
			return err
		}
	}

	client := openai.NewClient(
		option.WithAPIKey(settings.APIKey),
		option.WithBaseURL(settings.BaseURL),
	)
	var readErr error
	getUserMessage := func() (string, bool) {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			readErr = err
			return "", false
		}
		if err == io.EOF && line == "" {
			return "", false
		}
		return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), true
	}

	tools := workspace.ToolDefinitions()
	tools = append(tools, NewSessionTools(session, store).ToolDefinitions()...)
	agent := NewAgent(&client, getUserMessage, tools)
	agent.output = stdout
	agent.session = session
	agent.store = store
	fmt.Fprintf(stdout, "Session: %s\nWorkspace: %s\n", session.ID, workspace.root)
	if err := agent.Run(ctx); err != nil {
		return err
	}
	return readErr
}

func effectiveSettings(settings Settings) Settings {
	if settings.Model == "" {
		settings.Model = defaultModel
	}
	if settings.BaseURL == "" {
		settings.BaseURL = defaultBaseURL
	}
	return overlayEnvironment(settings)
}

func overlayEnvironment(settings Settings) Settings {
	if value := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); value != "" {
		settings.APIKey = value
	}
	if value := strings.TrimSpace(os.Getenv("OPENAI_MODEL")); value != "" {
		settings.Model = value
	}
	if value := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")); value != "" {
		settings.BaseURL = value
	}
	return settings
}

func applySettings(settings Settings) {
	setEnvironmentIfUnset("OPENAI_API_KEY", settings.APIKey)
	setEnvironmentIfUnset("OPENAI_MODEL", settings.Model)
	setEnvironmentIfUnset("OPENAI_BASE_URL", settings.BaseURL)
}

func setEnvironmentIfUnset(name, value string) {
	if value != "" && os.Getenv(name) == "" {
		_ = os.Setenv(name, value)
	}
}

const usageText = `Usage:
  meldra [options]               Start a chat in a bounded workspace.
  meldra resume [session-id]     Resume the latest or selected session.
  meldra sessions                List saved sessions.
  meldra config init             Create ~/.meldra configuration files.
  meldra config [show]           Show effective configuration without secrets.
  meldra config path             Print configuration file paths.
  meldra version                 Print the installed version.

Options:
  --workspace PATH               Restrict all file and command tools to PATH.
  --resume ID                    Resume ID (or "latest") in its saved workspace.
  --yes                          Approve file writes and executable commands without confirmation.

Configuration:
  Meldra reads ~/.meldra/config.toml and ~/.meldra/credentials.env by default.
  Set MELDRA_HOME to use another configuration directory. OPENAI_* environment
  variables override file values. Meldra never automatically loads a project's .env.
`

const configUsageText = `Usage:
  meldra config init
  meldra config [show]
  meldra config path
`

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// version is set by GoReleaser for tagged builds.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Bubble Tea restores raw terminal state during shutdown. Closing its input
	// descriptor on a signal can make that restoration fail, so only interrupt
	// the blocking line-based reader.
	if !shouldUseTUI(os.Stdin, os.Stdout) {
		go func() {
			<-ctx.Done()
			_ = os.Stdin.Close()
		}()
	}
	if err := runCLIContext(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		printCLIError(os.Stderr, err)
		os.Exit(1)
	}
}

func runCLI(args []string, stdin io.Reader, stdout io.Writer) error {
	return runCLIContext(context.Background(), args, stdin, stdout)
}

func printCLIError(stderr io.Writer, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(stderr, "Error: %s\n", sanitizeTerminalText(err.Error()))
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
			options, err := parseResumeOptions(args[1:])
			if err != nil {
				return err
			}
			chatInput := stdin
			if options.selectResume {
				resumeWorkspace, err := resumeWorkspacePath(options)
				if err != nil {
					return err
				}
				if shouldUseTUI(stdin, stdout) {
					selected, err := runSessionPicker(stdin.(*os.File), stdout.(*os.File), resumeWorkspace)
					if err != nil {
						return err
					}
					if selected == "" {
						return nil
					}
					options.Resume = selected
				} else {
					// Keep a single buffered reader for the line-based picker and
					// chat so a piped follow-up prompt cannot be consumed by the picker.
					chatInput = bufferedInput(stdin)
					paths, err := ResolveConfigPaths()
					if err != nil {
						return err
					}
					options.Resume, err = selectResumeSession(chatInput, stdout, NewSessionStore(paths), resumeWorkspace)
					if err != nil {
						return err
					}
				}
			}
			if options.Resume == "latest" {
				resumeWorkspace, err := resumeWorkspacePath(options)
				if err != nil {
					return err
				}
				paths, err := ResolveConfigPaths()
				if err != nil {
					return err
				}
				sessions, diagnostics, err := NewSessionStore(paths).ListWorkspaceWithDiagnostics(resumeWorkspace)
				if err != nil {
					return err
				}
				if diagnostics.SkippedFiles > 0 {
					if _, err := fmt.Fprintln(stdout, sessionListDiagnosticsWarning(diagnostics)); err != nil {
						return err
					}
				}
				if len(sessions) == 0 {
					return fmt.Errorf("no saved sessions for workspace %s", resumeWorkspace)
				}
				options.Resume = sessions[0].ID
			}
			return runChat(ctx, chatInput, stdout, options)
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
		settings, err = effectiveSettings(settings)
		if err != nil {
			return err
		}
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
	default:
		return fmt.Errorf("unknown config command %q\n\n%s", args[0], configUsageText)
	}
}

type ChatOptions struct {
	Workspace         string
	Resume            string
	Prompt            string
	AutoApprove       bool
	workspaceExplicit bool
	selectResume      bool
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
			if index >= len(args) || args[index] == "" || strings.HasPrefix(args[index], "-") {
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
			if index >= len(args) || args[index] == "" || strings.HasPrefix(args[index], "-") {
				return ChatOptions{}, fmt.Errorf("--resume requires a session ID or latest")
			}
			options.Resume = args[index]
		case strings.HasPrefix(argument, "--resume="):
			options.Resume = strings.TrimPrefix(argument, "--resume=")
			if options.Resume == "" {
				return ChatOptions{}, fmt.Errorf("--resume requires a session ID or latest")
			}
		case argument == "--prompt":
			index++
			if index >= len(args) || args[index] == "" || strings.HasPrefix(args[index], "-") {
				return ChatOptions{}, fmt.Errorf("--prompt requires a non-empty message")
			}
			options.Prompt = args[index]
		case strings.HasPrefix(argument, "--prompt="):
			options.Prompt = strings.TrimPrefix(argument, "--prompt=")
			if options.Prompt == "" {
				return ChatOptions{}, fmt.Errorf("--prompt requires a non-empty message")
			}
		default:
			return ChatOptions{}, fmt.Errorf("unknown command or option %q\n\n%s", argument, usageText)
		}
	}
	return options, nil
}

func parseResumeOptions(args []string) (ChatOptions, error) {
	optionArgs := make([]string, 0, len(args))
	var sessionID string
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--workspace" || argument == "--resume" || argument == "--prompt" {
			optionArgs = append(optionArgs, argument)
			if index+1 < len(args) {
				index++
				optionArgs = append(optionArgs, args[index])
			}
			continue
		}
		if strings.HasPrefix(argument, "-") {
			optionArgs = append(optionArgs, argument)
			continue
		}
		if sessionID != "" {
			return ChatOptions{}, fmt.Errorf("resume accepts at most one session ID")
		}
		sessionID = argument
	}

	options, err := parseChatOptions(optionArgs)
	if err != nil {
		return ChatOptions{}, err
	}
	if options.Resume != "" && sessionID != "" {
		return ChatOptions{}, fmt.Errorf("resume accepts at most one session ID")
	}
	if options.Resume == "" && sessionID == "" {
		options.selectResume = true
	} else if options.Resume == "" {
		options.Resume = sessionID
	}
	return options, nil
}

func resumeWorkspacePath(options ChatOptions) (string, error) {
	workspace := options.Workspace
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve current workspace: %w", err)
		}
	}
	return canonicalWorkspacePath(workspace)
}

func bufferedInput(input io.Reader) *bufio.Reader {
	if reader, ok := input.(*bufio.Reader); ok {
		return reader
	}
	return bufio.NewReader(input)
}

func selectResumeSession(input io.Reader, output io.Writer, store *SessionStore, workspace string) (string, error) {
	sessions, diagnostics, err := store.ListWorkspaceWithDiagnostics(workspace)
	if err != nil {
		return "", err
	}
	if diagnostics.SkippedFiles > 0 {
		if _, err := fmt.Fprintln(output, sessionListDiagnosticsWarning(diagnostics)); err != nil {
			return "", err
		}
	}
	if len(sessions) == 0 {
		return "", fmt.Errorf("no saved sessions")
	}

	if _, err := fmt.Fprintln(output, "Saved sessions:"); err != nil {
		return "", err
	}
	for index, session := range sessions {
		id := strings.ReplaceAll(sanitizeTerminalText(session.ID), "\n", " ")
		workspace := sessionWorkspaceLabel(session)
		summary := sessionListPreview(session)
		if _, err := fmt.Fprintf(output, "%d. %s  %s  %s\n   %s\n", index+1, id, session.UpdatedAt.Format(time.RFC3339), workspace, summary); err != nil {
			return "", err
		}
	}

	reader := bufferedInput(input)
	for {
		if _, err := fmt.Fprintf(output, "Select a session [1-%d] (or q to cancel): ", len(sessions)); err != nil {
			return "", err
		}
		line, readErr := reader.ReadString('\n')
		choice := strings.TrimSpace(line)
		if choice == "" && readErr == io.EOF {
			return "", fmt.Errorf("session selection cancelled")
		}
		if strings.EqualFold(choice, "q") {
			return "", fmt.Errorf("session selection cancelled")
		}
		selection, err := strconv.Atoi(choice)
		if err == nil && selection >= 1 && selection <= len(sessions) {
			return sessions[selection-1].ID, nil
		}
		if _, err := fmt.Fprintf(output, "Invalid session selection. Enter a number from 1 to %d, or q to cancel.\n", len(sessions)); err != nil {
			return "", err
		}
		if readErr != nil {
			if readErr == io.EOF {
				return "", fmt.Errorf("session selection cancelled")
			}
			return "", fmt.Errorf("read session selection: %w", readErr)
		}
	}
}

func runSessionsCommand(stdout io.Writer) error {
	paths, err := ResolveConfigPaths()
	if err != nil {
		return err
	}
	sessions, diagnostics, err := NewSessionStore(paths).ListWithDiagnostics()
	if err != nil {
		return err
	}
	if diagnostics.SkippedFiles > 0 {
		if _, err := fmt.Fprintln(stdout, sessionListDiagnosticsWarning(diagnostics)); err != nil {
			return err
		}
	}
	if len(sessions) == 0 {
		_, err = fmt.Fprintln(stdout, "No saved sessions.")
		return err
	}
	for _, session := range sessions {
		summary := sessionListPreview(session)
		if _, err := fmt.Fprintf(stdout, "%s  %s  %s  %s\n", session.ID, session.UpdatedAt.Format(time.RFC3339), sessionWorkspaceLabel(session), summary); err != nil {
			return err
		}
	}
	return nil
}

func sessionListDiagnosticsWarning(diagnostics SessionListDiagnostics) string {
	if diagnostics.SkippedFiles == 1 {
		return "Warning: skipped 1 unreadable or invalid saved session file."
	}
	return fmt.Sprintf("Warning: skipped %d unreadable or invalid saved session files.", diagnostics.SkippedFiles)
}

func sessionWorkspaceLabel(session Session) string {
	workspace := strings.ReplaceAll(sanitizeTerminalText(session.Workspace), "\n", " ")
	if session.WorkspaceUnavailable {
		return workspace + " [unavailable]"
	}
	return workspace
}

func sessionListPreview(session Session) string {
	preview := session.Summary
	if preview == "" {
		for index := len(session.Messages) - 1; index >= 0; index-- {
			if session.Messages[index].Role == "user" {
				preview = session.Messages[index].Content
				break
			}
		}
	}
	preview = strings.ReplaceAll(sanitizeTerminalText(preview), "\n", " ")
	if preview == "" {
		preview = "(untitled session)"
	}
	if runes := []rune(preview); len(runes) > 80 {
		preview = string(runes[:80]) + "..."
	}
	return preview
}

type sessionPickerModel struct {
	sessions []Session
	warning  string
	cursor   int
	offset   int
	width    int
	height   int
	selected string
}

func runSessionPicker(input, output *os.File, workspace string) (string, error) {
	paths, err := ResolveConfigPaths()
	if err != nil {
		return "", err
	}
	sessions, diagnostics, err := NewSessionStore(paths).ListWorkspaceWithDiagnostics(workspace)
	if err != nil {
		return "", err
	}
	if len(sessions) == 0 {
		if diagnostics.SkippedFiles > 0 {
			if _, err := fmt.Fprintln(output, sessionListDiagnosticsWarning(diagnostics)); err != nil {
				return "", err
			}
		}
		_, err := fmt.Fprintln(output, "No saved sessions.")
		return "", err
	}

	model := &sessionPickerModel{sessions: sessions}
	if diagnostics.SkippedFiles > 0 {
		model.warning = sessionListDiagnosticsWarning(diagnostics)
	}
	program := tea.NewProgram(model, tea.WithInput(input), tea.WithOutput(output), tea.WithoutSignalHandler())
	final, err := program.Run()
	if err != nil {
		return "", err
	}
	return final.(*sessionPickerModel).selected, nil
}

func (m *sessionPickerModel) Init() tea.Cmd { return nil }

func (m *sessionPickerModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.keepCursorVisible()
	case tea.KeyPressMsg:
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor+1 < len(m.sessions) {
				m.cursor++
			}
		case "pgup":
			m.cursor -= m.visibleRows()
			if m.cursor < 0 {
				m.cursor = 0
			}
		case "pgdown":
			m.cursor += m.visibleRows()
			if m.cursor >= len(m.sessions) {
				m.cursor = len(m.sessions) - 1
			}
		case "enter":
			m.selected = m.sessions[m.cursor].ID
			return m, tea.Quit
		case "esc", "q", "ctrl+c":
			return m, tea.Quit
		}
		m.keepCursorVisible()
	}
	return m, nil
}

func (m *sessionPickerModel) visibleRows() int {
	reservedRows := 6
	if m.warning != "" {
		reservedRows += 2
	}
	return max(1, m.height-reservedRows)
}

func (m *sessionPickerModel) keepCursorVisible() {
	rows := m.visibleRows()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+rows {
		m.offset = m.cursor - rows + 1
	}
}

func (m *sessionPickerModel) View() tea.View {
	var content strings.Builder
	content.WriteString(lipgloss.NewStyle().Bold(true).Render("Select a session to resume"))
	content.WriteString("\n")
	content.WriteString(tuiDimStyle.Render("↑/↓ Move  PgUp/PgDn Page  Enter Select  Esc Cancel"))
	content.WriteString("\n\n")
	if m.warning != "" {
		content.WriteString(tuiWarnStyle.Render(m.warning))
		content.WriteString("\n\n")
	}
	rows := m.visibleRows()
	end := min(len(m.sessions), m.offset+rows)
	for index := m.offset; index < end; index++ {
		prefix := "  "
		if index == m.cursor {
			prefix = "> "
		}
		session := m.sessions[index]
		preview := sessionListPreview(session)
		if session.WorkspaceUnavailable {
			preview += "  [unavailable]"
		}
		line := fmt.Sprintf("%s%s  %s  %s", prefix, session.ID, session.UpdatedAt.Format("2006-01-02 15:04"), preview)
		if index == m.cursor {
			line = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true).Render(line)
		}
		content.WriteString(line)
		content.WriteString("\n")
	}
	content.WriteString(tuiDimStyle.Render(fmt.Sprintf("%d/%d", m.cursor+1, len(m.sessions))))
	view := tea.NewView(content.String())
	view.AltScreen = true
	return view
}

type chatRuntime struct {
	store      *SessionStore
	session    *Session
	workspace  *Workspace
	agent      *Agent
	newSession bool
}

func newChatRuntime(
	ctx context.Context,
	paths ConfigPaths,
	settings Settings,
	options ChatOptions,
	newWorkspace func(string, bool) (*Workspace, error),
	getUserMessage func() (string, bool),
	output io.Writer,
) (*chatRuntime, error) {
	store := NewSessionStore(paths)
	var session *Session
	var err error
	if options.Resume != "" {
		session, err = store.Load(options.Resume)
		if err != nil {
			return nil, err
		}
		if !options.workspaceExplicit {
			options.Workspace = session.Workspace
		}
	}
	if options.Workspace == "" {
		options.Workspace, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve current workspace: %w", err)
		}
	}
	workspace, err := newWorkspace(options.Workspace, options.AutoApprove)
	if err != nil {
		return nil, err
	}
	if err := workspace.ProtectPath(paths.Home); err != nil {
		return nil, err
	}
	workspace.SetContext(ctx)
	if session != nil && session.Workspace != workspace.root {
		return nil, fmt.Errorf("session %s belongs to workspace %s, not %s", session.ID, session.Workspace, workspace.root)
	}
	if session == nil {
		session, err = store.New(workspace.root)
		if err != nil {
			return nil, err
		}
	}
	client := openai.NewClient(
		option.WithAPIKey(settings.APIKey),
		option.WithBaseURL(settings.BaseURL),
	)
	tools := workspace.ToolDefinitions()
	tools = append(tools, NewSessionTools(session, store).ToolDefinitions()...)
	agent := NewAgent(&client, getUserMessage, tools)
	agent.model = settings.Model
	agent.customProvider = isCustomBaseURL(settings.BaseURL)
	agent.maxProviderResponseBytes = settings.MaxProviderResponseBytes
	agent.maxCustomTurnInputBytes = int(settings.MaxCustomTurnInputBytes)
	agent.output = output
	agent.session = session
	agent.store = store
	return &chatRuntime{
		store:      store,
		session:    session,
		workspace:  workspace,
		agent:      agent,
		newSession: !session.resumed,
	}, nil
}

func (runtime *chatRuntime) deleteEmptyNewSession() {
	if runtime.newSession && len(runtime.session.Messages) == 0 {
		_ = runtime.store.Delete(runtime.session.ID)
	}
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
	settings, err = effectiveSettings(settings)
	if err != nil {
		return err
	}
	if settings.APIKey == "" {
		return fmt.Errorf("OPENAI_API_KEY is not configured; run \"meldra config init\" and add it to %s, or set OPENAI_API_KEY", paths.CredentialsFile)
	}
	providerWarning := providerCredentialWarning(settings)
	if shouldUseTUI(stdin, stdout) {
		return runTUIChat(ctx, stdin.(*os.File), stdout.(*os.File), paths, settings, options, providerWarning)
	}
	if providerWarning != "" {
		fmt.Fprintln(stdout, providerWarning)
	}

	reader := bufferedInput(stdin)
	var readErr error
	initialPrompt := options.Prompt
	getUserMessage := func() (string, bool) {
		if initialPrompt != "" {
			prompt := initialPrompt
			initialPrompt = ""
			return prompt, true
		}
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
	runtime, err := newChatRuntime(ctx, paths, settings, options, func(root string, autoApprove bool) (*Workspace, error) {
		return NewWorkspace(root, reader, stdout, autoApprove)
	}, getUserMessage, stdout)
	if err != nil {
		return err
	}
	defer runtime.deleteEmptyNewSession()

	fmt.Fprintf(stdout, "Session: %s\nWorkspace: %s\n", runtime.session.ID, sanitizeTerminalText(runtime.workspace.root))
	if err := runtime.agent.Run(ctx); err != nil {
		return err
	}
	return readErr
}

func effectiveSettings(settings Settings) (Settings, error) {
	if settings.Model == "" {
		settings.Model = defaultModel
	}
	if settings.BaseURL == "" {
		settings.BaseURL = defaultBaseURL
	}
	if settings.MaxProviderResponseBytes == 0 {
		settings.MaxProviderResponseBytes = defaultProviderResponseBytes
	}
	if settings.MaxCustomTurnInputBytes == 0 {
		settings.MaxCustomTurnInputBytes = defaultMaxCustomTurnInputBytes
	}
	settings, err := overlayEnvironment(settings)
	if err != nil {
		return Settings{}, err
	}
	if err := validateProviderBaseURL(settings.BaseURL, settings.AllowInsecureBaseURL); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

func overlayEnvironment(settings Settings) (Settings, error) {
	if value := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); value != "" {
		settings.APIKey = value
	}
	if value := strings.TrimSpace(os.Getenv("OPENAI_MODEL")); value != "" {
		settings.Model = value
	}
	if value := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")); value != "" {
		settings.BaseURL = value
	}
	if value := strings.TrimSpace(os.Getenv(ProviderResponseLimitEnv)); value != "" {
		limit, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return Settings{}, fmt.Errorf("%s must be an integer: %w", ProviderResponseLimitEnv, err)
		}
		if err := validateProviderResponseLimit(limit); err != nil || limit == 0 {
			if err == nil {
				err = fmt.Errorf("must be positive")
			}
			return Settings{}, fmt.Errorf("%s: %w", ProviderResponseLimitEnv, err)
		}
		settings.MaxProviderResponseBytes = limit
	}
	if value := strings.TrimSpace(os.Getenv(CustomTurnInputLimitEnv)); value != "" {
		limit, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return Settings{}, fmt.Errorf("%s must be an integer: %w", CustomTurnInputLimitEnv, err)
		}
		if err := validateCustomTurnInputLimit(limit); err != nil || limit == 0 {
			if err == nil {
				err = fmt.Errorf("must be positive")
			}
			return Settings{}, fmt.Errorf("%s: %w", CustomTurnInputLimitEnv, err)
		}
		settings.MaxCustomTurnInputBytes = limit
	}
	return settings, nil
}

const usageText = `Usage:
  meldra [options]               Start a chat in a bounded workspace.
  meldra resume [session-id]     Select a saved session, or resume the specified session.
  meldra sessions                List saved sessions.
  meldra config init             Create ~/.meldra configuration files.
  meldra config [show]           Show effective configuration without secrets.
  meldra version                 Print the installed version.

Options:
  --workspace PATH               Restrict all file and command tools to PATH.
  --resume ID                    Resume ID (or "latest") in its saved workspace.
  --prompt TEXT                  Start with a non-interactive prompt; stdin is still read for follow-ups.
  --yes                          Skip approvals; use only in an isolated container or VM.

Configuration:
  Meldra reads ~/.meldra/config.toml and ~/.meldra/credentials.env by default.
  Set MELDRA_HOME to use another configuration directory. OPENAI_* environment
  variables override file values. Meldra never automatically loads a project's .env.
`

const configUsageText = `Usage:
  meldra config init
  meldra config [show]
`

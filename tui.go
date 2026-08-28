package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

const (
	tuiHeaderHeight      = 1
	tuiFooterHeight      = 1
	tuiLayoutSeparators  = 3
	tuiStreamFrameDelay  = time.Second / 30
	tuiMaxVisibleEntries = 200
)

var (
	tuiBrandStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	tuiDimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("246"))
	tuiUserStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("75"))
	tuiAgentStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("221"))
	tuiToolStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("44"))
	tuiErrorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	tuiWarnStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	tuiInputStyle = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
)

// tuiInputStyles keeps the composer background transparent so text always
// remains legible against the user's terminal theme. Once Bubble Tea reports
// the terminal background, use its contrast-aware palette; until then, leave
// text at the terminal's default foreground.
func tuiInputStyles(darkBackground *bool) textarea.Styles {
	text := lipgloss.NewStyle()
	placeholder := tuiDimStyle
	prompt := tuiBrandStyle
	cursorColor := lipgloss.Color("39")
	if darkBackground != nil {
		if *darkBackground {
			text = lipgloss.NewStyle().Foreground(lipgloss.Color("#f8fafc"))
			placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color("#94a3b8"))
			prompt = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#60a5fa"))
			cursorColor = lipgloss.Color("#60a5fa")
		} else {
			text = lipgloss.NewStyle().Foreground(lipgloss.Color("#1f2937"))
			placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color("#64748b"))
			prompt = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#1d4ed8"))
			cursorColor = lipgloss.Color("#1d4ed8")
		}
	}

	state := textarea.StyleState{
		Text:        text,
		Placeholder: placeholder,
		Prompt:      prompt,
	}
	return textarea.Styles{
		Focused: state,
		Blurred: state,
		Cursor: textarea.CursorStyle{
			Color: cursorColor,
			Shape: tea.CursorBlock,
			Blink: true,
		},
	}
}

// tuiController is the concurrency boundary between the synchronous agent and
// Bubble Tea's event loop. Agent and workspace goroutines never mutate the UI
// model directly.
type tuiController struct {
	messages chan string
	done     chan struct{}
	ready    chan struct{}
	cancel   context.CancelFunc
	program  *tea.Program

	readyOnce   sync.Once
	closeOnce   sync.Once
	dispatchMu  sync.Mutex
	deltaMu     sync.Mutex
	deltas      strings.Builder
	deltaTimer  *time.Timer
	deltaActive bool
}

type tuiEventMsg struct {
	event UIEvent
}

type tuiApprovalMsg struct {
	request ApprovalRequest
	answer  chan bool
}

type tuiApprovalPreviewMsg struct {
	request ApprovalRequest
}

type tuiAgentStoppedMsg struct {
	err error
}

type tuiInitialState struct {
	workspace string
	sessionID string
	model     string
	messages  []SessionMessage
	notices   []string
}

// tuiUserMessageSource supplies --prompt before waiting for interactive TUI
// input. Agent.Run calls its message source serially, so the closure does not
// need additional synchronization.
func tuiUserMessageSource(initialPrompt string, next func() (string, bool)) func() (string, bool) {
	return func() (string, bool) {
		if initialPrompt != "" {
			prompt := initialPrompt
			initialPrompt = ""
			return prompt, true
		}
		return next()
	}
}

func newTUIController(cancel context.CancelFunc) *tuiController {
	return &tuiController{
		messages: make(chan string, 1),
		done:     make(chan struct{}),
		ready:    make(chan struct{}),
		cancel:   cancel,
	}
}

func (t *tuiController) markReady() {
	t.readyOnce.Do(func() { close(t.ready) })
}

func (t *tuiController) stop() {
	t.closeOnce.Do(func() {
		close(t.done)
		t.deltaMu.Lock()
		if t.deltaTimer != nil {
			t.deltaTimer.Stop()
			t.deltaTimer = nil
		}
		t.deltas.Reset()
		t.deltaActive = false
		t.deltaMu.Unlock()
		if t.cancel != nil {
			t.cancel()
		}
	})
}

func (t *tuiController) Emit(event UIEvent) {
	if event.Kind == UIEventAssistantDelta {
		t.queueAssistantDelta(event.Text)
		return
	}
	t.send(tuiEventMsg{event: event})
	if event.Kind == UIEventAssistantDone {
		t.resetAssistantDeltaState()
	}
}

func (t *tuiController) send(message tea.Msg) {
	t.dispatchMu.Lock()
	defer t.dispatchMu.Unlock()
	t.flushAssistantDeltasLocked()
	t.sendRaw(message)
}

func (t *tuiController) sendRaw(message tea.Msg) {
	select {
	case <-t.done:
		return
	default:
	}
	if t.program != nil {
		t.program.Send(message)
	}
}

func (t *tuiController) queueAssistantDelta(delta string) {
	if delta == "" {
		return
	}
	select {
	case <-t.done:
		return
	default:
	}
	t.dispatchMu.Lock()
	defer t.dispatchMu.Unlock()
	t.deltaMu.Lock()
	select {
	case <-t.done:
		t.deltaMu.Unlock()
		return
	default:
	}
	if !t.deltaActive {
		t.deltaActive = true
		t.deltaMu.Unlock()
		// Render the first token immediately. This makes short replies visibly
		// stream even when all later deltas arrive within one frame.
		t.sendRaw(tuiEventMsg{event: UIEvent{Kind: UIEventAssistantDelta, Text: delta}})
		return
	}
	t.deltas.WriteString(delta)
	if t.deltaTimer == nil {
		t.deltaTimer = time.AfterFunc(tuiStreamFrameDelay, t.flushAssistantDeltas)
	}
	t.deltaMu.Unlock()
}

func (t *tuiController) resetAssistantDeltaState() {
	t.deltaMu.Lock()
	t.deltaActive = false
	t.deltaMu.Unlock()
}

func (t *tuiController) flushAssistantDeltas() {
	t.dispatchMu.Lock()
	defer t.dispatchMu.Unlock()
	t.flushAssistantDeltasLocked()
}

func (t *tuiController) flushAssistantDeltasLocked() {
	t.deltaMu.Lock()
	if t.deltaTimer != nil {
		t.deltaTimer.Stop()
		t.deltaTimer = nil
	}
	delta := t.deltas.String()
	t.deltas.Reset()
	t.deltaMu.Unlock()
	if delta != "" {
		t.sendRaw(tuiEventMsg{event: UIEvent{Kind: UIEventAssistantDelta, Text: delta}})
	}
}

func (t *tuiController) nextMessage() (string, bool) {
	select {
	case <-t.done:
		return "", false
	default:
	}
	select {
	case message := <-t.messages:
		select {
		case <-t.done:
			return "", false
		default:
		}
		return message, true
	case <-t.done:
		return "", false
	}
}

func (t *tuiController) approve(ctx context.Context, request ApprovalRequest) bool {
	answer := make(chan bool, 1)
	t.send(tuiApprovalMsg{request: request, answer: answer})
	select {
	case approved := <-answer:
		return approved
	case <-ctx.Done():
		return false
	case <-t.done:
		return false
	}
}

func (t *tuiController) presentApproval(request ApprovalRequest) {
	t.send(tuiApprovalPreviewMsg{request: request})
}

func (t *tuiController) run(ctx context.Context, input io.Reader, output io.Writer, initial tuiInitialState, startAgent func()) error {
	model := newTUIModel(t, initial)
	t.program = tea.NewProgram(
		model,
		tea.WithInput(input),
		tea.WithOutput(output),
		tea.WithContext(ctx),
		tea.WithoutSignalHandler(),
	)

	result := make(chan error, 1)
	go func() {
		_, err := t.program.Run()
		result <- err
	}()

	select {
	case <-t.ready:
		if ctx.Err() == nil {
			startAgent()
		} else {
			t.stop()
		}
	case <-ctx.Done():
		t.stop()
	case err := <-result:
		t.stop()
		if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, tea.ErrInterrupted) {
			return nil
		}
		return err
	}

	err := <-result
	t.stop()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, tea.ErrInterrupted) {
		return nil
	}
	return err
}

type tuiEntryKind uint8

const (
	tuiEntryUser tuiEntryKind = iota
	tuiEntryAssistant
	tuiEntryTool
	tuiEntryNotice
	tuiEntryError
)

type tuiEntry struct {
	kind        tuiEntryKind
	text        string
	stream      []byte
	name        string
	detail      string
	active      bool
	cachedWidth int
	cached      string
}

func (e *tuiEntry) content() string {
	if e.stream != nil {
		return string(e.stream)
	}
	return e.text
}

func (e *tuiEntry) appendStream(text string) {
	e.stream = append(e.stream, text...)
	e.cached = ""
}

func (e *tuiEntry) finishStream() {
	if e.stream != nil {
		e.text = string(e.stream)
		e.stream = nil
	}
	e.active = false
	e.cached = ""
}

type tuiModel struct {
	controller *tuiController
	workspace  string
	sessionID  string
	modelName  string

	width  int
	height int

	viewport      viewport.Model
	input         textarea.Model
	entries       []tuiEntry
	hiddenEntries int
	metrics       UIMetrics

	activeAssistant int
	activeTool      int
	status          string
	busy            bool
	// submissionPending prevents a Ready event from an earlier agent turn
	// from reopening the composer after Enter has already submitted input.
	submissionPending bool
	stopped           bool
	pending           *tuiApprovalMsg

	completedPrefix        string
	completedPrefixWidth   int
	completedPrefixEntries int
	completedPrefixValid   bool
	completedPrefixBuilds  uint64
}

func newTUIModel(controller *tuiController, initial tuiInitialState) *tuiModel {
	input := textarea.New()
	input.Prompt = "> "
	input.Placeholder = "Describe the change you want"
	input.SetStyles(tuiInputStyles(nil))
	input.ShowLineNumbers = false
	input.DynamicHeight = true
	input.MaxHeight = 4
	input.SetHeight(1)

	activity := viewport.New()
	// Pre-wrap content before it reaches the viewport. Its SoftWrap path
	// repeatedly truncates a whole long line while scrolling to the bottom,
	// which becomes quadratic for a large streamed response.
	activity.SoftWrap = false
	activity.MouseWheelEnabled = true

	entries := make([]tuiEntry, 0, len(initial.messages))
	for _, message := range initial.messages {
		kind := tuiEntryNotice
		switch message.Role {
		case "user":
			kind = tuiEntryUser
		case "assistant":
			kind = tuiEntryAssistant
		}
		entries = append(entries, tuiEntry{kind: kind, text: message.Content})
	}
	for _, notice := range initial.notices {
		if notice != "" {
			entries = append(entries, tuiEntry{kind: tuiEntryNotice, text: notice})
		}
	}

	return &tuiModel{
		controller:      controller,
		workspace:       initial.workspace,
		sessionID:       initial.sessionID,
		modelName:       initial.model,
		viewport:        activity,
		input:           input,
		entries:         entries,
		activeAssistant: -1,
		activeTool:      -1,
		status:          "Starting",
	}
}

func (m *tuiModel) Init() tea.Cmd {
	return tea.Batch(
		m.input.Focus(),
		tea.RequestBackgroundColor,
		func() tea.Msg {
			m.controller.markReady()
			return nil
		},
	)
}

func (m *tuiModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resize()
		return m, nil
	case tea.BackgroundColorMsg:
		darkBackground := msg.IsDark()
		m.input.SetStyles(tuiInputStyles(&darkBackground))
		return m, nil
	case tuiEventMsg:
		return m, m.applyEvent(msg.event)
	case tuiApprovalMsg:
		if m.pending != nil {
			msg.answer <- false
			return m, nil
		}
		m.pending = &msg
		m.busy = true
		m.status = "Approval required"
		m.input.Blur()
		m.refreshViewport()
		m.viewport.GotoBottom()
		return m, nil
	case tuiApprovalPreviewMsg:
		m.addEntry(tuiEntry{
			kind: tuiEntryNotice,
			text: "Auto-approved: " + msg.request.Title + "\n" + msg.request.Detail,
		})
		m.viewport.GotoBottom()
		return m, nil
	case tuiAgentStoppedMsg:
		m.stopped = true
		m.busy = true
		m.submissionPending = false
		m.input.Blur()
		if m.activeAssistant >= 0 {
			m.entries[m.activeAssistant].finishStream()
			m.activeAssistant = -1
		}
		if m.activeTool >= 0 {
			m.entries[m.activeTool].active = false
			m.activeTool = -1
		}
		m.invalidateCompletedPrefix()
		if msg.err != nil {
			m.status = "Stopped after an error"
			m.addEntry(tuiEntry{kind: tuiEntryError, text: msg.err.Error()})
		} else {
			m.status = "Stopped"
		}
		return m, nil
	case tea.KeyPressMsg:
		return m, m.handleKey(msg)
	case tea.MouseWheelMsg:
		var command tea.Cmd
		m.viewport, command = m.viewport.Update(msg)
		return m, command
	}

	if !m.busy && m.pending == nil && !m.stopped {
		var command tea.Cmd
		m.input, command = m.input.Update(message)
		if _, pasted := message.(tea.PasteMsg); pasted {
			m.resize()
		}
		return m, command
	}
	var command tea.Cmd
	m.viewport, command = m.viewport.Update(message)
	return m, command
}

func (m *tuiModel) handleKey(key tea.KeyPressMsg) tea.Cmd {
	if key.String() == "ctrl+c" {
		m.resolveApproval(false)
		m.controller.stop()
		return tea.Quit
	}
	if key.String() == "pgup" || key.String() == "pgdown" {
		var command tea.Cmd
		m.viewport, command = m.viewport.Update(key)
		return command
	}

	if m.pending != nil {
		switch key.String() {
		case "y":
			m.resolveApproval(true)
		case "n", "esc", "enter":
			m.resolveApproval(false)
		}
		return nil
	}
	if m.busy || m.stopped {
		return nil
	}

	switch key.String() {
	case "enter":
		message := m.input.Value()
		if strings.TrimSpace(message) == "" {
			return nil
		}
		m.input.Reset()
		m.resize()
		m.input.Blur()
		m.busy = true
		m.status = "Sending"
		m.submissionPending = true
		select {
		case m.controller.messages <- message:
		case <-m.controller.done:
		}
		return nil
	case "alt+enter", "shift+enter":
		m.input.InsertString("\n")
		m.resize()
		return nil
	}

	var command tea.Cmd
	m.input, command = m.input.Update(key)
	m.resize()
	return command
}

func (m *tuiModel) resolveApproval(approved bool) {
	if m.pending == nil {
		return
	}
	m.pending.answer <- approved
	m.pending = nil
	m.status = "Working"
	m.refreshViewport()
}

func (m *tuiModel) applyEvent(event UIEvent) tea.Cmd {
	switch event.Kind {
	case UIEventStatus:
		if event.Text == "Ready" && m.submissionPending {
			// The initial Ready can race with the Enter key because both are
			// delivered to Bubble Tea from different goroutines. Keep the
			// submitted turn busy until its Thinking event arrives.
			return nil
		}
		if event.Text != "Ready" {
			m.submissionPending = false
		}
		m.status = event.Text
		if event.Text == "Ready" && !m.stopped && m.pending == nil {
			m.busy = false
			return m.input.Focus()
		}
		m.busy = event.Text != "Ready"
		if m.busy {
			m.input.Blur()
		}
	case UIEventUserMessage:
		m.activeAssistant = -1
		m.addEntry(tuiEntry{kind: tuiEntryUser, text: event.Text})
	case UIEventAssistantDelta:
		m.submissionPending = false
		m.status = "Streaming"
		m.busy = true
		m.input.Blur()
		if m.activeAssistant < 0 {
			m.appendEntry(tuiEntry{kind: tuiEntryAssistant, stream: make([]byte, 0, len(event.Text)), active: true})
			m.activeAssistant = len(m.entries) - 1
		}
		m.entries[m.activeAssistant].appendStream(event.Text)
		m.refreshViewport()
	case UIEventAssistantMessage:
		m.submissionPending = false
		m.activeAssistant = -1
		m.addEntry(tuiEntry{kind: tuiEntryAssistant, text: event.Text})
	case UIEventAssistantDone:
		m.submissionPending = false
		if m.activeAssistant >= 0 {
			m.entries[m.activeAssistant].finishStream()
		}
		m.activeAssistant = -1
		m.invalidateCompletedPrefix()
		m.refreshViewport()
	case UIEventToolStarted:
		m.submissionPending = false
		m.activeAssistant = -1
		m.appendEntry(tuiEntry{kind: tuiEntryTool, name: event.Name, active: true})
		m.activeTool = len(m.entries) - 1
		m.status = "Running " + event.Name
		m.busy = true
		m.refreshViewport()
	case UIEventToolFinished:
		if m.activeTool >= 0 && m.entries[m.activeTool].name == event.Name {
			m.entries[m.activeTool].active = false
			m.entries[m.activeTool].detail = event.Detail
			m.entries[m.activeTool].cached = ""
		} else {
			m.appendEntry(tuiEntry{kind: tuiEntryTool, name: event.Name, detail: event.Detail})
		}
		m.activeTool = -1
		m.invalidateCompletedPrefix()
		// The tool result is now being sent back to the model. Gateways do not
		// always emit a new response.in_progress event for that follow-up, so
		// update the visible state here.
		m.status = "Thinking"
		m.busy = true
		m.input.Blur()
		m.refreshViewport()
	case UIEventMetrics:
		if event.Metrics != nil {
			m.metrics = *event.Metrics
		}
	case UIEventNotice:
		m.addEntry(tuiEntry{kind: tuiEntryNotice, text: event.Text})
	case UIEventError:
		m.addEntry(tuiEntry{kind: tuiEntryError, text: event.Text})
	}
	return nil
}

func (m *tuiModel) addEntry(entry tuiEntry) {
	m.appendEntry(entry)
	m.refreshViewport()
}

func (m *tuiModel) appendEntry(entry tuiEntry) {
	m.invalidateCompletedPrefix()
	if len(m.entries) >= tuiMaxVisibleEntries {
		m.entries = append(m.entries[:0], m.entries[1:]...)
		m.hiddenEntries++
		if m.activeAssistant >= 0 {
			m.activeAssistant--
		}
		if m.activeTool >= 0 {
			m.activeTool--
		}
	}
	m.entries = append(m.entries, entry)
}

func (m *tuiModel) resize() {
	width := max(12, m.width-2)
	m.input.SetWidth(width - tuiInputStyle.GetHorizontalFrameSize())
	m.viewport.SetWidth(width)
	composerHeight := m.input.Height() + tuiInputStyle.GetVerticalFrameSize()
	m.viewport.SetHeight(max(1, m.height-tuiHeaderHeight-tuiFooterHeight-tuiLayoutSeparators-composerHeight))
	m.refreshViewport()
}

func (m *tuiModel) refreshViewport() {
	wasAtBottom := m.viewport.AtBottom()
	m.viewport.SetContent(m.renderViewportTimeline())
	if wasAtBottom {
		m.viewport.GotoBottom()
	}
}

func (m *tuiModel) renderViewportTimeline() string {
	width := m.viewport.Width()
	if width <= 0 {
		return m.renderTimeline()
	}
	if !m.completedPrefixValid || m.completedPrefixWidth != width {
		var prefix strings.Builder
		if m.hiddenEntries > 0 {
			prefix.WriteString(ansi.Hardwrap(tuiDimStyle.Render(fmt.Sprintf("%d older UI entries hidden; saved session is unaffected", m.hiddenEntries)), width, true))
		}
		m.completedPrefixEntries = 0
		for m.completedPrefixEntries < len(m.entries) && !m.entries[m.completedPrefixEntries].active {
			if prefix.Len() > 0 {
				prefix.WriteString("\n\n")
			}
			prefix.WriteString(m.renderEntry(&m.entries[m.completedPrefixEntries], width))
			m.completedPrefixEntries++
		}
		m.completedPrefix = prefix.String()
		m.completedPrefixWidth = width
		m.completedPrefixValid = true
		m.completedPrefixBuilds++
	}
	var output strings.Builder
	output.WriteString(m.completedPrefix)
	for index := m.completedPrefixEntries; index < len(m.entries); index++ {
		if output.Len() > 0 {
			output.WriteString("\n\n")
		}
		output.WriteString(m.renderEntry(&m.entries[index], width))
	}
	if pending := m.renderPending(); pending != "" {
		if output.Len() > 0 {
			output.WriteString("\n\n")
		}
		output.WriteString(ansi.Hardwrap(pending, width, true))
	}
	return output.String()
}

func (m *tuiModel) invalidateCompletedPrefix() {
	m.completedPrefixValid = false
}

func (m *tuiModel) renderTimeline() string {
	var output strings.Builder
	for index, entry := range m.entries {
		if index > 0 {
			output.WriteString("\n\n")
		}
		output.WriteString(m.renderEntryUnwrapped(&entry))
	}
	if pending := m.renderPending(); pending != "" {
		if output.Len() > 0 {
			output.WriteString("\n\n")
		}
		output.WriteString(pending)
	}
	return output.String()
}

func (m *tuiModel) renderEntry(entry *tuiEntry, width int) string {
	if !entry.active && entry.cached != "" && entry.cachedWidth == width {
		return entry.cached
	}
	rendered := ansi.Hardwrap(m.renderEntryUnwrapped(entry), width, true)
	if !entry.active {
		entry.cached = rendered
		entry.cachedWidth = width
	}
	return rendered
}

func (m *tuiModel) renderEntryUnwrapped(entry *tuiEntry) string {
	var output strings.Builder
	switch entry.kind {
	case tuiEntryUser:
		output.WriteString(tuiUserStyle.Render("You"))
		output.WriteString("\n")
		output.WriteString(sanitizeTerminalText(entry.content()))
	case tuiEntryAssistant:
		output.WriteString(tuiAgentStyle.Render("Meldra"))
		if entry.active {
			output.WriteString(tuiDimStyle.Render("  streaming"))
		}
		output.WriteString("\n")
		output.WriteString(sanitizeTerminalText(entry.content()))
	case tuiEntryTool:
		state := "done"
		if entry.active {
			state = "working"
		}
		output.WriteString(tuiToolStyle.Render("tool  " + sanitizeTerminalText(entry.name) + "  " + state))
		if entry.detail != "" {
			output.WriteString("\n")
			output.WriteString(tuiDimStyle.Render(sanitizeTerminalText(entry.detail)))
		}
	case tuiEntryNotice:
		output.WriteString(tuiDimStyle.Render(sanitizeTerminalText(entry.content())))
	case tuiEntryError:
		output.WriteString(tuiErrorStyle.Render("Error: " + sanitizeTerminalText(entry.content())))
	}
	return output.String()
}

func (m *tuiModel) renderPending() string {
	var output strings.Builder
	if m.pending != nil {
		output.WriteString(tuiWarnStyle.Render(sanitizeTerminalText(m.pending.request.Title)))
		if m.pending.request.Detail != "" {
			output.WriteString("\n")
			output.WriteString(sanitizeTerminalText(m.pending.request.Detail))
		}
		output.WriteString("\n")
		output.WriteString(tuiDimStyle.Render("[y] approve   [n] reject   [enter/esc] reject"))
	}
	return output.String()
}

func (m *tuiModel) View() tea.View {
	if m.width == 0 || m.height == 0 {
		view := tea.NewView("Starting Meldra...")
		view.AltScreen = true
		view.MouseMode = tea.MouseModeNone
		return view
	}

	workspace := sanitizeTerminalText(filepath.Base(m.workspace))
	header := tuiBrandStyle.Render("MELDRA") + tuiDimStyle.Render("  "+sanitizeTerminalText(version)+"  "+workspace+"  "+sanitizeTerminalText(m.modelName))
	if m.sessionID != "" {
		header += tuiDimStyle.Render("  session " + sanitizeTerminalText(m.sessionID))
	}

	composer := ""
	switch {
	case m.pending != nil:
		composer = tuiWarnStyle.Render("Approval is waiting above")
	case m.stopped:
		composer = tuiDimStyle.Render("The agent stopped. Press Ctrl-C to exit.")
	case m.busy:
		composer = tuiDimStyle.Render("Working... Ctrl-C cancels and saves the session.")
	default:
		composer = tuiInputStyle.Width(max(12, m.width-2)).Render(m.input.View())
	}

	footerText := sanitizeTerminalText(m.status)
	if m.metrics.InferenceLimit > 0 {
		footerText += fmt.Sprintf("  steps %d/%d  tools %d/%d", m.metrics.InferenceSteps, m.metrics.InferenceLimit, m.metrics.ToolCalls, m.metrics.ToolCallLimit)
		if m.metrics.ContextBytes > 0 {
			footerText += fmt.Sprintf("  ctx %.1f KiB", float64(m.metrics.ContextBytes)/1024)
		}
		if m.metrics.InputTokens > 0 || m.metrics.OutputTokens > 0 {
			footerText += fmt.Sprintf("  tokens %d↓/%d↑", m.metrics.InputTokens, m.metrics.OutputTokens)
		}
	}
	footerText += "  |  "
	if m.pending != nil {
		footerText += "PgUp/PgDn scroll  y approve  n/Enter/Esc reject  Ctrl-C exit"
	} else {
		footerText += "Enter send  Alt+Enter newline  PgUp/PgDn scroll  Ctrl-C exit"
	}
	footer := tuiDimStyle.Render(footerText)
	content := strings.Join([]string{header, m.viewport.View(), composer, footer}, "\n")
	view := tea.NewView(content)
	view.AltScreen = true
	// Leave mouse tracking disabled so the terminal can natively select and
	// copy rendered output. Keyboard PgUp/PgDn remains available for scroll.
	view.MouseMode = tea.MouseModeNone
	return view
}

func shouldUseTUI(stdin io.Reader, stdout io.Writer) bool {
	if terminalIsDumb() {
		return false
	}
	input, inputOK := stdin.(*os.File)
	output, outputOK := stdout.(*os.File)
	if !inputOK || !outputOK {
		return false
	}
	if _, err := input.Stat(); err != nil {
		return false
	}
	if _, err := output.Stat(); err != nil {
		return false
	}
	return term.IsTerminal(input.Fd()) && term.IsTerminal(output.Fd())
}

func terminalIsDumb() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb")
}

func newTUIWorkspace(root string, autoApprove bool) (*Workspace, error) {
	// TUI confirmations are routed through controller.approve. Keep a non-nil
	// fallback reader so an unexpected fallback fails closed at EOF.
	return NewWorkspace(root, bufio.NewReader(strings.NewReader("")), io.Discard, autoApprove)
}

func runTUIChat(ctx context.Context, stdin *os.File, stdout *os.File, paths ConfigPaths, settings Settings, options ChatOptions, providerWarning string) error {
	chatCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	controller := newTUIController(cancel)
	runtime, err := newChatRuntime(chatCtx, paths, settings, options, newTUIWorkspace, tuiUserMessageSource(options.Prompt, controller.nextMessage), stdout)
	if err != nil {
		return err
	}
	defer runtime.deleteEmptyNewSession()
	runtime.workspace.SetApprovalFunc(controller.approve)
	runtime.workspace.SetApprovalPresenter(controller.presentApproval)
	runtime.agent.events = controller

	agentDone := make(chan error, 1)
	started := false
	uiErr := controller.run(chatCtx, stdin, stdout, tuiInitialState{
		workspace: runtime.workspace.root,
		sessionID: runtime.session.ID,
		model:     settings.Model,
		messages:  append([]SessionMessage(nil), runtime.session.Messages...),
		notices:   []string{providerWarning},
	}, func() {
		started = true
		go func() {
			agentErr := runtime.agent.Run(chatCtx)
			if agentErr != nil && chatCtx.Err() == nil {
				controller.send(tuiAgentStoppedMsg{err: agentErr})
			}
			agentDone <- agentErr
		}()
	})
	var agentErr error
	if started {
		agentErr = <-agentDone
	}
	if uiErr != nil {
		return uiErr
	}
	if agentErr != nil {
		return agentErr
	}
	if started && chatCtx.Err() != nil {
		_, err := fmt.Fprintf(stdout, "Interrupted. Session %s was saved; resume with: meldra resume %s\n", runtime.session.ID, runtime.session.ID)
		return err
	}
	return nil
}

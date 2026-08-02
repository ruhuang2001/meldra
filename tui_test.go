package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/openai/openai-go/v3/responses"
)

type observingTUIModel struct {
	model  *tuiModel
	events chan UIEvent
}

func (m *observingTUIModel) Init() tea.Cmd {
	return m.model.Init()
}

func (m *observingTUIModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	_, command := m.model.Update(message)
	if event, ok := message.(tuiEventMsg); ok {
		select {
		case m.events <- event.event:
		default:
		}
	}
	return m, command
}

func (m *observingTUIModel) View() tea.View {
	return m.model.View()
}

func TestTUIModelAccumulatesAndCompletesAssistantDeltas(t *testing.T) {
	model := newTUIModel(newTUIController(nil), tuiInitialState{
		workspace: "/tmp/project",
		sessionID: "session_1",
		model:     "gpt-test",
	})
	model.width = 80
	model.height = 24
	model.resize()

	model.applyEvent(UIEvent{Kind: UIEventUserMessage, Text: "show progress"})
	model.applyEvent(UIEvent{Kind: UIEventAssistantDelta, Text: "first "})
	model.applyEvent(UIEvent{Kind: UIEventAssistantDelta, Text: "second"})
	if model.status != "Streaming" || !model.busy {
		t.Fatalf("streaming state = status %q, busy %v", model.status, model.busy)
	}

	if model.activeAssistant != 1 {
		t.Fatalf("active assistant index = %d, want 1", model.activeAssistant)
	}
	entry := model.entries[model.activeAssistant]
	if entry.text != "first second" || !entry.active {
		t.Fatalf("active assistant entry = %#v", entry)
	}

	model.applyEvent(UIEvent{Kind: UIEventAssistantDone})
	if model.activeAssistant != -1 {
		t.Fatalf("active assistant index = %d after completion", model.activeAssistant)
	}
	if model.entries[1].active {
		t.Fatalf("assistant entry remains active: %#v", model.entries[1])
	}
	if timeline := model.renderTimeline(); !strings.Contains(timeline, "first second") {
		t.Fatalf("timeline does not include streamed reply: %q", timeline)
	}
}

func TestTUIAgentStopClosesActiveEntries(t *testing.T) {
	model := newTUIModel(newTUIController(nil), tuiInitialState{})
	model.entries = []tuiEntry{
		{kind: tuiEntryAssistant, text: "partial", active: true},
		{kind: tuiEntryTool, name: "run_command", active: true},
	}
	model.activeAssistant = 0
	model.activeTool = 1

	model.Update(tuiAgentStoppedMsg{err: errors.New("stream disconnected")})

	if model.entries[0].active || model.entries[1].active {
		t.Fatalf("active entries were not closed: %#v", model.entries)
	}
	if model.activeAssistant != -1 || model.activeTool != -1 {
		t.Fatalf("active indexes = assistant %d tool %d", model.activeAssistant, model.activeTool)
	}
	if model.status != "Stopped after an error" {
		t.Fatalf("status = %q", model.status)
	}
}

func TestNewTUIWorkspaceProvidesAClosedFallbackReader(t *testing.T) {
	workspace, err := newTUIWorkspace(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.input == nil {
		t.Fatal("TUI workspace did not receive a fallback reader")
	}
	if workspace.confirmPrompt("Apply changes? [y/N] ") {
		t.Fatal("empty TUI fallback reader approved a change")
	}
}

func TestTUIKeepsNonEmptyInputVerbatim(t *testing.T) {
	controller := newTUIController(nil)
	model := newTUIModel(controller, tuiInitialState{})
	model.input.SetValue("  preserve this  ")

	model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))

	select {
	case message := <-controller.messages:
		if message != "  preserve this  " {
			t.Fatalf("sent message = %q", message)
		}
	default:
		t.Fatal("non-empty input was not sent")
	}
}

func TestTUIIgnoresStaleReadyAfterSubmit(t *testing.T) {
	controller := newTUIController(nil)
	model := newTUIModel(controller, tuiInitialState{})
	model.input.SetValue("request")

	model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if !model.busy || !model.submissionPending || model.status != "Sending" {
		t.Fatalf("submitted state = busy %v, pending %v, status %q", model.busy, model.submissionPending, model.status)
	}

	// A Ready emitted during startup may be delivered after the Enter key.
	model.applyEvent(UIEvent{Kind: UIEventStatus, Text: "Ready"})
	if !model.busy || !model.submissionPending || model.status != "Sending" {
		t.Fatalf("stale Ready reopened composer: busy %v, pending %v, status %q", model.busy, model.submissionPending, model.status)
	}

	model.applyEvent(UIEvent{Kind: UIEventStatus, Text: "Thinking"})
	if !model.busy || model.submissionPending || model.status != "Thinking" {
		t.Fatalf("Thinking state = busy %v, pending %v, status %q", model.busy, model.submissionPending, model.status)
	}

	model.applyEvent(UIEvent{Kind: UIEventAssistantDone})
	model.applyEvent(UIEvent{Kind: UIEventStatus, Text: "Ready"})
	if model.busy || model.submissionPending || model.status != "Ready" {
		t.Fatalf("post-response Ready state = busy %v, pending %v, status %q", model.busy, model.submissionPending, model.status)
	}
}

func TestTUIToolCompletionReturnsToThinking(t *testing.T) {
	model := newTUIModel(newTUIController(nil), tuiInitialState{})
	model.width = 80
	model.height = 24
	model.resize()

	model.applyEvent(UIEvent{Kind: UIEventToolStarted, Name: "list_files"})
	if model.status != "Running list_files" || !model.busy {
		t.Fatalf("tool start state = status %q, busy %v", model.status, model.busy)
	}

	model.applyEvent(UIEvent{Kind: UIEventToolFinished, Name: "list_files", Detail: "[\"main.go\"]"})
	if model.status != "Thinking" || !model.busy {
		t.Fatalf("tool finish state = status %q, busy %v", model.status, model.busy)
	}
	if model.activeTool != -1 || model.entries[0].active {
		t.Fatalf("tool entry remained active: %#v", model.entries)
	}
	if !strings.Contains(model.renderTimeline(), "done") {
		t.Fatalf("completed tool was not rendered as done: %q", model.renderTimeline())
	}
}

func TestTUIPendingApprovalRequiresExplicitY(t *testing.T) {
	controller := newTUIController(nil)
	model := newTUIModel(controller, tuiInitialState{})
	answer := make(chan bool, 1)
	model.pending = &tuiApprovalMsg{answer: answer}

	model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	select {
	case approved := <-answer:
		if approved {
			t.Fatal("Enter approved a pending request")
		}
	default:
		t.Fatal("Enter did not resolve the pending request")
	}

	answer = make(chan bool, 1)
	model.pending = &tuiApprovalMsg{answer: answer}
	model.handleKey(tea.KeyPressMsg(tea.Key{Text: "y"}))
	select {
	case approved := <-answer:
		if !approved {
			t.Fatal("y did not approve a pending request")
		}
	default:
		t.Fatal("y did not resolve the pending request")
	}
}

func TestTUIPendingApprovalInstructionsAndFooterMatch(t *testing.T) {
	model := newTUIModel(newTUIController(nil), tuiInitialState{})
	model.width = 120
	model.height = 24
	model.pending = &tuiApprovalMsg{request: ApprovalRequest{Title: "Review changes"}}
	model.resize()

	content := model.View().Content
	for _, text := range []string{"[y] approve", "[n] reject", "[enter/esc] reject", "PgUp/PgDn scroll", "y approve  n/Enter/Esc reject"} {
		if !strings.Contains(content, text) {
			t.Fatalf("pending approval view is missing %q: %q", text, content)
		}
	}
	if strings.Contains(content, "Enter send") {
		t.Fatalf("pending approval footer still advertises send: %q", content)
	}
}

func TestTUIPendingApprovalAllowsViewportScrolling(t *testing.T) {
	model := newTUIModel(newTUIController(nil), tuiInitialState{})
	model.width = 80
	model.height = 10
	model.resize()
	answer := make(chan bool, 1)
	model.Update(tuiApprovalMsg{request: ApprovalRequest{
		Title:  "Review file changes",
		Detail: strings.Repeat("changed line\n", 30),
	}, answer: answer})
	if model.viewport.YOffset() == 0 {
		t.Fatal("long approval was not positioned at the bottom")
	}
	before := model.viewport.YOffset()
	model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyPgUp}))
	if model.viewport.YOffset() >= before {
		t.Fatalf("page-up did not scroll approval: before %d, after %d", before, model.viewport.YOffset())
	}
	model.resolveApproval(false)
}

func TestTUIAutoApprovedOperationStaysVisible(t *testing.T) {
	model := newTUIModel(newTUIController(nil), tuiInitialState{})
	model.width = 80
	model.height = 24
	model.resize()
	model.Update(tuiApprovalPreviewMsg{request: ApprovalRequest{
		Title:  "Review file changes",
		Detail: "--- a/main.go\n+++ b/main.go\n+safe change\n",
	}})
	if timeline := model.renderTimeline(); !strings.Contains(timeline, "Auto-approved: Review file changes") || !strings.Contains(timeline, "+safe change") {
		t.Fatalf("auto-approved operation was not visible: %q", timeline)
	}
}

func TestTUIViewEnablesMouseWheelEvents(t *testing.T) {
	model := newTUIModel(newTUIController(nil), tuiInitialState{})
	model.width = 80
	model.height = 24
	model.resize()

	if got := model.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Fatalf("mouse mode = %v, want %v", got, tea.MouseModeCellMotion)
	}
}

func TestTerminalIsDumb(t *testing.T) {
	t.Setenv("TERM", "dumb")
	if !terminalIsDumb() {
		t.Fatal("TERM=dumb did not disable the TUI")
	}

	t.Setenv("TERM", "xterm-256color")
	if terminalIsDumb() {
		t.Fatal("non-dumb terminal disabled the TUI")
	}
}

func TestShouldUseTUIRejectsCharacterDevicesThatAreNotTerminals(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()

	if shouldUseTUI(input, output) {
		t.Fatal("non-terminal character devices enabled the TUI")
	}
}

func TestTUIRoutesMouseWheelToTimelineWhileIdle(t *testing.T) {
	model := newTUIModel(newTUIController(nil), tuiInitialState{})
	model.width = 80
	model.height = 10
	model.entries = []tuiEntry{{kind: tuiEntryAssistant, text: strings.Repeat("history\n", 30)}}
	model.resize()
	model.viewport.GotoBottom()
	before := model.viewport.YOffset()
	if before == 0 {
		t.Fatal("timeline did not overflow")
	}
	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if model.viewport.YOffset() >= before {
		t.Fatalf("mouse wheel did not scroll timeline: before %d, after %d", before, model.viewport.YOffset())
	}
}

func TestTUIPasteResizesViewport(t *testing.T) {
	model := newTUIModel(newTUIController(nil), tuiInitialState{})
	model.width = 80
	model.height = 24
	model.resize()
	model.input.Focus()
	model.Update(tea.PasteMsg{Content: "one\ntwo\nthree\nfour"})

	want := model.height - tuiHeaderHeight - tuiFooterHeight - tuiLayoutSeparators - model.input.Height() - tuiInputStyle.GetVerticalFrameSize()
	if got := model.viewport.Height(); got != want {
		t.Fatalf("viewport height after paste = %d, want %d", got, want)
	}
}

func TestTUIControllerDropsQueuedMessageAfterStop(t *testing.T) {
	controller := newTUIController(nil)
	controller.messages <- "cancelled request"
	controller.stop()

	if message, ok := controller.nextMessage(); ok || message != "" {
		t.Fatalf("next message = %q, %v after stop", message, ok)
	}
}

func TestTUIControllerRendersFirstDeltaAndBatchesTheRest(t *testing.T) {
	controller := newTUIController(nil)
	controller.queueAssistantDelta("first ")
	controller.queueAssistantDelta("second")
	controller.deltaMu.Lock()
	got := controller.deltas.String()
	timer := controller.deltaTimer
	active := controller.deltaActive
	controller.deltaMu.Unlock()
	if got != "second" || timer == nil || !active {
		t.Fatalf("pending deltas = %q, timer = %v, active = %v", got, timer, active)
	}

	controller.flushAssistantDeltas()
	controller.deltaMu.Lock()
	got = controller.deltas.String()
	timer = controller.deltaTimer
	controller.deltaMu.Unlock()
	if got != "" || timer != nil {
		t.Fatalf("flush left deltas = %q, timer = %v", got, timer)
	}
	controller.Emit(UIEvent{Kind: UIEventAssistantDone})
	controller.deltaMu.Lock()
	active = controller.deltaActive
	controller.deltaMu.Unlock()
	controller.stop()
	if active {
		t.Fatal("assistant delta state was not reset after completion")
	}
}

func TestTUIControllerStartsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := newTUIController(cancel)
	input, closeInput := io.Pipe()
	defer closeInput.Close()
	var output bytes.Buffer
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- controller.run(ctx, input, &output, tuiInitialState{}, func() {
			close(started)
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("TUI controller did not start")
	}
	cancel()
	_ = closeInput.Close()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("TUI controller returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("TUI controller did not stop")
	}
}

func TestTUIControllerDeliversAgentEventsInOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := newTUIController(cancel)
	input, closeInput := io.Pipe()
	defer closeInput.Close()
	var output bytes.Buffer
	model := &observingTUIModel{
		model:  newTUIModel(controller, tuiInitialState{}),
		events: make(chan UIEvent, 8),
	}
	program := tea.NewProgram(
		model,
		tea.WithInput(input),
		tea.WithOutput(&output),
		tea.WithContext(ctx),
		tea.WithWindowSize(80, 24),
		tea.WithoutSignalHandler(),
	)
	controller.program = program
	programDone := make(chan error, 1)
	go func() {
		_, err := program.Run()
		programDone <- err
	}()
	defer func() {
		controller.stop()
		select {
		case <-programDone:
		case <-time.After(time.Second):
			t.Error("Bubble Tea program did not stop")
		}
	}()

	select {
	case <-controller.ready:
	case <-time.After(time.Second):
		t.Fatal("Bubble Tea model did not become ready")
	}

	stream := &scriptedResponseStream{events: []responses.ResponseStreamEventUnion{
		{Type: "response.output_text.delta", Delta: "streamed reply"},
		{Type: "response.completed", Response: streamedCompletedResponse(t, "streamed reply")},
	}}
	agent := Agent{
		getUserMessage: controller.nextMessage,
		createStream: func(context.Context, responses.ResponseNewParams) responseStream {
			return stream
		},
		events: controller,
	}
	agentDone := make(chan error, 1)
	go func() { agentDone <- agent.Run(ctx) }()

	controller.messages <- "reply"
	want := []UIEventKind{
		UIEventStatus,
		UIEventUserMessage,
		UIEventStatus,
		UIEventAssistantDelta,
		UIEventAssistantDone,
		UIEventStatus,
	}
	for index, kind := range want {
		select {
		case event := <-model.events:
			if event.Kind != kind {
				t.Fatalf("event %d = %q, want %q", index, event.Kind, kind)
			}
		case <-time.After(time.Second):
			t.Fatalf("event %d (%q) was not delivered", index, kind)
		}
	}

	controller.stop()
	select {
	case err := <-agentDone:
		if err != nil {
			t.Fatalf("agent returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("agent did not stop")
	}
}

func TestTUIViewportAccountsForMultiLineComposer(t *testing.T) {
	model := newTUIModel(newTUIController(nil), tuiInitialState{})
	model.width = 80
	model.height = 24
	model.input.SetValue("one\ntwo\nthree\nfour")
	model.resize()

	want := model.height - tuiHeaderHeight - tuiFooterHeight - tuiLayoutSeparators - model.input.Height() - tuiInputStyle.GetVerticalFrameSize()
	if got := model.viewport.Height(); got != want {
		t.Fatalf("viewport height = %d, want %d", got, want)
	}
}

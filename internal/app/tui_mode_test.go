package app

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestTUIShiftTabTogglesModeFromComposer(t *testing.T) {
	controller := newTUIController(nil)
	model := newTUIModel(controller, tuiInitialState{mode: string(ModeBuild)})
	model.width, model.height = 80, 24
	model.resize()

	command := model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab, Mod: tea.ModShift}))
	if command == nil {
		t.Fatal("idle shift+tab did not submit a mode request")
	}
	if result, ok := command().(tuiModeRequestResultMsg); !ok || result.err != nil {
		t.Fatalf("idle shift+tab result = %#v", result)
	}
	if got, ok := controller.nextMessage(); !ok || got != "/plan" {
		t.Fatalf("idle shift+tab = %q %v, want /plan", got, ok)
	}

	model.applyEvent(UIEvent{Kind: UIEventMode, Text: string(ModePlan)})
	command = model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab, Mod: tea.ModShift}))
	if command == nil {
		t.Fatal("plan shift+tab did not submit a mode request")
	}
	if result, ok := command().(tuiModeRequestResultMsg); !ok || result.err != nil {
		t.Fatalf("plan shift+tab result = %#v", result)
	}
	if got, ok := controller.nextMessage(); !ok || got != "/build" {
		t.Fatalf("plan shift+tab = %q %v, want /build", got, ok)
	}
}

func TestTUIShiftTabUsesTurnControlWhileBusy(t *testing.T) {
	controller := newTUIController(nil)
	var gotKind, gotValue string
	controller.submitControl = func(kind, value string) error {
		gotKind, gotValue = kind, value
		return nil
	}
	model := newTUIModel(controller, tuiInitialState{mode: string(ModeBuild)})
	model.width, model.height = 80, 24
	model.resize()
	model.busy = true

	command := model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab, Mod: tea.ModShift}))
	if command == nil {
		t.Fatal("busy shift+tab did not submit a control")
	}
	if result, ok := command().(tuiModeRequestResultMsg); !ok || result.err != nil {
		t.Fatalf("busy shift+tab result = %#v", result)
	}
	if gotKind != "mode" || gotValue != "plan" {
		t.Fatalf("control = %q %q, want mode plan", gotKind, gotValue)
	}
	select {
	case message := <-controller.messages:
		t.Fatalf("busy shift+tab leaked an ordinary message %q", message)
	default:
	}
}

func TestTUIComposerShowsModeBadgeInTopBorder(t *testing.T) {
	model := newTUIModel(newTUIController(nil), tuiInitialState{
		workspace: "/tmp/project",
		model:     "gpt-test",
		mode:      string(ModePlan),
	})
	model.width, model.height = 80, 24
	model.resize()

	view := model.View()
	if strings.Contains(view.Content, "Read-only") {
		t.Fatalf("mode is still drawn in the header: %q", view.Content)
	}
	box := model.composerBox()
	top, rest, ok := strings.Cut(box, "\n")
	if !ok {
		t.Fatalf("composer has no frame: %q", box)
	}
	plain := ansi.Strip(top)
	if !strings.HasPrefix(plain, "┌") || !strings.HasSuffix(plain, "┐") {
		t.Fatalf("badge did not stay inside the border corners: %q", plain)
	}
	if !strings.Contains(plain, "PLAN") {
		t.Fatalf("missing plan badge: %q", plain)
	}
	if width := ansi.StringWidth(top); width != ansi.StringWidth(rest[:strings.IndexByte(rest, '\n')]) {
		t.Fatalf("badge changed the frame width: top %d body line %d", width,
			ansi.StringWidth(rest[:strings.IndexByte(rest, '\n')]))
	}
}

func TestTUIComposerBadgeIsTheModeNameAlone(t *testing.T) {
	// The permission profile is policy detail, not a mode. Keep it out of the
	// badge so the composer reads as PLAN or BUILD and nothing else.
	for _, want := range []struct {
		permissions string
		label       string
	}{
		{string(PermissionInteractive), "BUILD"},
		{string(PermissionWorkspaceEdit), "BUILD"},
	} {
		model := newTUIModel(newTUIController(nil), tuiInitialState{mode: string(ModeBuild)})
		model.width, model.height = 80, 24
		model.permissionProfile = want.permissions
		model.resize()
		box := ansi.Strip(model.composerBox())
		if !strings.Contains(box, want.label) {
			t.Fatalf("permissions %q badge = %q, want %q", want.permissions, box, want.label)
		}
		for _, unwanted := range []string{"confirm", "edits", "read-only"} {
			if strings.Contains(box, unwanted) {
				t.Fatalf("badge still carries %q: %q", unwanted, box)
			}
		}
	}
}

func TestTUIComposerBadgeFitsTheNarrowestFrame(t *testing.T) {
	// The badge is a mode name alone, so it fits the minimum composer width and
	// must never push the frame wider or break its corners.
	model := newTUIModel(newTUIController(nil), tuiInitialState{mode: string(ModePlan)})
	model.width, model.height = 14, 24
	model.resize()

	box := model.composerBox()
	top, _, _ := strings.Cut(box, "\n")
	plain := ansi.Strip(top)
	if !strings.HasPrefix(plain, "┌") || !strings.HasSuffix(plain, "┐") {
		t.Fatalf("narrow badge broke the frame corners: %q", plain)
	}
	if !strings.Contains(plain, "PLAN") {
		t.Fatalf("narrow composer dropped the mode badge: %q", plain)
	}
	for _, line := range strings.Split(box, "\n") {
		if width := ansi.StringWidth(line); width != max(12, model.width-2) {
			t.Fatalf("narrow composer line width %d differs from the frame: %q", width, ansi.Strip(line))
		}
	}
}

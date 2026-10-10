package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestTUIReferencePickerSelectsStructuredUnicodePath(t *testing.T) {
	controller := newTUIController(nil)
	controller.referenceFiles = func() (referenceCandidateList, error) {
		return referenceCandidateList{Files: []FileReference{{Path: "README.md"}, {Path: "unicodé/你好 file.txt"}}}, nil
	}
	m := newTUIModel(controller, tuiInitialState{})
	m.width = 80
	m.height = 24
	m.resize()
	m.input.SetValue("Inspect")
	command := m.openReferencePicker()
	if command == nil {
		t.Fatal("picker did not open")
	}
	m.Update(command())
	m.Update(tea.PasteMsg{Content: "你好"})
	if m.input.Value() != "Inspect" {
		t.Fatal("picker filter modified the task draft")
	}
	m.referencePickerKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	refs, err := ParseFileReferences(m.input.Value())
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Path != "unicodé/你好 file.txt" {
		t.Fatalf("selection lost raw path: input=%q refs=%v", m.input.Value(), refs)
	}
	if m.referencePicker != nil {
		t.Fatal("picker stayed open after selection")
	}
}

func TestTUIReferencePickerPreservesDraftAndDoesNotLeakIntoInteractions(t *testing.T) {
	for _, kind := range []string{"approval", "form"} {
		t.Run(kind, func(t *testing.T) {
			controller := newTUIController(nil)
			controller.referenceFiles = func() (referenceCandidateList, error) {
				return referenceCandidateList{Files: []FileReference{{Path: "not-a-form-answer.txt"}}}, nil
			}
			m := newTUIModel(controller, tuiInitialState{})
			m.input.SetValue("existing user draft")
			load := m.openReferencePicker()
			m.referencePicker.query = "private picker query"
			if kind == "approval" {
				m.Update(tuiApprovalMsg{request: ApprovalRequest{Title: "approve"}, answer: make(chan bool, 1)})
			} else {
				m.Update(tuiMCPInputMsg{prompt: "form", answer: make(chan string, 1)})
			}
			m.Update(load())
			if m.referencePicker != nil {
				t.Fatal("late picker results reopened over interaction")
			}
			if cmd := m.openReferencePicker(); cmd != nil {
				t.Fatal("picker opened while interaction owned input")
			}
			if strings.Contains(m.input.Value(), "picker query") || strings.Contains(m.input.Value(), "not-a-form") {
				t.Fatal("picker state leaked into interaction input")
			}
		})
	}
	controller := newTUIController(nil)
	controller.referenceFiles = func() (referenceCandidateList, error) { return referenceCandidateList{}, nil }
	m := newTUIModel(controller, tuiInitialState{})
	m.input.SetValue("keep this")
	load := m.openReferencePicker()
	m.referencePickerKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	m.Update(load())
	if m.referencePicker != nil || m.input.Value() != "keep this" {
		t.Fatal("cancelling picker changed draft or late result reopened picker")
	}
}

func TestReferenceFileLoaderBoundsAndProtectedPaths(t *testing.T) {
	w := contextWorkspace(t)
	writeContextFixture(t, w, "visible/file.txt", "not read")
	writeContextFixture(t, w, ".git/config", "secret")
	writeContextFixture(t, w, "private/key.txt", "secret")
	if err := w.ProtectPath(filepath.Join(w.root, "private")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(w.root, "private"), filepath.Join(w.root, "linked")); err != nil {
		t.Fatal(err)
	}
	files, err := referenceFileLoader(t.Context(), w)()
	if err != nil {
		t.Fatal(err)
	}
	if len(files.Files) != 1 || files.Files[0].Path != "visible/file.txt" {
		t.Fatalf("unsafe candidates=%v", files.Files)
	}
	for i := range maxReferenceCandidates + 1 {
		writeContextFixture(t, w, fmt.Sprintf("many/%04d.txt", i), "")
	}
	files, err = referenceFileLoader(t.Context(), w)()
	if err != nil {
		t.Fatal(err)
	}
	if !files.Truncated || len(files.Files) != maxReferenceCandidates {
		t.Fatalf("candidate bounds=%d truncated=%t", len(files.Files), files.Truncated)
	}
}

package app

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTUIManagedOutputStaysWithProcessAcrossTools(t *testing.T) {
	m := newTUIModel(newTUIController(nil), tuiInitialState{})
	m.width, m.height = 80, 24
	m.resize()
	m.applyEvent(UIEvent{Kind: UIEventToolStarted, Name: "start_process"})
	m.applyEvent(UIEvent{Kind: UIEventCommandOutput, Name: "process-one", Text: "initial\n"})
	m.applyEvent(UIEvent{Kind: UIEventToolFinished, Name: "start_process", Detail: "started"})
	m.applyEvent(UIEvent{Kind: UIEventCommandOutput, Name: "process-one", Text: "after-start\n"})
	m.applyEvent(UIEvent{Kind: UIEventToolStarted, Name: "read_file"})
	m.applyEvent(UIEvent{Kind: UIEventCommandOutput, Name: "process-one", Text: "during-read\n"})
	if m.entries[m.activeTool].name != "read_file" || m.entries[m.activeTool].detail != "" {
		t.Fatalf("process output attached to unrelated tool: %+v", m.entries[m.activeTool])
	}
	if m.entries[0].detail != "started" || m.entries[1].kind != tuiEntryProcess || m.entries[1].detail != "initial\nafter-start\nduring-read\n" {
		t.Fatalf("process output lost: %+v", m.entries)
	}
	if !strings.Contains(m.renderViewportTimeline(), "during-read") {
		t.Fatal("cached timeline hides process updates")
	}
	m.applyEvent(UIEvent{Kind: UIEventCommandOutput, Text: "late synchronous output"})
	if m.entries[m.activeTool].detail != "" {
		t.Fatal("late synchronous output attached to unrelated tool")
	}
}

func TestTUIManagedOutputRetentionIsBoundedAndKeepsNewestText(t *testing.T) {
	m := newTUIModel(newTUIController(nil), tuiInitialState{})
	for index := range tuiMaxVisibleEntries + 5 {
		m.applyEvent(UIEvent{Kind: UIEventCommandOutput, Name: fmt.Sprintf("process-%d", index), Text: "ready"})
	}
	if len(m.entries) != tuiMaxVisibleEntries || m.hiddenEntries != 5 {
		t.Fatalf("process entries grow without bound: visible=%d hidden=%d", len(m.entries), m.hiddenEntries)
	}
	m.applyEvent(UIEvent{Kind: UIEventCommandOutput, Name: "process-0", Text: strings.Repeat("中", 9000) + "newest"})
	entry := m.entries[len(m.entries)-1]
	if entry.processID != "process-0" || len(entry.detail) > 8192 || !utf8.ValidString(entry.detail) || !strings.HasSuffix(entry.detail, "newest") || !strings.Contains(entry.detail, "[live output truncated]") {
		t.Fatalf("invalid retained process output: id=%s length=%d", entry.processID, len(entry.detail))
	}
	m.applyEvent(UIEvent{Kind: UIEventCommandOutput, Name: "process-0", Text: "post-gap", Detail: "truncated"})
	if !strings.HasSuffix(m.entries[len(m.entries)-1].detail, "[live output truncated]\npost-gap") {
		t.Fatal("skipped live output is not marked at its boundary")
	}
}

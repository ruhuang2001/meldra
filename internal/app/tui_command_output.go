package app

import (
	"slices"
	"unicode/utf8"
)

func (m *tuiModel) appendCommandOutput(event UIEvent) {
	index := m.activeTool
	if event.Name != "" {
		// Managed process output outlives its start tool. Keep it in an entry
		// owned by the process, even while another tool or the model is active.
		index = slices.IndexFunc(m.entries, func(entry tuiEntry) bool {
			return entry.kind == tuiEntryProcess && entry.processID == event.Name
		})
		if index < 0 {
			m.appendEntry(tuiEntry{kind: tuiEntryProcess, processID: event.Name})
			index = len(m.entries) - 1
		}
	} else if index < 0 || (m.entries[index].name != "run_command" && m.entries[index].name != "verify") {
		// Late synchronous updates must not overwrite unrelated tool results.
		return
	}
	entry := &m.entries[index]
	entry.detail = appendLiveOutput(entry.detail, event.Text, event.Detail == "truncated")
	entry.cached = ""
	// A process entry may be in the cached completed prefix of the timeline.
	m.invalidateCompletedPrefix()
	m.refreshViewport()
}

func appendLiveOutput(previous, text string, truncated bool) string {
	const marker = "[live output truncated]\n"
	const limit = 8192
	if truncated {
		text = marker + text
	}
	combined := previous + text
	if len(combined) <= limit {
		return combined
	}
	start := len(combined) - (limit - len(marker))
	for start < len(combined) && !utf8.RuneStart(combined[start]) {
		start++
	}
	return marker + combined[start:]
}

package app

import (
	"fmt"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func (m *tuiModel) commandMatches() []slashCommand {
	if m.pending != nil || m.mcpPending != nil || m.correcting || m.stopped {
		return nil
	}
	input := m.input.Value()
	if !strings.HasPrefix(input, "/") || strings.ContainsFunc(input, unicode.IsSpace) || input == m.commandDismissed {
		return nil
	}
	var matches []slashCommand
	for _, command := range m.controller.commands {
		if strings.HasPrefix("/"+command.Name, input) {
			matches = append(matches, command)
		}
	}
	return matches
}

func (m *tuiModel) handleCommandCompletion(key tea.KeyPressMsg) bool {
	matches := m.commandMatches()
	if len(matches) == 0 {
		m.commandCursor = 0
		if m.input.Value() != m.commandDismissed {
			m.commandDismissed = ""
		}
		return false
	}
	m.commandCursor = min(m.commandCursor, len(matches)-1)
	switch key.String() {
	case "up":
		m.commandCursor = max(0, m.commandCursor-1)
	case "down":
		m.commandCursor = min(len(matches)-1, m.commandCursor+1)
	case "tab":
		m.input.SetValue("/" + matches[m.commandCursor].Name + " ")
		m.input.CursorEnd()
		m.commandCursor = 0
	case "esc":
		m.commandDismissed = m.input.Value()
	default:
		m.commandCursor = 0
		return false
	}
	return true
}

func (m *tuiModel) commandCompletionHeight() int {
	matches := m.commandMatches()
	if len(matches) == 0 {
		return 0
	}
	return min(6, len(matches)) + 1
}

func (m *tuiModel) commandCompletionView() string {
	matches := m.commandMatches()
	if len(matches) == 0 {
		return ""
	}
	cursor := min(m.commandCursor, len(matches)-1)
	start := max(0, cursor-5)
	var lines []string
	for i := start; i < min(start+6, len(matches)); i++ {
		prefix := "  "
		if i == cursor {
			prefix = "> "
		}
		label := fmt.Sprintf("%s/%s %s  %s", prefix, matches[i].Name, matches[i].ArgumentHint, matches[i].Description)
		lines = append(lines, ansi.Truncate(skillDisplayLine(label), max(1, m.width-2), "…"))
	}
	lines = append(lines, tuiDimStyle.Render("↑/↓ select · Tab complete · Enter send · Esc dismiss"))
	return strings.Join(lines, "\n")
}

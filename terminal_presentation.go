package main

import (
	"strings"
	"unicode/utf8"
)

// sanitizeTerminalText removes terminal control instructions from untrusted
// presentation text while preserving printable content and line boundaries.
func sanitizeTerminalText(text string) string {
	var output strings.Builder
	output.Grow(len(text))

	for index := 0; index < len(text); {
		switch text[index] {
		case '\x1b':
			index = skipTerminalEscape(text, index)
			continue
		case '\r':
			if index+1 < len(text) && text[index+1] == '\n' {
				output.WriteByte('\n')
				index += 2
			} else {
				index++
			}
			continue
		case '\n':
			output.WriteByte('\n')
			index++
			continue
		}

		character, size := utf8.DecodeRuneInString(text[index:])
		if character == utf8.RuneError && size == 1 {
			// Replace invalid bytes so they cannot be interpreted as C1 controls.
			output.WriteRune(utf8.RuneError)
			index++
			continue
		}
		if terminalControlRune(character) {
			index += size
			continue
		}
		output.WriteString(text[index : index+size])
		index += size
	}

	return output.String()
}

func terminalControlRune(character rune) bool {
	return character <= 0x1f || character == 0x7f || (character >= 0x80 && character <= 0x9f)
}

func skipTerminalEscape(text string, start int) int {
	if start+1 >= len(text) {
		return start + 1
	}

	switch text[start+1] {
	case '[':
		for index := start + 2; index < len(text); index++ {
			character := text[index]
			switch {
			case character >= 0x30 && character <= 0x3f, character >= 0x20 && character <= 0x2f:
				continue
			case character >= 0x40 && character <= 0x7e:
				return index + 1
			default:
				return start + 1
			}
		}
	case ']', 'P', 'X', '^', '_':
		for index := start + 2; index < len(text); index++ {
			if text[index] == '\a' {
				return index + 1
			}
			if text[index] == '\x1b' && index+1 < len(text) && text[index+1] == '\\' {
				return index + 2
			}
			if text[index] < 0x20 || text[index] == 0x7f {
				return start + 1
			}
		}
	default:
		index := start + 1
		for index < len(text) && text[index] >= 0x20 && text[index] <= 0x2f {
			index++
		}
		if index < len(text) && text[index] >= 0x30 && text[index] <= 0x7e {
			return index + 1
		}
	}

	// An incomplete escape is harmless once its ESC introducer is removed.
	return start + 1
}

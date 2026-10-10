package app

import (
	"strings"
	"unicode/utf8"
)

// terminalStream has a constant-size checkpoint, even for unterminated control
// strings. Raw logs stay intact; only presentation uses this incremental decoder.
type terminalStream struct {
	state      uint8
	pending    [utf8.UTFMax]byte
	pendingLen int
}

const (
	terminalPlain uint8 = iota
	terminalEscape
	terminalIntermediate
	terminalCSI
	terminalString
	terminalStringEscape
)

func (s *terminalStream) append(data []byte, final bool, output *strings.Builder) {
	if s.pendingLen != 0 {
		joined := make([]byte, 0, s.pendingLen+len(data))
		joined = append(joined, s.pending[:s.pendingLen]...)
		data = append(joined, data...)
		s.pendingLen = 0
	}
	for index := 0; index < len(data); {
		char := data[index]
		switch s.state {
		case terminalEscape:
			switch char {
			case '[':
				s.state = terminalCSI
			case ']', 'P', 'X', '^', '_':
				s.state = terminalString
			default:
				if char >= 0x20 && char <= 0x2f {
					s.state = terminalIntermediate
				} else {
					s.state = terminalPlain
					if char < 0x30 || char > 0x7e {
						continue
					}
				}
			}
			index++
			continue
		case terminalIntermediate, terminalCSI:
			if char >= 0x20 && char <= 0x2f || s.state == terminalCSI && char >= 0x30 && char <= 0x3f {
				index++
				continue
			}
			minimum := byte(0x30)
			if s.state == terminalCSI {
				minimum = 0x40
			}
			s.state = terminalPlain
			if char >= minimum && char <= 0x7e {
				index++
			}
			continue
		case terminalStringEscape:
			if char == '\\' {
				s.state = terminalPlain
				index++
				continue
			}
			s.state = terminalString
			continue
		case terminalString:
			switch {
			case char == '\a':
				s.state = terminalPlain
			case char == '\x1b':
				s.state = terminalStringEscape
			case char < 0x20 || char == 0x7f:
				s.state = terminalPlain
				continue
			}
			index++
			continue
		}
		if char == '\x1b' {
			s.state = terminalEscape
			index++
			continue
		}
		if !final && !utf8.FullRune(data[index:]) {
			s.pendingLen = copy(s.pending[:], data[index:])
			break
		}
		character, size := utf8.DecodeRune(data[index:])
		if output != nil && (character == '\n' || !terminalControlRune(character)) {
			output.WriteRune(character)
		}
		index += size
	}
	if final {
		s.state = terminalPlain
	}
}

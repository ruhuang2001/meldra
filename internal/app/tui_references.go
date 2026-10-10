package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const maxReferenceCandidates = 1024

type referenceCandidateList struct {
	Files     []FileReference
	Truncated bool
}

type tuiReferencePicker struct {
	query     string
	cursor    int
	files     []FileReference
	loading   bool
	truncated bool
	err       error
}

type tuiReferencesLoadedMsg struct {
	picker *tuiReferencePicker
	list   referenceCandidateList
	err    error
}

// referenceFileLoader captures immutable workspace boundaries. It opens each
// directory through an os.Root handle and never reads candidate file contents.
func referenceFileLoader(ctx context.Context, w *Workspace) func() (referenceCandidateList, error) {
	rootPath := w.root
	protected := slices.Clone(w.protected)
	return func() (referenceCandidateList, error) {
		root, err := os.OpenRoot(rootPath)
		if err != nil {
			return referenceCandidateList{}, err
		}
		defer root.Close()
		var result referenceCandidateList
		visited, totalBytes := 0, 0
		bounded := errors.New("file picker bounded")
		var walk func(*os.Root, string) error
		walk = func(root *os.Root, relative string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			dir, err := root.Open(".")
			if err != nil {
				return err
			}
			defer dir.Close()
			for {
				entries, readErr := dir.ReadDir(128)
				for _, entry := range entries {
					if err := ctx.Err(); err != nil {
						return err
					}
					visited++
					if visited > maxWalkEntries {
						return bounded
					}
					if strings.EqualFold(entry.Name(), ".git") || entry.Type()&os.ModeSymlink != 0 {
						continue
					}
					path := filepath.Join(relative, entry.Name())
					absolute := filepath.Join(rootPath, path)
					blocked := false
					for _, p := range protected {
						if withinFold(p, absolute) {
							blocked = true
							break
						}
					}
					if blocked {
						continue
					}
					info, err := root.Lstat(entry.Name())
					if err != nil {
						return err
					}
					if info.Mode()&os.ModeSymlink != 0 {
						continue
					}
					if info.IsDir() {
						child, err := root.OpenRoot(entry.Name())
						if err != nil {
							return err
						}
						opened, err := child.Stat(".")
						if err != nil || !os.SameFile(info, opened) {
							_ = child.Close()
							return fmt.Errorf("file picker directory changed while opening")
						}
						err = walk(child, path)
						_ = child.Close()
						if err != nil {
							return err
						}
					} else if info.Mode().IsRegular() {
						totalBytes += len(path)
						if len(result.Files) >= maxReferenceCandidates || totalBytes > maxReferenceTotalBytes {
							return bounded
						}
						result.Files = append(result.Files, FileReference{Path: filepath.ToSlash(path)})
					}
				}
				if errors.Is(readErr, io.EOF) {
					return nil
				}
				if readErr != nil {
					return readErr
				}
			}
		}
		err = walk(root, "")
		if errors.Is(err, bounded) {
			result.Truncated = true
			err = nil
		}
		slices.SortFunc(result.Files, func(a, b FileReference) int { return strings.Compare(a.Path, b.Path) })
		return result, err
	}
}

func (p *tuiReferencePicker) matches() []FileReference {
	query := strings.ToLower(p.query)
	var matches []FileReference
	for _, file := range p.files {
		if strings.Contains(strings.ToLower(file.Path), query) {
			matches = append(matches, file)
		}
	}
	return matches
}

func (m *tuiModel) openReferencePicker() tea.Cmd {
	if m.pending != nil || m.mcpPending != nil || m.stopped || m.controller.referenceFiles == nil {
		return nil
	}
	picker := &tuiReferencePicker{loading: true}
	m.referencePicker = picker
	return func() tea.Msg {
		list, err := m.controller.referenceFiles()
		return tuiReferencesLoadedMsg{picker: picker, list: list, err: err}
	}
}

func (m *tuiModel) referencePickerKey(key tea.KeyPressMsg) tea.Cmd {
	p := m.referencePicker
	switch key.String() {
	case "esc", "ctrl+f":
		m.referencePicker = nil
		return nil
	case "up":
		p.cursor = max(0, p.cursor-1)
	case "down":
		p.cursor = min(max(0, len(p.matches())-1), p.cursor+1)
	case "backspace":
		if len(p.query) > 0 {
			_, size := utf8.DecodeLastRuneInString(p.query)
			p.query = p.query[:len(p.query)-size]
		}
		p.cursor = 0
	case "enter":
		files := p.matches()
		if p.loading || p.err != nil || len(files) == 0 {
			return nil
		}
		selected := files[min(p.cursor, len(files)-1)]
		// The selected structured path, not its sanitized UI label, determines
		// the reference. Quoting protects spaces, punctuation and Unicode.
		prefix := ""
		if m.input.Value() != "" {
			prefix = " "
		}
		m.input.InsertString(prefix + "@" + strconv.Quote(selected.Path) + " ")
		m.referencePicker = nil
		m.resize()
		return m.input.Focus()
	default:
		if key.Text != "" && len(p.query)+len(key.Text) <= 256 {
			p.query += key.Text
			p.cursor = 0
		}
	}
	return nil
}

func (m *tuiModel) referencePickerView() tea.View {
	p := m.referencePicker
	var b strings.Builder
	b.WriteString(tuiBrandStyle.Render("Attach a workspace file"))
	b.WriteByte('\n')
	b.WriteString("Filter: " + sanitizeTerminalText(p.query))
	b.WriteString("\n\n")
	switch {
	case p.loading:
		b.WriteString("Loading file names…")
	case p.err != nil:
		b.WriteString("Cannot list files: " + sanitizeTerminalText(p.err.Error()))
	default:
		files := p.matches()
		height := max(1, m.height-7)
		start := max(0, p.cursor-height+1)
		end := min(len(files), start+height)
		if len(files) == 0 {
			b.WriteString("No matching files.\n")
		}
		for i := start; i < end; i++ {
			prefix := "  "
			if i == p.cursor {
				prefix = "> "
			}
			b.WriteString(prefix + ansi.Truncate(sanitizeTerminalText(files[i].Path), max(1, m.width-3), "…") + "\n")
		}
		if p.truncated {
			b.WriteString(fmt.Sprintf("[List bounded to %d files; type a path directly if it is missing.]\n", maxReferenceCandidates))
		}
	}
	b.WriteString("\n↑/↓ select · Enter attach · Esc cancel")
	view := tea.NewView(b.String())
	view.AltScreen = true
	view.MouseMode = tea.MouseModeNone
	return view
}

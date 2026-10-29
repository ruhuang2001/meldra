package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const (
	maxCommandFileBytes = 32 << 10
	maxCommands         = 128
)

type slashCommand struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	ArgumentHint string `json:"argument_hint,omitempty"`
	Path         string `json:"path,omitempty"`
	body         string
}

type commandCatalog struct {
	Commands []slashCommand `json:"commands"`
	Warnings []string       `json:"warnings"`
}

var builtinSlashCommands = []slashCommand{
	{Name: "plan", Description: "Enter read-only Plan mode"},
	{Name: "build", Description: "Enter Build mode"},
	{Name: "stop", Description: "Stop the current turn"},
	{Name: "steer", Description: "Correct the active task", ArgumentHint: "<correction>"},
	{Name: "queue", Description: "Queue a follow-up request", ArgumentHint: "<request>"},
	{Name: "continue-queue", Description: "Apply recovered queued requests"},
	{Name: "context", Description: "Inspect project instructions while idle", ArgumentHint: "[path]"},
}

// Commands and bodies form an immutable startup snapshot. Expanding a command
// creates ordinary user text; it never dispatches a tool or reparses controls.
func discoverCommands(ctx context.Context, workspace *Workspace, paths ConfigPaths) (*commandCatalog, error) {
	catalog := &commandCatalog{Commands: []slashCommand{}, Warnings: []string{}}
	warn := func(path string, err error) {
		if pathErr, ok := errors.AsType[*fs.PathError](err); ok {
			err = pathErr.Err
		}
		catalog.Warnings = append(catalog.Warnings, fmt.Sprintf("%s: %s", path, err))
	}
	home, err := canonicalSkillHome(paths.Home)
	roots := []string{filepath.Join(workspace.root, ".meldra", "commands")}
	if err != nil {
		warn(paths.Home, err)
	} else {
		roots = append(roots, filepath.Join(home, "commands"))
	}
	seenRoots, seenNames := map[string]bool{}, map[string]bool{}
	entries := 0
	for _, directory := range roots {
		if seenRoots[directory] {
			continue
		}
		seenRoots[directory] = true
		if err := checkCommandPath(workspace, home, directory); err != nil {
			warn(directory, err)
			continue
		}
		root, err := openSkillDirectory(directory)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			warn(directory, err)
			continue
		}
		file, err := root.Open(".")
		if err != nil {
			root.Close()
			warn(directory, err)
			continue
		}
		var children []fs.DirEntry
		visited := 0
		for {
			batch, readErr := file.ReadDir(128)
			visited += len(batch)
			for _, child := range batch {
				if strings.HasSuffix(child.Name(), ".md") {
					children = append(children, child)
				}
			}
			if len(children) > maxCommands-entries || visited > maxWalkEntries {
				err = fmt.Errorf("command discovery limit exceeded (maximum %d command files); this and later roots skipped", maxCommands)
				break
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					err = readErr
				}
				break
			}
			if err = ctx.Err(); err != nil {
				break
			}
		}
		file.Close()
		root.Close()
		if err != nil {
			warn(directory, err)
			break
		}
		entries += len(children)
		slices.SortFunc(children, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
		for _, child := range children {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			path := filepath.Join(directory, child.Name())
			if err := checkCommandPath(workspace, home, path); err != nil {
				warn(path, err)
				continue
			}
			contents, err := readWorkflowFile(directory, child.Name(), maxCommandFileBytes)
			if err != nil {
				warn(path, err)
				continue
			}
			item, err := parseSlashCommand(contents, path)
			if err != nil {
				warn(path, err)
				continue
			}
			if slices.ContainsFunc(builtinSlashCommands, func(b slashCommand) bool { return b.Name == item.Name }) {
				warn(path, fmt.Errorf("built-in command /%s takes priority", item.Name))
				continue
			}
			if seenNames[item.Name] {
				warn(path, fmt.Errorf("duplicate command /%s shadowed by higher-priority source", item.Name))
				continue
			}
			seenNames[item.Name] = true
			catalog.Commands = append(catalog.Commands, item)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(catalog.Commands, func(a, b slashCommand) int { return strings.Compare(a.Name, b.Name) })
	return catalog, nil
}

func checkCommandPath(workspace *Workspace, home, path string) error {
	nativeRoot := filepath.Join(home, "commands")
	for _, protected := range workspace.protected {
		if !withinFold(protected, path) {
			continue
		}
		if home != "" && within(nativeRoot, path) && withinFold(protected, nativeRoot) {
			continue
		}
		return fmt.Errorf("access to Meldra configuration, sessions and execution locks is forbidden")
	}
	return nil
}

func parseSlashCommand(contents []byte, path string) (slashCommand, error) {
	name := strings.TrimSuffix(filepath.Base(path), ".md")
	if len(name) > 64 || !skillNamePattern.MatchString(name) {
		return slashCommand{}, fmt.Errorf("command filename must contain 1..64 lowercase ASCII letters, digits or single hyphens")
	}
	text := strings.ReplaceAll(strings.TrimPrefix(string(contents), "\ufeff"), "\r\n", "\n")
	text, found := strings.CutPrefix(text, "---\n")
	if !found {
		return slashCommand{}, fmt.Errorf("command requires YAML frontmatter")
	}
	header, body, found := strings.Cut(text, "\n---\n")
	if !found {
		return slashCommand{}, fmt.Errorf("command requires closed YAML frontmatter and a nonempty body")
	}
	var metadata map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(header), &metadata); err != nil {
		return slashCommand{}, fmt.Errorf("invalid YAML frontmatter: %w", err)
	}
	description := metadata["description"]
	if description.Kind != yaml.ScalarNode || description.Tag != "!!str" || strings.TrimSpace(description.Value) == "" || utf8.RuneCountInString(description.Value) > 512 {
		return slashCommand{}, fmt.Errorf("description must be a nonempty string of at most 512 Unicode characters")
	}
	hint, hasHint := metadata["argument-hint"]
	if hasHint && (hint.Kind != yaml.ScalarNode || hint.Tag != "!!str") {
		return slashCommand{}, fmt.Errorf("argument-hint must be a string")
	}
	if strings.TrimSpace(body) == "" {
		return slashCommand{}, fmt.Errorf("command body must be nonempty")
	}
	return slashCommand{Name: name, Description: description.Value, ArgumentHint: hint.Value, Path: path, body: body}, nil
}

func splitSlashInput(text string) (string, string) {
	text = strings.TrimSpace(text)
	if index := strings.IndexFunc(text, unicode.IsSpace); index >= 0 {
		return text[:index], strings.TrimSpace(text[index:])
	}
	return text, ""
}

func (c *commandCatalog) expand(text string) (string, error) {
	if c == nil {
		return text, nil
	}
	name, arguments := splitSlashInput(text)
	index := slices.IndexFunc(c.Commands, func(command slashCommand) bool { return "/"+command.Name == name })
	if index < 0 {
		return text, nil
	}
	fields := strings.Fields(arguments)
	body := c.Commands[index].body
	var expanded strings.Builder
	for offset := 0; offset < len(body); {
		value, consumed := body[offset:offset+1], 1
		if strings.HasPrefix(body[offset:], "$ARGUMENTS") {
			value, consumed = arguments, len("$ARGUMENTS")
		} else if body[offset] == '$' && offset+1 < len(body) && body[offset+1] >= '1' && body[offset+1] <= '9' {
			position := int(body[offset+1] - '1')
			value, consumed = "", 2
			if position < len(fields) {
				value = fields[position]
			}
		}
		if expanded.Len()+len(value) > maxSessionMessageBytes {
			return "", fmt.Errorf("expanded /%s exceeds %d byte message limit", c.Commands[index].Name, maxSessionMessageBytes)
		}
		expanded.WriteString(value)
		offset += consumed
	}
	if strings.TrimSpace(expanded.String()) == "" {
		return "", fmt.Errorf("expanded /%s is empty", c.Commands[index].Name)
	}
	return expanded.String(), nil
}

// emitCommandWarnings is called only by the serial conversation owner.
func (a *Agent) emitCommandWarnings() {
	if a.commandWarningsShown {
		return
	}
	a.commandWarningsShown = true
	if a.commands != nil {
		for _, warning := range a.commands.Warnings {
			a.emitNotice("Warning: " + skillDisplayLine(warning))
		}
	}
}

func runCommandsCommand(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("commands", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	workspacePath := flags.String("workspace", ".", "workspace to inspect")
	asJSON := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = fmt.Fprintln(output, "Usage: meldra commands [--workspace PATH] [--json]")
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("commands accepts only --workspace PATH and --json")
	}
	paths, err := ResolveConfigPaths()
	if err != nil {
		return err
	}
	workspace, err := NewWorkspace(*workspacePath, bufio.NewReader(strings.NewReader("")), io.Discard, false)
	if err != nil {
		return err
	}
	if err := protectRuntimePaths(workspace, paths); err != nil {
		return err
	}
	catalog, err := discoverCommands(ctx, workspace, paths)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(output).Encode(catalog)
	}
	for _, warning := range catalog.Warnings {
		if _, err := fmt.Fprintln(output, "Warning: "+skillDisplayLine(warning)); err != nil {
			return err
		}
	}
	for _, item := range catalog.Commands {
		if _, err := fmt.Fprintf(output, "/%s %s\t%s\n  %s\n", item.Name, skillDisplayLine(item.ArgumentHint), skillDisplayLine(item.Description), skillDisplayLine(item.Path)); err != nil {
			return err
		}
	}
	if len(catalog.Commands) == 0 {
		_, err = fmt.Fprintln(output, "No custom commands found.")
	}
	return err
}

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"meldra/internal/tool"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type verificationCommand struct {
	command string
	args    []string
}

func (w *Workspace) verify(raw json.RawMessage) (string, error) {
	var in struct {
		Preset string `json:"preset"`
	}
	if e := decodeToolInput(raw, &in, "preset"); e != nil {
		return "", e
	}
	if in.Preset == "diff" {
		return w.execute("git", []string{"diff", "--"}, 60)
	}
	commands, err := w.verificationCommands(in.Preset)
	if err != nil {
		return "", err
	}
	needsApproval := false
	var plan strings.Builder
	specs := make([]commandSpec, 0, len(commands))
	for _, command := range commands {
		spec, err := w.prepareCommand(command.command, command.args, 0)
		if err != nil {
			return "", err
		}
		specs = append(specs, spec)
		if spec.ApprovalRequired {
			needsApproval = true
		}
		if plan.Len() > 0 {
			plan.WriteByte('\n')
		}
		plan.WriteString(renderCommand(spec.Executable, spec.Arguments))
	}
	state, _ := json.Marshal(map[string]any{"commands": specs})
	if needsApproval && !w.requestApproval(ApprovalRequest{
		Kind:           ApprovalCommand,
		WorkspaceState: state,
		Title:          "Run verification plan with OS user privileges",
		Detail:         plan.String() + "\n\nThese commands run repository code with your OS user privileges and may access the filesystem and network.",
		Prompt:         fmt.Sprintf("Run verification plan with OS-user privileges?\n%s\nApprove %d command(s)? [y/N] ", plan.String(), len(commands)),
	}) {
		return "Declined; verification not run.", nil
	}

	var output strings.Builder
	for index, spec := range specs {
		// Completed earlier commands do not make a later unstarted process
		// uncertain. Keep accumulated exit codes and output, reset the boundary.
		previousFailure := false
		tool.Observe(w.ctx, func(o *tool.Observation) {
			previousFailure = o.Result.Status == tool.Failed
			o.Started = false
			o.Result.Status = ""
		})
		result, err := w.executePreparedCommand(spec, false)
		if err != nil {
			return "", err
		}
		tool.Observe(w.ctx, func(o *tool.Observation) {
			if previousFailure && o.Result.Status == "" {
				o.Result.Status = tool.Failed
			}
		})
		if index > 0 {
			output.WriteString("\n\n")
		}
		fmt.Fprintf(&output, "verification %d/%d\n", index+1, len(commands))
		output.WriteString(result)
		stop := false
		tool.Observe(w.ctx, func(o *tool.Observation) { stop = o.Result.Status == tool.Unknown || o.Err != nil })
		if stop {
			return output.String(), nil
		}
	}
	return output.String(), nil
}

func (w *Workspace) verificationCommands(preset string) ([]verificationCommand, error) {
	if !allIn([]string{preset}, []string{"test", "check", "build", "format"}) {
		return nil, fmt.Errorf("unknown preset")
	}
	if (preset == "test" || preset == "check") && w.makeTargetExists(preset) {
		return []verificationCommand{{command: "make", args: []string{preset}}}, nil
	}

	var commands []verificationCommand
	if w.hasRootFile("go.mod") {
		switch preset {
		case "test":
			commands = append(commands, verificationCommand{command: "go", args: []string{"test", "./..."}})
		case "check":
			commands = append(commands,
				verificationCommand{command: "go", args: []string{"vet", "./..."}},
				verificationCommand{command: "go", args: []string{"test", "./..."}},
			)
		case "build":
			commands = append(commands, verificationCommand{command: "go", args: []string{"build", "./..."}})
		case "format":
			args, err := w.goFormatArgs()
			if err != nil {
				return nil, err
			}
			if len(args) > 1 {
				commands = append(commands, verificationCommand{command: "gofmt", args: args})
			}
		}
	}

	if w.hasRootFile("pyproject.toml") || w.hasRootFile("pytest.ini") {
		if preset == "test" || preset == "check" {
			commands = append(commands, verificationCommand{command: "python3", args: []string{"-m", "pytest"}})
		}
	}

	if scripts, packageManager, ok, err := w.nodeProject(); err != nil {
		return nil, err
	} else if ok {
		script := nodeVerificationScript(preset, scripts)
		if script != "" {
			args := []string{"run", script}
			if script == "test" {
				args = []string{"test"}
			}
			commands = append(commands, verificationCommand{command: packageManager, args: args})
		}
	}

	if w.hasRootFile("Cargo.toml") {
		switch preset {
		case "test", "check", "build":
			commands = append(commands, verificationCommand{command: "cargo", args: []string{preset}})
		case "format":
			commands = append(commands, verificationCommand{command: "cargo", args: []string{"fmt", "--", "--check"}})
		}
	}

	if len(commands) == 0 {
		return nil, fmt.Errorf("no supported %s verification found from root project files", preset)
	}
	return commands, nil
}

func (w *Workspace) hasRootFile(name string) bool {
	info, err := os.Lstat(filepath.Join(w.root, name))
	return err == nil && info.Mode().IsRegular()
}

func (w *Workspace) makeTargetExists(target string) bool {
	contents, ok, err := w.readRootFile("Makefile")
	if err != nil || !ok {
		return false
	}
	for _, rawLine := range strings.Split(string(contents), "\n") {
		if rawLine == "" || rawLine[0] == '\t' {
			continue
		}
		line, _, _ := strings.Cut(rawLine, "#")
		left, _, found := strings.Cut(line, ":")
		if !found || strings.Contains(left, "=") {
			continue
		}
		for _, candidate := range strings.Fields(left) {
			if candidate == target {
				return true
			}
		}
	}
	return false
}

func (w *Workspace) readRootFile(name string) ([]byte, bool, error) {
	path := filepath.Join(w.root, name)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, nil
	}
	if info.Size() > maxEditableFileBytes {
		return nil, false, fmt.Errorf("project marker %s exceeds the %d byte limit", name, maxEditableFileBytes)
	}
	file, openedInfo, err := openCheckedRegularFile(path, info)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	if openedInfo.Size() > maxEditableFileBytes {
		return nil, false, fmt.Errorf("project marker %s exceeds the %d byte limit", name, maxEditableFileBytes)
	}
	contents, err := io.ReadAll(io.LimitReader(file, int64(maxEditableFileBytes)+1))
	if err != nil {
		return nil, false, err
	}
	if len(contents) > maxEditableFileBytes {
		return nil, false, fmt.Errorf("project marker %s exceeds the %d byte limit", name, maxEditableFileBytes)
	}
	return contents, true, nil
}

func (w *Workspace) nodeProject() (map[string]string, string, bool, error) {
	contents, ok, err := w.readRootFile("package.json")
	if err != nil || !ok {
		return nil, "", ok, err
	}
	var manifest struct {
		PackageManager string            `json:"packageManager"`
		Scripts        map[string]string `json:"scripts"`
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	if err := decoder.Decode(&manifest); err != nil {
		return nil, "", false, fmt.Errorf("parse package.json: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, "", false, fmt.Errorf("parse package.json: trailing data")
		}
		return nil, "", false, fmt.Errorf("parse package.json: trailing data: %w", err)
	}
	packageManager := "npm"
	if w.hasRootFile("pnpm-lock.yaml") || strings.HasPrefix(strings.ToLower(manifest.PackageManager), "pnpm@") {
		packageManager = "pnpm"
	}
	return manifest.Scripts, packageManager, true, nil
}

func nodeVerificationScript(preset string, scripts map[string]string) string {
	candidates := map[string][]string{
		"test":   {"test"},
		"check":  {"check", "test"},
		"build":  {"build"},
		"format": {"format:check", "fmt:check"},
	}
	for _, candidate := range candidates[preset] {
		if value := strings.TrimSpace(scripts[candidate]); value != "" {
			return candidate
		}
	}
	return ""
}

func (w *Workspace) goFormatArgs() ([]string, error) {
	args := []string{"-d"}
	err := w.walk(w.root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Type()&os.ModeSymlink == 0 && strings.HasSuffix(path, ".go") {
			rel, _ := filepath.Rel(w.root, path)
			args = append(args, rel)
		}
		return err
	})
	return args, err
}

func (w *Workspace) gitReview(raw json.RawMessage) (string, error) {
	var in struct{}
	if e := decodeToolInput(raw, &in); e != nil {
		return "", e
	}
	gitExecutable, err := w.trustedExecutable("git")
	if err != nil {
		return "", err
	}
	topLevel, err := gitTopLevel(w.ctx, w.root, gitExecutable)
	if err != nil {
		return "", err
	}
	if topLevel != w.root {
		return "", fmt.Errorf("workspace must be the Git repository root (%s)", topLevel)
	}
	run := func(args ...string) (string, error) {
		r, e := w.execute("git", args, 60)
		return r, e
	}
	var summary strings.Builder
	for _, section := range []struct {
		title string
		args  []string
	}{
		{"Recent commits", []string{"log", "-5", "--oneline"}},
		{"Status", []string{"status", "--short"}},
		{"Staged changes", []string{"diff", "--cached", "--stat", "--"}},
		{"Unstaged changes", []string{"diff", "--stat", "--"}},
	} {
		text, e := run(section.args...)
		if e != nil {
			return "", e
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		summary.WriteString("## " + section.title + "\n")
		summary.WriteString(strings.TrimRight(text, "\n"))
		summary.WriteString("\n\n")
	}
	staged, err := run("diff", "--cached", "--")
	if err != nil {
		return "", err
	}
	unstaged, err := run("diff", "--")
	if err != nil {
		return "", err
	}
	out := summary.String()
	// The summary is cheap to keep in context; the diffs are bounded and say so
	// rather than silently dropping the tail. Individual files can be read on
	// demand or reviewed one at a time.
	var diffs strings.Builder
	if staged = strings.TrimSpace(staged); staged != "" {
		diffs.WriteString("## Staged diff\n" + staged + "\n\n")
	}
	if unstaged = strings.TrimSpace(unstaged); unstaged != "" {
		diffs.WriteString("## Unstaged diff\n" + unstaged + "\n")
	}
	if diffs.Len() > 0 {
		body, truncated := capTextWithFlag(diffs.String())
		out += body + "\n"
		if truncated {
			out += fmt.Sprintf("\n[diffs truncated to %d bytes; the stat summary above lists every changed file]\n", maxToolOutput)
		}
	}
	if strings.TrimSpace(out) == "" {
		return "Working tree clean; no staged or unstaged changes.", nil
	}
	return out, nil
}

func gitTopLevel(parent context.Context, workspace, gitExecutable string) (string, error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	environment, cleanupEnvironment, err := newCommandEnvironment()
	if err != nil {
		return "", err
	}
	defer cleanupEnvironment()
	check := exec.CommandContext(ctx, gitExecutable, "-c", "core.fsmonitor=false", "rev-parse", "--show-toplevel")
	check.Dir = workspace
	check.Env = environment
	var output limitedBuffer
	output.limit = maxToolOutput
	check.Stdout = &output
	check.Stderr = io.Discard
	// rev-parse is a read-only preflight. A missing repository or cancelled
	// lookup has a known outcome; only the actual operation marks effects started.
	if err := runCommandProcess(ctx, check); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("Git workspace lookup cancelled: %w", ctx.Err())
		}
		return "", fmt.Errorf("workspace is not a git repository")
	}
	if output.truncated {
		return "", fmt.Errorf("Git workspace path exceeds output limit")
	}
	topLevel, err := filepath.EvalSymlinks(strings.TrimSpace(output.String()))
	if err != nil {
		return "", fmt.Errorf("resolve Git repository root: %w", err)
	}
	return filepath.Clean(topLevel), nil
}

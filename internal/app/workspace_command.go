package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

func (w *Workspace) runCommand(raw json.RawMessage) (string, error) {
	var in struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Timeout int      `json:"timeout"`
	}
	if e := decodeToolInput(raw, &in, "command"); e != nil {
		return "", e
	}
	return w.execute(in.Command, in.Args, in.Timeout)
}

func allowed(command string, args []string) bool {
	if command != filepath.Base(command) || !allSafeArgs(args) {
		return false
	}
	switch command {
	case "python3":
		return allowedPython3(args)
	case "go":
		return allowedGo(args)
	case "npm", "pnpm":
		return allowedNodePackageManager(args)
	case "cargo":
		return allowedCargo(args)
	case "gofmt":
		if len(args) <= 1 || args[0] != "-d" {
			return false
		}
		for _, argument := range args[1:] {
			if strings.HasPrefix(argument, "-") {
				return false
			}
		}
		return true
	case "make":
		return len(args) > 0 && allIn(args, []string{"check", "test"})
	case "git":
		return allowedGit(args)
	}
	return false
}

func allowedNodePackageManager(args []string) bool {
	if len(args) == 1 && args[0] == "test" {
		return true
	}
	return len(args) == 2 && args[0] == "run" && allIn(args[1:], []string{"test", "check", "build", "format:check", "fmt:check"})
}

func allowedCargo(args []string) bool {
	if len(args) == 1 {
		return allIn(args, []string{"test", "check", "build"})
	}
	return len(args) == 3 && args[0] == "fmt" && args[1] == "--" && args[2] == "--check"
}

func allowedPython3(args []string) bool {
	if len(args) == 1 && strings.HasSuffix(strings.ToLower(args[0]), ".py") {
		return true
	}
	// Support running pytest: python3 -m pytest [options...] [paths...]
	if len(args) >= 2 && args[0] == "-m" && args[1] == "pytest" {
		return allowedPytestArgs(args[2:])
	}
	return false
}

func allowedPytestArgs(args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			if !allowedPytestFlag(arg) {
				return false
			}
			continue
		}
		// A pytest node ID is a workspace path followed by one or more :: selectors.
		// Only the path portion is resolved at execution time.
		if !allowedPytestNodeID(arg) {
			return false
		}
	}
	return true
}

func allowedPytestNodeID(nodeID string) bool {
	// Pytest expands a positional argument beginning with @ as an argument
	// file after Meldra has validated the command line. That would allow the
	// file contents to bypass this allowlist.
	if strings.HasPrefix(nodeID, "@") {
		return false
	}
	path, _, _ := strings.Cut(nodeID, "::")
	return path != "" && allSafeArgs([]string{nodeID, path})
}

func allowedPytestFlag(arg string) bool {
	allowedFlags := []string{
		"-v", "-vv", "-q", "-s", "-x", "-rf", "-rE", "-rP", "-rN",
		"--no-header", "--disable-warnings", "--capture=no", "--tb=short", "--tb=line", "--tb=no",
	}
	for _, f := range allowedFlags {
		if arg == f {
			return true
		}
	}
	allowedPrefixes := []string{
		"--tb=", "--maxfail=", "-k=", "-m=", "-p=", "-W=", "--ignore=",
		"--durations=", "--rootdir=", "--config-file=", "--override-ini=",
		"--pythonpath=", "--junitxml=", "--log-file=", "--log-format=",
	}
	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(arg, prefix) {
			value := strings.TrimPrefix(arg, prefix)
			if prefix == "--override-ini=" {
				return allowedPytestOverrideINI(value)
			}
			return allSafeArgs([]string{value})
		}
	}
	return false
}

// allowedPytestOverrideINI accepts a single pytest name=value override. The
// addopts setting is deliberately excluded because it is parsed as another
// command line and could bypass the command allowlist.
func allowedPytestOverrideINI(override string) bool {
	key, value, ok := strings.Cut(override, "=")
	if !ok || !safePytestINIKey(key) || value == "" || key == "addopts" {
		return false
	}
	return allSafeArgs([]string{value})
}

func safePytestINIKey(key string) bool {
	for _, char := range key {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' && char != '-' && char != '.' {
			return false
		}
	}
	return key != ""
}

func (w *Workspace) validatePytestArgs(args []string) error {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			if err := w.validatePytestFlag(arg); err != nil {
				return err
			}
			continue
		}
		if err := w.validatePytestNodeID(arg); err != nil {
			return err
		}
	}
	return nil
}

func (w *Workspace) validatePytestNodeID(nodeID string) error {
	if !allowedPytestNodeID(nodeID) {
		return fmt.Errorf("pytest node ID is not allowlisted")
	}
	path, _, _ := strings.Cut(nodeID, "::")
	if _, err := w.resolve(path, false); err != nil {
		return fmt.Errorf("pytest node ID %q: %w", nodeID, err)
	}
	return nil
}

func (w *Workspace) validatePytestFlag(arg string) error {
	if !allowedPytestFlag(arg) {
		return fmt.Errorf("pytest option is not allowlisted")
	}
	for _, flag := range []struct {
		prefix string
		write  bool
	}{
		{"--ignore=", false},
		{"--rootdir=", false},
		{"--config-file=", false},
		{"--pythonpath=", false},
		{"--junitxml=", true},
		{"--log-file=", true},
	} {
		if strings.HasPrefix(arg, flag.prefix) {
			return w.validatePytestPath(strings.TrimPrefix(arg, flag.prefix), flag.write)
		}
	}
	if strings.HasPrefix(arg, "--override-ini=") {
		return w.validatePytestOverrideINI(strings.TrimPrefix(arg, "--override-ini="))
	}
	return nil
}

func (w *Workspace) validatePytestOverrideINI(override string) error {
	key, value, _ := strings.Cut(override, "=")
	switch key {
	case "cache_dir", "log_file":
		return w.validatePytestPath(value, true)
	case "pythonpath", "testpaths":
		return w.validatePytestPathList(value)
	default:
		return nil
	}
}

func (w *Workspace) validatePytestPathList(value string) error {
	paths := strings.Fields(value)
	if len(paths) == 0 {
		return fmt.Errorf("pytest path list must not be empty")
	}
	for _, path := range paths {
		if err := w.validatePytestPath(path, false); err != nil {
			return err
		}
	}
	return nil
}

func (w *Workspace) validatePytestPath(path string, write bool) error {
	if path == "" {
		return fmt.Errorf("pytest path must not be empty")
	}
	if pytestPathUsesExpansion(path) {
		return fmt.Errorf("pytest paths must not use home or environment expansion")
	}
	if _, err := w.resolve(path, write); err != nil {
		return fmt.Errorf("pytest path %q: %w", path, err)
	}
	return nil
}

// pytest expands ~ and environment-variable syntax in some path options after
// command validation. Reject them so the path passed to pytest is the same path
// that resolve verifies remains inside the workspace. Percent syntax matters on
// Windows, where os.path.expandvars supports %NAME%.
func pytestPathUsesExpansion(path string) bool {
	return strings.HasPrefix(path, "~") || strings.ContainsAny(path, "$%")
}

func allowedGo(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "run":
		if len(args) != 2 || !allSafeArgs(args[1:]) || strings.ContainsAny(args[1], "*?[]") || strings.Contains(args[1], "...") {
			return false
		}
		if args[1] != "." && !strings.HasPrefix(args[1], "."+string(filepath.Separator)) {
			return false
		}
		target := filepath.Clean(args[1])
		return target != ".." && !strings.HasPrefix(target, ".."+string(filepath.Separator))
	case "build":
		hasPackage := false
		for _, argument := range args[1:] {
			if argument == "./..." {
				hasPackage = true
				continue
			}
			if argument != "-trimpath" {
				return false
			}
		}
		return hasPackage
	case "vet":
		return len(args) == 1 || (len(args) == 2 && args[1] == "./...")
	case "test":
		for _, argument := range args[1:] {
			if argument == "." || argument == "./..." || argument == "-race" || argument == "-short" || argument == "-v" || strings.HasPrefix(argument, "-run=") || strings.HasPrefix(argument, "-count=") {
				continue
			}
			return false
		}
		return true
	default:
		return false
	}
}

func allowedGit(args []string) bool {
	if len(args) == 0 {
		return false
	}
	for _, argument := range args[1:] {
		if !allSafeArgs([]string{argument}) {
			return false
		}
	}
	switch args[0] {
	case "status":
		return allIn(args[1:], []string{"--short", "--porcelain"})
	case "diff":
		return allIn(args[1:], []string{"--", "--cached", "--stat", "--name-only", "--name-status"})
	case "log":
		for _, argument := range args[1:] {
			if argument == "--oneline" || argument == "--stat" || argument == "--" || regexp.MustCompile(`^-[1-9][0-9]*$`).MatchString(argument) {
				continue
			}
			return false
		}
		return true
	default:
		return false
	}
}

func allIn(xs, ok []string) bool {
	for _, x := range xs {
		found := false
		for _, y := range ok {
			if x == y {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func allSafeArgs(args []string) bool {
	for _, a := range args {
		if strings.ContainsAny(a, "\x00\n\r") || filepath.IsAbs(a) || a == ".." || strings.HasPrefix(a, ".."+string(filepath.Separator)) || a == "-C" || strings.HasPrefix(a, "-C=") || strings.HasPrefix(a, "--git-dir") || strings.HasPrefix(a, "--work-tree") || strings.HasPrefix(a, "--output") || a == "--ext-diff" || a == "--textconv" || strings.HasPrefix(a, "--open-files-in-pager") || strings.HasPrefix(a, "-exec") || strings.HasPrefix(a, "-toolexec") || strings.HasPrefix(a, "-vettool") || a == "-o" || strings.HasPrefix(a, "-o=") || strings.HasPrefix(a, "-coverprofile") {
			return false
		}
	}
	return true
}

func (w *Workspace) execute(command string, args []string, seconds int) (string, error) {
	return w.executeWithApproval(command, args, seconds, true)
}

func (w *Workspace) prepareCommand(command string, args []string, seconds int) (commandSpec, error) {
	args = slices.Clone(args)
	configured, custom := w.commandExecutables[command]
	if !custom && !allowed(command, args) {
		return commandSpec{}, fmt.Errorf("command is not allowlisted")
	}
	if command == "python3" && len(args) >= 2 && args[0] == "-m" && args[1] == "pytest" {
		if err := w.validatePytestArgs(args[2:]); err != nil {
			return commandSpec{}, err
		}
	}
	executable, err := w.trustedExecutable(command)
	if custom {
		executable, err = w.trustedExecutable(configured)
	}
	if err != nil {
		return commandSpec{}, err
	}
	if filepath.Base(executable) == "git" && custom && !allowedGit(args) {
		return commandSpec{}, fmt.Errorf("configured git still requires read-only allowlisted arguments")
	}
	if command == "git" || filepath.Base(executable) == "git" {
		topLevel, err := gitTopLevel(w.ctx, w.root, executable)
		if err != nil {
			return commandSpec{}, err
		}
		if topLevel != w.root {
			return commandSpec{}, fmt.Errorf("workspace must be the Git repository root (%s)", topLevel)
		}
		gitArgs := []string{"-c", "core.fsmonitor=false"}
		if len(args) > 0 && (args[0] == "diff" || args[0] == "show" || args[0] == "log") {
			gitArgs = append(gitArgs, args[0], "--no-ext-diff", "--no-textconv")
			args = append(gitArgs, args[1:]...)
		} else {
			args = append(gitArgs, args...)
		}
	}
	if command == "gofmt" {
		for _, argument := range args[1:] {
			path, err := w.resolve(argument, false)
			if err != nil {
				return commandSpec{}, err
			}
			info, err := os.Stat(path)
			if err != nil {
				return commandSpec{}, err
			}
			if !info.Mode().IsRegular() || filepath.Ext(path) != ".go" {
				return commandSpec{}, fmt.Errorf("gofmt paths must be Go files inside the workspace")
			}
		}
		args = append([]string{"-d", "--"}, args[1:]...)
	}
	if command == "go" && len(args) == 2 && args[0] == "run" {
		path, err := w.resolve(args[1], false)
		if err != nil {
			return commandSpec{}, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return commandSpec{}, err
		}
		if !info.IsDir() && (!info.Mode().IsRegular() || filepath.Ext(path) != ".go") {
			return commandSpec{}, fmt.Errorf("go run target must be a workspace directory or Go file")
		}
	}
	if command == "python3" && !(len(args) >= 2 && args[0] == "-m" && args[1] == "pytest") {
		path, err := w.resolve(args[0], false)
		if err != nil {
			return commandSpec{}, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return commandSpec{}, err
		}
		if !info.Mode().IsRegular() || strings.ToLower(filepath.Ext(path)) != ".py" {
			return commandSpec{}, fmt.Errorf("python3 target must be a workspace Python file")
		}
	}
	if seconds == 0 {
		seconds = w.commandTimeout
		if seconds == 0 {
			seconds = defaultTimeout
		}
	}
	if seconds < 1 || seconds > maxCommandTimeout {
		return commandSpec{}, fmt.Errorf("timeout must be 1..86400 seconds")
	}
	if len(args) > 256 {
		return commandSpec{}, fmt.Errorf("command has too many arguments")
	}
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) || len(arg) > 64<<10 {
			return commandSpec{}, fmt.Errorf("invalid command argument")
		}
	}
	mode, permissions, generation := w.policy.snapshot()
	return commandSpec{Mode: mode, PermissionPolicy: permissions, Generation: generation, Executable: executable, Arguments: args, Directory: w.root, TimeoutSeconds: seconds, EnvironmentPolicy: "isolated-home-v1", ApprovalRequired: custom || commandRequiresApproval(command), DisplayCommand: command}, nil
}

func (w *Workspace) trustedExecutable(command string) (string, error) {
	path, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("find executable %s: %w", command, err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve executable %s: %w", command, err)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve executable %s: %w", command, err)
	}
	canonical = filepath.Clean(canonical)
	if withinFold(w.root, canonical) {
		return "", fmt.Errorf("refusing to execute %s from inside the workspace", command)
	}
	for _, protected := range w.protected {
		if withinFold(protected, canonical) {
			return "", fmt.Errorf("refusing to execute %s from Meldra configuration storage", command)
		}
	}
	return canonical, nil
}

func commandRequiresApproval(command string) bool {
	return command == "python3" || command == "go" || command == "npm" || command == "pnpm" || command == "cargo" || command == "make"
}

func (w *Workspace) confirmCommand(command string, args []string) bool {
	text := renderCommand(command, args)
	return w.requestApproval(ApprovalRequest{
		Kind:   ApprovalCommand,
		Title:  "Run command with OS user privileges",
		Detail: text + "\n\nThis command runs repository code with your OS user privileges and may access the filesystem and network.",
		Prompt: fmt.Sprintf("Run command with OS-user privileges? %s [y/N] ", text),
	})
}

func renderCommand(command string, args []string) string {
	var rendered strings.Builder
	rendered.WriteString(command)
	for _, argument := range args {
		rendered.WriteByte(' ')
		rendered.WriteString(strconv.Quote(argument))
	}
	return rendered.String()
}

func newCommandEnvironment() ([]string, func(), error) {
	directory, err := os.MkdirTemp("", "meldra-command-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create command environment: %w", err)
	}
	home := filepath.Join(directory, "home")
	temporary := filepath.Join(directory, "tmp")
	if err := os.Mkdir(home, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return nil, nil, fmt.Errorf("create command home: %w", err)
	}
	if err := os.Mkdir(temporary, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return nil, nil, fmt.Errorf("create command temporary directory: %w", err)
	}
	return safeCommandEnvironment(home, temporary), func() { _ = os.RemoveAll(directory) }, nil
}

// safeCommandEnvironment deliberately starts from an empty environment. A
// temporary HOME keeps repository commands from loading user shell or tool
// configuration, while the narrow allowlist retains normal command runtime.
func safeCommandEnvironment(home, temporary string) []string {
	environment := []string{
		"HOME=" + home,
		"TMPDIR=" + temporary,
		"TMP=" + temporary,
		"TEMP=" + temporary,
		"GOFLAGS=",
		"GIT_PAGER=cat",
		"PAGER=cat",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_OPTIONAL_LOCKS=0",
	}
	for _, name := range []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TZ"} {
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

type limitedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	room := b.limit - b.Len()
	if room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
			b.truncated = true
		} else {
			b.Buffer.Write(p)
		}
	} else {
		b.truncated = true
	}
	return n, nil
}

func (b *limitedBuffer) String() string {
	return truncateUTF8Text(b.Buffer.String(), b.limit, b.truncated)
}

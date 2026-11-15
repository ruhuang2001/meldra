package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"meldra/internal/tool"
)

type gitCommitInput struct {
	Paths   []string `json:"paths"`
	Message string   `json:"message"`
}

// Git writes have their own approval boundary; run_command stays read-only.
func (w *Workspace) gitCommit(raw json.RawMessage) (string, error) {
	prepared, err := w.prepareGitCommit(raw)
	if err != nil {
		return "", err
	}
	spec, paths, request := prepared.spec, prepared.paths, prepared.request
	state := string(request.WorkspaceState)
	if !w.requestApproval(request) {
		return "Declined; no commit created.", nil
	}
	return w.executeGitCommit(spec, paths, prepared.message, state)
}

type preparedGitCommit struct {
	spec    commandSpec
	paths   []string
	message string
	request ApprovalRequest
}

func (w *Workspace) prepareGitCommit(raw json.RawMessage) (_ *preparedGitCommit, err error) {
	var in gitCommitInput
	if err := decodeToolInput(raw, &in, "paths", "message"); err != nil {
		return nil, err
	}
	if err := validateChangeCount(len(in.Paths)); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Message) == "" || len(in.Message) > 4096 || !utf8.ValidString(in.Message) || strings.ContainsRune(in.Message, 0) {
		return nil, fmt.Errorf("message must be nonempty UTF-8 text of at most 4096 bytes without NUL")
	}
	spec, err := w.prepareCommand("git", []string{"status", "--short"}, 60)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(in.Paths))
	for i, path := range in.Paths {
		if filepath.IsAbs(path) || path == "" || strings.ContainsAny(path, "\x00\r\n") {
			return nil, fmt.Errorf("paths must be explicit workspace-relative files")
		}
		abs, err := w.resolve(path, true)
		if err != nil {
			return nil, err
		}
		paths[i], err = filepath.Rel(w.root, abs)
		if err != nil || paths[i] == "." {
			return nil, fmt.Errorf("directories cannot be committed; list individual files")
		}
	}
	slices.Sort(paths)
	if len(slices.Compact(slices.Clone(paths))) != len(paths) {
		return nil, fmt.Errorf("duplicate commit paths")
	}
	if w.projectContext != nil {
		if err := w.projectContext.BeforeWrite(paths); err != nil {
			return nil, err
		}
	}
	// Refuse an existing staged selection rather than silently absorbing it.
	staged, err := w.commitGitRead(spec, "diff", "--cached", "--name-only")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(staged) != "" {
		return nil, fmt.Errorf("index already has staged changes; commit or unstage them explicitly first")
	}
	state, preview, err := w.gitCommitState(spec, paths)
	if err != nil {
		return nil, err
	}
	// Reuse git_review's status/log evidence, supplemented with exact selected
	// diffs and complete new-file contents that git diff does not display.
	review, err := w.gitReview(json.RawMessage(`{}`))
	if err != nil {
		return nil, err
	}
	var previewErr error
	tool.Observe(w.ctx, func(o *tool.Observation) {
		if o.Result.Status != "" || o.Err != nil {
			previewErr = fmt.Errorf("Git review did not complete successfully")
			return
		}
		// Completed read-only previews are not evidence that a write started.
		o.Started = false
		o.Result.ExitCode = nil
	})
	if previewErr != nil {
		return nil, previewErr
	}
	request := ApprovalRequest{Kind: ApprovalCommand, Title: "Commit selected workspace files", WorkspaceState: json.RawMessage(state),
		Detail: "Message:\n" + in.Message + "\n\nSelected files:\n" + strings.Join(paths, "\n") + "\n\n" + preview + "\n\n" + review + "\nHooks and commit signing are disabled; configured Git filters are rejected. Failure can leave these files staged; inspect git_review before retrying.",
		Prompt: "Commit these files? [y/N] "}
	return &preparedGitCommit{spec: spec, paths: paths, message: in.Message, request: request}, nil
}

func (w *Workspace) executeGitCommit(spec commandSpec, paths []string, message, state string) (string, error) {
	current, _, err := w.gitCommitState(spec, paths)
	if err != nil {
		return "", err
	}
	if current != state {
		return "", fmt.Errorf("repository changed after review; request a new approval")
	}
	if w.projectContext != nil {
		if err := w.projectContext.BeforeWrite(paths); err != nil {
			return "", err
		}
	}
	// Every mutating subprocess uses the normal admission, cancellation, logs,
	// and unknown-outcome path. Never infer success from a nil Go error.
	spec.Arguments = append(commitGitPrefix(), "add", "--")
	spec.Arguments = append(spec.Arguments, paths...)
	output, err := w.executePreparedCommand(spec, false)
	if err != nil {
		return output, err
	}
	stop := false
	tool.Observe(w.ctx, func(o *tool.Observation) {
		stop = o.Result.Status == tool.Unknown || o.Result.Status == tool.Failed || o.Result.Status == tool.Cancelled || o.Err != nil
	})
	if stop || !strings.Contains(output, "\nstatus: 0\n") {
		return output, nil
	}
	// Check the complete staged diff, so an unrelated concurrent git add or a
	// content-changing clean filter cannot enter the approved commit.
	names, err := w.commitGitRead(spec, "diff", "--cached", "--name-only", "--no-renames", "-z", "--")
	if err != nil {
		return output, err
	}
	for name := range strings.SplitSeq(strings.TrimSuffix(names, "\x00"), "\x00") {
		if !slices.Contains(paths, name) {
			return output, fmt.Errorf("unreviewed files are staged; inspect git_review")
		}
	}
	var evidence struct{ Head, Index, Files, Entries string }
	if err := json.Unmarshal([]byte(state), &evidence); err != nil {
		return output, err
	}
	current, _, err = w.gitCommitState(spec, paths)
	if err != nil {
		return output, err
	}
	var after struct{ Head, Index, Files, Entries string }
	if err := json.Unmarshal([]byte(current), &after); err != nil {
		return output, err
	}
	entries, err := w.commitGitRead(spec, append([]string{"ls-files", "--stage", "-z", "--"}, paths...)...)
	if err != nil {
		return output, err
	}
	if after.Head != evidence.Head || after.Files != evidence.Files || digest([]byte(entries)) != evidence.Entries {
		return output, fmt.Errorf("staged content differs from the reviewed files; files may remain staged, inspect git_review")
	}
	spec.Arguments = append(commitGitPrefix(), "commit", "--no-gpg-sign", "-m", message)
	result, err := w.executePreparedCommand(spec, false)
	return output + "\n" + result, err
}

func commitGitPrefix() []string {
	return []string{"--literal-pathspecs", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "core.quotePath=true"}
}

func (w *Workspace) commitGitRead(spec commandSpec, args ...string) (string, error) {
	return w.commitGitReadInput(spec, nil, args...)
}

func (w *Workspace) commitGitReadInput(spec commandSpec, input io.Reader, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(w.ctx, time.Minute)
	defer cancel()
	env, cleanup, err := newCommandEnvironment()
	if err != nil {
		return "", err
	}
	defer cleanup()
	cmd := exec.CommandContext(ctx, spec.Executable, append(commitGitPrefix(), args...)...)
	cmd.Dir, cmd.Env = w.root, env
	cmd.Stdin = input
	output := &limitedBuffer{limit: maxApprovalPreviewBytes}
	cmd.Stdout, cmd.Stderr = output, output
	if err := runCommandProcess(ctx, cmd); err != nil {
		return "", fmt.Errorf("Git preflight: %w: %s", err, output.String())
	}
	if output.truncated {
		return "", fmt.Errorf("commit evidence exceeds preview limit; select fewer files")
	}
	return output.String(), nil
}

func (w *Workspace) gitCommitState(spec commandSpec, paths []string) (string, string, error) {
	if err := w.rejectGitFilters(spec); err != nil {
		return "", "", err
	}
	// These states can change commit parentage or finish a sequenced operation
	// without changing HEAD or the staged diff. Recheck after approval/staging.
	directory, err := w.commitGitRead(spec, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", "", err
	}
	for _, operation := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply", "sequencer"} {
		if _, err := os.Lstat(filepath.Join(strings.TrimSpace(directory), operation)); err == nil {
			return "", "", fmt.Errorf("Git operation %s is in progress; finish or abort it outside Meldra before committing", operation)
		} else if !os.IsNotExist(err) {
			return "", "", err
		}
	}
	head, err := w.commitGitRead(spec, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("git_commit requires an existing initial commit: %w", err)
	}
	index, err := w.commitGitRead(spec, "diff", "--cached", "--no-ext-diff", "--no-textconv", "--binary")
	if err != nil {
		return "", "", err
	}
	diff, err := w.commitGitRead(spec, append([]string{"diff", "HEAD", "--no-ext-diff", "--no-textconv", "--no-renames", "--binary", "--"}, paths...)...)
	if err != nil {
		return "", "", err
	}
	var files, additions, entries strings.Builder
	for _, path := range paths {
		abs, err := w.resolve(path, true)
		if err != nil {
			return "", "", err
		}
		data, exists, mode, err := readEditableFile(abs)
		if err != nil {
			return "", "", err
		}
		fmt.Fprintf(&files, "%q %t %o %s\n", path, exists, mode, digest(data))
		if exists {
			blob, err := w.commitGitReadInput(spec, strings.NewReader(string(data)), "hash-object", "--stdin", "--no-filters")
			if err != nil {
				return "", "", err
			}
			gitMode := "100644"
			if mode&0o111 != 0 {
				gitMode = "100755"
			}
			fmt.Fprintf(&entries, "%s %s 0\t%s\x00", gitMode, strings.TrimSpace(blob), path)
		}
		tracked, err := w.commitGitRead(spec, "ls-tree", "HEAD", "--", path)
		if err != nil {
			return "", "", err
		}
		if tracked == "" {
			if !exists {
				return "", "", fmt.Errorf("%q does not exist and is not tracked", path)
			}
			if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
				return "", "", fmt.Errorf("new binary files require manual review and commit")
			}
			fmt.Fprintf(&additions, "\nNew file %q (mode %o):\n%s\n", path, mode, data)
			if additions.Len() > maxApprovalPreviewBytes {
				return "", "", fmt.Errorf("new files exceed commit preview limit")
			}
		}
	}
	if diff == "" && additions.Len() == 0 {
		return "", "", fmt.Errorf("selected files have no changes")
	}
	if len(diff)+additions.Len() > maxApprovalPreviewBytes {
		return "", "", fmt.Errorf("commit evidence exceeds preview limit; select fewer files")
	}
	state, _ := json.Marshal(struct{ Head, Index, Files, Entries string }{head, digest([]byte(index)), digest([]byte(files.String())), digest([]byte(entries.String()))})
	return string(state), diff + additions.String(), nil
}

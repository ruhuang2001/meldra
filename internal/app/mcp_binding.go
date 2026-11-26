package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"meldra/internal/task"
)

func mcpConnectionBinding(ctx context.Context, paths ConfigPaths, name string, config mcpServerConfig, workspace *Workspace) (string, error) {
	var executable, executableDigest string
	var oauthGeneration string
	var environment []string
	if config.Command != "" {
		command := newMCPCommand(config, workspace)
		if command.Err != nil {
			return "", fmt.Errorf("MCP executable cannot be resolved")
		}
		path := command.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(command.Dir, path)
		}
		path, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", fmt.Errorf("MCP executable cannot be resolved")
		}
		executable, err = filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("MCP executable cannot be resolved")
		}
		bundled := config.fromPlugin && filepath.IsAbs(config.Command)
		var file *os.File
		if bundled {
			if executable != config.Command || config.pluginExecDigest == "" {
				return "", fmt.Errorf("bundled MCP executable changed since approval")
			}
			file, err = openPluginRegularFile(config.pluginRoot, executable, 1<<30)
		} else {
			info, statErr := os.Stat(executable)
			if statErr != nil || !info.Mode().IsRegular() || info.Size() > 1<<30 {
				return "", fmt.Errorf("MCP executable must be a regular file within 1 GiB")
			}
			file, _, err = openCheckedRegularFile(executable, info)
		}
		if err != nil {
			return "", fmt.Errorf("MCP executable cannot be inspected")
		}
		executableDigest, err = hashMCPExecutable(ctx, file)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			return "", fmt.Errorf("MCP executable cannot be inspected")
		}
		if bundled && executableDigest != config.pluginExecDigest {
			return "", fmt.Errorf("bundled MCP executable changed since approval")
		}
		environment = command.Env
	} else {
		if config.BearerTokenEnv != "" {
			environment = append(environment, os.Getenv(config.BearerTokenEnv))
		}
		if config.OAuth != nil && config.OAuth.ClientSecretEnv != "" {
			environment = append(environment, os.Getenv(config.OAuth.ClientSecretEnv))
		}
		if config.OAuth != nil {
			data, _, err := readMCPOAuthFile(mcpOAuthPath(paths, name) + ".generation")
			if err != nil {
				return "", err
			}
			oauthGeneration = string(data)
		}
	}
	raw, err := json.Marshal(struct {
		Config           mcpServerConfig
		Transport        string            `json:",omitempty"`
		CWD              string            `json:",omitempty"`
		Headers          map[string]string `json:",omitempty"`
		PluginRoot       string            `json:",omitempty"`
		PluginData       string            `json:",omitempty"`
		FromPlugin       bool              `json:",omitzero"`
		Executable       string
		ExecutableDigest string
		Environment      []string
		OAuthGeneration  string
	}{config, config.transport, config.cwd, config.headers, config.pluginRoot, config.pluginData, config.fromPlugin, executable, executableDigest, environment, oauthGeneration})
	if err != nil {
		return "", err
	}
	return digest(raw), nil
}

func hashMCPExecutable(ctx context.Context, file *os.File) (string, error) {
	return copyAndHashMCPExecutable(ctx, file, io.Discard)
}

func copyAndHashMCPExecutable(ctx context.Context, file *os.File, dst io.Writer) (string, error) {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<30 {
		return "", fmt.Errorf("MCP executable must be a regular file within 1 GiB")
	}
	hash := sha256.New()
	w := io.MultiWriter(dst, hash)
	reader := io.LimitReader(file, info.Size()+1)
	buffer := make([]byte, 128<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := reader.Read(buffer)
		total += int64(n)
		if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
			return "", writeErr
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	if total != info.Size() {
		return "", fmt.Errorf("MCP executable changed during inspection")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func snapshotPluginCommand(ctx context.Context, home string, config mcpServerConfig) (path string, cleanup func() error, err error) {
	if _, err := exec.LookPath(config.Command); err != nil {
		return "", nil, err
	}
	src, err := openPluginRegularFile(config.pluginRoot, config.Command, 1<<30)
	if err != nil {
		return "", nil, err
	}
	defer src.Close()
	if err := checkPluginSnapshotSource(src); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(home, ".plugin-exec-*")
	if err != nil {
		return "", nil, err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dir)
		}
	}()
	path = filepath.Join(dir, filepath.Base(config.Command))
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o500)
	if err != nil {
		return "", nil, err
	}
	sum, err := copyAndHashMCPExecutable(ctx, src, dst)
	if err := errors.Join(err, dst.Close()); err != nil {
		return "", nil, err
	}
	if sum != config.pluginExecDigest {
		return "", nil, fmt.Errorf("bundled MCP executable changed since approval")
	}
	return path, func() error { return os.RemoveAll(dir) }, nil
}

func checkApprovalConnections(ctx context.Context, paths ConfigPaths, options ChatOptions, workspace *Workspace) error {
	if options.ApproveDetailHash == "" {
		return nil
	}
	db, err := taskStoreForRead(paths)
	if err != nil {
		return err
	}
	defer db.Close()
	approvals, err := db.PendingApprovals(ctx, options.Resume)
	if err != nil {
		return err
	}
	for _, approval := range approvals {
		if approvalHash(approval) != options.ApproveDetailHash {
			continue
		}
		call, err := db.GetToolCall(ctx, approval.ToolCallID)
		if err != nil {
			return err
		}
		run, err := db.GetRun(ctx, approval.RunID)
		if err != nil {
			return err
		}
		if run.Status != task.RunWaitingApproval || call.Status != task.ToolCancelled || call.Result.Output != "Awaiting explicit approval; operation not admitted." {
			return fmt.Errorf("approval is not a cleanly suspended operation")
		}
		var detail approvalDetail
		if err := json.Unmarshal(approval.Detail, &detail); err != nil {
			return err
		}
		if detail.MCPServer == "" {
			if strings.HasPrefix(call.Name, "mcp_") {
				return fmt.Errorf("MCP approval has no connection binding; request a new approval")
			}
			return nil
		}
		servers, _, err := loadAllMCPConfigs(ctx, paths)
		if err != nil {
			return err
		}
		config, ok := servers[detail.MCPServer]
		if !ok || config.Disabled || detail.MCPBinding == "" {
			return fmt.Errorf("MCP connection configuration changed; request a new approval")
		}
		binding, err := mcpConnectionBinding(ctx, paths, detail.MCPServer, config, workspace)
		if err != nil {
			return err
		}
		if binding != detail.MCPBinding {
			return fmt.Errorf("MCP connection configuration changed; request a new approval")
		}
		workspace.mcpExpectedBinding = map[string]string{detail.MCPServer: binding}
		return nil
	}
	return fmt.Errorf("no pending approval matches --approve-detail-hash")
}

func mcpApprovalArguments(input json.RawMessage) string {
	compact, err := json.Marshal(input)
	if err != nil {
		return "invalid JSON"
	}
	return string(compact)
}

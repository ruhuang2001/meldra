//go:build !darwin && !linux

package app

import (
	"context"
	"fmt"
	"os"
)

type privateLock struct{}

func acquirePrivateLock(context.Context, string) (*privateLock, error) {
	return nil, fmt.Errorf("private file locking is supported on Darwin and Linux only")
}
func (*privateLock) Close() error { return nil }
func mcpOAuthLockPath(paths ConfigPaths, name string) string {
	return paths.Home + string(os.PathSeparator) + "mcp-oauth-" + name + ".lock"
}

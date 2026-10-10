//go:build !darwin && !linux

package app

import (
	"context"
	"fmt"
	"os"
)

type mcpOAuthLock struct{}

func acquireMCPOAuthLock(context.Context, string) (*mcpOAuthLock, error) {
	return nil, fmt.Errorf("OAuth cache ownership is supported on Darwin and Linux only")
}
func (*mcpOAuthLock) Close() error { return nil }
func mcpOAuthLockPath(paths ConfigPaths, name string) string {
	return paths.Home + string(os.PathSeparator) + "mcp-oauth-" + name + ".lock"
}

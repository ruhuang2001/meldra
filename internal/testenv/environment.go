// Package testenv isolates application test state and shares it with helper
// subprocesses. It is only imported by test entry points.
package testenv

import (
	"fmt"
	"os"
	"path/filepath"
)

func Run(run func() int) (code int) {
	if root := os.Getenv("MELDRA_TEST_HOME"); root != "" && os.Getenv("HOME") == root && os.Getenv("XDG_CACHE_HOME") == filepath.Join(root, "cache") {
		// Preserve a child helper's explicit fixture within the isolated home.
		return run()
	}
	root, err := os.MkdirTemp("", "meldra-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() {
		if err := os.RemoveAll(root); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}()
	for key, value := range map[string]string{"HOME": root, "XDG_CACHE_HOME": filepath.Join(root, "cache"), "MELDRA_TEST_HOME": root, "MELDRA_HOME": ""} {
		old, exists := os.LookupEnv(key)
		defer func() {
			if exists {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		}()
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return run()
}

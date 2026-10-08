package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("MELDRA_TEST_HOME") != "" {
		os.Exit(m.Run())
	}
	root, err := os.MkdirTemp("", "meldra-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for key, value := range map[string]string{"HOME": root, "XDG_CACHE_HOME": filepath.Join(root, "cache"), "MELDRA_TEST_HOME": root} {
		if err := os.Setenv(key, value); err != nil {
			os.RemoveAll(root)
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	code := m.Run()
	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

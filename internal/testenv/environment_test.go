package testenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunClearsInheritedConfigAndKeepsChildFixtures(t *testing.T) {
	external := t.TempDir()
	t.Setenv("MELDRA_HOME", external)
	t.Setenv("MELDRA_TEST_HOME", "")
	keys := []string{"HOME", "XDG_CACHE_HOME", "MELDRA_TEST_HOME", "MELDRA_HOME"}
	original := make(map[string]string)
	present := make(map[string]bool)
	for _, key := range keys {
		original[key], present[key] = os.LookupEnv(key)
	}
	var isolated string
	code := Run(func() int {
		isolated = os.Getenv("HOME")
		if os.Getenv("MELDRA_HOME") != "" || isolated == external {
			t.Fatal("inherited config escaped isolation")
		}
		if _, err := os.Stat(isolated); err != nil {
			t.Fatal(err)
		}
		fixture := filepath.Join(isolated, "fixture")
		if err := os.Setenv("MELDRA_HOME", fixture); err != nil {
			t.Fatal(err)
		}
		return Run(func() int {
			if os.Getenv("HOME") != isolated || os.Getenv("MELDRA_HOME") != fixture {
				t.Fatal("child fixture lost")
			}
			return 7
		})
	})
	if code != 7 || os.Getenv("MELDRA_HOME") != external {
		t.Fatal("test environment not restored")
	}
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok != present[key] || ok && value != original[key] {
			t.Fatalf("%s leaked: %q", key, value)
		}
	}
	if _, err := os.Stat(isolated); !os.IsNotExist(err) {
		t.Fatal("test home leaked")
	}
}

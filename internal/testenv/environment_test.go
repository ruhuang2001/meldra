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
	if _, err := os.Stat(isolated); !os.IsNotExist(err) {
		t.Fatal("test home leaked")
	}
}

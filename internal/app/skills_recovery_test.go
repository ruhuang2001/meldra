package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillDiscoverySkipsUnresolvableNativeHome(t *testing.T) {
	w, _ := testWorkspace(t, t.TempDir(), "", false)
	path := filepath.Join(w.root, ".agents", "skills", "demo", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: demo\ndescription: valid\n---\nbody\n"), 0600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "loop")
	if err := os.Symlink(bad, bad); err != nil {
		t.Fatal(err)
	}
	catalog, err := discoverSkills(t.Context(), w, ConfigPaths{Home: bad})
	if err != nil || catalog.meldraHome != "" || len(catalog.Skills) != 1 || catalog.Skills[0].Name != "demo" {
		t.Fatalf("catalog=%+v err=%v", catalog, err)
	}
	if len(catalog.Warnings) != 1 || !strings.Contains(catalog.Warnings[0], filepath.Join(bad, "skills")) {
		t.Fatalf("warnings=%v", catalog.Warnings)
	}
	w.protected = []string{w.root}
	if err := catalog.checkPath(path); err == nil {
		t.Fatal("missing canonical home granted a protected-path exception")
	}
}

package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meldra/internal/tool"
)

func TestSkillsReadProtectsResourcesAndLoadsFreshContent(t *testing.T) {
	w, _ := testWorkspace(t, t.TempDir(), "", false)
	state := t.TempDir()
	w.protected = append(w.protected, state)
	root := filepath.Join(w.root, ".agents", "skills", "demo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	header := "---\nname: demo\ndescription: Fixture metadata\n---\n"
	write(filepath.Join(root, "SKILL.md"), header+"INITIAL_BODY")
	secret := filepath.Join(state, "credentials.env")
	write(secret, "PRIVATE_CREDENTIAL")
	if err := os.Link(secret, filepath.Join(root, "hardlink.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "symlink.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(state, filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	catalog, err := discoverSkills(t.Context(), w, ConfigPaths{Home: state})
	if err != nil || len(catalog.Skills) != 1 {
		t.Fatalf("catalog=%+v err=%v", catalog, err)
	}
	if strings.Contains(catalog.instructions(), "INITIAL_BODY") {
		t.Fatal("discovery exposed skill body")
	}
	write(filepath.Join(root, "SKILL.md"), header+"FRESH_BODY")
	write(filepath.Join(root, "reference.md"), "REFERENCE_BODY")
	registry, err := tool.New([]ToolDefinition{catalog.definition()})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"SKILL.md", "reference.md", "hardlink.txt", "symlink.txt", "linked-dir/credentials.env", secret, "../SKILL.md"} {
		raw, _ := json.Marshal(map[string]string{"name": "demo", "path": path})
		result, err := registry.Invoke(t.Context(), "read_skill", raw)
		want := map[string]string{"SKILL.md": "FRESH_BODY", "reference.md": "REFERENCE_BODY"}[path]
		if want != "" {
			if err != nil || result.Status != tool.Succeeded || !strings.Contains(result.Output, want) {
				t.Fatalf("read %s: status=%s err=%v output=%s", path, result.Status, err, result.Output)
			}
		} else if err == nil || result.Status != tool.Failed {
			t.Fatalf("unsafe read %s: status=%s err=%v", path, result.Status, err)
		}
		if strings.Contains(result.Output, "PRIVATE_CREDENTIAL") || len(result.Attachments) > 0 && strings.Contains(string(result.Attachments[0].Content), "PRIVATE_CREDENTIAL") {
			t.Fatal("protected content leaked into result or evidence")
		}
	}
}

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

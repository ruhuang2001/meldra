package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMakeDirectoryTreeDurableSyncsNewParents(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "one", "two")
	var synced []string
	if err := makeDirectoryTreeDurable(target, 0o700, func(path string) error {
		synced = append(synced, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{root, filepath.Join(root, "one")}; !reflect.DeepEqual(synced, want) {
		t.Fatalf("synced parents = %q, want %q", synced, want)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("created directory = %v, %v", info, err)
	}

	if err := makeDirectoryTreeDurable(target, 0o700, func(path string) error {
		t.Fatalf("synced existing directory parent %q", path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMakeDirectoryTreeDurableStopsOnParentSyncFailure(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "one", "two")
	want := errors.New("sync failed")
	err := makeDirectoryTreeDurable(target, 0o700, func(string) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("directory creation error = %v, want sync failure", err)
	}
	if _, err := os.Stat(filepath.Join(root, "one")); err != nil {
		t.Fatalf("first directory was not created: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("directory below failed sync was created: %v", err)
	}
}

//go:build darwin || linux

package app

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"meldra/internal/store"
	"meldra/internal/task"
)

func updateTestArchive(t *testing.T, headers ...tar.Header) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	for _, header := range headers {
		if err := tw.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := io.CopyN(tw, strings.NewReader("new-binary"), header.Size); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

type updateFixture struct {
	u        releaseUpdater
	target   string
	archive  []byte
	checksum string
	release  string
	requests atomic.Int32
}

func newUpdateFixture(t *testing.T, current string) *updateFixture {
	t.Helper()
	old := version
	version = current
	t.Cleanup(func() { version = old })
	t.Setenv(MeldraHomeEnv, filepath.Join(t.TempDir(), "config"))
	f := &updateFixture{
		target:  filepath.Join(t.TempDir(), "meldra"),
		archive: updateTestArchive(t, tar.Header{Name: "meldra", Typeflag: tar.TypeReg, Size: 10, Mode: 0755}),
		release: `{"tag_name":"v0.3.0"}`,
	}
	if err := os.WriteFile(f.target, []byte("old-binary"), 0755); err != nil {
		t.Fatal(err)
	}
	archiveName, err := updateArchiveName()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		switch r.URL.Path {
		case "/latest":
			fmt.Fprint(w, f.release)
		case "/v0.3.0/meldra_checksums.txt":
			if f.checksum != "" {
				fmt.Fprint(w, f.checksum)
			} else {
				fmt.Fprintf(w, "%x  %s\n", sha256.Sum256(f.archive), archiveName)
			}
		case "/v0.3.0/" + archiveName:
			_, _ = w.Write(f.archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	f.u = releaseUpdater{client: server.Client(), apiURL: server.URL + "/latest", downloadURL: server.URL, executable: func() (string, error) { return f.target, nil }}
	return f
}

func TestUpdateVersionContract(t *testing.T) {
	for _, tc := range []struct {
		current                string
		available, development bool
	}{
		{"v0.2.0", true, false},
		{"0.3.0", false, false},
		{"v0.4.0", false, false},
		{"0.3.0-rc.10", true, false},
		{"1.0.0-rc.2", false, false},
		{"0.3.0+build.2", false, false},
		{"dev", false, true},
		{"abc123-dirty", false, true},
		{"0.03.0", false, true},
		{"0.3.0-01", false, true},
		{"0.10.0", false, false},
		{"999999999999999999999999.0.0", false, false},
	} {
		t.Run(tc.current, func(t *testing.T) {
			f := newUpdateFixture(t, tc.current)
			var out bytes.Buffer
			err := f.u.run(t.Context(), nil, &out)
			if tc.development {
				if err == nil || !strings.Contains(err.Error(), "development build") || f.requests.Load() != 0 {
					t.Fatalf("development build: %v, %d requests", err, f.requests.Load())
				}
			} else if err != nil {
				t.Fatal(err)
			}
			want := "old-binary"
			if tc.available {
				want = "new-binary"
				if !strings.Contains(out.String(), "Updated Meldra to v0.3.0") {
					t.Fatalf("missing result: %s", out.String())
				}
			} else if !tc.development && !strings.Contains(out.String(), "no update installed") {
				t.Fatalf("missing no-update result: %s", out.String())
			}
			if data, _ := os.ReadFile(f.target); string(data) != want {
				t.Fatalf("binary %q, want %q", data, want)
			}
			if _, err := os.Stat(os.Getenv(MeldraHomeEnv)); !os.IsNotExist(err) {
				t.Fatalf("update created configuration state: %v", err)
			}
		})
	}
}

func TestUpdateInstallAndRejections(t *testing.T) {
	for _, scenario := range []string{"install", "checksum", "corrupt", "gzip-crc", "missing", "symlink-entry", "duplicate", "traversal", "symlink-target", "managed", "readonly", "task-lock", "session-lock", "legacy-session-lock"} {
		t.Run(scenario, func(t *testing.T) {
			f := newUpdateFixture(t, "0.2.0")
			wantError, installed := "", false
			original := f.target
			var afterUpdate func()
			switch scenario {
			case "install":
				installed = true
			case "checksum":
				f.checksum, wantError = strings.Repeat("0", 64)+"  "+mustUpdateArchiveName(t), "checksum mismatch"
			case "corrupt":
				f.archive, wantError = []byte("not gzip"), "unpack update"
			case "gzip-crc":
				f.archive[len(f.archive)-8] ^= 0xff
				wantError = "unpack update"
			case "missing", "traversal":
				name := "other"
				if scenario == "traversal" {
					name = "../meldra"
				}
				f.archive = updateTestArchive(t, tar.Header{Name: name, Typeflag: tar.TypeReg, Size: 10})
				wantError = "does not contain"
			case "symlink-entry":
				f.archive = updateTestArchive(t, tar.Header{Name: "meldra", Typeflag: tar.TypeSymlink, Linkname: "elsewhere"})
				wantError = "invalid or duplicate"
			case "duplicate":
				f.archive = updateTestArchive(t, tar.Header{Name: "meldra", Typeflag: tar.TypeReg, Size: 10}, tar.Header{Name: "meldra", Typeflag: tar.TypeReg, Size: 10})
				wantError = "invalid or duplicate"
			case "symlink-target":
				f.target = filepath.Join(t.TempDir(), "meldra-link")
				if err := os.Symlink(original, f.target); err != nil {
					t.Fatal(err)
				}
				installed = true
			case "managed":
				f.target = filepath.Join(t.TempDir(), "Cellar", "meldra")
				if err := os.MkdirAll(filepath.Dir(f.target), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.target, []byte("old-binary"), 0755); err != nil {
					t.Fatal(err)
				}
				original, wantError = f.target, "brew upgrade"
			case "readonly":
				if os.Geteuid() == 0 {
					t.Skip("root bypasses directory permissions")
				}
				if err := os.Chmod(filepath.Dir(original), 0555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(filepath.Dir(original), 0755) })
				wantError = "not writable"
			case "task-lock":
				s, err := store.Open(filepath.Join(t.TempDir(), "tasks"))
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				record, err := s.EnsureTask(t.Context(), task.Task{ID: store.NewID(), Goal: "continue across update", Workspace: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				lease, err := s.Acquire(t.Context(), record.ID, record.Workspace)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Close()
				run, err := s.StartRun(t.Context(), lease, task.Run{TaskID: record.ID, Config: task.Config{Model: "offline", Provider: "fake"}, Executor: "test"})
				if err != nil {
					t.Fatal(err)
				}
				installed = true
				afterUpdate = func() {
					if err := s.FinishRun(t.Context(), lease, run.ID, task.RunSucceeded, "continued after update"); err != nil {
						t.Fatal(err)
					}
					got, err := s.GetTask(t.Context(), record.ID)
					if err != nil || got.Status != task.Completed {
						t.Fatalf("task could not continue: %+v %v", got, err)
					}
				}
			case "session-lock", "legacy-session-lock":
				paths, _ := ResolveConfigPaths()
				dir := filepath.Join(taskDirectory(paths), "sessions")
				if scenario == "legacy-session-lock" {
					dir = filepath.Join(paths.Home, "sessions")
				}
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				unlock, err := acquireSessionStoreLock(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
				installed = true
			}
			var out bytes.Buffer
			err := f.u.run(t.Context(), nil, &out)
			if wantError == "" && err != nil || wantError != "" && (err == nil || !strings.Contains(err.Error(), wantError)) {
				t.Fatalf("error = %v, want %q; output %s", err, wantError, out.String())
			}
			want := "old-binary"
			if installed {
				want = "new-binary"
			}
			if got, err := os.ReadFile(original); err != nil || string(got) != want {
				t.Fatalf("binary %q, %v; want %q", got, err, want)
			}
			if afterUpdate != nil {
				afterUpdate()
			}
			if installed {
				info, err := os.Stat(original)
				if err != nil || info.Mode().Perm() != 0755 {
					t.Fatalf("binary mode: %v %v", info, err)
				}
				if scenario == "symlink-target" {
					info, _ := os.Lstat(f.target)
					if info.Mode()&os.ModeSymlink == 0 {
						t.Fatal("symlink replaced")
					}
				}
			}
			leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(original), ".meldra.update-*"))
			if len(leftovers) != 0 {
				t.Fatalf("temporary files left: %v", leftovers)
			}
		})
	}
}

func mustUpdateArchiveName(t *testing.T) string {
	t.Helper()
	name, err := updateArchiveName()
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func TestUpdateRunningExecutableHelper(t *testing.T) {
	if os.Getenv("MELDRA_UPDATE_RUNNING_HELPER") != "1" {
		return
	}
	fmt.Println("old session ready")
	if line, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil || line != "continue\n" {
		t.Fatalf("continue input: %q %v", line, err)
	}
	fmt.Println("old session continued")
}

func TestUpdateKeepsRunningExecutableAlive(t *testing.T) {
	f := newUpdateFixture(t, "0.2.0")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.OpenFile(f.target, os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, f.target, "-test.run=^TestUpdateRunningExecutableHelper$")
	command.Env = append(os.Environ(), "MELDRA_UPDATE_RUNNING_HELPER=1")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = command.Process.Kill(); _ = command.Wait() }()
	reader := bufio.NewReader(stdout)
	if line, err := reader.ReadString('\n'); err != nil || line != "old session ready\n" {
		t.Fatalf("start: %q %v", line, err)
	}
	if err := f.u.run(t.Context(), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, "continue\n"); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || line != "old session continued\n" {
		t.Fatalf("old process stopped after update: %q %v", line, err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("old process exited unsuccessfully: %v %s", err, stderr.String())
	}
	if data, _ := os.ReadFile(f.target); string(data) != "new-binary" {
		t.Fatalf("update not installed: %q", data)
	}
}

func TestUpdateRejectsInvalidReleaseAndArguments(t *testing.T) {
	for _, release := range []string{`{`, `{"tag_name":"v0.3.0-rc.1"}`, `{"tag_name":"v0.3.0","draft":true}`, `{"tag_name":"v0.3.0","prerelease":true}`, `{"tag_name":"../escape"}`, strings.Repeat("x", (1<<20)+1)} {
		f := newUpdateFixture(t, "0.2.0")
		f.release = release
		if err := f.u.run(t.Context(), nil, io.Discard); err == nil {
			t.Fatal("accepted invalid release")
		}
	}
	for _, args := range [][]string{{"update", "--check"}, {"update", "--json"}, {"update", "--yes"}, {"update", "--force"}, {"update", "--unknown"}} {
		if err := runCLI(args, strings.NewReader(""), io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	var out bytes.Buffer
	if err := runCLI([]string{"update", "--help"}, strings.NewReader(""), &out); err != nil || !strings.Contains(out.String(), "Usage: meldra update\n") || strings.Contains(out.String(), "--force") {
		t.Fatalf("help: %s %v", out.String(), err)
	}
}

func TestUpdateCachedAndAsyncNotices(t *testing.T) {
	for _, scenario := range []string{"fresh", "expired", "missing", "failure", "disabled", "dev"} {
		t.Run(scenario, func(t *testing.T) {
			f := newUpdateFixture(t, "0.2.0")
			t.Setenv("MELDRA_DISABLE_UPDATE_CHECK", "")
			paths, _ := ResolveConfigPaths()
			if err := ensureConfigHome(paths); err != nil {
				t.Fatal(err)
			}
			cachePath := filepath.Join(paths.Home, "update-check.json")
			state := updateCheckState{CheckedAt: time.Now(), Latest: "v0.3.0"}
			if scenario == "expired" || scenario == "failure" {
				state.CheckedAt = time.Now().Add(-25 * time.Hour)
				state.Latest = "v0.2.0"
			}
			if scenario != "missing" {
				data, _ := json.Marshal(state)
				if err := os.WriteFile(cachePath, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "failure" {
				f.release = "invalid"
			}
			if scenario == "disabled" {
				t.Setenv("MELDRA_DISABLE_UPDATE_CHECK", "1")
			}
			current := "0.2.0"
			if scenario == "dev" {
				current = "dev"
			}
			events := make(chan UIEvent, 4)
			f.u.startUpdateCheck(t.Context(), paths, current, UIEventSinkFunc(func(event UIEvent) { events <- event }))
			if scenario == "disabled" || scenario == "dev" || scenario == "failure" {
				select {
				case event := <-events:
					t.Fatalf("unexpected notice: %+v", event)
				case <-time.After(60 * time.Millisecond):
				}
				if scenario != "failure" && f.requests.Load() != 0 {
					t.Fatal("disabled check made request")
				}
				return
			}
			select {
			case event := <-events:
				if event.Kind != UIEventNotice || event.Text != `Update available: 0.2.0 -> v0.3.0. Run "meldra update" in another terminal.` {
					t.Fatalf("notice: %+v", event)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("missing update notice")
			}
			if scenario == "fresh" {
				if f.requests.Load() != 0 {
					t.Fatal("fresh cache fetched network")
				}
				return
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				data, _ := os.ReadFile(cachePath)
				var refreshed updateCheckState
				_ = json.Unmarshal(data, &refreshed)
				if refreshed.Latest == "v0.3.0" && time.Since(refreshed.CheckedAt) < time.Minute {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("cache was not refreshed: %s", data)
				}
				time.Sleep(time.Millisecond)
			}
			info, err := os.Stat(cachePath)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("cache permissions: %v %v", info, err)
			}
		})
	}
}

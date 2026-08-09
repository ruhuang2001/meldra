package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeResult(t *testing.T, root, name, contents string) string {
	t.Helper()
	path := filepath.Join(root, name, "result.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildBaselineAggregatesOneAttemptPerTask(t *testing.T) {
	root := t.TempDir()
	first := writeResult(t, root, "go-task", `{
		"task_slug":"task","language":"go","status":"pass",
		"attempts":[{"duration_ns":1000000}],"total_time_ns":1500000,
		"completed_at":"2026-08-09T10:00:00+08:00"
	}`)
	second := writeResult(t, root, "rust-other", `{
		"task_slug":"other","language":"rust","status":"fail",
		"attempts":[{"duration_ns":2000000}],"total_time_ns":0,
		"completed_at":"2026-08-09T11:00:00+08:00"
	}`)

	got, err := buildBaseline(options{
		model:         "gpt-test",
		provider:      "test-provider",
		tier:          "core",
		version:       "v0.1.0-test",
		estimatedCost: "$0.12",
		results:       []string{second, first},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tasks != 2 || got.Passed != 1 || got.Failed != 1 || got.DurationMS != 3 {
		t.Fatalf("baseline totals = %#v", got)
	}
	if got.Date != "2026-08-09T03:00:00Z" || got.Duration != "3.5ms" {
		t.Fatalf("baseline metadata = %#v", got)
	}
	if len(got.Results) != 2 || got.Results[0].Language != "go" || got.Results[1].Language != "rust" {
		t.Fatalf("baseline results are not stable and sorted: %#v", got.Results)
	}
}

func TestBuildBaselineRejectsDuplicateTasks(t *testing.T) {
	root := t.TempDir()
	first := writeResult(t, root, "first", `{"task_slug":"task","language":"go","status":"pass","total_time_ns":1}`)
	second := writeResult(t, root, "second", `{"task_slug":"task","language":"go","status":"fail","total_time_ns":1}`)
	_, err := buildBaseline(options{model: "gpt-test", results: []string{first, second}}, time.Now)
	if err == nil || !strings.Contains(err.Error(), "duplicate result") {
		t.Fatalf("duplicate task error = %v", err)
	}
}

func TestRunWritesExplicitBaselineAndRefusesOverwrite(t *testing.T) {
	root := t.TempDir()
	result := writeResult(t, root, "task", `{"task_slug":"task","language":"go","status":"pass","total_time_ns":1000000}`)
	output := filepath.Join(root, "baselines", "baseline.json")
	var stdout bytes.Buffer
	now := func() time.Time { return time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC) }
	version := func() string { return "v0.1.0-test" }
	args := []string{"--model", "gpt-test", "--date", "2026-08-09T12:00:00+08:00", "--output", output, result}
	if err := run(args, &stdout, now, version); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("file output wrote to stdout: %q", stdout.String())
	}
	contents, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var recorded baseline
	if err := json.Unmarshal(contents, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.MeldraVersion != "v0.1.0-test" || recorded.Date != "2026-08-09T04:00:00Z" {
		t.Fatalf("recorded baseline = %#v", recorded)
	}
	if err := run(args, &stdout, now, version); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("overwrite error = %v", err)
	}
}

func TestRunRequiresModelAndResult(t *testing.T) {
	var stdout bytes.Buffer
	if err := run(nil, &stdout, time.Now, func() string { return "v0" }); err == nil || !strings.Contains(err.Error(), "--model") {
		t.Fatalf("missing model error = %v", err)
	}
	if err := run([]string{"--model", "gpt-test"}, &stdout, time.Now, func() string { return "v0" }); err == nil || !strings.Contains(err.Error(), "result.json") {
		t.Fatalf("missing result error = %v", err)
	}
}

func TestRunRejectsInvalidDateAndMissingGlob(t *testing.T) {
	root := t.TempDir()
	result := writeResult(t, root, "task", `{"task_slug":"task","language":"go","status":"pass"}`)
	var stdout bytes.Buffer
	if err := run([]string{"--model", "gpt-test", "--date", "tomorrow", result}, &stdout, time.Now, func() string { return "v0" }); err == nil || !strings.Contains(err.Error(), "RFC3339") {
		t.Fatalf("invalid date error = %v", err)
	}
	missing := filepath.Join(root, "missing", "*.json")
	if err := run([]string{"--model", "gpt-test", missing}, &stdout, time.Now, func() string { return "v0" }); err == nil || !strings.Contains(err.Error(), "matched no files") {
		t.Fatalf("missing glob error = %v", err)
	}
}

func TestReadResultRejectsMalformedAndOversizedFiles(t *testing.T) {
	root := t.TempDir()
	malformed := writeResult(t, root, "malformed", "not json")
	if _, err := readResult(malformed); err == nil || !strings.Contains(err.Error(), "parse result") {
		t.Fatalf("malformed result error = %v", err)
	}
	oversized := filepath.Join(root, "oversized.json")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("x", maxResultFileBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readResult(oversized); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized result error = %v", err)
	}
}

func TestBuildBaselineRejectsInvalidFieldsAndDurations(t *testing.T) {
	root := t.TempDir()
	missingField := writeResult(t, root, "missing", `{"task_slug":"task","status":"pass"}`)
	if _, err := buildBaseline(options{model: "gpt-test", results: []string{missingField}}, time.Now); err == nil || !strings.Contains(err.Error(), "missing language") {
		t.Fatalf("missing-field error = %v", err)
	}
	negativeTotal := writeResult(t, root, "negative-total", `{"task_slug":"task","language":"go","status":"pass","total_time_ns":-1}`)
	if _, err := buildBaseline(options{model: "gpt-test", results: []string{negativeTotal}}, time.Now); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("negative total error = %v", err)
	}
	negativeAttempt := writeResult(t, root, "negative-attempt", `{"task_slug":"task","language":"go","status":"pass","attempts":[{"duration_ns":-1}]}`)
	if _, err := buildBaseline(options{model: "gpt-test", results: []string{negativeAttempt}}, time.Now); err == nil || !strings.Contains(err.Error(), "attempt duration_ns") {
		t.Fatalf("negative attempt error = %v", err)
	}
}

func TestBaselineDateUsesInjectedClockWithoutCompletion(t *testing.T) {
	root := t.TempDir()
	result := writeResult(t, root, "task", `{"task_slug":"task","language":"go","status":"pass"}`)
	now := func() time.Time { return time.Date(2026, 8, 9, 1, 2, 3, 0, time.FixedZone("CST", 8*60*60)) }
	got, err := buildBaseline(options{model: "gpt-test", results: []string{result}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Date != "2026-08-08T17:02:03Z" {
		t.Fatalf("injected baseline date = %q", got.Date)
	}
}

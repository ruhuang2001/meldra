// Command sanity-report records a comparable Meldra SanityHarness baseline.
// It intentionally consumes only result.json files supplied by the caller and
// never executes a benchmark or reads task source, prompts, or raw output.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	baselineSchemaVersion = 1
	maxResultFileBytes    = 8 << 20
)

type baseline struct {
	SchemaVersion int            `json:"schema_version"`
	Harness       string         `json:"harness"`
	MeldraVersion string         `json:"meldra_version"`
	Model         string         `json:"model"`
	Provider      string         `json:"provider"`
	Tier          string         `json:"tier"`
	Date          string         `json:"date"`
	Tasks         int            `json:"tasks"`
	Passed        int            `json:"passed"`
	Failed        int            `json:"failed"`
	Duration      string         `json:"duration"`
	DurationMS    int64          `json:"duration_ms"`
	EstimatedCost string         `json:"estimated_cost"`
	Results       []taskBaseline `json:"results"`
}

type taskBaseline struct {
	Language   string `json:"language"`
	Task       string `json:"task"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Attempts   int    `json:"attempts"`
}

type harnessResult struct {
	TaskSlug    string    `json:"task_slug"`
	Language    string    `json:"language"`
	Status      string    `json:"status"`
	Attempts    []attempt `json:"attempts"`
	TotalTimeNS int64     `json:"total_time_ns"`
	CompletedAt time.Time `json:"completed_at"`
}

type attempt struct {
	DurationNS int64 `json:"duration_ns"`
}

type options struct {
	model         string
	provider      string
	tier          string
	version       string
	estimatedCost string
	date          string
	output        string
	results       []string
}

type repeatedFlag []string

func (values *repeatedFlag) String() string {
	return strings.Join(*values, ",")
}

func (values *repeatedFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout, time.Now, currentVersion); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer, now func() time.Time, version func() string) error {
	flags := flag.NewFlagSet("sanity-report", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var resultPaths repeatedFlag
	options := options{}
	flags.StringVar(&options.model, "model", "", "model identifier used for the evaluation (required)")
	flags.StringVar(&options.provider, "provider", "unknown", "provider identifier")
	flags.StringVar(&options.tier, "tier", "core", "SanityHarness tier")
	flags.StringVar(&options.version, "meldra-version", "", "Meldra version or git revision")
	flags.StringVar(&options.estimatedCost, "estimated-cost", "unknown", "estimated evaluation cost")
	flags.StringVar(&options.date, "date", "", "RFC3339 run date; defaults to the latest result completion time")
	flags.StringVar(&options.output, "output", "-", "baseline output path, or - for stdout")
	flags.Var(&resultPaths, "result", "result.json path or glob; repeatable")
	flags.Usage = func() {
		fmt.Fprintln(stdout, "Usage: sanity-report --model MODEL [options] RESULT.json [RESULT.json ...]")
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "Record an explicit, comparable baseline from one SanityHarness run.")
		fmt.Fprintln(stdout, "The tool rejects duplicate language/task results so stale attempts cannot be mixed in.")
		flags.SetOutput(stdout)
		flags.PrintDefaults()
		flags.SetOutput(io.Discard)
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	options.results = append(resultPaths, flags.Args()...)
	if options.model == "" {
		return fmt.Errorf("--model is required")
	}
	if len(options.results) == 0 {
		return fmt.Errorf("at least one result.json path is required")
	}
	if options.version == "" {
		options.version = version()
	}
	if options.version == "" {
		options.version = "unknown"
	}

	baseline, err := buildBaseline(options, now)
	if err != nil {
		return err
	}
	contents, err := json.MarshalIndent(baseline, "", "  ")
	if err != nil {
		return fmt.Errorf("encode baseline: %w", err)
	}
	contents = append(contents, '\n')
	if options.output == "-" {
		_, err := stdout.Write(contents)
		return err
	}
	return writeBaseline(options.output, contents)
}

func buildBaseline(options options, now func() time.Time) (baseline, error) {
	paths, err := expandResultPaths(options.results)
	if err != nil {
		return baseline{}, err
	}
	seenTasks := make(map[string]struct{}, len(paths))
	results := make([]taskBaseline, 0, len(paths))
	var durationNS int64
	var latest time.Time
	passed := 0
	for _, path := range paths {
		result, err := readResult(path)
		if err != nil {
			return baseline{}, err
		}
		if result.TaskSlug == "" || result.Language == "" || result.Status == "" {
			return baseline{}, fmt.Errorf("%s: result is missing language, task_slug, or status", path)
		}
		key := result.Language + "/" + result.TaskSlug
		if _, duplicate := seenTasks[key]; duplicate {
			return baseline{}, fmt.Errorf("duplicate result for %s; pass exactly one attempt per task", key)
		}
		seenTasks[key] = struct{}{}
		taskDurationNS, err := resultDurationNS(result)
		if err != nil {
			return baseline{}, fmt.Errorf("%s: %w", path, err)
		}
		if taskDurationNS > (1<<63-1)-durationNS {
			return baseline{}, fmt.Errorf("combined result duration overflows int64")
		}
		durationNS += taskDurationNS
		if result.CompletedAt.After(latest) {
			latest = result.CompletedAt
		}
		results = append(results, taskBaseline{
			Language:   result.Language,
			Task:       result.TaskSlug,
			Status:     result.Status,
			DurationMS: taskDurationNS / int64(time.Millisecond),
			Attempts:   len(result.Attempts),
		})
		if result.Status == "pass" {
			passed++
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Language == results[j].Language {
			return results[i].Task < results[j].Task
		}
		return results[i].Language < results[j].Language
	})

	date, err := baselineDate(options.date, latest, now)
	if err != nil {
		return baseline{}, err
	}
	duration := time.Duration(durationNS)
	return baseline{
		SchemaVersion: baselineSchemaVersion,
		Harness:       "SanityHarness",
		MeldraVersion: options.version,
		Model:         options.model,
		Provider:      options.provider,
		Tier:          options.tier,
		Date:          date.Format(time.RFC3339),
		Tasks:         len(results),
		Passed:        passed,
		Failed:        len(results) - passed,
		Duration:      duration.String(),
		DurationMS:    durationNS / int64(time.Millisecond),
		EstimatedCost: options.estimatedCost,
		Results:       results,
	}, nil
}

func expandResultPaths(inputs []string) ([]string, error) {
	seenPaths := map[string]struct{}{}
	var paths []string
	for _, input := range inputs {
		matches, err := filepath.Glob(input)
		if err != nil {
			return nil, fmt.Errorf("expand result path %q: %w", input, err)
		}
		if len(matches) == 0 {
			if strings.ContainsAny(input, "*?[") {
				return nil, fmt.Errorf("result path pattern %q matched no files", input)
			}
			matches = []string{input}
		}
		for _, match := range matches {
			clean := filepath.Clean(match)
			if _, duplicate := seenPaths[clean]; duplicate {
				continue
			}
			seenPaths[clean] = struct{}{}
			paths = append(paths, clean)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func readResult(path string) (harnessResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return harnessResult{}, fmt.Errorf("open result %s: %w", path, err)
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maxResultFileBytes+1))
	if err != nil {
		return harnessResult{}, fmt.Errorf("read result %s: %w", path, err)
	}
	if len(contents) > maxResultFileBytes {
		return harnessResult{}, fmt.Errorf("result %s exceeds the %d byte limit", path, maxResultFileBytes)
	}
	var result harnessResult
	if err := json.Unmarshal(contents, &result); err != nil {
		return harnessResult{}, fmt.Errorf("parse result %s: %w", path, err)
	}
	return result, nil
}

func resultDurationNS(result harnessResult) (int64, error) {
	if result.TotalTimeNS < 0 {
		return 0, fmt.Errorf("total_time_ns must not be negative")
	}
	if result.TotalTimeNS > 0 {
		return result.TotalTimeNS, nil
	}
	var total int64
	for _, attempt := range result.Attempts {
		if attempt.DurationNS < 0 {
			return 0, fmt.Errorf("attempt duration_ns must not be negative")
		}
		if attempt.DurationNS > (1<<63-1)-total {
			return 0, fmt.Errorf("attempt durations overflow int64")
		}
		total += attempt.DurationNS
	}
	return total, nil
}

func baselineDate(value string, latest time.Time, now func() time.Time) (time.Time, error) {
	if value != "" {
		date, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return time.Time{}, fmt.Errorf("--date must be RFC3339: %w", err)
		}
		return date.UTC(), nil
	}
	if !latest.IsZero() {
		return latest.UTC(), nil
	}
	return now().UTC(), nil
}

func writeBaseline(path string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create baseline directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("baseline already exists at %s", path)
		}
		return fmt.Errorf("create baseline: %w", err)
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return fmt.Errorf("write baseline: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close baseline: %w", err)
	}
	return nil
}

func currentVersion() string {
	command := exec.Command("git", "describe", "--tags", "--always", "--dirty")
	output, err := command.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

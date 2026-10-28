package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkSearchFiles_SmallRepo benchmarks searching a small repository (100 files).
// This establishes a baseline for the searchFiles optimization in commit 9e82501.
func BenchmarkSearchFiles_SmallRepo(b *testing.B) {
	w := setupBenchWorkspace(b, 100, 5*1024) // 100 files, 5KB each
	query := json.RawMessage(`{"query":"TODO","path":null,"glob":null,"max_results":100}`)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := w.searchFiles(query)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSearchFiles_LargeRepo benchmarks searching approaching maxSearchFiles limit.
func BenchmarkSearchFiles_LargeRepo(b *testing.B) {
	w := setupBenchWorkspace(b, 1000, 10*1024) // 1000 files, 10KB each
	query := json.RawMessage(`{"query":"func","path":null,"glob":null,"max_results":500}`)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := w.searchFiles(query)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSearchFiles_NoMatches benchmarks the case where no matches are found.
// This tests the early-exit optimization path.
func BenchmarkSearchFiles_NoMatches(b *testing.B) {
	w := setupBenchWorkspace(b, 500, 10*1024)
	query := json.RawMessage(`{"query":"ZZZNOMATCHPATTERN","path":null,"glob":null,"max_results":100}`)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		w.searchFiles(query)
	}
}

// BenchmarkSearchFiles_GlobFilter benchmarks searching with glob filtering.
func BenchmarkSearchFiles_GlobFilter(b *testing.B) {
	w := setupBenchWorkspace(b, 500, 10*1024)
	// Create mix of .go and .txt files
	for i := 0; i < 100; i++ {
		path := filepath.Join(w.root, fmt.Sprintf("file%d.txt", i))
		os.WriteFile(path, []byte("some content\n"), 0644)
	}

	query := json.RawMessage(`{"query":"content","path":null,"glob":"*.go","max_results":100}`)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		w.searchFiles(query)
	}
}

// BenchmarkReadFile_Sequential benchmarks sequential file reads.
func BenchmarkReadFile_Sequential(b *testing.B) {
	w := setupBenchWorkspace(b, 10, 50*1024) // 10 files, 50KB each
	queries := make([]json.RawMessage, 10)
	for i := 0; i < 10; i++ {
		queries[i] = json.RawMessage(fmt.Sprintf(`{"path":"file%d.go","offset":0,"limit":100}`, i))
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		for _, query := range queries {
			_, err := w.readFile(query)
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkListFiles_FlatDirectory benchmarks listing a flat directory structure.
func BenchmarkListFiles_FlatDirectory(b *testing.B) {
	w := setupBenchWorkspace(b, 1000, 1024)
	query := json.RawMessage(`{"path":null}`)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := w.listFiles(query)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkListFiles_NestedDirectory benchmarks listing a nested directory structure.
func BenchmarkListFiles_NestedDirectory(b *testing.B) {
	w := setupNestedBenchWorkspace(b, 10, 10) // 10 dirs, 10 files each = 100 total
	query := json.RawMessage(`{"path":null}`)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := w.listFiles(query)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// setupBenchWorkspace creates a temporary workspace with the specified number of files.
func setupBenchWorkspace(b *testing.B, fileCount, avgSize int) *Workspace {
	b.Helper()
	dir := b.TempDir()

	// Create files with realistic Go code patterns
	for i := 0; i < fileCount; i++ {
		path := filepath.Join(dir, fmt.Sprintf("file%d.go", i))
		content := generateGoCode(avgSize, i)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			b.Fatal(err)
		}
	}

	w := &Workspace{
		root:      dir,
		protected: []string{},
	}

	return w
}

// setupNestedBenchWorkspace creates a nested directory structure.
func setupNestedBenchWorkspace(b *testing.B, dirCount, filesPerDir int) *Workspace {
	b.Helper()
	dir := b.TempDir()

	for d := 0; d < dirCount; d++ {
		subdir := filepath.Join(dir, fmt.Sprintf("pkg%d", d))
		if err := os.MkdirAll(subdir, 0755); err != nil {
			b.Fatal(err)
		}

		for f := 0; f < filesPerDir; f++ {
			path := filepath.Join(subdir, fmt.Sprintf("file%d.go", f))
			content := generateGoCode(10*1024, d*filesPerDir+f)
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				b.Fatal(err)
			}
		}
	}

	w := &Workspace{
		root:      dir,
		protected: []string{},
	}

	return w
}

// generateGoCode generates realistic Go code with the specified size and seed.
func generateGoCode(targetSize, seed int) string {
	var code string

	// Package declaration
	code += fmt.Sprintf("package bench%d\n\n", seed)

	// Some imports
	code += "import (\n\t\"fmt\"\n\t\"strings\"\n)\n\n"

	// Generate functions until we reach target size
	funcCount := 0
	for len(code) < targetSize {
		code += fmt.Sprintf("// TODO: Implement func%d\n", funcCount)
		code += fmt.Sprintf("func Process%d(input string) (string, error) {\n", funcCount)
		code += "\tif input == \"\" {\n"
		code += "\t\treturn \"\", fmt.Errorf(\"empty input\")\n"
		code += "\t}\n"
		code += "\tresult := strings.ToUpper(input)\n"
		code += "\treturn result, nil\n"
		code += "}\n\n"
		funcCount++
	}

	return code
}

package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

func readEditableFile(path string) ([]byte, bool, fs.FileMode, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, 0o644, nil
	}
	if err != nil {
		return nil, false, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, 0, fmt.Errorf("target is not a regular file")
	}
	if info.Size() > maxEditableFileBytes {
		return nil, false, 0, fmt.Errorf("refusing to edit %s: file exceeds the %d byte editable-file limit", filepath.Base(path), maxEditableFileBytes)
	}

	file, openedInfo, err := openCheckedRegularFile(path, info)
	if err != nil {
		return nil, false, 0, err
	}
	defer file.Close()
	if openedInfo.Size() > maxEditableFileBytes {
		return nil, false, 0, fmt.Errorf("refusing to edit %s: file exceeds the %d byte editable-file limit", filepath.Base(path), maxEditableFileBytes)
	}
	contents, err := io.ReadAll(io.LimitReader(file, int64(maxEditableFileBytes)+1))
	if err != nil {
		return nil, false, 0, err
	}
	if len(contents) > maxEditableFileBytes {
		return nil, false, 0, fmt.Errorf("refusing to edit %s: file exceeds the %d byte editable-file limit", filepath.Base(path), maxEditableFileBytes)
	}
	return contents, true, openedInfo.Mode().Perm(), nil
}

func openCheckedRegularFile(path string, expected fs.FileInfo) (*os.File, fs.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !opened.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, fmt.Errorf("target is not a regular file")
	}
	if expected == nil || !os.SameFile(expected, opened) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("file changed while it was being opened")
	}
	return file, opened, nil
}

func (w *Workspace) readFile(raw json.RawMessage) (string, error) {
	var in struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decodeToolInput(raw, &in, "path"); err != nil {
		return "", err
	}
	if err := w.contextErr(); err != nil {
		return "", fmt.Errorf("read file cancelled: %w", err)
	}
	p, err := w.resolve(in.Path, false)
	if err != nil {
		return "", err
	}
	if err := w.contextObservePath(p); err != nil {
		return "", err
	}
	pathInfo, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if !pathInfo.Mode().IsRegular() {
		return "", fmt.Errorf("read_file only supports regular files")
	}
	file, _, err := openCheckedRegularFile(p, pathInfo)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var probe [8192]byte
	probeN, probeErr := file.Read(probe[:])
	if probeErr != nil && !errors.Is(probeErr, io.EOF) {
		return "", fmt.Errorf("read file: %w", probeErr)
	}
	if binarySample(probe[:probeN]) {
		return fmt.Sprintf("[binary file; %d bytes; use a binary-aware tool to inspect it]", pathInfo.Size()), nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind file: %w", err)
	}
	off := in.Offset
	if off == 0 {
		off = 1
	}
	if off < 1 {
		return "", fmt.Errorf("offset must be at least 1")
	}
	limit := in.Limit
	if limit == 0 {
		limit = 200
	}
	if limit < 1 {
		return "", fmt.Errorf("limit must be positive")
	}
	if limit > maxReadLines {
		limit = maxReadLines
	}
	var out strings.Builder
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), maxLineBytes)
	lineNumber, shown, scannedBytes := 0, 0, 0
	more := false
	for scanner.Scan() {
		if err := w.contextErr(); err != nil {
			return "", fmt.Errorf("read file cancelled: %w", err)
		}
		if binarySample(scanner.Bytes()) {
			return fmt.Sprintf("[binary file; %d bytes; use a binary-aware tool to inspect it]", pathInfo.Size()), nil
		}
		lineNumber++
		scannedBytes += len(scanner.Bytes()) + 1
		if scannedBytes > maxSearchBytes {
			more = true
			break
		}
		if lineNumber > off+limit-1 {
			more = true
			break
		}
		if lineNumber >= off {
			line := fmt.Sprintf("%6d\t%s\n", lineNumber, scanner.Text())
			if out.Len()+len(line) > maxToolOutput {
				more = true
				break
			}
			out.WriteString(line)
			shown++
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read file lines: %w", err)
	}
	start, end := 0, 0
	if shown > 0 {
		start, end = off, off+shown-1
	}
	fmt.Fprintf(&out, "[lines %d-%d; truncated=%t]", start, end, more)
	return out.String(), nil
}

func binarySample(sample []byte) bool {
	if bytes.IndexByte(sample, 0) >= 0 {
		return true
	}
	if utf8.Valid(sample) {
		return false
	}
	lastRune := len(sample) - 1
	for lastRune >= 0 && !utf8.RuneStart(sample[lastRune]) {
		lastRune--
	}
	return lastRune < 0 || utf8.FullRune(sample[lastRune:]) || !utf8.Valid(sample[:lastRune])
}

func (w *Workspace) walk(start string, fn fs.WalkDirFunc) error {
	if err := w.contextErr(); err != nil {
		return err
	}
	entry, err := os.Lstat(start)
	if err != nil {
		return err
	}
	rootEntry := fs.FileInfoToDirEntry(entry)
	if err := fn(start, rootEntry, nil); err != nil {
		return err
	}
	if !rootEntry.IsDir() {
		return nil
	}
	visited := 0
	var visit func(string) error
	visit = func(directory string) error {
		if err := w.contextErr(); err != nil {
			return err
		}
		handle, err := os.Open(directory)
		if err != nil {
			return err
		}
		defer handle.Close()
		for {
			if err := w.contextErr(); err != nil {
				return err
			}
			entries, readErr := handle.ReadDir(128)
			for _, entry := range entries {
				if err := w.contextErr(); err != nil {
					return err
				}
				visited++
				if visited > maxWalkEntries {
					return errWalkBounded
				}
				path := filepath.Join(directory, entry.Name())
				if entry.IsDir() && strings.EqualFold(entry.Name(), ".git") {
					continue
				}
				protected := false
				for _, protectedPath := range w.protected {
					if entry.IsDir() && withinFold(protectedPath, path) {
						protected = true
						break
					}
				}
				if protected || entry.Type()&os.ModeSymlink != 0 {
					continue
				}
				if err := fn(path, entry, nil); err != nil {
					return err
				}
				if entry.IsDir() {
					if err := visit(path); err != nil {
						return err
					}
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	return visit(start)
}

func (w *Workspace) listFiles(raw json.RawMessage) (string, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := decodeToolInput(raw, &in); err != nil {
		return "", err
	}
	p, err := w.resolve(in.Path, false)
	if err != nil {
		return "", err
	}
	if err := w.contextObservePath(p); err != nil {
		return "", err
	}
	var items []string
	truncated := false
	errListStop := errors.New("list bounded")
	err = w.walk(p, func(path string, d fs.DirEntry, _ error) error {
		if path == p {
			return nil
		}
		rel, _ := filepath.Rel(w.root, path)
		if d.IsDir() {
			rel += "/"
		}
		items = append(items, filepath.ToSlash(rel))
		if len(items) >= maxListEntries {
			truncated = true
			return errListStop
		}
		return nil
	})
	if errors.Is(err, errWalkBounded) {
		truncated = true
	} else if err != nil && !errors.Is(err, errListStop) {
		return "", err
	}
	if truncated {
		items = append(items, "[truncated]")
	}
	b, _ := json.Marshal(items)
	return capText(string(b)), nil
}

func (w *Workspace) searchFiles(raw json.RawMessage) (string, error) {
	var in struct {
		Query, Path, Glob string
		Regex             bool `json:"regex"`
		MaxResults        int  `json:"max_results"`
	}
	if err := decodeToolInput(raw, &in, "query"); err != nil {
		return "", err
	}
	if in.Query == "" {
		return "", fmt.Errorf("query must not be empty")
	}
	var pattern *regexp.Regexp
	if in.Regex {
		var err error
		pattern, err = regexp.Compile(in.Query)
		if err != nil {
			return "", fmt.Errorf("invalid search regular expression: %w", err)
		}
	}
	p, err := w.resolve(in.Path, false)
	if err != nil {
		return "", err
	}
	if err := w.contextObservePath(p); err != nil {
		return "", err
	}
	max := in.MaxResults
	if max == 0 {
		max = 100
	}
	if max < 1 {
		return "", fmt.Errorf("max_results must be positive")
	}
	if max > maxSearchHits {
		max = maxSearchHits
	}
	var out strings.Builder
	hits := 0
	filesScanned := 0
	bytesScanned := int64(0)
	query := []byte(in.Query)
	scanBuffer := make([]byte, 64<<10)
	bounded := false
	errStop := errors.New("bounded")
	err = w.walk(p, func(path string, d fs.DirEntry, _ error) error {
		if err := w.contextErr(); err != nil {
			return err
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, _ := filepath.Rel(w.root, path)
		rel = filepath.ToSlash(rel)
		if in.Glob != "" {
			ok, e := filepath.Match(in.Glob, rel)
			if e == nil && !ok && !strings.Contains(in.Glob, "/") {
				ok, e = filepath.Match(in.Glob, filepath.Base(rel))
			}
			if e != nil {
				return e
			}
			if !ok {
				return nil
			}
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if info.Size() > maxSearchBytes {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := w.contextObservePath(path); err != nil {
			return err
		}
		filesScanned++
		if filesScanned > maxSearchFiles {
			bounded = true
			return errStop
		}
		file, openedInfo, e := openCheckedRegularFile(path, info)
		if e != nil {
			return e
		}
		if openedInfo.Size() > maxSearchBytes {
			_ = file.Close()
			return nil
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(scanBuffer, maxLineBytes)
		lineNumber := 0
		stop := false
		binary := false
		for scanner.Scan() {
			if err := w.contextErr(); err != nil {
				_ = file.Close()
				return err
			}
			lineNumber++
			line := scanner.Bytes()
			bytesScanned += int64(len(line) + 1)
			if bytesScanned > maxSearchBytes*4 {
				bounded = true
				stop = true
				break
			}
			if bytes.IndexByte(line, 0) >= 0 {
				binary = true
				break
			}
			matches := false
			if pattern != nil {
				matches = pattern.Match(line)
			} else {
				matches = bytes.Contains(line, query)
			}
			if matches {
				if pattern != nil && len(line) > 16<<10 {
					fmt.Fprintf(&out, "%s:%d:%s\n", rel, lineNumber, truncateUTF8Text(string(line[:16<<10]), 16<<10, true))
				} else {
					fmt.Fprintf(&out, "%s:%d:%s\n", rel, lineNumber, line)
				}
				hits++
				if hits >= max || out.Len() >= maxToolOutput {
					bounded = true
					stop = true
					break
				}
			}
		}
		scanErr := scanner.Err()
		closeErr := file.Close()
		if scanErr != nil {
			return scanErr
		}
		if closeErr != nil {
			return closeErr
		}
		if binary {
			return nil
		}
		if stop {
			return errStop
		}
		return nil
	})
	if errors.Is(err, errWalkBounded) {
		bounded = true
	} else if err != nil && !errors.Is(err, errStop) {
		return "", err
	}
	if bounded {
		fmt.Fprintf(&out, "[truncated after %d results]\n", hits)
	}
	return capText(out.String()), nil
}

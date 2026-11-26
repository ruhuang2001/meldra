package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	agentPluginsSpecVersion    = "1.0.0"
	agentPluginsManifestSchema = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	agentPluginsMCPSchema      = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
	pluginsRegistryFile        = "plugins.json"
	pluginDataDirName          = "plugin-data"
	maxPluginJSONBytes         = 1 << 20
	maxRegisteredPlugins       = 64
	maxPluginDiscoveryEntries  = 256
	maxPluginMCPServers        = 16
	pluginServerIDPrefix       = "plugin/"
)

var pluginNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)

type pluginAuthor struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	URL   string `json:"url,omitempty"`
}

type pluginManifest struct {
	Schema      string        `json:"$schema"`
	Name        string        `json:"name"`
	Version     string        `json:"version,omitempty"`
	Description string        `json:"description,omitempty"`
	Author      *pluginAuthor `json:"author,omitempty"`
	Homepage    string        `json:"homepage,omitempty"`
	Repository  string        `json:"repository,omitempty"`
	License     string        `json:"license,omitempty"`
	Keywords    []string      `json:"keywords,omitempty"`
	Extensions  []string      `json:"extensions,omitempty"`
}

type pluginMCPServer struct {
	Name       string            `json:"name"`
	ServerID   string            `json:"server_id"`
	Type       string            `json:"type"`
	RawCommand string            `json:"raw_command,omitempty"`
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	CWD        string            `json:"cwd,omitempty"`
	URL        string            `json:"url,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	PluginRoot string            `json:"plugin_root,omitempty"`
	PluginData string            `json:"plugin_data,omitempty"`
	execDigest string
}

type loadedPlugin struct {
	Root           string            `json:"root"`
	DataDir        string            `json:"data_dir"`
	Manifest       pluginManifest    `json:"manifest"`
	Skills         []skill           `json:"skills"`
	MCPServers     []pluginMCPServer `json:"mcp_servers"`
	SkillsDisabled bool              `json:"skills_disabled,omitempty"`
	MCPDisabled    bool              `json:"mcp_disabled,omitempty"`
	Warnings       []string          `json:"warnings"`
}

type pluginRegistryEntry struct {
	Path        string `json:"path"`
	Enabled     bool   `json:"enabled"`
	MCPApproved bool   `json:"mcp_approved"`
	MCPDigest   string `json:"mcp_digest,omitempty"`
}

type pluginRegistry struct {
	Plugins map[string]pluginRegistryEntry `json:"plugins"`
}

func validatePluginName(name string) error {
	if len(name) < 1 || len(name) > 64 || !pluginNamePattern.MatchString(name) || strings.Contains(name, "--") || strings.Contains(name, "..") {
		return fmt.Errorf("plugin name must be 1..64 lowercase ASCII letters, digits, hyphens, or periods, start and end with an alphanumeric character, and contain no -- or ..")
	}
	return nil
}

func pluginMCPServerID(pluginName, serverName string) string {
	return pluginServerIDPrefix + pluginName + "/" + url.PathEscape(serverName)
}

func isValidPluginMCPServerID(id string) bool {
	rest, ok := strings.CutPrefix(id, pluginServerIDPrefix)
	if !ok {
		return false
	}
	pluginName, serverName, ok := strings.Cut(rest, "/")
	if !ok || validatePluginName(pluginName) != nil {
		return false
	}
	decoded, err := url.PathUnescape(serverName)
	return err == nil && pluginMCPServerID(pluginName, decoded) == id
}

func pluginDataPath(paths ConfigPaths, pluginName string) (string, error) {
	if err := validatePluginName(pluginName); err != nil {
		return "", err
	}
	home, err := canonicalSkillHome(paths.Home)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, pluginDataDirName, pluginName), nil
}

func ensurePluginDataDir(paths ConfigPaths, pluginName string) (string, error) {
	dir, err := pluginDataPath(paths, pluginName)
	if err != nil {
		return "", err
	}
	if err := ensureConfigHome(paths); err != nil {
		return "", err
	}
	for _, directory := range []string{filepath.Dir(dir), dir} {
		parent, err := openSkillDirectory(filepath.Dir(directory))
		if err != nil {
			return "", err
		}
		err = parent.Mkdir(filepath.Base(directory), privateDirPerm)
		parent.Close()
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		if err == nil {
			if err := syncDirectory(filepath.Dir(directory)); err != nil {
				return "", err
			}
		}
	}
	root, err := openSkillDirectory(dir)
	if err != nil {
		return "", fmt.Errorf("open plugin data directory: %w", err)
	}
	root.Close()
	return dir, nil
}

func resolvePluginRoot(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("plugin path must not be empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve plugin path: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve plugin directory %s: %w", abs, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("stat plugin directory %s: %w", canonical, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("plugin path %s must be a directory", canonical)
	}
	return canonical, nil
}

func resolveContainedPath(canonicalRoot, candidate string) (string, error) {
	if candidate == canonicalRoot {
		return canonicalRoot, nil
	}
	rel, ok := strings.CutPrefix(candidate, strings.TrimSuffix(canonicalRoot, string(filepath.Separator))+string(filepath.Separator))
	if !ok {
		return "", fmt.Errorf("path escapes boundary %s", canonicalRoot)
	}
	current := canonicalRoot
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if !within(canonicalRoot, current) {
			return "", fmt.Errorf("path escapes boundary %s", canonicalRoot)
		}
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			current, err = filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			if !within(canonicalRoot, current) {
				return "", fmt.Errorf("path escapes boundary %s", canonicalRoot)
			}
		}
	}
	return current, nil
}

func hasGitPathSegment(rel string) bool {
	for part := range strings.SplitSeq(filepath.ToSlash(rel), "/") {
		if strings.EqualFold(part, ".git") {
			return true
		}
	}
	return false
}

func openPluginRegularFile(canonicalRoot, resolved string, limit int64) (*os.File, error) {
	rel, err := filepath.Rel(canonicalRoot, resolved)
	if err != nil || !filepath.IsLocal(rel) || hasGitPathSegment(rel) {
		return nil, fmt.Errorf("path escapes plugin root or references .git")
	}
	root, err := openSkillDirectory(filepath.Dir(resolved))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	name := filepath.Base(resolved)
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("expected regular file")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("file exceeds %d byte limit", limit)
	}
	file, err := root.OpenFile(name, skillReadFlags, 0)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		file.Close()
		return nil, fmt.Errorf("file changed while opening")
	}
	if err := checkSkillLinkCount(opened); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func readPluginRegularFile(canonicalRoot, candidate string, limit int64, requireUTF8 bool, checkPath func(string) error) ([]byte, error) {
	resolved, err := resolveContainedPath(canonicalRoot, candidate)
	if err != nil {
		return nil, err
	}
	if checkPath != nil {
		if err := checkPath(resolved); err != nil {
			return nil, err
		}
	}
	file, err := openPluginRegularFile(canonicalRoot, resolved, limit)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, fmt.Errorf("file exceeds %d byte limit", limit)
	}
	if requireUTF8 && (!utf8.Valid(contents) || bytes.IndexByte(contents, 0) >= 0) {
		return nil, fmt.Errorf("skill files must contain UTF-8 text, not binary data")
	}
	return contents, nil
}

func readPluginSkillFile(canonicalRoot, skillDir, relativePath string, checkPath func(string) error) ([]byte, error) {
	if !filepath.IsLocal(relativePath) || strings.Contains(relativePath, "\\") {
		return nil, fmt.Errorf("skill path must be relative and stay inside its directory")
	}
	for part := range strings.SplitSeq(filepath.ToSlash(relativePath), "/") {
		if part == ".." || strings.EqualFold(part, ".git") {
			return nil, fmt.Errorf("skill path traversal and .git access are forbidden")
		}
	}
	candidate := filepath.Join(skillDir, relativePath)
	return readPluginRegularFile(canonicalRoot, candidate, maxSkillFileBytes, true, checkPath)
}

type jsonObjectField struct {
	key string
	raw json.RawMessage
}

func decodeStrictJSONObject(data []byte, rejectDuplicateKeys bool) ([]jsonObjectField, map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, nil, fmt.Errorf("invalid UTF-8 in JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	tok, err := decoder.Token()
	if err != nil {
		return nil, nil, fmt.Errorf("invalid JSON: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil, nil, fmt.Errorf("expected JSON object")
	}
	fields := []jsonObjectField{}
	values := map[string]json.RawMessage{}
	for decoder.More() {
		keyTok, err := decoder.Token()
		if err != nil {
			return nil, nil, fmt.Errorf("invalid JSON object key: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, nil, fmt.Errorf("expected string key")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, nil, fmt.Errorf("invalid JSON value for %q: %w", key, err)
		}
		if rejectDuplicateKeys {
			if _, exists := values[key]; exists {
				return nil, nil, fmt.Errorf("duplicate key %q", key)
			}
		}
		fields = append(fields, jsonObjectField{key: key, raw: raw})
		values[key] = raw
	}
	endTok, err := decoder.Token()
	if err != nil {
		return nil, nil, fmt.Errorf("invalid JSON object closing: %w", err)
	}
	if endDelim, ok := endTok.(json.Delim); !ok || endDelim != '}' {
		return nil, nil, fmt.Errorf("expected end of JSON object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, nil, fmt.Errorf("JSON document must contain a single top-level object")
	}
	return fields, values, nil
}

func decodeStrictJSONString(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", fmt.Errorf("must be a string")
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", fmt.Errorf("must be a string")
	}
	return value, nil
}

func decodeStrictJSONStringSlice(raw json.RawMessage) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, fmt.Errorf("must be an array of strings")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, fmt.Errorf("must be an array of strings")
	}
	result := make([]string, len(items))
	for i, item := range items {
		value, err := decodeStrictJSONString(item)
		if err != nil {
			return nil, fmt.Errorf("element %d %w", i, err)
		}
		result[i] = value
	}
	return result, nil
}

func decodeStrictJSONStringMap(raw json.RawMessage, rejectDuplicateKeys bool) ([]jsonObjectField, map[string]string, error) {
	fields, _, err := decodeStrictJSONObject(raw, rejectDuplicateKeys)
	if err != nil {
		return nil, nil, fmt.Errorf("must be an object of strings: %w", err)
	}
	result := make(map[string]string, len(fields))
	for _, field := range fields {
		value, err := decodeStrictJSONString(field.raw)
		if err != nil {
			return nil, nil, fmt.Errorf("field %q %w", field.key, err)
		}
		result[field.key] = value
	}
	return fields, result, nil
}

func loadPluginManifest(canonicalRoot string) (pluginManifest, []string, error) {
	manifestPath := filepath.Join(canonicalRoot, "plugin.json")
	data, err := readPluginRegularFile(canonicalRoot, manifestPath, maxPluginJSONBytes, false, nil)
	if err != nil {
		return pluginManifest{}, nil, fmt.Errorf("plugin.json: %w", err)
	}
	_, values, err := decodeStrictJSONObject(data, false)
	if err != nil {
		return pluginManifest{}, nil, fmt.Errorf("plugin.json: %w", err)
	}
	schemaRaw, hasSchema := values["$schema"]
	if !hasSchema {
		return pluginManifest{}, nil, fmt.Errorf("plugin.json: missing required field \"$schema\"")
	}
	schema, err := decodeStrictJSONString(schemaRaw)
	if err != nil || schema == "" {
		return pluginManifest{}, nil, fmt.Errorf("plugin.json: invalid required field \"$schema\"")
	}
	if schema != agentPluginsManifestSchema {
		return pluginManifest{}, nil, fmt.Errorf("plugin.json: unsupported \"$schema\" %q (supported: %s)", schema, agentPluginsManifestSchema)
	}
	nameRaw, hasName := values["name"]
	if !hasName {
		return pluginManifest{}, nil, fmt.Errorf("plugin.json: missing required field \"name\"")
	}
	name, err := decodeStrictJSONString(nameRaw)
	if err != nil {
		return pluginManifest{}, nil, fmt.Errorf("plugin.json: invalid required field \"name\": %w", err)
	}
	if err := validatePluginName(name); err != nil {
		return pluginManifest{}, nil, fmt.Errorf("plugin.json: %w", err)
	}
	manifest := pluginManifest{
		Schema: schema,
		Name:   name,
	}
	var warnings []string
	for _, key := range slices.Sorted(maps.Keys(values)) {
		raw := values[key]
		switch key {
		case "$schema", "name":
		case "version":
			v, err := decodeStrictJSONString(raw)
			if err != nil {
				return pluginManifest{}, nil, fmt.Errorf("plugin.json: field \"version\" %w", err)
			}
			manifest.Version = v
		case "description":
			v, err := decodeStrictJSONString(raw)
			if err != nil {
				return pluginManifest{}, nil, fmt.Errorf("plugin.json: field \"description\" %w", err)
			}
			manifest.Description = v
		case "homepage":
			v, err := decodeStrictJSONString(raw)
			if err != nil {
				return pluginManifest{}, nil, fmt.Errorf("plugin.json: field \"homepage\" %w", err)
			}
			manifest.Homepage = v
		case "repository":
			v, err := decodeStrictJSONString(raw)
			if err != nil {
				return pluginManifest{}, nil, fmt.Errorf("plugin.json: field \"repository\" %w", err)
			}
			manifest.Repository = v
		case "license":
			v, err := decodeStrictJSONString(raw)
			if err != nil {
				return pluginManifest{}, nil, fmt.Errorf("plugin.json: field \"license\" %w", err)
			}
			manifest.License = v
		case "keywords":
			v, err := decodeStrictJSONStringSlice(raw)
			if err != nil {
				return pluginManifest{}, nil, fmt.Errorf("plugin.json: field \"keywords\" %w", err)
			}
			manifest.Keywords = v
		case "author":
			author, err := parsePluginAuthor(raw)
			if err != nil {
				return pluginManifest{}, nil, fmt.Errorf("plugin.json: field \"author\": %w", err)
			}
			manifest.Author = author
		case "extensions":
			_, extMap, err := decodeStrictJSONObject(raw, false)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("%s: ignoring non-object \"extensions\" field", manifestPath))
				continue
			}
			manifest.Extensions = slices.Sorted(maps.Keys(extMap))
		default:
			warnings = append(warnings, fmt.Sprintf("%s: ignoring unknown top-level field %q", manifestPath, key))
		}
	}
	return manifest, warnings, nil
}

func parsePluginAuthor(raw json.RawMessage) (*pluginAuthor, error) {
	_, fields, err := decodeStrictJSONObject(raw, false)
	if err != nil {
		return nil, err
	}
	author := &pluginAuthor{}
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		v, err := decodeStrictJSONString(fields[key])
		if err != nil {
			return nil, fmt.Errorf("field %q %w", key, err)
		}
		switch key {
		case "name":
			author.Name = v
		case "email":
			author.Email = v
		case "url":
			author.URL = v
		default:
			return nil, fmt.Errorf("unknown field %q", key)
		}
	}
	return author, nil
}

func expandPluginPlaceholders(value, pluginRoot, pluginData string) string {
	return strings.NewReplacer("${PLUGIN_ROOT}", pluginRoot, "${PLUGIN_DATA}", pluginData).Replace(value)
}

func isReservedPluginEnvKey(key string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(key, "PLUGIN_ROOT") || strings.EqualFold(key, "PLUGIN_DATA")
	}
	return key == "PLUGIN_ROOT" || key == "PLUGIN_DATA"
}

func validatePluginCommand(canonicalRoot, command string) (string, error) {
	if command == "" || strings.ContainsAny(command, "\x00\r\n") {
		return "", fmt.Errorf("command must be a nonempty single executable token")
	}
	if strings.HasPrefix(command, "./") {
		if strings.Contains(command, "\\") {
			return "", fmt.Errorf("plugin-relative command must use forward slashes")
		}
		candidate := canonicalRoot + string(filepath.Separator) + filepath.FromSlash(command)
		resolved, err := resolveContainedPath(canonicalRoot, candidate)
		if err != nil {
			return "", fmt.Errorf("plugin-relative command escapes plugin root: %w", err)
		}
		rel, err := filepath.Rel(canonicalRoot, resolved)
		if err != nil || rel == "." || !filepath.IsLocal(rel) || hasGitPathSegment(rel) {
			return "", fmt.Errorf("plugin-relative command escapes plugin root")
		}
		return resolved, nil
	}
	if filepath.IsAbs(command) || strings.ContainsAny(command, "/\\") || command == "." || command == ".." {
		return "", fmt.Errorf("command must be a bare executable name or a plugin-relative path starting with ./")
	}
	return command, nil
}

func validatePluginCWD(canonicalRoot, canonicalData, rawCWD string) (string, error) {
	if rawCWD == "" {
		return canonicalRoot, nil
	}
	if strings.ContainsAny(rawCWD, "\x00\r\n\\") {
		return "", fmt.Errorf("cwd contains invalid characters")
	}
	switch {
	case strings.HasPrefix(rawCWD, "./"):
		expanded := expandPluginPlaceholders(rawCWD, canonicalRoot, canonicalData)
		candidate := canonicalRoot + string(filepath.Separator) + filepath.FromSlash(expanded)
		resolved, err := resolveContainedPath(canonicalRoot, candidate)
		if err != nil {
			return "", fmt.Errorf("cwd escapes plugin root: %w", err)
		}
		return resolved, nil
	case rawCWD == "${PLUGIN_ROOT}" || strings.HasPrefix(rawCWD, "${PLUGIN_ROOT}/"):
		expanded := expandPluginPlaceholders(rawCWD, canonicalRoot, canonicalData)
		resolved, err := resolveContainedPath(canonicalRoot, filepath.FromSlash(expanded))
		if err != nil {
			return "", fmt.Errorf("cwd escapes plugin root: %w", err)
		}
		return resolved, nil
	case rawCWD == "${PLUGIN_DATA}" || strings.HasPrefix(rawCWD, "${PLUGIN_DATA}/"):
		expanded := expandPluginPlaceholders(rawCWD, canonicalRoot, canonicalData)
		resolved, err := resolveContainedPath(canonicalData, filepath.FromSlash(expanded))
		if err != nil {
			return "", fmt.Errorf("cwd escapes PLUGIN_DATA: %w", err)
		}
		return resolved, nil
	default:
		return "", fmt.Errorf("cwd must start with ./, ${PLUGIN_ROOT}, or ${PLUGIN_DATA}")
	}
}

func isLoopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validatePluginMCPURL(raw string) error {
	if raw == "" || strings.ContainsAny(raw, " \t\r\n\x00#") {
		return fmt.Errorf("url must be an absolute HTTP(S) URL without fragments or whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("url must be an absolute HTTP(S) URL without userinfo or fragment")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && scheme != "http" {
		return fmt.Errorf("url must use http or https")
	}
	if scheme == "http" && !isLoopbackHostname(parsed.Hostname()) {
		return fmt.Errorf("non-loopback MCP endpoints must use https")
	}
	return nil
}

func isValidHTTPHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func isValidHTTPHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '\t' || (c >= 0x20 && c != 0x7f) {
			continue
		}
		return false
	}
	return true
}

func validatePluginMCPHeaders(raw json.RawMessage) (map[string]string, error) {
	fields, _, err := decodeStrictJSONStringMap(raw, true)
	if err != nil {
		return nil, err
	}
	headers := make(map[string]string, len(fields))
	seenLower := make(map[string]string, len(fields))
	for _, field := range fields {
		value, err := decodeStrictJSONString(field.raw)
		if err != nil {
			return nil, fmt.Errorf("header %q %w", field.key, err)
		}
		if !isValidHTTPHeaderName(field.key) {
			return nil, fmt.Errorf("invalid HTTP header name %q", field.key)
		}
		if !isValidHTTPHeaderValue(value) {
			return nil, fmt.Errorf("invalid HTTP header value for %q", field.key)
		}
		lower := strings.ToLower(field.key)
		if prev, exists := seenLower[lower]; exists {
			return nil, fmt.Errorf("duplicate HTTP header %q and %q", prev, field.key)
		}
		seenLower[lower] = field.key
		headers[field.key] = value
	}
	return headers, nil
}

func parsePluginMCPServer(canonicalRoot, canonicalData, pluginName, serverName string, raw json.RawMessage) (pluginMCPServer, error) {
	_, fields, err := decodeStrictJSONObject(raw, false)
	if err != nil {
		return pluginMCPServer{}, err
	}
	typeRaw, hasType := fields["type"]
	if !hasType {
		return pluginMCPServer{}, fmt.Errorf("missing required field \"type\"")
	}
	transportType, err := decodeStrictJSONString(typeRaw)
	if err != nil {
		return pluginMCPServer{}, fmt.Errorf("field \"type\" %w", err)
	}
	server := pluginMCPServer{
		Name:       serverName,
		ServerID:   pluginMCPServerID(pluginName, serverName),
		Type:       transportType,
		PluginRoot: canonicalRoot,
		PluginData: canonicalData,
	}
	switch transportType {
	case "stdio":
		for _, key := range slices.Sorted(maps.Keys(fields)) {
			switch key {
			case "type", "command", "args", "env", "cwd":
			default:
				return pluginMCPServer{}, fmt.Errorf("unknown stdio field %q", key)
			}
		}
		cmdRaw, hasCmd := fields["command"]
		if !hasCmd {
			return pluginMCPServer{}, fmt.Errorf("missing required field \"command\"")
		}
		rawCmd, err := decodeStrictJSONString(cmdRaw)
		if err != nil {
			return pluginMCPServer{}, fmt.Errorf("field \"command\" %w", err)
		}
		resolvedCmd, err := validatePluginCommand(canonicalRoot, rawCmd)
		if err != nil {
			return pluginMCPServer{}, err
		}
		server.RawCommand = rawCmd
		server.Command = resolvedCmd
		if argsRaw, ok := fields["args"]; ok {
			rawArgs, err := decodeStrictJSONStringSlice(argsRaw)
			if err != nil {
				return pluginMCPServer{}, fmt.Errorf("field \"args\" %w", err)
			}
			expanded := make([]string, len(rawArgs))
			for i, arg := range rawArgs {
				if strings.ContainsRune(arg, 0) {
					return pluginMCPServer{}, fmt.Errorf("args[%d] contains NUL byte", i)
				}
				expanded[i] = expandPluginPlaceholders(arg, canonicalRoot, canonicalData)
			}
			server.Args = expanded
		}
		if envRaw, ok := fields["env"]; ok {
			_, rawEnv, err := decodeStrictJSONStringMap(envRaw, false)
			if err != nil {
				return pluginMCPServer{}, fmt.Errorf("field \"env\" %w", err)
			}
			expanded := make(map[string]string, len(rawEnv))
			for key, val := range rawEnv {
				if key == "" || strings.ContainsAny(key, "=\x00") {
					return pluginMCPServer{}, fmt.Errorf("invalid env key %q", key)
				}
				if isReservedPluginEnvKey(key) {
					return pluginMCPServer{}, fmt.Errorf("env must not set reserved variable %q", key)
				}
				if strings.ContainsRune(val, 0) {
					return pluginMCPServer{}, fmt.Errorf("env %q contains NUL byte", key)
				}
				expanded[key] = expandPluginPlaceholders(val, canonicalRoot, canonicalData)
			}
			server.Env = expanded
		}
		rawCWD := ""
		if cwdRaw, ok := fields["cwd"]; ok {
			v, err := decodeStrictJSONString(cwdRaw)
			if err != nil {
				return pluginMCPServer{}, fmt.Errorf("field \"cwd\" %w", err)
			}
			if v == "" {
				return pluginMCPServer{}, fmt.Errorf("cwd must not be empty")
			}
			rawCWD = v
		}
		cwd, err := validatePluginCWD(canonicalRoot, canonicalData, rawCWD)
		if err != nil {
			return pluginMCPServer{}, err
		}
		server.CWD = cwd
		return server, nil
	case "streamable-http", "sse":
		for _, key := range slices.Sorted(maps.Keys(fields)) {
			switch key {
			case "type", "url", "headers":
			default:
				return pluginMCPServer{}, fmt.Errorf("unknown %s field %q", transportType, key)
			}
		}
		urlRaw, hasURL := fields["url"]
		if !hasURL {
			return pluginMCPServer{}, fmt.Errorf("missing required field \"url\"")
		}
		endpoint, err := decodeStrictJSONString(urlRaw)
		if err != nil {
			return pluginMCPServer{}, fmt.Errorf("field \"url\" %w", err)
		}
		if err := validatePluginMCPURL(endpoint); err != nil {
			return pluginMCPServer{}, err
		}
		server.URL = endpoint
		if headersRaw, ok := fields["headers"]; ok {
			headers, err := validatePluginMCPHeaders(headersRaw)
			if err != nil {
				return pluginMCPServer{}, fmt.Errorf("field \"headers\": %w", err)
			}
			server.Headers = headers
		}
		return server, nil
	default:
		return pluginMCPServer{}, fmt.Errorf("unsupported transport type %q", transportType)
	}
}

func discoverPluginMCP(canonicalRoot, canonicalData, pluginName string) ([]pluginMCPServer, bool, []string) {
	mcpPath := filepath.Join(canonicalRoot, "mcp.json")
	if _, err := os.Lstat(mcpPath); errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	data, err := readPluginRegularFile(canonicalRoot, mcpPath, maxPluginJSONBytes, false, nil)
	if err != nil {
		return nil, true, []string{fmt.Sprintf("%s: MCP disabled: %s", mcpPath, err)}
	}
	_, top, err := decodeStrictJSONObject(data, false)
	if err != nil {
		return nil, true, []string{fmt.Sprintf("%s: MCP disabled: %s", mcpPath, err)}
	}
	for _, key := range slices.Sorted(maps.Keys(top)) {
		if key != "$schema" && key != "mcpServers" {
			return nil, true, []string{fmt.Sprintf("%s: MCP disabled: unknown top-level field %q", mcpPath, key)}
		}
	}
	schemaRaw, hasSchema := top["$schema"]
	if !hasSchema {
		return nil, true, []string{fmt.Sprintf("%s: MCP disabled: missing required field \"$schema\"", mcpPath)}
	}
	schema, err := decodeStrictJSONString(schemaRaw)
	if err != nil || schema != agentPluginsMCPSchema {
		return nil, true, []string{fmt.Sprintf("%s: MCP disabled: unsupported or mismatched \"$schema\" (expected %s)", mcpPath, agentPluginsMCPSchema)}
	}
	serversRaw, hasServers := top["mcpServers"]
	if !hasServers {
		return nil, true, []string{fmt.Sprintf("%s: MCP disabled: missing required field \"mcpServers\"", mcpPath)}
	}
	_, rawServers, err := decodeStrictJSONObject(serversRaw, false)
	if err != nil {
		return nil, true, []string{fmt.Sprintf("%s: MCP disabled: field \"mcpServers\" %s", mcpPath, err)}
	}
	var servers []pluginMCPServer
	var warnings []string
	for _, name := range slices.Sorted(maps.Keys(rawServers)) {
		if len(servers) >= maxPluginMCPServers {
			warnings = append(warnings, fmt.Sprintf("%s: server %q skipped: plugin exceeds limit of %d MCP servers", mcpPath, name, maxPluginMCPServers))
			continue
		}
		server, err := parsePluginMCPServer(canonicalRoot, canonicalData, pluginName, name, rawServers[name])
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: server %q skipped: %s", mcpPath, name, err))
			continue
		}
		servers = append(servers, server)
	}
	return servers, false, warnings
}

func discoverPluginSkills(ctx context.Context, canonicalRoot, pluginName string, checkPath func(string) error, remaining *int) ([]skill, bool, []string, error) {
	skillsPath := filepath.Join(canonicalRoot, "skills")
	if _, err := os.Lstat(skillsPath); errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil, nil
	}
	resolvedSkillsDir, err := resolveContainedPath(canonicalRoot, skillsPath)
	if err != nil {
		return nil, true, []string{fmt.Sprintf("%s: skills disabled: %s", skillsPath, err)}, nil
	}
	info, err := os.Stat(resolvedSkillsDir)
	if err != nil || !info.IsDir() {
		return nil, true, []string{fmt.Sprintf("%s: skills disabled: skills location must resolve to a directory", skillsPath)}, nil
	}
	if checkPath != nil {
		if err := checkPath(resolvedSkillsDir); err != nil {
			return nil, true, []string{fmt.Sprintf("%s: skills disabled: %s", skillsPath, err)}, nil
		}
	}
	root, err := openSkillDirectory(resolvedSkillsDir)
	if err != nil {
		return nil, true, []string{fmt.Sprintf("%s: skills disabled: %s", skillsPath, err)}, nil
	}
	defer root.Close()
	file, err := root.Open(".")
	if err != nil {
		return nil, true, []string{fmt.Sprintf("%s: skills disabled: %s", skillsPath, err)}, nil
	}
	children, readErr := file.ReadDir(*remaining + 1)
	file.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, true, []string{fmt.Sprintf("%s: skills disabled: %s", skillsPath, readErr)}, nil
	}
	var warnings []string
	if len(children) > *remaining {
		warnings = append(warnings, fmt.Sprintf("%s: discovery limit of %d entries exceeded; extra entries skipped", skillsPath, maxSkillEntries))
		children = children[:*remaining]
	}
	*remaining -= len(children)
	slices.SortFunc(children, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	var skills []skill
	for _, child := range children {
		if err := ctx.Err(); err != nil {
			return nil, false, nil, err
		}
		childPath := filepath.Join(resolvedSkillsDir, child.Name())
		resolvedChild, err := resolveContainedPath(canonicalRoot, childPath)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %s", filepath.Join(skillsPath, child.Name(), "SKILL.md"), err))
			continue
		}
		childInfo, err := os.Stat(resolvedChild)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				warnings = append(warnings, fmt.Sprintf("%s: %s", filepath.Join(skillsPath, child.Name(), "SKILL.md"), err))
			}
			continue
		}
		if !childInfo.IsDir() {
			continue
		}
		skillFileCandidate := filepath.Join(resolvedChild, "SKILL.md")
		if _, err := os.Lstat(skillFileCandidate); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		logicalPath := filepath.Join(skillsPath, child.Name(), "SKILL.md")
		contents, err := readPluginRegularFile(canonicalRoot, skillFileCandidate, maxSkillFileBytes, true, checkPath)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %s", logicalPath, err))
			continue
		}
		item, err := parseSkill(contents, logicalPath, true)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %s", logicalPath, err))
			continue
		}
		item.Plugin = pluginName
		item.pluginRoot = canonicalRoot
		skills = append(skills, item)
	}
	return skills, false, warnings, nil
}

func loadPluginFromDir(ctx context.Context, paths ConfigPaths, dir string, checkPath func(string) error) (*loadedPlugin, error) {
	canonicalRoot, err := resolvePluginRoot(dir)
	if err != nil {
		return nil, err
	}
	return loadPluginAtRoot(ctx, paths, canonicalRoot, checkPath)
}

func loadPluginAtRoot(ctx context.Context, paths ConfigPaths, canonicalRoot string, checkPath func(string) error) (*loadedPlugin, error) {
	if checkPath != nil {
		if err := checkPath(canonicalRoot); err != nil {
			return nil, err
		}
	}
	manifest, manifestWarnings, err := loadPluginManifest(canonicalRoot)
	if err != nil {
		return nil, err
	}
	dataDir, err := pluginDataPath(paths, manifest.Name)
	if err != nil {
		return nil, err
	}
	skills, skillsDisabled, skillWarnings, err := discoverPluginSkills(ctx, canonicalRoot, manifest.Name, checkPath, new(maxSkillEntries))
	if err != nil {
		return nil, err
	}
	mcpServers, mcpDisabled, mcpWarnings := discoverPluginMCP(canonicalRoot, dataDir, manifest.Name)
	warnings := make([]string, 0, len(manifestWarnings)+len(skillWarnings)+len(mcpWarnings))
	warnings = append(warnings, manifestWarnings...)
	warnings = append(warnings, skillWarnings...)
	warnings = append(warnings, mcpWarnings...)
	if skills == nil {
		skills = []skill{}
	}
	if mcpServers == nil {
		mcpServers = []pluginMCPServer{}
	}
	if warnings == nil {
		warnings = []string{}
	}
	return &loadedPlugin{
		Root:           canonicalRoot,
		DataDir:        dataDir,
		Manifest:       manifest,
		Skills:         skills,
		MCPServers:     mcpServers,
		SkillsDisabled: skillsDisabled,
		MCPDisabled:    mcpDisabled,
		Warnings:       warnings,
	}, nil
}

func pluginExecutableDigest(ctx context.Context, canonicalRoot, path string) (string, error) {
	file, err := openPluginRegularFile(canonicalRoot, path, 1<<30)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return hashMCPExecutable(ctx, file)
}

func reviewPluginMCP(ctx context.Context, loaded *loadedPlugin) (string, error) {
	type serverDigestEntry struct {
		Server     pluginMCPServer `json:"server"`
		ExecDigest string          `json:"exec_digest,omitempty"`
	}
	entries := make([]serverDigestEntry, 0, len(loaded.MCPServers))
	accepted := loaded.MCPServers[:0]
	for _, server := range loaded.MCPServers {
		if server.Type == "stdio" && strings.HasPrefix(server.RawCommand, "./") {
			var err error
			server.execDigest, err = pluginExecutableDigest(ctx, loaded.Root, server.Command)
			if err != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				loaded.Warnings = append(loaded.Warnings, fmt.Sprintf("server %q skipped: cannot inspect bundled executable: %s", server.Name, err))
				continue
			}
		}
		accepted = append(accepted, server)
		entries = append(entries, serverDigestEntry{server, server.execDigest})
	}
	loaded.MCPServers = accepted
	if len(entries) == 0 {
		return "", fmt.Errorf("plugin %q has no approvable MCP servers: %s", loaded.Manifest.Name, strings.Join(loaded.Warnings, "; "))
	}
	raw, err := json.Marshal(struct {
		Root    string              `json:"root"`
		Name    string              `json:"name"`
		Servers []serverDigestEntry `json:"servers"`
	}{loaded.Root, loaded.Manifest.Name, entries})
	if err != nil {
		return "", err
	}
	return digest(raw), nil
}

func pluginServerToMCPConfig(pluginName string, server pluginMCPServer) mcpServerConfig {
	cfg := mcpServerConfig{
		transport:        server.Type,
		pluginName:       pluginName,
		pluginRoot:       server.PluginRoot,
		pluginData:       server.PluginData,
		pluginExecDigest: server.execDigest,
		fromPlugin:       true,
	}
	if server.Type == "stdio" {
		cfg.Command = server.Command
		cfg.Args = append([]string(nil), server.Args...)
		cfg.Env = maps.Clone(server.Env)
		cfg.cwd = server.CWD
	} else {
		cfg.URL = server.URL
		cfg.headers = maps.Clone(server.Headers)
	}
	return cfg
}

func normalizeEnvKey(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}

func buildPluginSubprocessEnv(configured map[string]string, pluginRoot, pluginData string) []string {
	type envEntry struct {
		key   string
		value string
	}
	entries := map[string]envEntry{}
	set := func(key, value string) {
		entries[normalizeEnvKey(key)] = envEntry{key: key, value: value}
	}
	for _, key := range []string{"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "TEMP", "TMP", "SystemRoot", "SYSTEMROOT"} {
		if value, ok := os.LookupEnv(key); ok {
			set(key, value)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(configured)) {
		set(key, configured[key])
	}
	set("PLUGIN_ROOT", pluginRoot)
	set("PLUGIN_DATA", pluginData)
	keys := slices.Sorted(maps.Keys(entries))
	env := make([]string, 0, len(keys))
	for _, norm := range keys {
		entry := entries[norm]
		env = append(env, entry.key+"="+entry.value)
	}
	return env
}

func localPluginCatalog(ctx context.Context, paths ConfigPaths) (map[string][]pluginRegistryEntry, []string, error) {
	reg, err := loadPluginRegistry(paths)
	if err != nil {
		return nil, nil, err
	}
	catalog := make(map[string][]pluginRegistryEntry, len(reg.Plugins))
	for name, entry := range reg.Plugins {
		catalog[name] = []pluginRegistryEntry{entry}
	}
	warnings := []string{}
	home, err := canonicalSkillHome(paths.Home)
	if err != nil {
		return nil, nil, err
	}
	dir := filepath.Join(home, "plugins")
	root, err := openSkillDirectory(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return catalog, warnings, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("scan plugin directory %s: %w", dir, err)
	}
	defer root.Close()
	f, err := root.Open(".")
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	children, err := f.ReadDir(maxPluginDiscoveryEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, err
	}
	if len(children) > maxPluginDiscoveryEntries {
		return nil, nil, fmt.Errorf("%s exceeds discovery limit of %d entries; use an explicit plugin path", dir, maxPluginDiscoveryEntries)
	}
	slices.SortFunc(children, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, child := range children {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if !child.IsDir() || strings.EqualFold(child.Name(), ".git") {
			continue
		}
		path := filepath.Join(dir, child.Name())
		manifest, _, err := loadPluginManifest(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				warnings = append(warnings, fmt.Sprintf("%s: %s", path, err))
			}
			continue
		}
		if reg.Plugins[manifest.Name].Path != path {
			catalog[manifest.Name] = append(catalog[manifest.Name], pluginRegistryEntry{Path: path})
		}
	}
	return catalog, warnings, nil
}

func loadPluginArgument(ctx context.Context, paths ConfigPaths, arg string, checkPath func(string) error) (*loadedPlugin, error) {
	if validatePluginName(arg) != nil {
		return loadPluginFromDir(ctx, paths, arg, checkPath)
	}
	catalog, _, err := localPluginCatalog(ctx, paths)
	if err != nil {
		return nil, err
	}
	entries := catalog[arg]
	switch len(entries) {
	case 0:
		return nil, fmt.Errorf("plugin %q not found; run 'meldra plugins list' or specify a directory such as ./%s", arg, arg)
	case 1:
		path := entries[0].Path
		loaded, err := loadPluginAtRoot(ctx, paths, path, checkPath)
		if err != nil {
			return nil, err
		}
		if loaded.Manifest.Name != arg {
			return nil, fmt.Errorf("plugin %q no longer matches %s; run 'meldra plugins list'", arg, path)
		}
		return loaded, nil
	default:
		return nil, fmt.Errorf("multiple packages named %q; run 'meldra plugins list' and specify an explicit path", arg)
	}
}

func loadPluginRegistry(paths ConfigPaths) (pluginRegistry, error) {
	if _, err := verifyConfigHome(paths); err != nil {
		return pluginRegistry{}, err
	}
	path := filepath.Join(paths.Home, pluginsRegistryFile)
	if info, err := os.Lstat(path); err == nil && info.Size() > maxPluginJSONBytes {
		return pluginRegistry{}, fmt.Errorf("plugins configuration exceeds 1 MiB")
	}
	data, found, err := readPrivateFile(path)
	if err != nil {
		return pluginRegistry{}, err
	}
	if !found {
		return pluginRegistry{Plugins: map[string]pluginRegistryEntry{}}, nil
	}
	var reg pluginRegistry
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&reg); err != nil {
		return pluginRegistry{}, fmt.Errorf("invalid plugins.json configuration")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return pluginRegistry{}, fmt.Errorf("plugins.json must contain one JSON object")
	}
	if reg.Plugins == nil {
		reg.Plugins = map[string]pluginRegistryEntry{}
	}
	if len(reg.Plugins) > maxRegisteredPlugins {
		return pluginRegistry{}, fmt.Errorf("plugins.json exceeds %d plugins", maxRegisteredPlugins)
	}
	for name, entry := range reg.Plugins {
		if err := validatePluginName(name); err != nil {
			return pluginRegistry{}, fmt.Errorf("plugins.json: invalid plugin name %q", name)
		}
		if entry.Path == "" || !filepath.IsAbs(entry.Path) {
			return pluginRegistry{}, fmt.Errorf("plugins.json: plugin %q requires an absolute path", name)
		}
	}
	return reg, nil
}

func savePluginRegistry(paths ConfigPaths, reg pluginRegistry) error {
	if err := ensureConfigHome(paths); err != nil {
		return err
	}
	if reg.Plugins == nil {
		reg.Plugins = map[string]pluginRegistryEntry{}
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maxPluginJSONBytes {
		return fmt.Errorf("plugins configuration exceeds 1 MiB")
	}
	path := filepath.Join(paths.Home, pluginsRegistryFile)
	if err := atomicWriteFile(path, data, privateFilePerm); err != nil {
		return fmt.Errorf("save plugins configuration: %w", err)
	}
	return syncDirectory(paths.Home)
}

func lockPluginRegistry(ctx context.Context, paths ConfigPaths) (pluginRegistry, func(), error) {
	if _, err := verifyConfigHome(paths); err != nil {
		return pluginRegistry{}, nil, err
	}
	if err := ensureConfigHome(paths); err != nil {
		return pluginRegistry{}, nil, err
	}
	lock, err := acquirePrivateLock(ctx, filepath.Join(paths.Home, pluginsRegistryFile+".lock"))
	if err != nil {
		return pluginRegistry{}, nil, err
	}
	reg, err := loadPluginRegistry(paths)
	if err != nil {
		lock.Close()
		return pluginRegistry{}, nil, err
	}
	return reg, func() { lock.Close() }, nil
}

func loadPluginSkillsOnly(ctx context.Context, canonicalRoot, registeredName string, checkPath func(string) error, remaining *int) ([]skill, []string, error) {
	if checkPath != nil {
		if err := checkPath(canonicalRoot); err != nil {
			return nil, nil, err
		}
	}
	manifest, manifestWarnings, err := loadPluginManifest(canonicalRoot)
	if err != nil {
		return nil, nil, err
	}
	if registeredName != "" && manifest.Name != registeredName {
		return nil, nil, fmt.Errorf("plugin manifest name %q differs from registered name %q", manifest.Name, registeredName)
	}
	skills, _, skillWarnings, err := discoverPluginSkills(ctx, canonicalRoot, manifest.Name, checkPath, remaining)
	if err != nil {
		return nil, nil, err
	}
	warnings := make([]string, 0, len(manifestWarnings)+len(skillWarnings))
	warnings = append(warnings, manifestWarnings...)
	warnings = append(warnings, skillWarnings...)
	return skills, warnings, nil
}

func loadApprovedPluginMCPConfigs(ctx context.Context, paths ConfigPaths) (map[string]mcpServerConfig, []string, error) {
	reg, err := loadPluginRegistry(paths)
	if err != nil {
		return nil, nil, err
	}
	servers := map[string]mcpServerConfig{}
	var warnings []string
	for _, name := range slices.Sorted(maps.Keys(reg.Plugins)) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		entry := reg.Plugins[name]
		if !entry.Enabled || !entry.MCPApproved {
			continue
		}
		canonicalRoot := entry.Path
		if err := checkPluginRootAllowed(paths, canonicalRoot); err != nil {
			warnings = append(warnings, fmt.Sprintf("Warning: plugin %s unavailable: %s", name, sanitizeTerminalText(err.Error())))
			continue
		}
		manifest, manifestWarnings, err := loadPluginManifest(canonicalRoot)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Warning: plugin %s unavailable: %s", name, sanitizeTerminalText(err.Error())))
			continue
		}
		if manifest.Name != name {
			warnings = append(warnings, fmt.Sprintf("Warning: plugin %s manifest name changed to %q; inspect and re-enable before using MCP", name, sanitizeTerminalText(manifest.Name)))
			continue
		}
		dataDir, err := pluginDataPath(paths, manifest.Name)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Warning: plugin %s unavailable: %s", name, sanitizeTerminalText(err.Error())))
			continue
		}
		mcpServers, mcpDisabled, mcpWarnings := discoverPluginMCP(canonicalRoot, dataDir, manifest.Name)
		for _, w := range manifestWarnings {
			warnings = append(warnings, "Warning: "+sanitizeTerminalText(w))
		}
		for _, w := range mcpWarnings {
			warnings = append(warnings, "Warning: "+sanitizeTerminalText(w))
		}
		if mcpDisabled || len(mcpServers) == 0 {
			continue
		}
		loaded := &loadedPlugin{
			Root:       canonicalRoot,
			DataDir:    dataDir,
			Manifest:   manifest,
			MCPServers: mcpServers,
		}
		currentDigest, err := reviewPluginMCP(ctx, loaded)
		for _, w := range loaded.Warnings {
			warnings = append(warnings, "Warning: "+sanitizeTerminalText(w))
		}
		if err != nil || entry.MCPDigest == "" || currentDigest != entry.MCPDigest {
			warnings = append(warnings, fmt.Sprintf("Warning: plugin %s MCP configuration changed since approval; run \"meldra plugins allow-mcp %s\" after reviewing it", name, name))
			continue
		}
		for _, server := range loaded.MCPServers {
			servers[server.ServerID] = pluginServerToMCPConfig(manifest.Name, server)
		}
	}
	return servers, warnings, nil
}

func sameHTTPOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	schemeA, schemeB := strings.ToLower(a.Scheme), strings.ToLower(b.Scheme)
	if schemeA != schemeB {
		return false
	}
	if !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	return effectiveURLPort(a) == effectiveURLPort(b)
}

func effectiveURLPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

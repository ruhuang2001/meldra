package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

const (
	// MeldraHomeEnv lets users keep separate Meldra configurations, which is
	// useful for CI and for keeping work and personal credentials apart.
	MeldraHomeEnv = "MELDRA_HOME"

	defaultMeldraHomeName = ".meldra"
	configFileName        = "config.toml"
	credentialsFileName   = "credentials.env"

	// ProviderResponseLimitEnv overrides the configured provider response budget
	// for one process without exposing any credential.
	ProviderResponseLimitEnv = "MELDRA_MAX_PROVIDER_RESPONSE_BYTES"

	privateDirPerm  fs.FileMode = 0o700
	privateFilePerm fs.FileMode = 0o600
)

const defaultConfigTemplate = `# Meldra configuration.
#
# model = "gpt-5.6-luna"
# base_url = "https://api.openai.com/v1"
# max_provider_response_bytes = 33554432
`

const defaultCredentialsTemplate = `# Meldra credentials. Keep this file private.
# OPENAI_API_KEY="replace_with_your_api_key"
`

// ConfigPaths contains every file that belongs to one Meldra configuration.
// Construct it with ResolveConfigPaths or ConfigPathsForHome so the files stay
// inside the same private configuration directory.
type ConfigPaths struct {
	Home            string
	ConfigFile      string
	CredentialsFile string
}

// Config contains non-secret settings stored in config.toml.
type Config struct {
	Model                    string
	BaseURL                  string
	MaxProviderResponseBytes int64
}

// Credentials contains the secret settings stored in credentials.env.
type Credentials struct {
	OpenAIAPIKey string
}

// Settings is the combined file-based Meldra configuration. Environment and
// command-line overrides intentionally belong to the caller, so their
// precedence stays explicit at the CLI boundary.
type Settings struct {
	Model                    string
	BaseURL                  string
	APIKey                   string
	MaxProviderResponseBytes int64
}

// ConfigShow is safe to display or serialize: APIKey is always redacted.
type ConfigShow struct {
	Home                     string `json:"home"`
	ConfigFile               string `json:"config_file"`
	CredentialsFile          string `json:"credentials_file"`
	Model                    string `json:"model"`
	BaseURL                  string `json:"base_url"`
	MaxProviderResponseBytes int64  `json:"max_provider_response_bytes"`
	APIKey                   string `json:"openai_api_key"`
}

// ResolveConfigPaths returns the current user's Meldra configuration paths.
// MELDRA_HOME takes precedence over the default ~/.meldra directory.
func ResolveConfigPaths() (ConfigPaths, error) {
	home := os.Getenv(MeldraHomeEnv)
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return ConfigPaths{}, fmt.Errorf("resolve user home directory: %w", err)
		}
		home = filepath.Join(userHome, defaultMeldraHomeName)
	}
	return ConfigPathsForHome(home)
}

// ConfigPathsForHome returns Meldra's configuration paths rooted at home.
// The returned paths are absolute to make the configuration independent of a
// later working-directory change.
func ConfigPathsForHome(home string) (ConfigPaths, error) {
	if strings.TrimSpace(home) == "" {
		return ConfigPaths{}, fmt.Errorf("Meldra home directory must not be empty")
	}

	absHome, err := filepath.Abs(filepath.Clean(home))
	if err != nil {
		return ConfigPaths{}, fmt.Errorf("resolve Meldra home directory: %w", err)
	}
	return ConfigPaths{
		Home:            absHome,
		ConfigFile:      filepath.Join(absHome, configFileName),
		CredentialsFile: filepath.Join(absHome, credentialsFileName),
	}, nil
}

// InitializeConfig creates a private Meldra configuration directory and
// non-overwriting starter files. Existing regular files are preserved while
// their permissions are tightened to 0600.
func InitializeConfig(paths ConfigPaths) error {
	if err := ensureConfigHome(paths); err != nil {
		return err
	}
	if err := createPrivateFileIfMissing(paths.ConfigFile, defaultConfigTemplate); err != nil {
		return fmt.Errorf("initialize config file: %w", err)
	}
	if err := createPrivateFileIfMissing(paths.CredentialsFile, defaultCredentialsTemplate); err != nil {
		return fmt.Errorf("initialize credentials file: %w", err)
	}
	return nil
}

// LoadConfig loads non-secret runtime settings from the explicit config.toml
// path. A missing configuration directory or file is treated as an empty
// config.
func LoadConfig(paths ConfigPaths) (Config, error) {
	if err := validateConfigPaths(paths); err != nil {
		return Config{}, err
	}
	if _, err := verifyConfigHome(paths); err != nil {
		return Config{}, err
	}

	contents, found, err := readPrivateFile(paths.ConfigFile)
	if err != nil {
		return Config{}, fmt.Errorf("read config file: %w", err)
	}
	if !found {
		return Config{}, nil
	}

	config, err := parseConfigTOML(string(contents))
	if err != nil {
		return Config{}, fmt.Errorf("parse config file %s: %w", paths.ConfigFile, err)
	}
	return config, nil
}

// LoadCredentials loads OPENAI_API_KEY from the explicit credentials.env path.
// It never reads a .env file from the current working directory.
func LoadCredentials(paths ConfigPaths) (Credentials, error) {
	if err := validateConfigPaths(paths); err != nil {
		return Credentials{}, err
	}
	if _, err := verifyConfigHome(paths); err != nil {
		return Credentials{}, err
	}

	if _, found, err := readPrivateFile(paths.CredentialsFile); err != nil {
		return Credentials{}, fmt.Errorf("read credentials file: %w", err)
	} else if !found {
		return Credentials{}, nil
	}

	// godotenv.Read receives the exact credentials path. Unlike godotenv.Load,
	// it does not search for or load the working directory's .env file.
	values, err := godotenv.Read(paths.CredentialsFile)
	if err != nil {
		// godotenv parse errors can include the malformed input, which may be
		// the API key itself. Do not propagate dependency error details.
		return Credentials{}, fmt.Errorf("parse credentials file %s: invalid credentials syntax", paths.CredentialsFile)
	}
	return Credentials{OpenAIAPIKey: values["OPENAI_API_KEY"]}, nil
}

// LoadSettings combines the file-based config and credentials. It does not
// apply process environment overrides; callers should apply those explicitly
// after loading so the precedence is visible in their command handling.
func LoadSettings(paths ConfigPaths) (Settings, error) {
	config, err := LoadConfig(paths)
	if err != nil {
		return Settings{}, err
	}
	credentials, err := LoadCredentials(paths)
	if err != nil {
		return Settings{}, err
	}
	return Settings{
		Model:                    config.Model,
		BaseURL:                  config.BaseURL,
		APIKey:                   credentials.OpenAIAPIKey,
		MaxProviderResponseBytes: config.MaxProviderResponseBytes,
	}, nil
}

// SaveConfig writes non-secret settings to config.toml with private file
// permissions. API keys are deliberately not part of Config and cannot be
// written through this method.
func SaveConfig(paths ConfigPaths, config Config) error {
	if err := validateConfigValue("model", config.Model); err != nil {
		return err
	}
	if err := validateConfigValue("base_url", config.BaseURL); err != nil {
		return err
	}
	if err := validateProviderResponseLimit(config.MaxProviderResponseBytes); err != nil {
		return err
	}

	contents := "# Meldra configuration.\n"
	if config.Model != "" {
		contents += "model = " + strconv.Quote(config.Model) + "\n"
	}
	if config.BaseURL != "" {
		contents += "base_url = " + strconv.Quote(config.BaseURL) + "\n"
	}
	if config.MaxProviderResponseBytes != 0 {
		contents += "max_provider_response_bytes = " + strconv.FormatInt(config.MaxProviderResponseBytes, 10) + "\n"
	}
	if err := writePrivateFile(paths, paths.ConfigFile, []byte(contents)); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}
	return nil
}

// SaveAPIKey replaces credentials.env with a single OPENAI_API_KEY entry.
// Callers should avoid accepting API keys directly as a positional shell
// argument because shells commonly retain those arguments in history.
func SaveAPIKey(paths ConfigPaths, apiKey string) error {
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("API key must not be empty")
	}
	if strings.ContainsAny(apiKey, "\r\n") {
		return fmt.Errorf("API key must not contain line breaks")
	}

	contents := "# Meldra credentials. Keep this file private.\n" +
		"OPENAI_API_KEY=" + strconv.Quote(apiKey) + "\n"
	if err := writePrivateFile(paths, paths.CredentialsFile, []byte(contents)); err != nil {
		return fmt.Errorf("write credentials file: %w", err)
	}
	return nil
}

// ShowConfig loads the file-based settings and returns data that is safe to
// print. For an effective view that includes caller-applied environment
// overrides, use ConfigShowData with those effective settings instead.
func ShowConfig(paths ConfigPaths) (ConfigShow, error) {
	settings, err := LoadSettings(paths)
	if err != nil {
		return ConfigShow{}, err
	}
	return ConfigShowData(paths, settings), nil
}

// ConfigShowData builds display-safe configuration data. Its APIKey field is
// always redacted, even when the given Settings came from another source.
func ConfigShowData(paths ConfigPaths, settings Settings) ConfigShow {
	return ConfigShow{
		Home:                     paths.Home,
		ConfigFile:               paths.ConfigFile,
		CredentialsFile:          paths.CredentialsFile,
		Model:                    settings.Model,
		BaseURL:                  settings.BaseURL,
		MaxProviderResponseBytes: settings.MaxProviderResponseBytes,
		APIKey:                   RedactSecret(settings.APIKey),
	}
}

// FormatConfigShow renders a compact, human-readable config show result.
func FormatConfigShow(show ConfigShow) string {
	return fmt.Sprintf(
		"MELDRA_HOME=%s\nconfig_file=%s\ncredentials_file=%s\nmodel=%s\nbase_url=%s\nmax_provider_response_bytes=%d\nopenai_api_key=%s\n",
		show.Home,
		show.ConfigFile,
		show.CredentialsFile,
		displayValue(show.Model),
		displayValue(show.BaseURL),
		show.MaxProviderResponseBytes,
		show.APIKey,
	)
}

// RedactSecret returns a display-safe replacement for a secret. It does not
// reveal a prefix or suffix, which prevents a config show command from leaking
// any part of an API key to logs, terminals, or bug reports.
func RedactSecret(secret string) string {
	if secret == "" {
		return "<not set>"
	}
	return "********"
}

func displayValue(value string) string {
	if value == "" {
		return "<not set>"
	}
	return value
}

func validateConfigPaths(paths ConfigPaths) error {
	if strings.TrimSpace(paths.Home) == "" {
		return fmt.Errorf("Meldra home directory must not be empty")
	}
	absHome, err := filepath.Abs(filepath.Clean(paths.Home))
	if err != nil {
		return fmt.Errorf("resolve Meldra home directory: %w", err)
	}
	if paths.Home != absHome {
		return fmt.Errorf("Meldra home directory must be an absolute, clean path")
	}
	if paths.ConfigFile != filepath.Join(absHome, configFileName) {
		return fmt.Errorf("config file must be inside Meldra home directory")
	}
	if paths.CredentialsFile != filepath.Join(absHome, credentialsFileName) {
		return fmt.Errorf("credentials file must be inside Meldra home directory")
	}
	return nil
}

func ensureConfigHome(paths ConfigPaths) error {
	if err := validateConfigPaths(paths); err != nil {
		return err
	}
	if err := makeDirectoryTreeDurable(paths.Home, privateDirPerm, syncDirectory); err != nil {
		return fmt.Errorf("create Meldra home directory: %w", err)
	}

	info, err := os.Lstat(paths.Home)
	if err != nil {
		return fmt.Errorf("inspect Meldra home directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Meldra home directory must not be a symbolic link")
	}
	if !info.IsDir() {
		return fmt.Errorf("Meldra home path is not a directory")
	}
	if err := os.Chmod(paths.Home, privateDirPerm); err != nil {
		return fmt.Errorf("secure Meldra home directory: %w", err)
	}
	return nil
}

// verifyConfigHome checks an existing config home without creating it. The
// bool result says whether it exists.
func verifyConfigHome(paths ConfigPaths) (bool, error) {
	info, err := os.Lstat(paths.Home)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect Meldra home directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("Meldra home directory must not be a symbolic link")
	}
	if !info.IsDir() {
		return false, fmt.Errorf("Meldra home path is not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return false, fmt.Errorf("Meldra home directory permissions are %04o; expected 0700", info.Mode().Perm())
	}
	return true, nil
}

func createPrivateFileIfMissing(path string, contents string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateFilePerm)
	if err == nil {
		if _, writeErr := file.WriteString(contents); writeErr != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return fmt.Errorf("write file: %w", writeErr)
		}
		if syncErr := file.Sync(); syncErr != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return fmt.Errorf("sync file: %w", syncErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("close file: %w", closeErr)
		}
		if syncErr := syncDirectory(filepath.Dir(path)); syncErr != nil {
			return fmt.Errorf("sync configuration directory: %w", syncErr)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return err
	}

	info, statErr := os.Lstat(path)
	if statErr != nil {
		return fmt.Errorf("inspect existing file: %w", statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("existing file must not be a symbolic link")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("existing path is not a regular file")
	}
	if chmodErr := os.Chmod(path, privateFilePerm); chmodErr != nil {
		return fmt.Errorf("secure existing file: %w", chmodErr)
	}
	return nil
}

func readPrivateFile(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("file must not be a symbolic link")
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("path is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, false, fmt.Errorf("file permissions are %04o; expected 0600", info.Mode().Perm())
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	return contents, true, nil
}

func writePrivateFile(paths ConfigPaths, target string, contents []byte) error {
	if err := ensureConfigHome(paths); err != nil {
		return err
	}
	if err := validatePrivateWriteTarget(paths, target); err != nil {
		return err
	}

	temp, err := os.CreateTemp(paths.Home, "."+filepath.Base(target)+".tmp-")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)

	if err := temp.Chmod(privateFilePerm); err != nil {
		_ = temp.Close()
		return fmt.Errorf("secure temporary file: %w", err)
	}
	if _, err := temp.Write(contents); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tempPath, target); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	if err := os.Chmod(target, privateFilePerm); err != nil {
		return fmt.Errorf("secure file: %w", err)
	}
	if err := syncDirectory(paths.Home); err != nil {
		return fmt.Errorf("sync configuration directory: %w", err)
	}
	return nil
}

func validatePrivateWriteTarget(paths ConfigPaths, target string) error {
	if target != paths.ConfigFile && target != paths.CredentialsFile {
		return fmt.Errorf("target file must be a Meldra configuration file")
	}
	info, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect target file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("target file must not be a symbolic link")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("target path is not a regular file")
	}
	return nil
}

func validateConfigValue(name, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s must not contain line breaks", name)
	}
	return nil
}

func validateProviderResponseLimit(value int64) error {
	if value < 0 {
		return fmt.Errorf("max_provider_response_bytes must be positive")
	}
	if value > maximumProviderResponseBytes {
		return fmt.Errorf("max_provider_response_bytes must not exceed %d", maximumProviderResponseBytes)
	}
	return nil
}

func parseConfigTOML(contents string) (Config, error) {
	var config Config
	seen := make(map[string]bool)

	for lineNumber, rawLine := range strings.Split(contents, "\n") {
		line := strings.TrimSpace(stripTOMLComment(rawLine))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			return Config{}, fmt.Errorf("line %d: TOML sections are not supported", lineNumber+1)
		}

		key, rawValue, ok := strings.Cut(line, "=")
		if !ok {
			return Config{}, fmt.Errorf("line %d: expected key = value", lineNumber+1)
		}
		key = strings.TrimSpace(key)
		rawValue = strings.TrimSpace(rawValue)

		switch key {
		case "model", "base_url":
			if seen[key] {
				return Config{}, fmt.Errorf("line %d: duplicate %q setting", lineNumber+1, key)
			}
			seen[key] = true
			value, err := parseTOMLString(rawValue)
			if err != nil {
				return Config{}, fmt.Errorf("line %d: %s: %w", lineNumber+1, key, err)
			}
			if err := validateConfigValue(key, value); err != nil {
				return Config{}, fmt.Errorf("line %d: %w", lineNumber+1, err)
			}
			if key == "model" {
				config.Model = value
			} else {
				config.BaseURL = value
			}
		case "max_provider_response_bytes":
			if seen[key] {
				return Config{}, fmt.Errorf("line %d: duplicate %q setting", lineNumber+1, key)
			}
			seen[key] = true
			value, err := strconv.ParseInt(rawValue, 10, 64)
			if err != nil {
				return Config{}, fmt.Errorf("line %d: %s must be an integer: %w", lineNumber+1, key, err)
			}
			if value == 0 {
				return Config{}, fmt.Errorf("line %d: max_provider_response_bytes must be positive", lineNumber+1)
			}
			if err := validateProviderResponseLimit(value); err != nil {
				return Config{}, fmt.Errorf("line %d: %w", lineNumber+1, err)
			}
			config.MaxProviderResponseBytes = value
		default:
			// Unknown top-level keys are deliberately ignored. That makes config
			// files forward-compatible without permitting unknown keys to affect
			// credentials, workspace boundaries, or other security policy.
		}
	}
	return config, nil
}

func stripTOMLComment(line string) string {
	inDoubleQuote := false
	inSingleQuote := false
	escaped := false
	for index, character := range line {
		if inDoubleQuote && escaped {
			escaped = false
			continue
		}
		switch character {
		case '\\':
			if inDoubleQuote {
				escaped = true
			}
		case '"':
			if !inSingleQuote {
				inDoubleQuote = !inDoubleQuote
			}
		case '\'':
			if !inDoubleQuote {
				inSingleQuote = !inSingleQuote
			}
		case '#':
			if !inDoubleQuote && !inSingleQuote {
				return line[:index]
			}
		}
	}
	return line
}

func parseTOMLString(value string) (string, error) {
	if len(value) < 2 {
		return "", fmt.Errorf("value must be a quoted string")
	}
	if value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1], nil
	}
	if value[0] != '"' || value[len(value)-1] != '"' {
		return "", fmt.Errorf("value must be a quoted string")
	}
	parsed, err := strconv.Unquote(value)
	if err != nil {
		return "", fmt.Errorf("invalid quoted string: %w", err)
	}
	return parsed, nil
}

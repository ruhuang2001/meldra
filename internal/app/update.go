package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const updateLimit = 64 << 20

var stableReleaseTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var semverPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

func parseUpdateVersion(value string) []string {
	parts := semverPattern.FindStringSubmatch(value)
	if parts == nil {
		return nil
	}
	for _, id := range strings.Split(parts[4], ".") {
		if numericVersionID(id) && len(id) > 1 && id[0] == '0' {
			return nil
		}
	}
	return parts
}

func numericVersionID(id string) bool {
	return id != "" && strings.Trim(id, "0123456789") == ""
}

// compareSemver compares validated versions without integer overflow.
func compareSemver(current, latest string) int {
	a, b := parseUpdateVersion(current), parseUpdateVersion(latest)
	for i := 1; i <= 3; i++ {
		if len(a[i]) != len(b[i]) {
			return len(a[i]) - len(b[i])
		}
		if n := strings.Compare(a[i], b[i]); n != 0 {
			return n
		}
	}
	if a[4] == b[4] {
		return 0
	}
	if a[4] == "" {
		return 1
	}
	if b[4] == "" {
		return -1
	}
	x, y := strings.Split(a[4], "."), strings.Split(b[4], ".")
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] == y[i] {
			continue
		}
		xn, yn := numericVersionID(x[i]), numericVersionID(y[i])
		if xn && !yn {
			return -1
		}
		if !xn && yn {
			return 1
		}
		if xn && len(x[i]) != len(y[i]) {
			return len(x[i]) - len(y[i])
		}
		return strings.Compare(x[i], y[i])
	}
	return len(x) - len(y)
}

type releaseUpdater struct {
	client      *http.Client
	apiURL      string
	downloadURL string
	executable  func() (string, error)
}

func newReleaseUpdater() releaseUpdater {
	return releaseUpdater{
		client:      &http.Client{Timeout: 10 * time.Second},
		apiURL:      "https://api.github.com/repos/ruhuang2001/meldra/releases/latest",
		downloadURL: "https://github.com/ruhuang2001/meldra/releases/download",
		executable:  os.Executable,
	}
}

func (u releaseUpdater) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "meldra-updater")
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update request returned HTTP %d", resp.StatusCode)
	}
	return readUpdateBytes(resp.Body, limit)
}

func readUpdateBytes(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("update response exceeds size limit")
	}
	return data, nil
}

func (u releaseUpdater) latest(ctx context.Context) (string, error) {
	data, err := u.fetch(ctx, u.apiURL, 1<<20)
	if err != nil {
		return "", err
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(data, &release); err != nil {
		return "", fmt.Errorf("decode latest release: %w", err)
	}
	if release.Draft || release.Prerelease || !stableReleaseTag.MatchString(release.Tag) {
		return "", errors.New("latest release is not a stable vMAJOR.MINOR.PATCH release")
	}
	return release.Tag, nil
}

func updateArchiveName() (string, error) {
	if (runtime.GOOS != "darwin" && runtime.GOOS != "linux") || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return "", errors.New("self-update is supported only on darwin/linux amd64/arm64")
	}
	osName := map[string]string{"darwin": "Darwin", "linux": "Linux"}[runtime.GOOS]
	return "meldra_" + osName + "_" + runtime.GOARCH + ".tar.gz", nil
}

const updateUsage = `Usage: meldra update
Check for the latest stable release and install it if newer.
Shows update progress and the current and target versions.
`

func runUpdateCommand(ctx context.Context, args []string, stdout io.Writer) error {
	return newReleaseUpdater().run(ctx, args, stdout)
}

func (u releaseUpdater) run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprint(stdout, updateUsage)
		return err
	}
	if len(args) != 0 {
		return fmt.Errorf("update does not accept arguments\n%s", updateUsage)
	}
	if parseUpdateVersion(version) == nil {
		return errors.New("self-update is unavailable for an unversioned development build; install a release build first")
	}
	current := "v" + strings.TrimPrefix(version, "v")
	if _, err := fmt.Fprintf(stdout, "Checking for updates (current: %s)...\n", current); err != nil {
		return err
	}
	latest, err := u.latest(ctx)
	if err != nil {
		return err
	}
	if comparison := compareSemver(version, latest); comparison >= 0 {
		status := "up to date"
		if comparison > 0 {
			status = "ahead of the latest stable release"
		}
		_, err := fmt.Fprintf(stdout, "Meldra %s is %s (latest: %s); no update installed.\n", current, status, latest)
		return err
	}
	archiveName, err := updateArchiveName()
	if err != nil {
		return err
	}
	target, err := u.executable()
	if err != nil {
		return err
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	if err := preflightUpdateTarget(target); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Updating Meldra: %s -> %s\nDownloading checksums...\n", current, latest); err != nil {
		return err
	}
	base := u.downloadURL + "/" + latest + "/"
	checksums, err := u.fetch(ctx, base+"meldra_checksums.txt", 1<<20)
	if err != nil {
		return err
	}
	expected, err := updateChecksum(checksums, archiveName)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Downloading %s...\n", archiveName); err != nil {
		return err
	}
	archive, err := u.fetch(ctx, base+archiveName, updateLimit)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, "Verifying download..."); err != nil {
		return err
	}
	actual := sha256.Sum256(archive)
	if !bytes.Equal(expected, actual[:]) {
		return errors.New("update SHA-256 checksum mismatch; existing binary unchanged")
	}
	binary, err := unpackUpdate(archive)
	if err != nil {
		return fmt.Errorf("unpack update: %w", err)
	}
	if _, err := fmt.Fprintln(stdout, "Installing update..."); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := replaceUpdateBinary(target, binary); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Updated Meldra: %s -> %s.\n", current, latest)
	return err
}

func updateChecksum(data []byte, name string) ([]byte, error) {
	var found []byte
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		digest, err := hex.DecodeString(fields[0])
		if err != nil || len(digest) != sha256.Size || found != nil {
			return nil, errors.New("invalid or duplicate update checksum")
		}
		found = digest
	}
	if found == nil {
		return nil, errors.New("archive missing from update checksums")
	}
	return found, nil
}

func unpackUpdate(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	// Bound total expansion as well as the extracted binary. No archive path
	// is ever used as a filesystem destination.
	limited := &io.LimitedReader{R: gz, N: 2*updateLimit + 1}
	tr := tar.NewReader(limited)
	var binary []byte
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Name != "meldra" {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > updateLimit || binary != nil {
			return nil, errors.New("invalid or duplicate meldra binary in archive")
		}
		binary, err = readUpdateBytes(tr, updateLimit)
		if err != nil {
			return nil, err
		}
	}
	// Read the gzip trailer too: tar EOF alone does not validate gzip CRC.
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return nil, err
	}
	if limited.N == 0 {
		return nil, errors.New("update archive expansion exceeds size limit")
	}
	if len(binary) == 0 {
		return nil, errors.New("archive does not contain a meldra binary")
	}
	return binary, nil
}

func replaceUpdateBinary(target string, binary []byte) error {
	if err := preflightUpdateTarget(target); err != nil {
		return err
	}
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".meldra.update-*")
	if err != nil {
		return fmt.Errorf("installation directory is not writable: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(binary); err != nil {
		return err
	}
	if err := tmp.Chmod(0755); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("binary replaced, but installation directory sync failed: %w", err)
	}
	return nil
}

type updateCheckState struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest_version"`
}

// startUpdateCheck never waits for the network. Call only after interactive
// output is ready, and cancel ctx before closing its event sink.
func (u releaseUpdater) startUpdateCheck(ctx context.Context, paths ConfigPaths, current string, sink UIEventSink) {
	if os.Getenv("MELDRA_DISABLE_UPDATE_CHECK") == "1" || parseUpdateVersion(current) == nil {
		return
	}
	path := filepath.Join(paths.Home, "update-check.json")
	var state updateCheckState
	if file, err := os.Open(path); err == nil {
		data, readErr := readUpdateBytes(file, 4096)
		_ = file.Close()
		if readErr == nil {
			_ = json.Unmarshal(data, &state)
		}
	}
	notify := func(latest string) {
		if ctx.Err() == nil && stableReleaseTag.MatchString(latest) && compareSemver(current, latest) < 0 {
			sink.Emit(UIEvent{Kind: UIEventNotice, Text: fmt.Sprintf("Update available: %s -> %s. Run \"meldra update\" in another terminal.", current, latest)})
		}
	}
	notify(state.Latest)
	age := time.Since(state.CheckedAt)
	if stableReleaseTag.MatchString(state.Latest) && age >= 0 && age < 24*time.Hour {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		latest, err := u.latest(ctx)
		if err != nil || ctx.Err() != nil {
			return
		}
		if latest != state.Latest {
			notify(latest)
		}
		data, _ := json.Marshal(updateCheckState{CheckedAt: time.Now().UTC(), Latest: latest})
		if ensureConfigHome(paths) != nil {
			return
		}
		tmp, err := os.CreateTemp(paths.Home, ".update-check-*")
		if err != nil {
			return
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()
		if _, err := tmp.Write(data); err != nil {
			return
		}
		if tmp.Close() != nil {
			return
		}
		_ = os.Rename(tmp.Name(), path)
	}()
}

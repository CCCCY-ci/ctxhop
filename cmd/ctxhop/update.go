package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/atomicfile"
	"github.com/CCCCY-ci/ctxhop/internal/config"
)

const (
	updateReleaseEndpoint = "https://api.github.com/repos/CCCCY-ci/ctxhop/releases/latest"
	updateCacheFile       = "update-check.json"
	updateCheckInterval   = 24 * time.Hour
	updateSkipInterval    = 7 * 24 * time.Hour
	updateCheckTimeout    = 3 * time.Second
	updateHTTPTimeout     = 15 * time.Second
	maxUpdateMetadata     = 4 << 20
	maxUpdateChecksum     = 4 << 20
	maxUpdateArchive      = 512 << 20
)

type updateVersion struct {
	major      uint64
	minor      uint64
	patch      uint64
	prerelease []string
	normalized string
}

type githubReleaseResponse struct {
	TagName    string               `json:"tag_name"`
	HTMLURL    string               `json:"html_url"`
	Draft      bool                 `json:"draft"`
	Prerelease bool                 `json:"prerelease"`
	Assets     []githubReleaseAsset `json:"assets"`
}

type githubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type updateRelease struct {
	Tag     string
	Version string
	URL     string
	Assets  []updateAsset
}

type updateAsset struct {
	Name string
	URL  string
	Size int64
}

type updateCheckState struct {
	CheckedAt      time.Time `json:"checkedAt"`
	LatestVersion  string    `json:"latestVersion,omitempty"`
	SkippedVersion string    `json:"skippedVersion,omitempty"`
	SkipUntil      time.Time `json:"skipUntil,omitempty"`
}

func init() {
	for i := range commands {
		if commands[i].name == "update" {
			commands[i].run = runUpdate
		}
	}
}

func runUpdate(args []string) error {
	return runUpdateWithOutput(args, os.Stdout)
}

func runUpdateWithOutput(args []string, output io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("update: unexpected argument %q", args[0])
	}
	if output == nil {
		return errors.New("update: output is required")
	}

	current, err := parseUpdateVersion(version)
	if err != nil {
		return fmt.Errorf("update: current build %q is not a release build; install a release build first", version)
	}

	ctx, cancel := context.WithTimeout(context.Background(), updateHTTPTimeout)
	defer cancel()
	client := newUpdateHTTPClient()
	release, err := fetchLatestRelease(ctx, client, updateReleaseEndpoint)
	if err != nil {
		return fmt.Errorf("update: check the latest release: %w", err)
	}
	_, err = applyUpdate(ctx, client, current, release, output)
	return err
}

func parseUpdateVersion(raw string) (updateVersion, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "v") || strings.HasPrefix(raw, "V") {
		raw = raw[1:]
	}
	if raw == "" {
		return updateVersion{}, errors.New("version is empty")
	}
	if plus := strings.IndexByte(raw, '+'); plus >= 0 {
		raw = raw[:plus]
	}
	parts := strings.SplitN(raw, "-", 2)
	core := strings.Split(parts[0], ".")
	if len(core) < 1 || len(core) > 3 {
		return updateVersion{}, fmt.Errorf("invalid release version %q", raw)
	}

	values := [3]uint64{}
	for i, part := range core {
		value, err := parseUpdateVersionNumber(part)
		if err != nil {
			return updateVersion{}, fmt.Errorf("invalid release version %q: %w", raw, err)
		}
		values[i] = value
	}

	var prerelease []string
	if len(parts) == 2 {
		if parts[1] == "" {
			return updateVersion{}, fmt.Errorf("invalid release version %q: empty pre-release", raw)
		}
		for _, identifier := range strings.Split(parts[1], ".") {
			if !validUpdateVersionIdentifier(identifier) {
				return updateVersion{}, fmt.Errorf("invalid release version %q: malformed pre-release", raw)
			}
			if isUpdateVersionNumber(identifier) && len(identifier) > 1 && identifier[0] == '0' {
				return updateVersion{}, fmt.Errorf("invalid release version %q: pre-release number has a leading zero", raw)
			}
			prerelease = append(prerelease, identifier)
		}
	}

	normalized := fmt.Sprintf("%d.%d.%d", values[0], values[1], values[2])
	if len(prerelease) > 0 {
		normalized += "-" + strings.Join(prerelease, ".")
	}
	return updateVersion{
		major:      values[0],
		minor:      values[1],
		patch:      values[2],
		prerelease: prerelease,
		normalized: normalized,
	}, nil
}

func parseUpdateVersionNumber(raw string) (uint64, error) {
	if raw == "" || (len(raw) > 1 && raw[0] == '0') || !isUpdateVersionNumber(raw) {
		return 0, errors.New("numeric component is malformed")
	}
	value, err := strconv.ParseUint(raw, 10, 63)
	if err != nil {
		return 0, errors.New("numeric component is out of range")
	}
	return value, nil
}

func validUpdateVersionIdentifier(raw string) bool {
	if raw == "" {
		return false
	}
	for _, character := range raw {
		if (character >= '0' && character <= '9') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= 'a' && character <= 'z') ||
			character == '-' {
			continue
		}
		return false
	}
	return true
}

func isUpdateVersionNumber(raw string) bool {
	if raw == "" {
		return false
	}
	for _, character := range raw {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func compareUpdateVersions(first, second updateVersion) int {
	for _, pair := range [][2]uint64{
		{first.major, second.major},
		{first.minor, second.minor},
		{first.patch, second.patch},
	} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}

	if len(first.prerelease) == 0 && len(second.prerelease) == 0 {
		return 0
	}
	if len(first.prerelease) == 0 {
		return 1
	}
	if len(second.prerelease) == 0 {
		return -1
	}
	for i := 0; i < len(first.prerelease) && i < len(second.prerelease); i++ {
		left, right := first.prerelease[i], second.prerelease[i]
		leftNumber, rightNumber := isUpdateVersionNumber(left), isUpdateVersionNumber(right)
		switch {
		case leftNumber && rightNumber:
			if comparison := compareUpdateVersionNumericIdentifiers(left, right); comparison != 0 {
				return comparison
			}
		case leftNumber != rightNumber:
			if leftNumber {
				return -1
			}
			return 1
		case left < right:
			return -1
		case left > right:
			return 1
		}
	}
	if len(first.prerelease) < len(second.prerelease) {
		return -1
	}
	if len(first.prerelease) > len(second.prerelease) {
		return 1
	}
	return 0
}

func compareUpdateVersionNumericIdentifiers(first, second string) int {
	// Numeric pre-release identifiers are valid beyond uint64. Leading zeroes
	// are already rejected by parseUpdateVersion, so length and lexical order
	// provide an overflow-safe numeric comparison.
	if len(first) < len(second) {
		return -1
	}
	if len(first) > len(second) {
		return 1
	}
	if first < second {
		return -1
	}
	if first > second {
		return 1
	}
	return 0
}

func newUpdateHTTPClient() *http.Client {
	return &http.Client{
		Timeout: updateHTTPTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			if !strings.EqualFold(request.URL.Scheme, "https") {
				return errors.New("redirected update request is not HTTPS")
			}
			return nil
		},
	}
}

func fetchLatestRelease(ctx context.Context, client *http.Client, endpoint string) (updateRelease, error) {
	if client == nil {
		client = newUpdateHTTPClient()
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" {
		return updateRelease{}, errors.New("release endpoint must use HTTPS")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return updateRelease{}, fmt.Errorf("create release request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", cliName+"/"+version)

	response, err := client.Do(request)
	if err != nil {
		return updateRelease{}, err
	}
	// The bounded read below determines the result; closing an HTTP response
	// body after that read is best-effort and must not hide the useful error.
	defer func() { _ = response.Body.Close() }()
	body, err := readUpdateResponse(response, maxUpdateMetadata)
	if err != nil {
		return updateRelease{}, err
	}
	if response.StatusCode != http.StatusOK {
		detail := strings.TrimSpace(string(body))
		if len(detail) > 240 {
			detail = detail[:240] + "..."
		}
		if detail == "" {
			return updateRelease{}, fmt.Errorf("release endpoint returned HTTP %d", response.StatusCode)
		}
		return updateRelease{}, fmt.Errorf("release endpoint returned HTTP %d: %s", response.StatusCode, detail)
	}

	var payload githubReleaseResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return updateRelease{}, fmt.Errorf("decode release metadata: %w", err)
	}
	if payload.Draft || payload.Prerelease {
		return updateRelease{}, errors.New("latest release is not a stable release")
	}
	releaseVersion, err := parseUpdateVersion(payload.TagName)
	if err != nil {
		return updateRelease{}, fmt.Errorf("release tag %q: %w", payload.TagName, err)
	}
	assets := make([]updateAsset, 0, len(payload.Assets))
	for _, asset := range payload.Assets {
		if strings.TrimSpace(asset.Name) == "" || strings.TrimSpace(asset.BrowserDownloadURL) == "" {
			continue
		}
		assets = append(assets, updateAsset{
			Name: asset.Name,
			URL:  asset.BrowserDownloadURL,
			Size: asset.Size,
		})
	}
	return updateRelease{
		Tag:     payload.TagName,
		Version: releaseVersion.normalized,
		URL:     payload.HTMLURL,
		Assets:  assets,
	}, nil
}

func readUpdateResponse(response *http.Response, maximum int64) ([]byte, error) {
	if response.ContentLength > maximum {
		return nil, fmt.Errorf("response is too large (%d bytes)", response.ContentLength)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maximum {
		return nil, fmt.Errorf("response exceeds the %d-byte limit", maximum)
	}
	return body, nil
}

func selectUpdateAssets(release updateRelease, goos, goarch string) (updateAsset, updateAsset, error) {
	if release.Version == "" {
		return updateAsset{}, updateAsset{}, errors.New("release version is empty")
	}
	executableAssetName := fmt.Sprintf("ctxhop_%s_%s_%s.zip", release.Version, goos, goarch)
	var archive, checksums updateAsset
	for _, asset := range release.Assets {
		switch asset.Name {
		case executableAssetName:
			archive = asset
		case "checksums.txt":
			checksums = asset
		}
	}
	if archive.Name == "" {
		return updateAsset{}, updateAsset{}, fmt.Errorf("release has no update package for %s/%s", goos, goarch)
	}
	if checksums.Name == "" {
		return updateAsset{}, updateAsset{}, errors.New("release has no checksums.txt asset")
	}
	if archive.Size < 0 || archive.Size > maxUpdateArchive {
		return updateAsset{}, updateAsset{}, errors.New("update package size is invalid")
	}
	return archive, checksums, nil
}

func downloadUpdateFile(ctx context.Context, client *http.Client, rawURL, destination string, maximum int64) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" {
		return errors.New("update asset URL must use HTTPS")
	}
	if client == nil {
		client = newUpdateHTTPClient()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", cliName+"/"+version)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	// The download result is determined by the bounded copy and file sync;
	// response-body cleanup is best-effort after those operations.
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download endpoint returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maximum {
		return fmt.Errorf("download is too large (%d bytes)", response.ContentLength)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			// Preserve the primary download error. Antivirus or another process
			// may briefly hold either path during best-effort cleanup.
			_ = file.Close()
			_ = os.Remove(destination)
		}
	}()
	count, err := io.Copy(file, io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return err
	}
	if count > maximum {
		return fmt.Errorf("download exceeds the %d-byte limit", maximum)
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

func checksumForUpdateAsset(contents []byte, assetName string) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(contents))
	scanner.Buffer(make([]byte, 1024), maxUpdateChecksum)
	var found string
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		if name != assetName {
			continue
		}
		digest := strings.ToLower(fields[0])
		if len(digest) != sha256.Size*2 {
			return "", errors.New("checksum has an invalid SHA-256 digest")
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return "", errors.New("checksum has an invalid SHA-256 digest")
		}
		if found != "" && found != digest {
			return "", errors.New("checksum file contains conflicting entries")
		}
		found = digest
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("checksum for %q is missing", assetName)
	}
	return found, nil
}

func verifyUpdateChecksum(path string, checksumContents []byte, assetName string) error {
	expected, err := checksumForUpdateAsset(checksumContents, assetName)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	// The hash result is authoritative; closing a read-only file afterward is
	// best-effort and cannot change the bytes that were already read.
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expected {
		return fmt.Errorf("checksum mismatch for %q", assetName)
	}
	return nil
}

func extractUpdateBinary(archivePath, expectedName, destination string) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	// The archive has already been fully inspected when this function returns;
	// closing its read handle is best-effort cleanup.
	defer func() { _ = archive.Close() }()

	var selected *zip.File
	for _, file := range archive.File {
		name := strings.ReplaceAll(file.Name, "\\", "/")
		if name != expectedName {
			continue
		}
		if selected != nil {
			return fmt.Errorf("update package contains duplicate %q entries", expectedName)
		}
		selected = file
	}
	if selected == nil {
		return fmt.Errorf("update package does not contain %q", expectedName)
	}
	info := selected.FileInfo()
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("update package entry %q is not a regular file", expectedName)
	}
	if selected.UncompressedSize64 > uint64(maxUpdateArchive) {
		return errors.New("unpacked update binary is too large")
	}

	source, err := selected.Open()
	if err != nil {
		return err
	}
	// The extracted file is validated by the bounded copy and sync; closing the
	// source stream afterward is best-effort cleanup.
	defer func() { _ = source.Close() }()
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			// Preserve the primary extraction error. Antivirus or another process
			// may briefly hold either path during best-effort cleanup.
			_ = file.Close()
			_ = os.Remove(destination)
		}
	}()
	count, err := io.Copy(file, io.LimitReader(source, int64(maxUpdateArchive)+1))
	if err != nil {
		return err
	}
	if count > maxUpdateArchive {
		return errors.New("unpacked update binary is too large")
	}
	if err := file.Chmod(0o755); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

func updateExecutableName() string {
	if runtime.GOOS == "windows" {
		return cliName + ".exe"
	}
	return cliName
}

func currentExecutablePath() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("current executable is a symbolic link; update it with its package manager")
	}
	if info.IsDir() {
		return "", errors.New("current executable is a directory")
	}
	return path, nil
}

func applyUpdate(ctx context.Context, client *http.Client, current updateVersion, release updateRelease, output io.Writer) (updateReplacementResult, error) {
	releaseVersion, err := parseUpdateVersion(release.Version)
	if err != nil {
		return updateReplacementResult{}, fmt.Errorf("update: release version is invalid: %w", err)
	}
	if compareUpdateVersions(current, releaseVersion) >= 0 {
		_, err := fmt.Fprintf(output, "%s: already up to date (%s)\n", cliName, current.normalized)
		return updateReplacementResult{}, err
	}
	archiveAsset, checksumAsset, err := selectUpdateAssets(release, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return updateReplacementResult{}, fmt.Errorf("update: %w", err)
	}
	if _, err := fmt.Fprintf(output, "%s: updating %s -> %s\n", cliName, current.normalized, releaseVersion.normalized); err != nil {
		return updateReplacementResult{}, err
	}

	workDir, err := os.MkdirTemp("", "ctxhop-update-*")
	if err != nil {
		return updateReplacementResult{}, fmt.Errorf("update: create working directory: %w", err)
	}
	keepWorkDir := false
	defer func() {
		if !keepWorkDir {
			// A failed cleanup must not replace the update/download error. A
			// scanner can briefly hold files in the temporary directory.
			_ = os.RemoveAll(workDir)
		}
	}()

	archivePath := filepath.Join(workDir, archiveAsset.Name)
	checksumPath := filepath.Join(workDir, checksumAsset.Name)
	if err := downloadUpdateFile(ctx, client, checksumAsset.URL, checksumPath, maxUpdateChecksum); err != nil {
		return updateReplacementResult{}, fmt.Errorf("update: download checksums: %w", err)
	}
	checksumContents, err := os.ReadFile(checksumPath)
	if err != nil {
		return updateReplacementResult{}, fmt.Errorf("update: read checksums: %w", err)
	}
	if err := downloadUpdateFile(ctx, client, archiveAsset.URL, archivePath, maxUpdateArchive); err != nil {
		return updateReplacementResult{}, fmt.Errorf("update: download package: %w", err)
	}
	if err := verifyUpdateChecksum(archivePath, checksumContents, archiveAsset.Name); err != nil {
		return updateReplacementResult{}, fmt.Errorf("update: verify package: %w", err)
	}

	executablePath := filepath.Join(workDir, updateExecutableName())
	if err := extractUpdateBinary(archivePath, updateExecutableName(), executablePath); err != nil {
		return updateReplacementResult{}, fmt.Errorf("update: extract package: %w", err)
	}
	replacement, err := replaceCurrentExecutable(executablePath, workDir)
	if err != nil {
		return updateReplacementResult{}, fmt.Errorf("update: replace executable: %w", err)
	}
	if replacement.keepWorkDir {
		keepWorkDir = true
	}
	if replacement.scheduled {
		_, err = fmt.Fprintf(output, "%s: update scheduled; restart %s after this command exits\n", cliName, cliName)
	} else {
		_, err = fmt.Fprintf(output, "%s: updated to %s\n", cliName, releaseVersion.normalized)
	}
	return replacement, err
}

func loadUpdateCheckState() (updateCheckState, error) {
	path, err := updateCachePath()
	if err != nil {
		return updateCheckState{}, err
	}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return updateCheckState{}, nil
	}
	if err != nil {
		return updateCheckState{}, err
	}
	var state updateCheckState
	if err := json.Unmarshal(contents, &state); err != nil {
		return updateCheckState{}, err
	}
	return state, nil
}

func saveUpdateCheckState(state updateCheckState) error {
	path, err := updateCachePath()
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	contents, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteBytes(path, append(contents, '\n'))
}

func updateCachePath() (string, error) {
	directory, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, updateCacheFile), nil
}

func updateCheckDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CTXHOP_NO_UPDATE_CHECK"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func maybeOfferUpdate(input io.Reader, output, prompt io.Writer) (bool, error) {
	if updateCheckDisabled() || !isInteractiveTerminal(input, output) {
		return false, nil
	}
	current, err := parseUpdateVersion(version)
	if err != nil {
		return false, nil
	}
	if prompt == nil {
		prompt = output
	}
	now := time.Now()
	state, stateErr := loadUpdateCheckState()
	if stateErr != nil {
		state = updateCheckState{}
	}

	var release updateRelease
	if updateCheckStateFresh(state, now) {
		cached, parseErr := parseUpdateVersion(state.LatestVersion)
		if parseErr == nil {
			release = updateRelease{Version: cached.normalized}
		}
	}
	if release.Version == "" {
		ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
		fetched, fetchErr := fetchLatestRelease(ctx, newUpdateHTTPClient(), updateReleaseEndpoint)
		cancel()
		state.CheckedAt = now
		if fetchErr != nil {
			// Cache a successful no-op result so an offline startup does not
			// wait for the network on every invocation. A later day will try
			// the release endpoint again.
			state.LatestVersion = current.normalized
			// Update-check state is advisory; a read-only config directory must
			// not make the normal CLI startup fail.
			_ = saveUpdateCheckState(state)
			return false, nil
		}
		release = fetched
		state.LatestVersion = fetched.Version
		if state.SkippedVersion != fetched.Version {
			state.SkippedVersion = ""
			state.SkipUntil = time.Time{}
		}
		// Update-check state is advisory; failure to persist the cache must not
		// block an otherwise usable CLI startup.
		_ = saveUpdateCheckState(state)
	}

	latest, err := parseUpdateVersion(release.Version)
	if err != nil || compareUpdateVersions(current, latest) >= 0 {
		return false, nil
	}
	if state.SkippedVersion == latest.normalized && now.Before(state.SkipUntil) {
		return false, nil
	}
	answer, err := readInteractiveLine(input, prompt, fmt.Sprintf("\n%s %s is available (current %s). Update now? [y/N] ", cliName, latest.normalized, current.normalized))
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(strings.TrimSpace(answer), "y") && !strings.EqualFold(strings.TrimSpace(answer), "yes") {
		state.SkippedVersion = latest.normalized
		state.SkipUntil = now.Add(updateSkipInterval)
		// A skipped-version cache miss only causes another prompt later; it is
		// not a reason to turn a successful user's choice into an error.
		_ = saveUpdateCheckState(state)
		return false, nil
	}

	if len(release.Assets) == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), updateHTTPTimeout)
		release, err = fetchLatestRelease(ctx, newUpdateHTTPClient(), updateReleaseEndpoint)
		cancel()
		if err != nil {
			return false, fmt.Errorf("update: refresh the latest release: %w", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), updateHTTPTimeout)
	_, err = applyUpdate(ctx, newUpdateHTTPClient(), current, release, output)
	cancel()
	return err == nil, err
}

func updateCheckStateFresh(state updateCheckState, now time.Time) bool {
	return !state.CheckedAt.IsZero() && now.Before(state.CheckedAt.Add(updateCheckInterval)) && strings.TrimSpace(state.LatestVersion) != ""
}

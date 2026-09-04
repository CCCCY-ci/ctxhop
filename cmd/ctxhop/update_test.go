package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseUpdateVersion(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "release tag", input: "v1.2.3", want: "1.2.3"},
		{name: "build metadata", input: "1.2.3+build.7", want: "1.2.3"},
		{name: "pre release", input: "1.2.3-rc.1", want: "1.2.3-rc.1"},
		{name: "short core", input: "2", want: "2.0.0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUpdateVersion(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if got.normalized != tc.want {
				t.Fatalf("normalized = %q, want %q", got.normalized, tc.want)
			}
		})
	}

	for _, input := range []string{"", "dev", "1.02.3", "1.2.3-", "1.2.3-01"} {
		if _, err := parseUpdateVersion(input); err == nil {
			t.Errorf("parseUpdateVersion(%q) accepted an invalid version", input)
		}
	}
}

func TestCompareUpdateVersions(t *testing.T) {
	tests := []struct {
		left  string
		right string
		want  int
	}{
		{left: "1.0.0", right: "1.0.1", want: -1},
		{left: "1.2.0", right: "1.1.9", want: 1},
		{left: "1.0.0-rc.1", right: "1.0.0", want: -1},
		{left: "1.0.0-1", right: "1.0.0-rc.1", want: -1},
		{left: "1.0.0-999999999999999999999", right: "1.0.0-2", want: 1},
		{left: "1.0.0", right: "1.0.0+build.1", want: 0},
	}
	for _, tc := range tests {
		left, err := parseUpdateVersion(tc.left)
		if err != nil {
			t.Fatal(err)
		}
		right, err := parseUpdateVersion(tc.right)
		if err != nil {
			t.Fatal(err)
		}
		got := compareUpdateVersions(left, right)
		if got != tc.want {
			t.Errorf("compareUpdateVersions(%q, %q) = %d, want %d", tc.left, tc.right, got, tc.want)
		}
	}
}

func TestFetchLatestRelease(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/releases/latest" {
			t.Errorf("request path = %q, want /releases/latest", request.URL.Path)
		}
		if got := request.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q", got)
		}
		response := githubReleaseResponse{
			TagName: "v1.4.0",
			HTMLURL: "https://github.com/CCCCY-ci/ctxhop/releases/tag/v1.4.0",
			Assets: []githubReleaseAsset{
				{Name: "checksums.txt", BrowserDownloadURL: "https://github.com/CCCCY-ci/ctxhop/releases/download/v1.4.0/checksums.txt"},
			},
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(response)
	}))
	defer server.Close()

	release, err := fetchLatestRelease(context.Background(), server.Client(), server.URL+"/releases/latest")
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != "1.4.0" || release.Tag != "v1.4.0" || len(release.Assets) != 1 {
		t.Fatalf("release = %+v", release)
	}
}

func TestSelectUpdateAssets(t *testing.T) {
	release := updateRelease{
		Version: "1.4.0",
		Assets: []updateAsset{
			{Name: "checksums.txt", URL: "https://github.com/CCCCY-ci/ctxhop/releases/download/v1.4.0/checksums.txt"},
			{Name: "ctxhop_1.4.0_windows_amd64.zip", URL: "https://github.com/CCCCY-ci/ctxhop/releases/download/v1.4.0/ctxhop_1.4.0_windows_amd64.zip", Size: 42},
		},
	}
	archive, checksums, err := selectUpdateAssets(release, "windows", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if archive.Name != "ctxhop_1.4.0_windows_amd64.zip" || checksums.Name != "checksums.txt" {
		t.Fatalf("archive = %+v, checksums = %+v", archive, checksums)
	}
	if _, _, err := selectUpdateAssets(release, "linux", "arm64"); err == nil {
		t.Fatal("selectUpdateAssets accepted a missing platform package")
	}
}

func TestChecksumForUpdateAsset(t *testing.T) {
	contents := []byte("1234  package.zip\n" + strings.Repeat("a", sha256.Size*2) + " *ctxhop_1.4.0_windows_amd64.zip\n")
	got, err := checksumForUpdateAsset(contents, "ctxhop_1.4.0_windows_amd64.zip")
	if err != nil {
		t.Fatal(err)
	}
	if got != strings.Repeat("a", sha256.Size*2) {
		t.Fatalf("checksum = %q", got)
	}
	if _, err := checksumForUpdateAsset(contents, "missing.zip"); err == nil {
		t.Fatal("checksumForUpdateAsset accepted a missing asset")
	}
}

func TestExtractUpdateBinary(t *testing.T) {
	directory := fixedUpdateTestDirectory(t, "extract")
	archivePath := filepath.Join(directory, "package.zip")
	destination := filepath.Join(directory, updateExecutableName())
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	header := &zip.FileHeader{Name: updateExecutableName(), Method: zip.Store}
	header.SetMode(0o755)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("new executable")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	if err := extractUpdateBinary(archivePath, updateExecutableName(), destination); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "new executable" {
		t.Fatalf("extracted contents = %q", contents)
	}
}

func TestDownloadUpdateFileAndVerifyChecksum(t *testing.T) {
	payload := []byte("release package")
	digest := sha256.Sum256(payload)
	assetName := "ctxhop_1.4.0_windows_amd64.zip"
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/checksums.txt" {
			_, _ = fmt.Fprintf(writer, "%s  %s\n", hex.EncodeToString(digest[:]), assetName)
			return
		}
		_, _ = writer.Write(payload)
	}))
	defer server.Close()

	directory := fixedUpdateTestDirectory(t, "download")
	archivePath := filepath.Join(directory, assetName)
	if err := downloadUpdateFile(context.Background(), server.Client(), server.URL+"/package.zip", archivePath, 1024); err != nil {
		t.Fatal(err)
	}
	checksums := []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(digest[:]), assetName))
	if err := verifyUpdateChecksum(archivePath, checksums, assetName); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateCheckStateFresh(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if !updateCheckStateFresh(updateCheckState{
		CheckedAt:     now.Add(-time.Hour),
		LatestVersion: "1.4.0",
	}, now) {
		t.Fatal("recent update check was not considered fresh")
	}
	if updateCheckStateFresh(updateCheckState{
		CheckedAt:     now.Add(-25 * time.Hour),
		LatestVersion: "1.4.0",
	}, now) {
		t.Fatal("expired update check was considered fresh")
	}
}

func fixedUpdateTestDirectory(t *testing.T, name string) string {
	t.Helper()
	root := `D:\Go\temp`
	directory := filepath.Join(root, "ctxhop-update-tests", name)
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

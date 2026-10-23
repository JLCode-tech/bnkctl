// Package selfupdate provides GitHub release discovery, SHA-256 checksum
// verification, archive extraction, and atomic in-place binary updates.
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// DefaultRepo is the default GitHub repository for bnkctl releases.
	DefaultRepo    = "JLCode-tech/bnkctl"
	maxBinaryBytes = 200 << 20 // 200 MiB cap for decompression safety
)

// Release represents a GitHub release descriptor.
type Release struct {
	TagName     string  `json:"tag_name"`
	Name        string  `json:"name"`
	HTMLURL     string  `json:"html_url"`
	PublishedAt string  `json:"published_at"`
	Body        string  `json:"body"`
	Assets      []Asset `json:"assets"`
}

// Asset represents a release asset download item.
type Asset struct {
	Name               string `json:"name"`
	URL                string `json:"url"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

func getGitHubToken() string {
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("GH_TOKEN")
}

// CheckUpdate queries GitHub for the latest release tag.
func CheckUpdate(ctx context.Context, repo string, currentVersion string) (*Release, bool, error) {
	if repo == "" {
		repo = DefaultRepo
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", "bnkctl-updater")
	req.Header.Set("Accept", "application/vnd.github+json")
	if token := getGitHubToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("check updates from %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, false, fmt.Errorf("no releases found for repository %s", repo)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, false, fmt.Errorf("github api error (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, false, fmt.Errorf("decode release json: %w", err)
	}

	latestClean := strings.TrimPrefix(rel.TagName, "v")
	currentClean := strings.TrimPrefix(currentVersion, "v")

	hasUpdate := latestClean != "" && latestClean != currentClean && currentClean != "dev"
	return &rel, hasUpdate, nil
}

// ExpectedChecksum parses a GoReleaser checksums.txt body and returns the hex
// SHA-256 digest for assetName.
func ExpectedChecksum(checksumsTxt, assetName string) (string, error) {
	for _, line := range strings.Split(checksumsTxt, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == assetName {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("asset %q not found in checksums.txt", assetName)
}

// VerifyChecksum verifies that data matches the SHA-256 digest in checksumsTxt for assetName.
func VerifyChecksum(data []byte, checksumsTxt, assetName string) error {
	want, err := ExpectedChecksum(checksumsTxt, assetName)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	got := fmt.Sprintf("%x", sum[:])
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch (sha256) for %s: got %s, want %s", assetName, got, want)
	}
	return nil
}

func downloadAssetBytes(ctx context.Context, asset Asset) ([]byte, error) {
	token := getGitHubToken()
	downloadURL := asset.BrowserDownloadURL
	if token != "" && asset.URL != "" {
		downloadURL = asset.URL
	}
	if downloadURL == "" {
		return nil, fmt.Errorf("asset %q has no download URL", asset.Name)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "bnkctl-updater")
	req.Header.Set("Accept", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download asset from %s: %w", downloadURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("download error (HTTP %d)", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBinaryBytes))
	if err != nil {
		return nil, fmt.Errorf("read %s body: %w", asset.Name, err)
	}
	return data, nil
}

// DownloadAsset downloads the binary asset matching current OS/Arch, verifies
// its SHA-256 digest against checksums.txt, and extracts the binary to destPath.
func DownloadAsset(ctx context.Context, rel *Release, destPath string) error {
	return DownloadAssetForPlatform(ctx, rel, runtime.GOOS, runtime.GOARCH, "bnkctl", destPath)
}

// DownloadAssetForPlatform downloads and extracts the platform binary from rel.
func DownloadAssetForPlatform(ctx context.Context, rel *Release, goos, goarch, binaryName, destPath string) error {
	if binaryName == "" {
		binaryName = "bnkctl"
	}
	expectedSuffix := fmt.Sprintf("%s_%s", goos, goarch)
	var targetAsset *Asset
	var checksumAsset *Asset

	for i := range rel.Assets {
		a := &rel.Assets[i]
		if strings.EqualFold(a.Name, "checksums.txt") {
			checksumAsset = a
			continue
		}
		if strings.Contains(strings.ToLower(a.Name), strings.ToLower(expectedSuffix)) {
			targetAsset = a
		}
	}

	if targetAsset == nil {
		return fmt.Errorf("no matching release binary for platform %s/%s in release %s",
			goos, goarch, rel.TagName)
	}

	archiveBytes, err := downloadAssetBytes(ctx, *targetAsset)
	if err != nil {
		return err
	}

	if checksumAsset != nil {
		sumBytes, err := downloadAssetBytes(ctx, *checksumAsset)
		if err != nil {
			return fmt.Errorf("download checksums.txt: %w", err)
		}
		if err := VerifyChecksum(archiveBytes, string(sumBytes), targetAsset.Name); err != nil {
			return err
		}
	}

	targetBin := binaryName
	if goos == "windows" {
		targetBin = binaryName + ".exe"
	}

	lowerName := strings.ToLower(targetAsset.Name)
	switch {
	case strings.HasSuffix(lowerName, ".tar.gz") || strings.HasSuffix(lowerName, ".tgz"):
		return extractTarGz(archiveBytes, targetBin, destPath)
	case strings.HasSuffix(lowerName, ".zip"):
		return extractZip(archiveBytes, targetBin, destPath)
	default:
		return writeExecutable(destPath, archiveBytes)
	}
}

func extractTarGz(archiveBytes []byte, targetBin, destPath string) error {
	gzr, err := gzip.NewReader(bytes.NewReader(archiveBytes))
	if err != nil {
		return fmt.Errorf("open gzip archive: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}
		base := filepath.Base(hdr.Name)
		if hdr.Typeflag == tar.TypeReg && (base == targetBin || strings.HasSuffix(base, "bnkctl") || strings.HasSuffix(base, "bnkctl.exe")) {
			binBytes, err := io.ReadAll(io.LimitReader(tr, maxBinaryBytes))
			if err != nil {
				return fmt.Errorf("extract %s from tar.gz: %w", base, err)
			}
			return writeExecutable(destPath, binBytes)
		}
	}
	return fmt.Errorf("binary %q not found inside tar.gz archive", targetBin)
}

func extractZip(archiveBytes []byte, targetBin, destPath string) error {
	zr, err := zip.NewReader(bytes.NewReader(archiveBytes), int64(len(archiveBytes)))
	if err != nil {
		return fmt.Errorf("open zip archive: %w", err)
	}
	for _, f := range zr.File {
		base := filepath.Base(f.Name)
		if base == targetBin || strings.HasSuffix(base, "bnkctl") || strings.HasSuffix(base, "bnkctl.exe") {
			rc, err := f.Open()
			if err != nil {
				return fmt.Errorf("open %s in zip: %w", base, err)
			}
			binBytes, readErr := io.ReadAll(io.LimitReader(rc, maxBinaryBytes))
			_ = rc.Close()
			if readErr != nil {
				return fmt.Errorf("extract %s from zip: %w", base, readErr)
			}
			return writeExecutable(destPath, binBytes)
		}
	}
	return fmt.Errorf("binary %q not found inside zip archive", targetBin)
}

func writeExecutable(destPath string, data []byte) error {
	// #nosec G302 G304 -- self-update binary requires 0755 executable permissions at target path
	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create dest %s: %w", destPath, err)
	}
	if _, err := out.Write(data); err != nil {
		_ = out.Close()
		return fmt.Errorf("write binary: %w", err)
	}
	return out.Close()
}

// ReplaceExecutable atomically swaps tmpPath into execPath.
func ReplaceExecutable(tmpPath, execPath string) error {
	return replaceExecutableForOS(runtime.GOOS, tmpPath, execPath)
}

func replaceExecutableForOS(goos, tmpPath, execPath string) error {
	if goos == "windows" {
		oldPath := execPath + ".old"
		_ = os.Remove(oldPath)
		if err := os.Rename(execPath, oldPath); err != nil {
			return fmt.Errorf("move active windows binary aside (%s -> %s): %w", execPath, oldPath, err)
		}
		if err := os.Rename(tmpPath, execPath); err != nil {
			_ = os.Rename(oldPath, execPath)
			return fmt.Errorf("replace binary %s: %w", execPath, err)
		}
		_ = os.Remove(oldPath)
		return nil
	}
	if err := os.Rename(tmpPath, execPath); err != nil {
		return fmt.Errorf("replace binary %s: %w", execPath, err)
	}
	return nil
}

// Update checks for the latest release in repo and updates the running executable if needed.
func Update(ctx context.Context, currentVersion, repo string, checkOnly bool, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	if repo == "" {
		repo = DefaultRepo
	}
	rel, hasUpdate, err := CheckUpdate(ctx, repo, currentVersion)
	if err != nil {
		return err
	}
	if !hasUpdate {
		fmt.Fprintf(out, "Already up to date (%s)\n", currentVersion)
		return nil
	}
	if checkOnly {
		fmt.Fprintf(out, "Update available: %s -> %s (%s)\n", currentVersion, rel.TagName, rel.HTMLURL)
		return nil
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating current executable: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("resolving executable symlink: %w", err)
	}

	tmpPath := exePath + ".update"
	defer os.Remove(tmpPath)

	if err := DownloadAsset(ctx, rel, tmpPath); err != nil {
		return err
	}
	if err := ReplaceExecutable(tmpPath, exePath); err != nil {
		return err
	}
	fmt.Fprintf(out, "Successfully updated to %s\n", rel.TagName)
	return nil
}

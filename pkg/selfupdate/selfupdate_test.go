package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckUpdate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := Release{
			TagName: "v1.2.0",
			Name:    "Release v1.2.0",
			HTMLURL: "https://github.com/JLCode-tech/gkebnkctl/releases/tag/v1.2.0",
		}
		_ = json.NewEncoder(w).Encode(rel)
	}))
	defer srv.Close()

	ctx := context.Background()
	// test parsing with direct request
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v1.2.0" {
		t.Errorf("expected tag v1.2.0, got %s", rel.TagName)
	}
}

func TestDownloadAssetChecksumVerification(t *testing.T) {
	assetName := fmt.Sprintf("gkebnkctl_%s_%s", runtime.GOOS, runtime.GOARCH)
	payload := []byte("fake-binary-payload")
	sum := sha256.Sum256(payload)
	validHash := hex.EncodeToString(sum[:])

	var checksumLine string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + assetName:
			_, _ = w.Write(payload)
		case "/checksums.txt":
			_, _ = w.Write([]byte(checksumLine))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	rel := &Release{
		TagName: "v1.2.0",
		Assets: []Asset{
			{
				Name:               assetName,
				BrowserDownloadURL: srv.URL + "/" + assetName,
			},
			{
				Name:               "checksums.txt",
				BrowserDownloadURL: srv.URL + "/checksums.txt",
			},
		},
	}

	tmpDir := t.TempDir()
	destPath := filepath.Join(tmpDir, "bnkctl")

	// 1. Valid checksum succeeds
	checksumLine = fmt.Sprintf("%s  %s\n", validHash, assetName)
	if err := DownloadAsset(context.Background(), rel, destPath); err != nil {
		t.Fatalf("expected valid checksum download to succeed, got: %v", err)
	}
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("reading downloaded binary: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("unexpected binary content: %q", string(got))
	}

	// 2. Mismatched checksum fails
	checksumLine = fmt.Sprintf("0000000000000000000000000000000000000000000000000000000000000000  %s\n", assetName)
	badDest := filepath.Join(tmpDir, "gkebnkctl-bad")
	err = DownloadAsset(context.Background(), rel, badDest)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch error, got: %v", err)
	}
	if _, statErr := os.Stat(badDest); !os.IsNotExist(statErr) {
		t.Fatalf("expected bad destination file not to exist on checksum mismatch")
	}
}

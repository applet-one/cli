package dev

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

// WorkerdVersion is the workerd release used by applet dev.
const WorkerdVersion = "1.20261019.1"

// workerdAssets maps GOOS/GOARCH to the release asset and the SHA-256 of its
// .gz file, as published by github.com/cloudflare/workerd.
var workerdAssets = map[string]struct{ name, sha256 string }{
	"darwin/amd64":  {"workerd-darwin-64.gz", "2ad5f99a596d26fb3b16136834e7b469e398dba5fd7d6d433e645cb60ed15e40"},
	"darwin/arm64":  {"workerd-darwin-arm64.gz", "e30d7f798d4f6450db0f8ce6044b73c05c5f4108b03373ab2d06b7240b8e4322"},
	"linux/amd64":   {"workerd-linux-64.gz", "ddf34551fb6283cf1020f6f417026d386e660fee7573fbfc18e989282e2d848a"},
	"linux/arm64":   {"workerd-linux-arm64.gz", "16db7cfad660cd0954b9649b1e5d8682617af78a720e0c64e2b9a3631e09fc41"},
	"windows/amd64": {"workerd-windows-64.gz", "c4f0f6990dd4bf2abd194e4ff0c62e3ee66e15050072d381c0071af534047f57"},
}

// WorkerdPath returns a workerd binary, downloading and verifying the pinned
// release into the user cache on first use. APPLET_WORKERD overrides it.
func WorkerdPath(ctx context.Context, progress io.Writer) (string, error) {
	if path := os.Getenv("APPLET_WORKERD"); path != "" {
		return path, nil
	}
	platform := runtime.GOOS + "/" + runtime.GOARCH
	asset, ok := workerdAssets[platform]
	if !ok {
		return "", fmt.Errorf("applet dev does not support %s yet. Set APPLET_WORKERD to the path of a workerd binary.", platform)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	name := "workerd"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(cache, "applet-one", "workerd", WorkerdVersion, name)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	fmt.Fprintf(progress, "Downloading the local Workers runtime (workerd %s)...\n", WorkerdVersion)
	if err := download(ctx, asset.name, asset.sha256, path); err != nil {
		return "", fmt.Errorf("Could not install the local Workers runtime: %w", err)
	}
	return path, nil
}

func download(ctx context.Context, asset, digest, destination string) error {
	url := fmt.Sprintf("https://github.com/cloudflare/workerd/releases/download/v%s/%s", WorkerdVersion, asset)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download of %s failed (HTTP %d)", url, response.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	compressed, err := os.CreateTemp(filepath.Dir(destination), asset+".*")
	if err != nil {
		return err
	}
	defer os.Remove(compressed.Name())
	defer compressed.Close()
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(compressed, hash), response.Body); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != digest {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", asset, got, digest)
	}
	if _, err := compressed.Seek(0, io.SeekStart); err != nil {
		return err
	}
	reader, err := gzip.NewReader(compressed)
	if err != nil {
		return err
	}
	binary, err := os.CreateTemp(filepath.Dir(destination), "workerd.*")
	if err != nil {
		return err
	}
	defer os.Remove(binary.Name())
	if _, err := io.Copy(binary, reader); err != nil {
		binary.Close()
		return err
	}
	if err := binary.Close(); err != nil {
		return err
	}
	if err := os.Chmod(binary.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(binary.Name(), destination)
}

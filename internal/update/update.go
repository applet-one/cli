// Package update performs a best-effort check for newer CLI releases.
package update

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ReleasesURL is where users download new versions.
const ReleasesURL = "https://github.com/applet-one/cli/releases/latest"

const (
	latestURL = "https://api.github.com/repos/applet-one/cli/releases/latest"
	cacheTTL  = 24 * time.Hour
)

type cache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
}

// Check starts a check and returns a function that waits until deadline for
// the result and returns the newer version, or "". It never fails: an update
// check must not prevent the CLI from running offline.
func Check(ctx context.Context, current string, deadline time.Time) func() string {
	if Compare(current, "0.0.0") < 0 {
		return func() string { return "" }
	}
	cachePath := ""
	if dir, err := os.UserCacheDir(); err == nil {
		cachePath = filepath.Join(dir, "applet-one", "update-check.json")
		var cached cache
		if data, err := os.ReadFile(cachePath); err == nil && json.Unmarshal(data, &cached) == nil && time.Since(cached.CheckedAt) < cacheTTL {
			return func() string { return newer(cached.Latest, current) }
		}
	}
	result := make(chan string, 1)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	go func() {
		defer cancel()
		latest := fetchLatest(ctx)
		if latest != "" && cachePath != "" {
			if data, err := json.Marshal(cache{CheckedAt: time.Now(), Latest: latest}); err == nil {
				_ = os.MkdirAll(filepath.Dir(cachePath), 0o755)
				_ = os.WriteFile(cachePath, data, 0o644)
			}
		}
		result <- latest
	}()
	return func() string {
		select {
		case latest := <-result:
			return newer(latest, current)
		case <-ctx.Done():
			return ""
		}
	}
}

func fetchLatest(ctx context.Context) string {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return ""
	}
	request.Header.Set("accept", "application/vnd.github+json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return ""
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ""
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if json.NewDecoder(response.Body).Decode(&release) != nil {
		return ""
	}
	return strings.TrimPrefix(release.TagName, "v")
}

func newer(latest, current string) string {
	if latest != "" && Compare(latest, current) > 0 {
		return latest
	}
	return ""
}

var semverPattern = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

// Compare compares semantic versions, returning -1, 0 or 1. Unparseable
// versions compare as -1 against anything parseable, and 0 otherwise.
func Compare(left, right string) int {
	a, b := semverPattern.FindStringSubmatch(left), semverPattern.FindStringSubmatch(right)
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(a[i])
		y, _ := strconv.Atoi(b[i])
		if x != y {
			return sign(x - y)
		}
	}
	if a[4] == "" || b[4] == "" {
		switch {
		case a[4] == b[4]:
			return 0
		case a[4] == "":
			return 1
		default:
			return -1
		}
	}
	ap, bp := strings.Split(a[4], "."), strings.Split(b[4], ".")
	for i := 0; i < max(len(ap), len(bp)); i++ {
		if i >= len(ap) {
			return -1
		}
		if i >= len(bp) {
			return 1
		}
		if ap[i] == bp[i] {
			continue
		}
		x, xErr := strconv.Atoi(ap[i])
		y, yErr := strconv.Atoi(bp[i])
		switch {
		case xErr == nil && yErr == nil:
			return sign(x - y)
		case xErr == nil:
			return -1
		case yErr == nil:
			return 1
		case ap[i] < bp[i]:
			return -1
		default:
			return 1
		}
	}
	return 0
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

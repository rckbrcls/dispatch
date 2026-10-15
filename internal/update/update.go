// Package update replaces the running binary with the latest GitHub release.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Source locates releases. Zero values use rckbrcls/dispatch on GitHub.
type Source struct {
	Repo string // owner/name
	// APIURL overrides the latest-release endpoint (tests, mirrors).
	APIURL string
	// BaseURL overrides the asset directory; {tag} is replaced by the tag.
	BaseURL string
	Client  *http.Client
}

func (s Source) repo() string {
	if s.Repo != "" {
		return s.Repo
	}
	return "rckbrcls/dispatch"
}

func (s Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

func (s Source) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// Latest returns the tag of the newest release.
func (s Source) Latest(ctx context.Context) (string, error) {
	url := s.APIURL
	if url == "" {
		url = "https://api.github.com/repos/" + s.repo() + "/releases/latest"
	}
	b, err := s.get(ctx, url)
	if err != nil {
		return "", err
	}
	var r struct {
		Tag string `json:"tag_name"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.Tag == "" {
		return "", fmt.Errorf("no tag_name in release response")
	}
	return r.Tag, nil
}

// AssetName is the release asset for this platform.
func AssetName() string {
	return fmt.Sprintf("dispatch_%s_%s", runtime.GOOS, runtime.GOARCH)
}

// Fetch downloads the binary for tag and verifies it against checksums.txt.
func (s Source) Fetch(ctx context.Context, tag, name string) ([]byte, error) {
	base := s.BaseURL
	if base == "" {
		base = "https://github.com/" + s.repo() + "/releases/download/{tag}"
	}
	base = strings.ReplaceAll(base, "{tag}", tag)
	sums, err := s.get(ctx, base+"/checksums.txt")
	if err != nil {
		return nil, err
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			want = f[0]
		}
	}
	if want == "" {
		return nil, fmt.Errorf("checksum for %s not found in release %s", name, tag)
	}
	bin, err := s.get(ctx, base+"/"+name)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(bin)
	if hex.EncodeToString(sum[:]) != want {
		return nil, fmt.Errorf("checksum mismatch for %s", name)
	}
	return bin, nil
}

// Replace atomically swaps the file at path for bin (rename in same dir).
func Replace(path string, bin []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".dispatch-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

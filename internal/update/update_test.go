package update

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func server(t *testing.T, sum string) *httptest.Server {
	bin := []byte("new-binary")
	if sum == "" {
		h := sha256.Sum256(bin)
		sum = hex.EncodeToString(h[:])
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"tag_name":"v9.9.9"}`) })
	mux.HandleFunc("/v9.9.9/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", sum, AssetName())
	})
	mux.HandleFunc("/v9.9.9/"+AssetName(), func(w http.ResponseWriter, r *http.Request) { w.Write(bin) })
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestLatestFetchReplace(t *testing.T) {
	srv := server(t, "")
	src := Source{APIURL: srv.URL + "/latest", BaseURL: srv.URL + "/{tag}"}
	tag, err := src.Latest(t.Context())
	if err != nil || tag != "v9.9.9" {
		t.Fatalf("latest = %q, %v", tag, err)
	}
	bin, err := src.Fetch(t.Context(), tag, AssetName())
	if err != nil || string(bin) != "new-binary" {
		t.Fatalf("fetch = %q, %v", bin, err)
	}
	path := filepath.Join(t.TempDir(), "dispatch")
	os.WriteFile(path, []byte("old"), 0o755)
	if err := Replace(path, bin); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new-binary" {
		t.Fatalf("replaced = %q", got)
	}
}

func TestFetchRejectsBadChecksum(t *testing.T) {
	srv := server(t, "deadbeef")
	src := Source{BaseURL: srv.URL + "/{tag}"}
	if _, err := src.Fetch(t.Context(), "v9.9.9", AssetName()); err == nil {
		t.Fatal("expected checksum mismatch")
	}
}

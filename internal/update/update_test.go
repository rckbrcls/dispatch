package update

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// handlerTransport exercises HTTP request/response handling without a socket.
type handlerTransport struct{ handler http.Handler }

func (tr handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	tr.handler.ServeHTTP(recorder, req)
	return recorder.Result(), nil
}

type testServer struct {
	URL    string
	Client *http.Client
}

func server(t *testing.T, sum string) testServer {
	t.Helper()
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
	return testServer{URL: "https://releases.test", Client: &http.Client{Transport: handlerTransport{handler: mux}}}
}

func TestLatestFetchReplace(t *testing.T) {
	srv := server(t, "")
	src := Source{Client: srv.Client, APIURL: srv.URL + "/latest", BaseURL: srv.URL + "/{tag}"}
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
	src := Source{Client: srv.Client, BaseURL: srv.URL + "/{tag}"}
	if _, err := src.Fetch(t.Context(), "v9.9.9", AssetName()); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
}

package conf

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNodeConfig_IncludeFromHTTP_URL is the C15 remote-Include
// regression. Pre-fix the http.Get path was double-broken:
// strings.CutPrefix(s, ":") returned s unchanged for a real URL, so
// the http/https switch never matched, and even when it would have it
// passed the SCHEME literal to http.Get instead of the URL. Net result:
// remote Include silently fell through to os.Open which failed.
func TestNodeConfig_IncludeFromHTTP_URL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"ApiHost": "https://panel.example.com",
			"NodeID": 42,
			"ApiKey": "secret",
			"NodeType": "vless"
		}`))
	}))
	defer srv.Close()

	wrapper := []byte(`{"Include": "` + srv.URL + `"}`)
	var n NodeConfig
	if err := json.Unmarshal(wrapper, &n); err != nil {
		t.Fatalf("unmarshal with HTTP Include: %v", err)
	}
	if n.ApiConfig.APIHost != "https://panel.example.com" {
		t.Errorf("APIHost = %q, want %q", n.ApiConfig.APIHost, "https://panel.example.com")
	}
	if n.ApiConfig.NodeID != 42 {
		t.Errorf("NodeID = %d, want 42", n.ApiConfig.NodeID)
	}
	if n.ApiConfig.NodeType != "vless" {
		t.Errorf("NodeType = %q, want %q", n.ApiConfig.NodeType, "vless")
	}
}

// TestNodeConfig_IncludeFromHTTPS_URL — same as above but verifies
// the prefix detection works for "https://" scheme too. We can't
// stand up a real TLS server in this test without certificate setup,
// so we just verify the URL-detection branch is taken (would fail
// with "open ... no such file" if the path branch fires by mistake).
func TestNodeConfig_IncludeFromHTTPS_URL(t *testing.T) {
	wrapper := []byte(`{"Include": "https://localhost:1/does-not-exist"}`)
	var n NodeConfig
	err := json.Unmarshal(wrapper, &n)
	if err == nil {
		t.Fatal("expected an error (server unreachable)")
	}
	// The error must be from the HTTP fetch path, NOT from os.Open.
	// Pre-fix the bug fell through to os.Open, which would say
	// "no such file or directory". Post-fix we get a connection
	// refused / dial error from http.Get.
	if strings.Contains(err.Error(), "no such file") {
		t.Fatalf("https URL leaked into os.Open path; got: %v", err)
	}
	if !strings.Contains(err.Error(), "fetch include URL") {
		t.Errorf("expected fetch-include error context, got: %v", err)
	}
}

// TestNodeConfig_IncludeFromFile is the back-compat case: local files
// continue to work after the rewrite.
func TestNodeConfig_IncludeFromFile(t *testing.T) {
	dir := t.TempDir()
	includePath := filepath.Join(dir, "node.json")
	body := []byte(`{
		"ApiHost": "http://127.0.0.1:8080",
		"NodeID": 7,
		"ApiKey": "k",
		"NodeType": "trojan"
	}`)
	if err := os.WriteFile(includePath, body, 0644); err != nil {
		t.Fatal(err)
	}
	wrapper := []byte(`{"Include": "` + filepath.ToSlash(includePath) + `"}`)
	var n NodeConfig
	if err := json.Unmarshal(wrapper, &n); err != nil {
		t.Fatalf("unmarshal with file Include: %v", err)
	}
	if n.ApiConfig.NodeID != 7 || n.ApiConfig.NodeType != "trojan" {
		t.Errorf("file Include did not propagate: %+v", n.ApiConfig)
	}
}

// TestNodeConfig_IncludeHTTP404IsCleanError — pre-fix the body of a
// 4xx/5xx response would have been read and json-unmarshaled, silently
// corrupting the config with whatever HTML error page the server
// returned. Post-fix we surface the status as a typed error.
func TestNodeConfig_IncludeHTTP404IsCleanError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer srv.Close()

	wrapper := []byte(`{"Include": "` + srv.URL + `"}`)
	var n NodeConfig
	err := json.Unmarshal(wrapper, &n)
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("expected HTTP 404 in error, got: %v", err)
	}
}

// TestNodeConfig_IncludeEmptyIsNoOp — when Include is empty, the
// unmarshal proceeds with the original wrapper data. Locks the
// no-Include path against a regression that would always trigger
// the include branch.
func TestNodeConfig_IncludeEmptyIsNoOp(t *testing.T) {
	wrapper := []byte(`{
		"ApiConfig": {"ApiHost": "http://inline", "NodeID": 1, "ApiKey": "x", "NodeType": "vless"}
	}`)
	var n NodeConfig
	if err := json.Unmarshal(wrapper, &n); err != nil {
		t.Fatalf("unmarshal without Include: %v", err)
	}
	if n.ApiConfig.APIHost != "http://inline" || n.ApiConfig.NodeID != 1 {
		t.Errorf("inline-only config did not parse: %+v", n.ApiConfig)
	}
}

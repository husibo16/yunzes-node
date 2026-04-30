package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-resty/resty/v2"
)

// TestReportNodeStatus_IncludesUptimeInBody is the C11 module 6
// regression: the request struct used to carry only cpu/mem/disk/
// updated_at, so the Uptime field on NodeStatus was set by the caller
// and silently dropped during JSON marshal. This test stands up an
// httptest server, captures the body, and asserts uptime made it
// through.
func TestReportNodeStatus_IncludesUptimeInBody(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	client := &Client{
		Client: resty.New(),
	}
	client.Client.SetBaseURL(srv.URL)

	if err := client.ReportNodeStatus(&NodeStatus{
		CPU:    12.5,
		Mem:    34.0,
		Disk:   56.0,
		Uptime: 4242,
	}); err != nil {
		t.Fatalf("ReportNodeStatus: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(capturedBody, &got); err != nil {
		t.Fatalf("server received non-JSON body %q: %v", string(capturedBody), err)
	}

	uptime, ok := got["uptime"]
	if !ok {
		t.Fatalf("body missing \"uptime\" field: %s", string(capturedBody))
	}
	// json.Unmarshal decodes JSON numbers into float64 by default.
	if v, _ := uptime.(float64); v != 4242 {
		t.Fatalf("uptime = %v, want 4242", uptime)
	}

	// Sanity: cpu/mem/disk/updated_at still there too.
	for _, k := range []string{"cpu", "mem", "disk", "updated_at"} {
		if _, ok := got[k]; !ok {
			t.Errorf("body missing %q: %s", k, string(capturedBody))
		}
	}
}

// TestReportNodeStatus_ZeroUptimeStillReported — when Uptime is zero
// (e.g. gopsutil failed to read host uptime and we fell through), the
// field is still present in the JSON. This matches the existing
// shape for cpu/mem/disk and avoids "missing field" surprises on the
// server side.
func TestReportNodeStatus_ZeroUptimeStillReported(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &Client{Client: resty.New()}
	client.Client.SetBaseURL(srv.URL)

	if err := client.ReportNodeStatus(&NodeStatus{Uptime: 0}); err != nil {
		t.Fatalf("ReportNodeStatus: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(capturedBody, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got["uptime"]; !ok {
		t.Fatalf("uptime field missing on zero value: %s", string(capturedBody))
	}
}

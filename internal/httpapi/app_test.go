package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func getLive(t *testing.T, mutate func(*http.Request)) *http.Response {
	t.Helper()
	app := newTestApp(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	if mutate != nil {
		mutate(req)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test() error = %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestHealthLive_Returns200(t *testing.T) {
	resp := getLive(t, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestHealthLive_JSONBody(t *testing.T) {
	resp := getLive(t, nil)

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("body %q is not valid JSON: %v", raw, err)
	}
	if len(got) != 1 || got["status"] != "ok" {
		t.Errorf("body = %s, want {\"status\":\"ok\"}", raw)
	}
}

func TestHealthLive_NoAuthenticationRequired(t *testing.T) {
	// Nenhum header Authorization é enviado; o endpoint público deve responder 200.
	resp := getLive(t, func(r *http.Request) { r.Header.Del("Authorization") })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 without credentials", resp.StatusCode)
	}
}

func TestUnknownRoute_Returns404(t *testing.T) {
	app := newTestApp(t, nil)
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/nope", nil))
	if err != nil {
		t.Fatalf("app.Test() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// server_test.go HTTP 端点安全行为测试（无网络依赖）。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"work2api-desktop/internal/auth"
	"work2api-desktop/internal/pool"
	"work2api-desktop/internal/upstream/workbuddy"
)

func newTestHandler(t *testing.T, apiKey string) *Handler {
	t.Helper()
	store, err := auth.LoadStore(t.TempDir() + "/accounts.dat")
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	return New(Deps{
		Pool:  pool.New(),
		Store: store,
		WB:    workbuddy.New(func(string, ...any) {}),
		APIKey: func() string {
			return apiKey
		},
		DefaultProvider: func() string { return "auto" },
		Logf:            func(string, ...any) {},
	})
}

func TestHealthzNoAuth(t *testing.T) {
	h := newTestHandler(t, "k1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", rec.Code)
	}
}

func TestRejectMissingAndWrongKey(t *testing.T) {
	h := newTestHandler(t, "k1")

	h0 := newTestHandler(t, "")
	gets := []string{"/v1/models"}
	for _, path := range gets {
		rec := httptest.NewRecorder()
		h0.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("empty-key GET %s = %d, want 503", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h0.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty-key POST chat = %d, want 503", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r2 := httptest.NewRecorder()
	h.ServeHTTP(r2, req)
	if r2.Code != http.StatusUnauthorized {
		t.Fatalf("no key = %d, want 401", r2.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	r2 = httptest.NewRecorder()
	h.ServeHTTP(r2, req)
	if r2.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key = %d, want 401", r2.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer k1")
	r2 = httptest.NewRecorder()
	h.ServeHTTP(r2, req)
	if r2.Code != http.StatusOK {
		t.Fatalf("right key = %d, want 200 (body=%s)", r2.Code, r2.Body.String())
	}
}

func TestChatCompletionsNoAccounts(t *testing.T) {
	h := newTestHandler(t, "k1")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer k1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway && rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusTooManyRequests {
		t.Fatalf("no accounts = %d, want 502/503/429", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body not json: %s", rec.Body.String())
	}
}

func TestBodySizeLimit(t *testing.T) {
	h := newTestHandler(t, "k1")
	big := strings.Repeat("x", maxBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(big))
	req.Header.Set("Authorization", "Bearer k1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413", rec.Code)
	}
}
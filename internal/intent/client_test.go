package intent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const responseSpec = `{"api_version":"envctl/v1alpha1","name":"dev","services":[{"name":"cache","template":"redis","version":"7","host_port":0,"persistent":false}]}`

func TestGenerate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer key" {
			t.Error("invalid request")
		}
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "messages") || !strings.Contains(string(b), "model") {
			t.Error("missing payload")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": responseSpec}, "finish_reason": "stop"}}})
	}))
	defer server.Close()
	c := Client{Endpoint: server.URL, APIKey: "key", Model: "test"}
	s, err := c.Generate(context.Background(), "Redis")
	if err != nil || s.Name != "dev" {
		t.Fatal(s, err)
	}
}
func TestResponseFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", 401, "secret server detail"}, {"bad envelope", 200, "not json"}, {"missing", 200, `{"choices":[]}`},
		{"truncated", 200, `{"choices":[{"finish_reason":"length","message":{"content":"{}"}}]}`},
		{"bad content", 200, `{"choices":[{"finish_reason":"stop","message":{"content":"hello"}}]}`},
		{"refusal", 200, `{"choices":[{"finish_reason":"stop","message":{"content":"{\"error\":\"unsupported\"}"}}]}`},
		{"oversized", 200, strings.Repeat("x", 65537)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := (Client{Endpoint: srv.URL, Model: "test"}).Generate(context.Background(), "redis")
			if err == nil || strings.Contains(err.Error(), "secret server detail") {
				t.Fatal(err)
			}
		})
	}
}
func TestEndpointPolicyAndCancel(t *testing.T) {
	for _, endpoint := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com?key=secret", "bad"} {
		if _, err := (Client{Endpoint: endpoint, Model: "m"}).Generate(context.Background(), "x"); err == nil {
			t.Fatal(endpoint)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Client{Endpoint: "http://127.0.0.1:1", Model: "m", HTTP: &http.Client{Timeout: time.Second}}).Generate(ctx, "redis"); err == nil {
		t.Fatal("cancellation ignored")
	}
}
func TestRedirectDoesNotForwardCredentials(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer source.Close()
	if _, err := (Client{Endpoint: source.URL, Model: "m", APIKey: "secret"}).Generate(context.Background(), "redis"); err == nil || called {
		t.Fatal("redirect followed")
	}
}

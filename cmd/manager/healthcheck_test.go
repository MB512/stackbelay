package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestHealthURL(t *testing.T) {
	tests := []struct {
		listen, want string
		wantErr      bool
	}{
		{"", "http://127.0.0.1:8080/healthz", false},
		{":8080", "http://127.0.0.1:8080/healthz", false},
		{":9000", "http://127.0.0.1:9000/healthz", false},
		{"0.0.0.0:8081", "http://127.0.0.1:8081/healthz", false},
		{"[::]:8082", "http://127.0.0.1:8082/healthz", false},
		{"127.0.0.1:9001", "http://127.0.0.1:9001/healthz", false},
		{"10.1.2.3:9002", "http://10.1.2.3:9002/healthz", false},
		{"8080", "", true},
	}
	for _, tt := range tests {
		got, err := healthURL(tt.listen)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("healthURL(%q) = %q, %v; want %q (error: %v)", tt.listen, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestHealthcheck(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	getenv := func(k string) string {
		if k == "STACKVAULT_LISTEN" {
			return u.Host
		}
		return ""
	}

	var stderr bytes.Buffer
	if code := healthcheck(context.Background(), getenv, &stderr); code != 0 {
		t.Fatalf("healthy server: exit code %d (%s)", code, stderr.String())
	}
	status = http.StatusServiceUnavailable
	if code := healthcheck(context.Background(), getenv, &stderr); code != 1 {
		t.Errorf("unhealthy server: exit code %d, want 1", code)
	}
	srv.Close()
	if code := healthcheck(context.Background(), getenv, &stderr); code != 1 {
		t.Errorf("server down: exit code %d, want 1", code)
	}
	if code := healthcheck(context.Background(), func(string) string { return "garbage" }, &stderr); code != 1 {
		t.Errorf("invalid listen address: exit code %d, want 1", code)
	}
}

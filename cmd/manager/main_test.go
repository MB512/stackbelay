package main

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe for concurrent writes (the server logs
// from other goroutines while the test reads).
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func noFiles(string) ([]byte, error) { return nil, fs.ErrNotExist }

func TestRunServesAndShutsDown(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	env := map[string]string{
		"STACKS_ROOT":           root,
		"STACKBELAY_DATA_DIR":   data,
		"STACKBELAY_HOSTNAME":   "host-a",
		"STACKBELAY_SECRET_KEY": "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
		"STACKBELAY_LISTEN":     "127.0.0.1:0", // any free port
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrCh := make(chan string, 1)
	codeCh := make(chan int, 1)
	var logs syncBuffer
	go func() {
		codeCh <- run(ctx, func(k string) string { return env[k] }, noFiles, &logs, func(a string) { addrCh <- a })
	}()

	var addr string
	select {
	case addr = <-addrCh:
	case code := <-codeCh:
		t.Fatalf("manager exited early with %d: %s", code, logs.String())
	case <-time.After(10 * time.Second):
		t.Fatal("manager did not start")
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Fatalf("healthz: %d %q", resp.StatusCode, body)
	}

	cancel() // like SIGTERM
	select {
	case code := <-codeCh:
		if code != 0 {
			t.Fatalf("exit code %d: %s", code, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("manager did not shut down")
	}

	out := logs.String()
	if !strings.Contains(out, `"stacks":["web"]`) || !strings.Contains(out, "shutting down") ||
		!strings.Contains(out, `INITIAL ADMIN PASSWORD (user "admin")`) {
		t.Fatalf("unexpected log:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(data, "stackbelay.db")); err != nil {
		t.Fatalf("database not created: %v", err)
	}
}

func TestRunConfigError(t *testing.T) {
	var out bytes.Buffer
	code := run(context.Background(), func(string) string { return "" }, noFiles, &out, nil)
	if code != 2 || !strings.Contains(out.String(), "STACKS_ROOT") {
		t.Fatalf("code %d, output %q", code, out.String())
	}
}

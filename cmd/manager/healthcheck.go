package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// healthcheckTimeout stays below the HEALTHCHECK --timeout in the Dockerfile.
const healthcheckTimeout = 4 * time.Second

// healthURL returns the /healthz URL of a manager that listens on listen
// (the value of STACKVAULT_LISTEN, default ":8080"). A wildcard or empty host
// is replaced by the loopback address: the check runs inside the container.
func healthURL(listen string) (string, error) {
	if listen == "" {
		listen = ":8080"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("invalid STACKVAULT_LISTEN %q: %w", listen, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

// healthcheck implements "manager healthcheck", the command behind the
// image's HEALTHCHECK: GET /healthz on the local server. It returns the
// process exit code (0 healthy, 1 unhealthy). Like a small console tool in
// C# that calls HttpClient.GetAsync and returns the result as its exit code.
func healthcheck(ctx context.Context, getenv func(string) string, stderr io.Writer) int {
	url, err := healthURL(getenv("STACKVAULT_LISTEN"))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "unhealthy: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "unhealthy: %s returned %s\n", url, resp.Status)
		return 1
	}
	return 0
}

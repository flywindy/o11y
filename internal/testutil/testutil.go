// Package testutil provides helpers shared across the o11y test suites.
// It lives under internal/ so it cannot be imported from outside this module
// even though the source file omits the _test.go suffix (a separate package
// is required so test files in different packages can share it).
//
// Helpers here are deliberately minimal — domain-specific fixtures stay in
// their owning test files; this package only collects fixtures that are
// duplicated across two or more packages.
package testutil

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// FakeOTLPServer returns an httptest.Server that accepts any OTLP/HTTP
// request with a 200 OK. The server is closed automatically via t.Cleanup.
//
// Use it whenever a test needs to satisfy the SDK's OTLP exporter without
// actually running an OTel Collector.
func FakeOTLPServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// CapturingOTLPServer is a fake OTLP/HTTP server that records every request
// it receives. It is returned with helper methods for asserting on captured
// state. Used by tests that need to verify outgoing OTLP headers, paths,
// or body sizes.
type CapturingOTLPServer struct {
	*httptest.Server

	mu       sync.Mutex
	requests []CapturedRequest
}

// CapturedRequest is the subset of an inbound request we retain for assertions.
// We deliberately drop the body to avoid retaining large payloads.
type CapturedRequest struct {
	Path   string
	Method string
	Header http.Header
}

// NewCapturingOTLPServer starts a fake OTLP/HTTP server that records each
// inbound request. Auto-closed via t.Cleanup.
func NewCapturingOTLPServer(t *testing.T) *CapturingOTLPServer {
	t.Helper()
	c := &CapturingOTLPServer{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.requests = append(c.requests, CapturedRequest{
			Path:   r.URL.Path,
			Method: r.Method,
			Header: r.Header.Clone(),
		})
		c.mu.Unlock()
		// Drain the body so the client's connection can be reused, but do
		// not retain it.
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.Close)
	return c
}

// Requests returns a snapshot of every request the server has received so
// far. The returned slice is a copy and is safe to retain.
func (c *CapturingOTLPServer) Requests() []CapturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CapturedRequest, len(c.requests))
	copy(out, c.requests)
	return out
}

// FreeAddr returns "127.0.0.1:<port>" where <port> was bindable when this
// function was called. The listener is closed before return so the caller
// may reuse the addr immediately. There is an inherent TOCTOU race that is
// fine for short-lived test fixtures and avoids the constant-port collisions
// that plague parallel `go test` runs.
func FreeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

// ScrapeMetrics issues a GET against http://addr/metrics and returns the body
// as a string. Test fails on any I/O error. The supplied parent context is
// honoured for cancellation; an additional 1-second deadline is applied per
// request so a hung listener cannot freeze the test.
//
// For use inside assert.Eventually / require.Eventually polling loops, use
// TryScrapeMetrics instead — it returns an error rather than calling
// t.FailNow, so transient scrape failures do not abort the whole test.
func ScrapeMetrics(ctx context.Context, t *testing.T, addr string) string {
	t.Helper()
	body, err := TryScrapeMetrics(ctx, addr)
	require.NoError(t, err)
	return body
}

// TryScrapeMetrics issues a GET against http://addr/metrics and returns the
// body and any I/O error. Unlike ScrapeMetrics, it never calls t.FailNow,
// so it is safe to use inside assert.Eventually / require.Eventually
// callbacks where transient failures should drive a retry rather than a
// test abort.
//
// A non-2xx response is also reported as an error so that polling loops do
// not silently treat error pages as a successful scrape body.
//
// Each request is bounded by an additional 1-second deadline derived from
// the supplied parent context so an unresponsive endpoint cannot starve the
// surrounding poll: assert.Eventually's tick interval stays meaningful even
// when the listener accepts but never replies. Caller cancellation of the
// parent ctx still aborts in-flight requests.
func TryScrapeMetrics(ctx context.Context, addr string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://"+addr+"/metrics", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("scrape %s returned HTTP %d", addr, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// Shutdowner is anything that has a context-aware Shutdown method.
// Both *o11y.SDK and the metrics Closer return type satisfy it.
type Shutdowner interface {
	Shutdown(context.Context) error
}

// MustShutdown runs s.Shutdown with a 5-second deadline derived from the
// supplied parent context, and fails the test on error. The deadline cap
// protects test runs from a hung exporter while still honouring caller
// cancellation.
func MustShutdown(ctx context.Context, t *testing.T, s Shutdowner) {
	t.Helper()
	shutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	require.NoError(t, s.Shutdown(shutCtx))
}

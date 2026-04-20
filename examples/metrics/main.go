// Package main demonstrates metrics with the o11y SDK using OTLP push.
//
// The example starts an HTTP server wrapped with o11yhttp middleware, then
// generates periodic synthetic traffic. Metrics flow:
//
//	App ──OTLP/HTTP──► OTel Collector ──prometheusremotewrite──► Prometheus ──► Grafana
//
// WithMetricsOTLPEndpoint reuses the same port-4318 path already used for
// traces and logs, so no extra port-forward is needed when running with kind.
//
// Prerequisites (kind cluster must be running):
//
//	# kind-config.yaml already maps localhost:4318 → OTel Collector via NodePort
//	go run examples/metrics/main.go
//
// In Grafana (http://localhost:3000): Explore → Prometheus →
// query http_server_request_duration_seconds_bucket.
// Click an exemplar dot to jump to the linked trace in Tempo.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/flywindy/o11y"
	o11yhttp "github.com/flywindy/o11y/http"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("metrics-example"),
		o11y.WithServiceVersion("0.1.0"),
		o11y.WithEnvironment("development"),
		o11y.WithServiceNamespace("platform"),
		o11y.WithOTLPEndpoint("http://localhost:4318"),
		// Push metrics via OTLP instead of exposing a Prometheus scrape endpoint.
		// The OTel Collector forwards them to Prometheus via remote write.
		o11y.WithMetricsOTLPEndpoint("http://localhost:4318"),
		o11y.WithLogLevel(slog.LevelInfo),
	)
	if err != nil {
		slog.ErrorContext(ctx, "failed to initialize o11y SDK", slog.Any("error", err))
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if err := obs.Shutdown(shutdownCtx); err != nil {
			obs.Logger.ErrorContext(shutdownCtx, "SDK shutdown error", slog.Any("error", err))
		}
	}()

	// Build an HTTP handler wrapped with the o11y middleware.
	// The middleware emits http_server_request_duration_seconds histogram with
	// service_namespace, service_name, service_version, and
	// deployment_environment_name as constant labels on every series.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/fast", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/api/slow", func(w http.ResponseWriter, r *http.Request) {
		// Simulate variable latency so the histogram has interesting shape.
		time.Sleep(time.Duration(50+rand.IntN(200)) * time.Millisecond)
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/api/error", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintln(w, "error")
	})

	handler := o11yhttp.New(ctx, obs.Meter("metrics-example"),
		o11yhttp.WithPathNormalizer(func(r *http.Request) string {
			return r.URL.Path // paths are already static templates in this example
		}),
	)(mux)

	// Start the app server on :8080.
	ln, err := net.Listen("tcp", ":8080")
	if err != nil {
		obs.Logger.ErrorContext(ctx, "failed to listen", slog.Any("error", err))
		os.Exit(1)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	obs.Logger.InfoContext(ctx, "servers started",
		slog.String("app", "http://localhost:8080"),
		slog.String("metrics_sink", "Grafana → Explore → Prometheus (via OTel Collector remote write)"),
	)

	// Generate synthetic traffic so metrics accumulate without manual curling.
	tracer := obs.Tracer("metrics-example")
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	paths := []string{"/api/fast", "/api/fast", "/api/slow", "/api/error"}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-quit:
			obs.Logger.InfoContext(ctx, "shutting down")
			shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
			defer c()
			_ = srv.Shutdown(shutdownCtx)
			return

		case <-ticker.C:
			// Each synthetic request runs inside its own span so the histogram
			// bucket carries an exemplar with the matching traceId.
			reqCtx, span := tracer.Start(ctx, "synthetic-request")
			path := paths[rand.IntN(len(paths))]

			req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://localhost:8080"+path, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				obs.Logger.ErrorContext(reqCtx, "request failed", slog.Any("error", err))
			} else {
				resp.Body.Close()
				obs.Logger.InfoContext(reqCtx, "request sent",
					slog.String("path", path),
					slog.Int("status", resp.StatusCode),
				)
			}
			span.End()
		}
	}
}

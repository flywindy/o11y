package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/flywindy/o11y"
	o11yhttp "github.com/flywindy/o11y/http"
)

// main initializes the observability SDK with service metadata and team labels, creates a root span and a nested child span to demonstrate tracing, and starts an HTTP server whose handlers are wrapped with httpmw to emit Prometheus metrics.
// The program registers a deferred SDK shutdown that flushes in-flight spans and metrics with a 5-second timeout before exit.
func main() {
	ctx := context.Background()

	// 1. Initialize the SDK (no global state mutated)
	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("basic-example"),
		o11y.WithServiceVersion("0.1.0"),
		o11y.WithEnvironment("development"),
		o11y.WithServiceNamespace("platform"),
		o11y.WithOTLPEndpoint("http://localhost:4318"),
		o11y.WithLogLevel(slog.LevelInfo),
	)
	if err != nil {
		slog.ErrorContext(ctx, "failed to initialize o11y SDK", slog.Any("error", err))
		return
	}

	// 2. Flush in-flight spans and metrics on exit.
	//    A dedicated context with a timeout ensures the shutdown completes promptly.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := obs.Shutdown(shutdownCtx); err != nil {
			obs.Logger.ErrorContext(shutdownCtx, "SDK shutdown error", slog.Any("error", err))
		}
	}()

	obs.Logger.Info("SDK initialized successfully")

	// 3. Start a root span using the SDK's TracerProvider (no global OTel state needed)
	tracer := obs.Tracer("example-tracer")
	ctx, rootSpan := tracer.Start(ctx, "root-operation")

	obs.Logger.InfoContext(ctx, "processing root operation")
	time.Sleep(100 * time.Millisecond)

	// 4. Child span
	performChildOperation(ctx, obs)

	rootSpan.End()
	obs.Logger.InfoContext(ctx, "example completed")

	// 5. Demonstrate the HTTP middleware. Any handler wrapped with
	//    o11yhttp.New will emit http_server_request_duration_seconds on
	//    the Prometheus scrape endpoint (default :2112/metrics) with a
	//    team="platform" label pre-populated by the SDK.
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello\n"))
	})
	wrapped := o11yhttp.New(obs.Meter("example"))(mux)

	obs.Logger.Info("serving demo handler on :8080 — curl http://localhost:2112/metrics to scrape")
	srv := &http.Server{
		Addr:              ":8080",
		Handler:           wrapped,
		ReadHeaderTimeout: 5 * time.Second,
	}
	_ = srv.ListenAndServe()
}

// performChildOperation starts a child tracing span from ctx and emits logs to indicate simulated work.
// The span is ended when the function returns, and logs are recorded with the span's context via the provided SDK.
func performChildOperation(ctx context.Context, obs *o11y.SDK) {
	tracer := obs.Tracer("example-tracer")
	ctx, span := tracer.Start(ctx, "child-operation")
	defer span.End()

	obs.Logger.InfoContext(ctx, "performing child operation")
	time.Sleep(100 * time.Millisecond)
	obs.Logger.InfoContext(ctx, "child operation finished")
}

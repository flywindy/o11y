// Package main demonstrates the safe context pattern for work that outlives an
// HTTP request — using github.com/flywindy/o11y/obsctx so a background MongoDB
// write keeps the request's trace but is not canceled when the request ends.
//
// The anti-pattern this prevents: threading the request context
// (c.Request.Context()) into a post-response / fire-and-forget write. The
// request context is canceled the moment the handler returns, which aborts the
// write mid-flight ("context canceled" and, for MongoDB, "connection reset by
// peer"). The failure is timing-dependent, so it tends to pass locally and only
// surface under production latency/concurrency. See ADR 0024.
//
// Run with:
//
//	kubectl port-forward -n infra svc/otel-collector 4318:4318
//	# MongoDB reachable at $MONGODB_URI (default mongodb://localhost:27017)
//	go run examples/background/main.go
//	curl http://localhost:8080/things/abc
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/flywindy/o11y"
	o11ygin "github.com/flywindy/o11y/gin"
	o11ymongo "github.com/flywindy/o11y/mongo"
	"github.com/flywindy/o11y/obsctx"
	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/bson"
	drivermongo "go.mongodb.org/mongo-driver/v2/mongo"
)

const (
	defaultMongoURI = "mongodb://localhost:27017"
	databaseName    = "o11y_examples"
	collectionName  = "things"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("background-example"),
		o11y.WithServiceVersion("0.1.0"),
		o11y.WithEnvironment("development"),
		o11y.WithServiceNamespace("platform"),
		o11y.WithOTLPEndpoint("http://localhost:4318"),
		o11y.WithMetricsOTLPEndpoint("http://localhost:4318"),
		o11y.WithLogLevel(slog.LevelInfo),
	)
	if err != nil {
		slog.ErrorContext(ctx, "failed to initialize o11y SDK", slog.Any("error", err))
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := obs.Shutdown(shutdownCtx); err != nil {
			obs.Logger.ErrorContext(shutdownCtx, "SDK shutdown error", slog.Any("error", err))
		}
	}()

	client, err := o11ymongo.Connect(ctx, envOrDefault("MONGODB_URI", defaultMongoURI),
		obs.TracerProvider(), obs.MeterProvider(), obs.Propagator)
	if err != nil {
		obs.Logger.ErrorContext(ctx, "failed to connect to MongoDB", slog.Any("error", err))
		os.Exit(1)
	}
	defer func() {
		disconnectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Disconnect(disconnectCtx); err != nil {
			obs.Logger.ErrorContext(disconnectCtx, "MongoDB disconnect failed", slog.Any("error", err))
		}
	}()

	router := gin.New()
	router.Use(o11ygin.Middleware("background-example", obs.TracerProvider(), obs.MeterProvider(), obs.Propagator)...)
	router.Use(gin.Recovery())

	coll := client.Database(databaseName).Collection(collectionName)

	router.GET("/things/:id", func(c *gin.Context) {
		id := c.Param("id")

		// In-request read: use the request context directly. If the client
		// disconnects this should be canceled — that is the correct behavior.
		var doc bson.M
		err := coll.FindOne(c.Request.Context(), bson.M{"_id": id}).Decode(&doc)
		if err != nil && !errors.Is(err, drivermongo.ErrNoDocuments) {
			_ = c.AbortWithError(http.StatusInternalServerError, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": id, "found": err == nil})

		// Post-response background write: it outlives the request, so it must
		// NOT use c.Request.Context() (canceled the instant this handler
		// returns). obsctx.Go keeps the request's trace context — so the
		// write's span stays in the same trace — but is not canceled with the
		// request, and bounds the work with a timeout.
		//
		// WRONG (causes "context canceled" / "connection reset by peer" in prod):
		//
		//	go func() {
		//		_, _ = coll.UpdateOne(c.Request.Context(), filter, update)
		//	}()
		obsctx.Go(c.Request.Context(), 5*time.Second, func(ctx context.Context) {
			if _, err := coll.UpdateOne(ctx,
				bson.M{"_id": id},
				bson.M{"$set": bson.M{"last_accessed": time.Now().UTC()}},
			); err != nil {
				obs.Logger.ErrorContext(ctx, "background last_accessed write failed", slog.Any("error", err))
			}
		})
	})

	server := &http.Server{Addr: ":8080", Handler: router, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		obs.Logger.InfoContext(ctx, "background example listening", slog.String("addr", "http://localhost:8080"))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			obs.Logger.ErrorContext(ctx, "server error", slog.Any("error", err))
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		obs.Logger.ErrorContext(shutdownCtx, "server shutdown error", slog.Any("error", err))
	}
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

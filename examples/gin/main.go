// Package main demonstrates gin instrumentation with the o11y SDK.
//
// Run with:
//
//	kubectl port-forward -n infra svc/otel-collector 4318:4318
//	go run examples/gin/main.go
//	curl http://localhost:8080/ok
//	curl http://localhost:8080/fail
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/flywindy/o11y"
	o11ygin "github.com/flywindy/o11y/gin"
	"github.com/gin-gonic/gin"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("gin-example"),
		o11y.WithServiceVersion("0.1.0"),
		o11y.WithEnvironment("development"),
		o11y.WithServiceNamespace("platform"),
		o11y.WithOTLPEndpoint("http://localhost:4318"),
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

	router := gin.New()
	router.Use(o11ygin.Middleware(
		"gin-example",
		obs.TracerProvider(),
		obs.MeterProvider(),
		obs.Propagator,
	)...)
	router.Use(gin.Recovery())

	router.GET("/ok", func(c *gin.Context) {
		obs.Logger.InfoContext(c.Request.Context(), "gin request succeeded")
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.GET("/fail", func(c *gin.Context) {
		err := errors.New("simulated gin failure")
		obs.Logger.ErrorContext(c.Request.Context(), "gin request failed", slog.Any("error", err))
		c.AbortWithError(http.StatusInternalServerError, err).SetType(gin.ErrorTypePublic) //nolint:errcheck
	})
	router.GET("/panic", func(*gin.Context) {
		panic("simulated panic")
	})

	server := &http.Server{
		Addr:              ":8080",
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		obs.Logger.InfoContext(ctx, "gin example listening", slog.String("addr", "http://localhost:8080"))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			obs.Logger.ErrorContext(ctx, "gin server error", slog.Any("error", err))
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		obs.Logger.ErrorContext(shutdownCtx, "gin server shutdown error", slog.Any("error", err))
	}
}

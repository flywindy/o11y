// Package main demonstrates MongoDB tracing and operation metrics through the
// o11y MongoDB facade.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/flywindy/o11y"
	o11ymongo "github.com/flywindy/o11y/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

const (
	defaultMongoURI = "mongodb://localhost:27017"
	databaseName    = "o11y_examples"
	collectionName  = "mongodb_events"
)

func main() {
	if err := run(); err != nil {
		slog.ErrorContext(context.Background(), "MongoDB example failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	obs, err := o11y.Init(ctx,
		o11y.WithServiceName("mongodb-example"),
		o11y.WithServiceVersion("0.1.0"),
		o11y.WithEnvironment("development"),
		o11y.WithServiceNamespace("platform"),
		o11y.WithOTLPEndpoint("http://localhost:4318"),
		o11y.WithMetricsOTLPEndpoint("http://localhost:4318"),
		o11y.WithLogLevel(slog.LevelInfo),
	)
	if err != nil {
		return fmt.Errorf("initialize o11y SDK: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := obs.Shutdown(shutdownCtx); err != nil {
			obs.Logger.ErrorContext(shutdownCtx, "SDK shutdown error", slog.Any("error", err))
		}
	}()

	logger := obs.Logger
	mongoURI := envOrDefault("MONGODB_URI", defaultMongoURI)
	client, err := o11ymongo.Connect(
		ctx,
		mongoURI,
		obs.TracerProvider(),
		obs.MeterProvider(),
		obs.Propagator,
	)
	if err != nil {
		return fmt.Errorf("create MongoDB client: %w", err)
	}
	defer func() {
		disconnectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Disconnect(disconnectCtx); err != nil {
			logger.ErrorContext(disconnectCtx, "MongoDB disconnect failed", slog.Any("error", err))
		}
	}()

	tracer := obs.Tracer("mongodb-example")
	runCtx, span := tracer.Start(ctx, "mongodb-example")
	defer span.End()

	opCtx, cancel := context.WithTimeout(runCtx, 10*time.Second)
	defer cancel()

	if err := client.Ping(opCtx, readpref.Primary()); err != nil {
		return fmt.Errorf("ping MongoDB: %w", err)
	}
	logger.InfoContext(opCtx, "connected to MongoDB",
		slog.String("db.namespace", databaseName),
		slog.String("db.collection.name", collectionName),
	)

	collection := client.Database(databaseName).Collection(collectionName)
	eventID := "mongodb-example-" + time.Now().UTC().Format("20060102T150405.000000000Z")

	if _, err := collection.InsertOne(opCtx, bson.M{
		"_id":        eventID,
		"type":       "example.created",
		"created_at": time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("insert document %s: %w", eventID, err)
	}
	logger.InfoContext(opCtx, "document inserted", slog.String("event_id", eventID))

	var found bson.M
	if err := collection.FindOne(opCtx, bson.M{"_id": eventID}).Decode(&found); err != nil {
		return fmt.Errorf("find document %s: %w", eventID, err)
	}
	logger.InfoContext(opCtx, "document found",
		slog.String("event_id", eventID),
		slog.Any("type", found["type"]),
	)

	if _, err := collection.UpdateOne(opCtx,
		bson.M{"_id": eventID},
		bson.M{"$set": bson.M{"processed_at": time.Now().UTC()}},
	); err != nil {
		return fmt.Errorf("update document %s: %w", eventID, err)
	}
	logger.InfoContext(opCtx, "document updated", slog.String("event_id", eventID))

	if _, err := collection.DeleteOne(opCtx, bson.M{"_id": eventID}); err != nil {
		return fmt.Errorf("delete document %s: %w", eventID, err)
	}
	logger.InfoContext(opCtx, "document deleted", slog.String("event_id", eventID))
	return nil
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

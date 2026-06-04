package minio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	o11yhttp "github.com/flywindy/o11y/http"
	miniogo "github.com/minio/minio-go/v7"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const instrumentationName = "github.com/flywindy/o11y/minio"

// ADR 0018 §4 attribute keys. The object_store.* namespace is package-local;
// see References in ADR 0018 for the OTel patterns it mirrors.
const (
	objectStoreSystemNameKey    = attribute.Key("object_store.system.name")
	objectStoreOperationNameKey = attribute.Key("object_store.operation.name")
	objectStoreBucketNameKey    = attribute.Key("object_store.bucket.name")
	objectStoreObjectKeyKey     = attribute.Key("object_store.object.key")
	objectStoreObjectSizeKey    = attribute.Key("object_store.object.size")
	minioErrorKindKey           = attribute.Key("minio.error.kind")

	awsS3BucketKey = attribute.Key("aws.s3.bucket")
	awsS3KeyKey    = attribute.Key("aws.s3.key")

	objectStoreSystemNameS3 = "s3"
)

// Client is a tracing-aware MinIO / S3 client.
//
// It embeds the upstream *minio.Client so the full driver API stays
// available; the data-plane methods documented in ADR 0018 §5 are shadowed
// by instrumented overrides on this type. Unwrapped methods flow through
// the embedded client and produce no logical span (e.g. RemoveObjects in
// v1, ADR 0018 §6).
type Client struct {
	*miniogo.Client

	tracer        trace.Tracer
	duration      metric.Float64Histogram
	cfg           config
	serverAddress string
	serverPort    int
}

// New constructs an instrumented MinIO client.
//
// endpoint and minioOpts are passed through to miniogo.New. The transport
// seam is set at construction (minio-go only accepts Options.Transport at
// New time), so this is the only entry point that can enable the optional
// HTTP child-span layer (WithHTTPChildSpans).
//
// tp, mp, and prop are required and are passed explicitly; the wrapper
// never reads or writes OTel globals (ADR 0003).
func New(
	endpoint string,
	minioOpts *miniogo.Options,
	tp trace.TracerProvider,
	mp metric.MeterProvider,
	prop propagation.TextMapPropagator,
	opts ...Option,
) (*Client, error) {
	if endpoint == "" {
		return nil, errors.New("minio new: endpoint must not be empty")
	}
	if minioOpts == nil {
		return nil, errors.New("minio new: options must not be nil")
	}
	if tp == nil {
		tp = tracenoop.NewTracerProvider()
	}
	if mp == nil {
		mp = metricnoop.NewMeterProvider()
	}
	// prop is accepted for API consistency with the other o11y wrappers
	// but intentionally unused: per ADR 0018 §7 the wrapper never injects
	// trace context toward the storage backend (the optional HTTP layer
	// uses its own empty propagator below).
	_ = prop

	cfg := newConfig(opts)

	if cfg.httpChildSpans {
		// Preserve minio-go's data-plane HTTP semantics: wrap the
		// caller's transport if set, otherwise wrap minio-go's own
		// DefaultTransport so we never silently substitute
		// http.DefaultTransport (which would re-enable compression
		// and change pool/timeout behavior). Empty propagator: no
		// traceparent flows toward the storage backend (ADR 0018 §7).
		base := minioOpts.Transport
		if base == nil {
			defTransport, err := miniogo.DefaultTransport(minioOpts.Secure)
			if err != nil {
				return nil, fmt.Errorf("minio new: build default transport: %w", err)
			}
			base = defTransport
		}
		minioOpts.Transport = o11yhttp.NewTransport(
			base, tp, mp, propagation.NewCompositeTextMapPropagator(),
		)
	}

	mc, err := miniogo.New(endpoint, minioOpts)
	if err != nil {
		return nil, fmt.Errorf("minio new: %w", err)
	}

	meter := mp.Meter(instrumentationName, metric.WithSchemaURL(semconv.SchemaURL))
	duration, err := meter.Float64Histogram(
		"minio.client.operation.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of MinIO/S3 logical operations."),
	)
	if err != nil {
		duration, _ = metricnoop.NewMeterProvider().Meter(instrumentationName).Float64Histogram(
			"minio.client.operation.duration",
			metric.WithUnit("s"),
			metric.WithDescription("Duration of MinIO/S3 logical operations."),
		)
	}

	host, port := splitHostPort(endpoint)

	return &Client{
		Client:        mc,
		tracer:        tp.Tracer(instrumentationName, trace.WithSchemaURL(semconv.SchemaURL)),
		duration:      duration,
		cfg:           cfg,
		serverAddress: host,
		serverPort:    port,
	}, nil
}

// op is the per-operation bookkeeping that brackets every wrapped call.
// Begin / finish own span lifecycle and histogram recording per
// ADR 0018 §1.
type op struct {
	ctx       context.Context
	span      trace.Span
	start     time.Time
	operation string
	bucket    string
}

func (c *Client) begin(ctx context.Context, operation, bucket, key string, size int64) op {
	attrs := []attribute.KeyValue{
		objectStoreSystemNameKey.String(objectStoreSystemNameS3),
		objectStoreOperationNameKey.String(operation),
	}
	if bucket != "" {
		attrs = append(attrs, objectStoreBucketNameKey.String(bucket))
		if c.cfg.awsS3CompatAliases {
			attrs = append(attrs, awsS3BucketKey.String(bucket))
		}
	}
	if key != "" && c.cfg.objectKeyAttribute {
		attrs = append(attrs, objectStoreObjectKeyKey.String(key))
		if c.cfg.awsS3CompatAliases {
			attrs = append(attrs, awsS3KeyKey.String(key))
		}
	}
	// ADR 0018 §4: size only when a real byte count is known. PutObject
	// callers pass size = -1 to mean "stream of unknown length, read to
	// EOF" — record nothing rather than -1 bytes.
	if size >= 0 {
		attrs = append(attrs, objectStoreObjectSizeKey.Int64(size))
	}
	if c.serverAddress != "" {
		attrs = append(attrs, semconv.ServerAddress(c.serverAddress))
	}
	if c.serverPort > 0 {
		attrs = append(attrs, semconv.ServerPort(c.serverPort))
	}
	name := defaultSpanName(operation, bucket)
	if c.cfg.spanNameFormatter != nil {
		if custom := c.cfg.spanNameFormatter(operation, bucket, key); custom != "" {
			name = custom
		}
	}
	spanCtx, span := c.tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	return op{
		ctx:       spanCtx,
		span:      span,
		start:     time.Now(),
		operation: operation,
		bucket:    bucket,
	}
}

func (c *Client) finish(o op, err error) {
	metricAttrs := []attribute.KeyValue{
		objectStoreOperationNameKey.String(o.operation),
	}
	if o.bucket != "" {
		metricAttrs = append(metricAttrs, objectStoreBucketNameKey.String(o.bucket))
	}
	if c.serverAddress != "" {
		metricAttrs = append(metricAttrs, semconv.ServerAddress(c.serverAddress))
	}
	if c.serverPort > 0 {
		metricAttrs = append(metricAttrs, semconv.ServerPort(c.serverPort))
	}
	if err != nil {
		errType := errorType(err)
		o.span.SetAttributes(
			semconv.ErrorTypeKey.String(errType),
			minioErrorKindKey.String(minioErrorKind(err)),
		)
		if status := s3StatusCode(err); status > 0 {
			o.span.SetAttributes(semconv.HTTPResponseStatusCode(status))
		}
		o.span.RecordError(err)
		o.span.SetStatus(codes.Error, err.Error())
		metricAttrs = append(metricAttrs, semconv.ErrorTypeKey.String(errType))
	}
	c.duration.Record(o.ctx, time.Since(o.start).Seconds(),
		metric.WithAttributes(metricAttrs...),
	)
	o.span.End()
}

// --- Data-plane method overrides (ADR 0018 §5) -----------------------------

// PutObject uploads object data with a size that may be -1 for unknown.
// The span captures the full upload, including multipart when minio-go
// chooses that path.
func (c *Client) PutObject(
	ctx context.Context,
	bucketName, objectName string,
	reader io.Reader,
	objectSize int64,
	opts miniogo.PutObjectOptions,
) (miniogo.UploadInfo, error) {
	o := c.begin(ctx, "PutObject", bucketName, objectName, objectSize)
	info, err := c.Client.PutObject(o.ctx, bucketName, objectName, reader, objectSize, opts)
	c.finish(o, err)
	return info, err
}

// FPutObject uploads filePath. Object size is sourced from UploadInfo on
// success (always a real value), not the caller — there is no -1 path here.
func (c *Client) FPutObject(
	ctx context.Context,
	bucketName, objectName, filePath string,
	opts miniogo.PutObjectOptions,
) (miniogo.UploadInfo, error) {
	o := c.begin(ctx, "FPutObject", bucketName, objectName, -1)
	info, err := c.Client.FPutObject(o.ctx, bucketName, objectName, filePath, opts)
	if err == nil && info.Size > 0 {
		o.span.SetAttributes(objectStoreObjectSizeKey.Int64(info.Size))
	}
	c.finish(o, err)
	return info, err
}

// FGetObject downloads to filePath synchronously. Size is not recorded:
// the call returns only an error, with no observable response. Callers
// who need download-size visibility issue StatObject first (ADR 0018 §4).
func (c *Client) FGetObject(
	ctx context.Context,
	bucketName, objectName, filePath string,
	opts miniogo.GetObjectOptions,
) error {
	o := c.begin(ctx, "FGetObject", bucketName, objectName, -1)
	err := c.Client.FGetObject(o.ctx, bucketName, objectName, filePath, opts)
	c.finish(o, err)
	return err
}

// GetObject is intentionally a thin span: minio-go stashes the ctx passed
// here on the returned *Object and reuses it for every Read/Stat/Seek, so
// the wrapper passes the caller's original ctx — not the span ctx — to
// avoid parenting later HTTP children to an already-ended span
// (ADR 0018 §5). The transfer itself is not in the logical layer in v1.
func (c *Client) GetObject(
	ctx context.Context,
	bucketName, objectName string,
	opts miniogo.GetObjectOptions,
) (*miniogo.Object, error) {
	o := c.begin(ctx, "GetObject", bucketName, objectName, -1)
	obj, err := c.Client.GetObject(ctx, bucketName, objectName, opts)
	c.finish(o, err)
	return obj, err
}

// StatObject issues a HEAD against the object.
func (c *Client) StatObject(
	ctx context.Context,
	bucketName, objectName string,
	opts miniogo.StatObjectOptions,
) (miniogo.ObjectInfo, error) {
	o := c.begin(ctx, "StatObject", bucketName, objectName, -1)
	info, err := c.Client.StatObject(o.ctx, bucketName, objectName, opts)
	if err == nil && info.Size > 0 {
		o.span.SetAttributes(objectStoreObjectSizeKey.Int64(info.Size))
	}
	c.finish(o, err)
	return info, err
}

// RemoveObject deletes object key from bucket.
func (c *Client) RemoveObject(
	ctx context.Context,
	bucketName, objectName string,
	opts miniogo.RemoveObjectOptions,
) error {
	o := c.begin(ctx, "RemoveObject", bucketName, objectName, -1)
	err := c.Client.RemoveObject(o.ctx, bucketName, objectName, opts)
	c.finish(o, err)
	return err
}

// CopyObject performs a server-side copy.
func (c *Client) CopyObject(
	ctx context.Context,
	dst miniogo.CopyDestOptions,
	src miniogo.CopySrcOptions,
) (miniogo.UploadInfo, error) {
	o := c.begin(ctx, "CopyObject", dst.Bucket, dst.Object, -1)
	info, err := c.Client.CopyObject(o.ctx, dst, src)
	c.finish(o, err)
	return info, err
}

// ListObjects returns a channel and streams results lazily. Per ADR 0018
// §6, the span ends when the wrapper's goroutine observes channel close
// (drain-to-completion) and surfaces the first streamed error as
// minio.error.kind. Callers must drain the channel or cancel the context;
// abandoning the channel leaks both minio-go's producer goroutine and the
// open span (the inherited contract from minio-go itself).
func (c *Client) ListObjects(
	ctx context.Context,
	bucketName string,
	opts miniogo.ListObjectsOptions,
) <-chan miniogo.ObjectInfo {
	o := c.begin(ctx, "ListObjects", bucketName, "", -1)
	upstream := c.Client.ListObjects(o.ctx, bucketName, opts)
	out := make(chan miniogo.ObjectInfo)
	go func() {
		defer close(out)
		var firstErr error
		for info := range upstream {
			if info.Err != nil && firstErr == nil {
				firstErr = info.Err
			}
			select {
			case out <- info:
			case <-o.ctx.Done():
				// Drain remaining items to release minio-go's
				// producer; then surface the context error.
				drainListChannel(upstream)
				if firstErr == nil {
					firstErr = o.ctx.Err()
				}
			}
		}
		c.finish(o, firstErr)
	}()
	return out
}

// --- Helpers ---------------------------------------------------------------

// drainListChannel reads upstream until close, releasing minio-go's
// producer goroutine after a caller-cancelled ListObjects.
func drainListChannel(upstream <-chan miniogo.ObjectInfo) {
	for range upstream { //nolint:revive // empty body is the idiomatic Go drain
	}
}

// splitHostPort parses an endpoint like "minio.internal:9000" into host
// and numeric port. The endpoint format mirrors miniogo.New: it does not
// carry a scheme.
func splitHostPort(endpoint string) (string, int) {
	if endpoint == "" {
		return "", 0
	}
	host, portStr, err := net.SplitHostPort(endpoint)
	if err != nil {
		return endpoint, 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return host, 0
	}
	return host, port
}

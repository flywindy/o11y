package minio

// Option configures MinIO instrumentation behavior.
type Option func(*config)

type config struct {
	httpChildSpans     bool
	objectKeyAttribute bool
	awsS3CompatAliases bool
	spanNameFormatter  func(operation, bucket, key string) string
}

func newConfig(opts []Option) config {
	cfg := config{
		objectKeyAttribute: true,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// WithHTTPChildSpans injects the o11y http.NewTransport into the MinIO
// client's transport so each underlying HTTP round-trip (including each
// multipart UploadPart) becomes a child span of the logical operation span.
// Default: off.
//
// To avoid silently changing minio-go's data-plane HTTP semantics, the base
// RoundTripper that o11y wraps is selected at construction as:
//
//   - the caller's Options.Transport when it is non-nil; the wrapper never
//     replaces a caller-supplied transport.
//   - otherwise miniogo.DefaultTransport(Secure) — minio-go's own default
//     transport (response decompression disabled, S3-tuned timeouts and
//     connection pool), NOT http.DefaultTransport. Passing a nil base to
//     the SDK transport helper would substitute http.DefaultTransport,
//     which would re-enable compression and shift pool/timeout behavior.
//
// When enabled, the underlying transport is constructed with an empty
// propagator so no traceparent header travels toward the object store
// (ADR 0018 §7).
func WithHTTPChildSpans(enabled bool) Option {
	return func(cfg *config) {
		cfg.httpChildSpans = enabled
	}
}

// WithObjectKeyAttribute controls whether the (potentially high-cardinality)
// object key is set on the span as object_store.object.key. Default: true.
//
// Spans tolerate high cardinality; metric labels never carry the key
// regardless of this option (ADR 0018 §3).
func WithObjectKeyAttribute(enabled bool) Option {
	return func(cfg *config) {
		cfg.objectKeyAttribute = enabled
	}
}

// WithAWSS3CompatAttributes additionally emits the experimental AWS-S3
// semantic-convention attribute keys (aws.s3.bucket, aws.s3.key) on spans
// alongside the default object_store.* attributes. Default: false.
//
// The two sets are dual-emitted — generic stays on when this is enabled.
// Compat aliases never appear on metric labels (cardinality is single-
// sourced on object_store.*).
func WithAWSS3CompatAttributes(enabled bool) Option {
	return func(cfg *config) {
		cfg.awsS3CompatAliases = enabled
	}
}

// WithSpanNameFormatter overrides the default span name
// "s3.{Operation} {bucket}" with the caller-supplied formatter.
//
// The formatter receives the logical operation name (e.g. "PutObject"),
// the bucket, and the object key (which may be empty for operations that
// do not target a single object). Returning an empty string falls back to
// the default name. A non-empty return is used verbatim — the "s3." system
// prefix is part of the default only and is not forced onto custom names.
func WithSpanNameFormatter(f func(operation, bucket, key string) string) Option {
	return func(cfg *config) {
		cfg.spanNameFormatter = f
	}
}

// defaultSpanName renders "{object_store.system.name}.{Operation} {bucket}"
// (e.g. "s3.PutObject media"), the data-store span shape shared with the
// redis and mongo wrappers. The object key is intentionally absent (high
// cardinality, span attribute only). A caller-supplied WithSpanNameFormatter
// replaces this verbatim and is not re-prefixed.
func defaultSpanName(operation, bucket string) string {
	if bucket == "" {
		return objectStoreSystemNameS3 + "." + operation
	}
	return objectStoreSystemNameS3 + "." + operation + " " + bucket
}

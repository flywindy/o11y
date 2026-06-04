// Package minio instruments github.com/minio/minio-go/v7 with o11y tracing
// and metrics for high-level object-storage operations.
// Tier: T3 SDK-owned method wrapper over github.com/minio/minio-go/v7
//
// minio-go exposes no hook / middleware API on the data-plane client, so the
// wrapper is a method-wrapping Client that embeds *minio.Client (mirroring
// the ADR 0005 mongo wrapper shape) and overrides the synchronous data-plane
// methods to bracket each call in a SpanKindClient span. The optional
// WithHTTPChildSpans transport layer reuses the SDK's http.NewTransport
// (ADR 0009) wrapped over minio-go's own DefaultTransport so per-request
// HTTP children appear under the logical span without changing minio-go's
// data-plane HTTP semantics.
//
// See ADR 0018 for the full design rationale, attribute schema, and
// error-classification taxonomy.
package minio

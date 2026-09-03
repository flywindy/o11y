# ADR 0027 — Elasticsearch Client Metrics

**Status**: Accepted
**Date**: 2026-09-03
**Relates to**: ADR 0020 (Elasticsearch integration — this ADR amends its §3
signature and supersedes its §6 deferral), ADR 0008 (instrumentation sourcing
policy), ADR 0003 (global state policy), ADR 0019 §7 (Cassandra — the reference
for a justified-T3 metric layer over a trace-only seam), ADR 0014 (MongoDB
metrics), ADR 0002 (metrics strategy), ADR 0006 (semconv pin).

---

## Context

ADR 0020 shipped the `elasticsearch/` package **trace-only** and deferred
metrics (§6) with two revisit triggers:

1. a concrete need for **client-attributed operation latency / error rate**
   that the server-side `elasticsearch_exporter` cannot satisfy — for the first
   consumer, per-worker bulk-indexing latency and error rate in
   `search-sync-worker`, correlated with application metrics in the same
   backend; or
2. the first-party instrumentation starts emitting OTel metrics.

Trigger 1 has fired: the consumer wants ES operation latency and error rate as
**metrics**, not spans, because metrics are not sampled (ADR 0015 head sampling
biases span-derived rates) and because they must sit on the same dashboards as
the worker's own counters. Trigger 2 has **not** fired. Verified against the
pinned `elastic-transport-go/v8 v8.8.0` (`elastictransport/instrumentation.go`):
the `Instrumentation` interface is trace-only (`Start`, `Close`, `RecordError`,
`RecordPathPart`, `RecordRequestBody`, `BeforeRequest`, `AfterRequest`,
`AfterResponse`), `NewOtelInstrumentation` takes only a `trace.TracerProvider`,
and the package imports no `go.opentelemetry.io/otel/metric` symbol.

### Seams available in the pinned client

| Seam | What it yields | Per-endpoint / per-index? | Assessment |
|---|---|---|---|
| **A. `elastictransport.Instrumentation` callbacks** (already wrapped by the facade for span naming and status normalization, ADR 0020 §4) | endpoint id (`Start`), index path part (`RecordPathPart`), terminal request URL → routed node (`AfterRequest`), per-attempt status (`AfterResponse`), terminal error (`RecordError`), request end (`Close`) | **Yes** | Everything the operation-duration metric needs already flows through the facade's `responseState`. Zero new dependencies. **Chosen.** |
| B. `elastictransport.Config.Interceptors` (per-attempt `RoundTripper` middleware) | per-attempt latency and status, request URL | No endpoint id or index without parsing the URL path | Right seam for a per-attempt/retry counter; not needed for v1 (see §8). |
| C. `Config.EnableMetrics` + `Measurable.Metrics()` | cumulative `Requests`, `Failures`, `Responses[status]`, per-connection `Failures`/`IsDead` | No | Coarse transport aggregate, as ADR 0020 §6 assessed; no latency. Deferred (§8). |

### ADR 0008 §2 evaluation — no candidate library → justified T3 (metric layer only)

There is no third-party library to facade: the first-party instrumentation is
trace-only, `otelelasticsearch`-style contrib modules do not exist for
`go-elasticsearch/v8`, and the upstream `Metrics()` snapshot is not a semconv
metric. Under ADR 0008 the metric layer is therefore a **justified T3**: a small
SDK-owned recorder at seam A. The span side is untouched and remains the T2
facade of ADR 0020 — the same split MongoDB uses (T2 command spans/metrics from
contrib plus SDK-owned pool metrics, ADR 0014) and the same posture Cassandra
took where no library existed (ADR 0019 §7).

---

## Decisions

### 1. Sourcing tier: justified-T3 metric layer at the `Instrumentation` seam

The facade's existing `instrumentation` wrapper (ADR 0020 §4) gains a
`MeterProvider`-backed recorder. No span or attribute code moves; the recorder
reads the per-request `responseState` the wrapper already maintains and records
one histogram sample at `Close`. Seam B/C are not used in v1.

### 2. Instrument

| Metric | Type | Unit | Recorded |
|---|---|---|---|
| `db.client.operation.duration` | Float64 histogram | `s` | Once per request at `Close`, measuring `Start → Close` |

This is the same instrument MongoDB (ADR 0014), Redis (ADR 0013), and Cassandra
(ADR 0019) emit, so the cross-integration `db_client_operation_duration_seconds`
dashboards pick up Elasticsearch with a `db_system_name="elasticsearch"` filter.

`Start → Close` is the same interval the upstream span covers: it includes
request building, every transport attempt and retry backoff, and the client's
product check. The sample therefore reflects the **caller's perceived latency
of one API call**, not a per-attempt round trip. A request retried from a 503 to
a 200 is one sample with no `error.type` (§3), mirroring the span-status rule
ADR 0020 §4 adopted ("final response wins").

### 3. Attributes (semconv v1.39.0, current spellings)

The metric is SDK-owned, so it carries the **current** semconv keys — it does
not inherit the legacy `db.system` / `db.operation` / `db.elasticsearch.*`
spellings the upstream span emits (ADR 0020 §4). Keys verified against the
pinned package source (see *Semconv verification* below).

| Attribute | Level (semconv) | Source callback | Cardinality bound |
|---|---|---|---|
| `db.system.name` = `elasticsearch` | Required | constant | 1 |
| `db.operation.name` | Required | endpoint id passed to `Start` (`search`, `bulk`, `index`, `indices.create`, …) | fixed set: the generated API's ~560 endpoint ids |
| `db.collection.name` | Conditionally Required | `RecordPathPart("index", …)` — see §4 | schema-shaped; capped by `o11y.WithMaxUniqueCollections` |
| `server.address` / `server.port` | Recommended | `AfterRequest` — the request URL, which the transport rewrites in place on every attempt with the node it selected, so the value is the node the **terminal** attempt was routed to | the cluster's node set (fixed per deployment); omitted when the transport never selected a node |
| `error.type` | Conditionally Required (failures only) | `RecordError` / `AfterResponse` — see below | bounded: HTTP status codes plus a handful of Go error types |
| `db.response.status_code` | Recommended (failures only here) | `AfterResponse` — the terminal response's HTTP status as a string, only when it is `> 299` | HTTP status codes; identical value set to the status branch of `error.type` |

**`error.type` classification.** Semconv asks for `error.type` only on failure
and lets HTTP-level failures be classified by status code. The facade applies
the same terminal-outcome decision the span status uses (ADR 0020 §4):

- `RecordError` fired for a terminal failure that returned no usable ES
  response (transport failure, context cancellation, product-check failure):
  `error.type` is the stable context sentinel (`context.Canceled`,
  `context.DeadlineExceeded`) or else the Go error type name — the classifier
  the cassandra and redis packages use. A status code stashed from an earlier
  retried attempt is **never** used here (it would be stale).
- Otherwise the request returned a response: `error.type` is the status code
  as a string (`"429"`, `"500"`, `"302"`) when it is `> 299` — the boundary
  `esapi.Response.IsError` and the span status share — and absent on success.
  The typed API's `ElasticsearchError` (which runs `RecordError` after decoding
  the error body but still has a final response) falls in this branch, so a
  typed 400 and a low-level 400 label identically.

**`db.response.status_code` accompanies `error.type` on HTTP failures.**
semconv's `error.type` guidance says that when a domain defines its own status
codes, instrumentation should record the domain-specific attribute *and* set
`error.type` to capture every failure. For Elasticsearch the domain attribute is
`db.response.status_code` (Recommended on `db.client.operation.duration`), whose
value is the HTTP status. It is recorded only when the request returned a
response with status `> 299` — never on success and never from a stale retried
attempt — so its value set is exactly the status branch of `error.type` and it
adds no cardinality. It is deliberately *not* set on success (semconv marks it
Conditionally Required only "if the operation failed"), so a 200/201 split
does not multiply series.

**Reported `error.type` values.** semconv asks instrumentations to document the
error classes they report. This package reports: HTTP status codes as strings
(`"300"`–`"599"`); `context.Canceled` and `context.DeadlineExceeded`; the Go
type of the terminal transport error, unwrapped past `fmt` wrappers
(typically `*net.OpError`, `*net.DNSError`, `*tls.CertificateVerificationError`,
`*url.Error`); `*errors.errorString` for the client's product-check failure
(the upstream builds it with `errors.New`); and, on the typed client, a decoder
error type (e.g. `*json.SyntaxError`) for an undecodable body. The `fmt`
unwrapping matters because the typed client's `Perform` wraps every transport
failure with `fmt.Errorf("…: %w", err)` before `RecordError`, which would
otherwise collapse all typed-client transport failures into one
`*fmt.wrapError` bucket (pinned by a test).

**`db.namespace` is not emitted on the metric.** The upstream learns the cluster
name only from Elastic Cloud response headers (`X-Found-Handling-Cluster`);
self-hosted clusters expose nothing at this seam. semconv marks it
Conditionally Required "if available", and it is not. Cluster identity for
metrics comes from the resource / `server.address` instead.

**Exemplars.** The sample is recorded against the request context — which holds
the ES CLIENT span — so the exemplar points at the ES span, not the caller's
parent (same rule as the Cassandra observer). Exemplars follow the SDK default
trace-based filter and attach only when that span is sampled.

**Sampling independence.** The metric must not depend on the span being sampled
or the tracer being real: metrics are not sampled. The facade's
`responseState` travels in the context regardless of `span.IsRecording()`, and
the recorder never consults the span. Pinned by tests with `NeverSample()` and a
no-op `TracerProvider`.

### 4. `db.collection.name` (index) — on by default, single-index only, capped

The index is the label that makes the metric useful (per-index bulk latency and
error rate is the consumer's ask) and the one that joins client-side series to
`elasticsearch_exporter`'s per-index server metrics. It is **on by default**,
following the Cassandra amendment (ADR 0019, 2026-07-29), with three bounds
because Elasticsearch index names are less schema-fixed than Cassandra tables
(date-rolled indices such as `logs-2026.09.03` are common):

1. **Semconv's "single collection" condition is enforced per request.** The
   label is emitted only when the request addresses exactly one index path
   part. It is omitted when there is no index (cross-index `_search`,
   `cluster.health`, a `bulk` whose index is set per action line) and when the
   generated API joined a multi-index list with commas (`WithIndex("a", "b")`
   → `a,b`). A wildcard or alias (`logs-*`) is one addressed name and is kept
   as written. The span keeps the full path-part value in every case.
2. **`elasticsearch.WithCollectionMetricLabel(false)`** drops the label for
   services whose index names roll and would otherwise mint a series per day.
3. **`o11y.WithMaxUniqueCollections`** (default 200) now also caps the
   Elasticsearch label at the export boundary, collapsing overflow to
   `"other"`. The rule is scoped to this package's instrumentation scope and
   carries its **own budget** (`elasticsearch/db.collection.name`), separate
   from Cassandra's: a table and an index are unrelated dimensions, so one
   integration's overflow must not evict the other's values. Both export paths
   (OTLP and Prometheus) install the rule.

Note the bulk caveat for the motivating consumer: `client.Bulk` records the
index only when the call uses `WithIndex`; a bulk body that names the index per
action line yields `db.operation.name="bulk"` with no `db.collection.name`.
Callers who want per-index bulk series should set the index on the request.

### 5. Instrumentation scope, views, and `o11y.Init` wiring

- The meter is created from the supplied `MeterProvider` under the scope
  `github.com/flywindy/o11y/elasticsearch` (constant `instrumentationName`,
  schema URL semconv v1.39.0). Spans keep the upstream tracer scope
  (`elasticsearch-api`); the two are independent.
- `elasticsearch.MetricViews(histogramBuckets)` returns a view scoped to that
  instrumentation scope that (a) applies the SDK's bucket policy
  (`WithHistogramBuckets`) instead of OTel's millisecond defaults and (b)
  installs an allow-keys filter for exactly the §3 label set. The scope filter
  matters because `db.client.operation.duration` is also emitted by the Redis,
  MongoDB, and Cassandra wrappers; an unscoped view would create a duplicate,
  conflicting stream.
- `o11y.Init` composes the views into `internal/metrics.Config.ExtraViews`
  alongside the redis/mongo/minio/cassandra views, and `internal/metrics`
  registers the §4 cap rule. Services that build their own `MeterProvider`
  must register `MetricViews` via `sdkmetric.WithView(...)` and an equivalent
  cardinality cap themselves.

### 6. Public API: `MeterProvider` becomes a positional parameter (breaking)

```go
func NewClient(cfg elasticsearch.Config, tp trace.TracerProvider, mp metric.MeterProvider, opts ...Option) (*elasticsearch.Client, error)
func NewTypedClient(cfg elasticsearch.Config, tp trace.TracerProvider, mp metric.MeterProvider, opts ...Option) (*elasticsearch.TypedClient, error)

func WithCollectionMetricLabel(enabled bool) Option // default: true (§4)
func MetricViews(histogramBuckets []float64) []sdkmetric.View
```

ADR 0020 §3 made the `(cfg, tp, opts)` shape a *deliberate* divergence from the
SDK norm **because** the integration emitted no metrics and threading an unused
`mp` would misrepresent that. That reason no longer holds: the SDK now records a
metric, so the signature converges on the `(…, tp, mp, opts…)` shape
`cassandra.NewSession` and `mongo.Connect` use. `mp` is **required and rejected
when nil**, exactly like `tp` — the facade never falls back to the global
`MeterProvider` (ADR 0003).

This is a breaking change to a pre-1.0 module, listed in the CHANGELOG with a
migration note. The non-breaking alternative — a `WithMeterProvider(mp)` option
defaulting to no metrics — was rejected: it would make ES the one integration
whose metrics are opt-in and whose provider hides in an option, and a forgotten
option would silently reproduce the "no ES metrics" gap this ADR closes. There is
still no propagator parameter (ADR 0020 §3 stands).

### 7. Coverage is identical to the spans

The metric is derived from the same callbacks as the span, so the two upstream
caveats ADR 0020 documents apply unchanged: a bare `esapi.SearchRequest{…}.Do`
(nil `Instrument` field) emits neither, and the typed client's `.Perform(ctx)`
starts the span on a shadowed context so its sample carries only
`db.system.name` / `db.operation.name` (no index, server, or error labels).
Use the client API methods and typed `.Do(ctx)`.

### 8. Out of scope (deferred, with triggers)

- **Per-attempt / retry counter** (an `elasticsearch.request.attempts`
  analogue of `cassandra.query.attempts`), best implemented at seam B
  (`Interceptors`, one call per HTTP attempt including transport failures).
  Trigger: a need to see retry amplification per node/endpoint that the
  terminal-outcome histogram cannot show.
- **Transport gauges** from seam C (`EnableMetrics` + `Metrics()`: dead
  connections, per-status cumulative counts). Low value against
  `elasticsearch_exporter`; revisit only with a concrete client-side
  connection-health need.
- **`db.namespace`** (§3) and any normalization of the upstream span's legacy
  keys (ADR 0020 §4 option (a) stands).
- `go-elasticsearch/v9` / OpenSearch (ADR 0020 §7).

---

## Global-state verification

No new third-party library is introduced; the ADR 0003 rows for
`go-elasticsearch/v8` and `elastic-transport-go/v8` are unchanged. The
SDK-owned recorder creates its meter from the `mp` argument only:

```text
$ grep -rn 'otel\.\(SetTracerProvider\|SetTextMapPropagator\|SetMeterProvider\|SetLoggerProvider\|GetMeterProvider\)' elasticsearch/*.go
(no matches outside client_test.go, which sets a sentinel global to prove it is not used)
```

`TestMeterProviderWiring` installs a recording global `MeterProvider`, builds
the facade with a different one, and asserts the sample lands only on the
supplied provider. `TestNewClient_NilMeterProvider` pins the nil rejection.

---

## Semconv verification (`verify-semconv-attributes`)

Pinned version: **v1.39.0** (`docs/semconv.md`, all SDK-owned imports). Source
grepped: `$(go env GOMODCACHE)/go.opentelemetry.io/otel@v1.44.0/semconv/v1.39.0/attribute_group.go`.

| Constant | Verbatim key | Deprecated predecessor (not used) |
|---|---|---|
| `DBSystemNameKey` / `DBSystemNameElasticsearch` | `db.system.name` = `"elasticsearch"` | `db.system` |
| `DBOperationNameKey` | `db.operation.name` | `db.operation` |
| `DBCollectionNameKey` | `db.collection.name` | (ES: `db.elasticsearch.path_parts.index` path variable) |
| `ServerAddressKey` | `server.address` | — |
| `ServerPortKey` | `server.port` | — |
| `ErrorTypeKey` | `error.type` | — |
| `DBResponseStatusCodeKey` | `db.response.status_code` | — (HTTP status for Elasticsearch) |
| `DBNamespaceKey` (not emitted, §3) | `db.namespace` | `db.elasticsearch.cluster.name` |

The Go code references these constants, never string literals
(`docs/semconv.md` Enforcement Rule #1).

---

## Required policy artifacts (ADR 0003 / 0008)

- **`elasticsearch/doc.go`**: tier line updated — T2 facade for spans plus an
  SDK-owned justified-T3 metric layer (this ADR). The ADR 0008 §7.2 gate keys on
  the `// Tier: T2` substring and stays green; this ADR names the
  `elasticsearch/` package path so a future `// Tier: T3` annotation would also
  satisfy the gate.
- **ADR 0008 §5 table**: Elasticsearch row amended ("trace-only, metrics
  deferred" → "plus SDK-owned `db.client.operation.duration`, ADR 0027").
- **ADR 0020**: amendment recorded (§3 signature, §6 superseded).
- **`docs/semconv.md`**: Instruments table added to the Elasticsearch section;
  the "Any Elasticsearch metric" not-emitted row removed.

---

## Testing

Unit tests in `elasticsearch/client_test.go` against an `httptest` ES stub with
a `ManualReader`:

- one sample per request under scope `github.com/flywindy/o11y/elasticsearch`,
  unit `s`, with the exact §3 label set and **no** legacy span keys; exemplar
  span id equals the ES CLIENT span's;
- `error.type` = `"500"` (low-level 5xx), `"400"` (typed `ElasticsearchError`),
  each paired with the same `db.response.status_code`; `*net.OpError` for a
  refused connection on both the low-level and the typed client (the latter
  proves the `fmt` unwrapping), with no `db.response.status_code`;
  `"context.Canceled"` for a caller cancellation; and present-but-not-`"200"`
  for a product-check failure on a 200;
- a retried 503 → 200 is one sample with no `error.type` and no
  `db.response.status_code`; a retried 503 → transport error carries the
  transport class and no stale `"503"`;
- `db.collection.name` policy: single index, wildcard kept, multi-index
  omitted, no index omitted, `WithCollectionMetricLabel(false)` omits the label
  while the span keeps its path part;
- recorded under `NeverSample()` (no exemplar) and under a no-op
  `TracerProvider`;
- nil `mp` rejected; global `MeterProvider` untouched;
- `MetricViews` pins the bucket boundaries, the allow-keys filter, and that an
  unscoped `db.client.operation.duration` does not match.

`internal/metrics` tests pin the cap rules: both export paths carry a scoped,
independently budgeted rule for the Elasticsearch scope, and an end-to-end
Prometheus scrape shows overflow indices collapsing to `"other"` without
evicting a Cassandra table admitted under its own budget.

---

## Consequences

**Positive**

- Elasticsearch gets the same unsampled `db.client.operation.duration` signal
  as the other database integrations, labeled with current semconv keys, with
  no new dependency and ~100 lines of SDK-owned code at a seam the facade
  already owned.
- Failure classification is consistent across spans and metric: the same
  terminal-outcome decision drives span status and `error.type`.
- The constructor shape converges on the SDK norm; the metric cannot be
  forgotten because the provider is positional and required.

**Negative / Trade-offs**

- Breaking signature change for every `NewClient` / `NewTypedClient` call site
  (one-line migration, documented in the CHANGELOG).
- `db.collection.name` on by default can grow with date-rolled indices; the
  per-request single-index rule, the opt-out, and the export cap bound it, and
  an `"other"` bucket appearing is the signal to opt out or raise the cap.
- Per-attempt visibility (retry amplification) is still only inferable from
  span duration; §8 names the seam and trigger for a counter.
- The metric's `server.address` reflects the terminal attempt's node; a
  request that failed over between nodes attributes its whole latency to the
  last one. Acceptable for cluster-scale dashboards; per-attempt attribution is
  the §8 counter's job.

---

## Open questions

None at acceptance. The §8 attempts counter is opened as its own ADR when its
trigger fires.

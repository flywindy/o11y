# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the project is pre-1.0 (no `v1.x.x` tag yet), breaking changes are
permitted between minor versions but are still listed here so external
adopters can plan their upgrades.

---

## [Unreleased]

### Added

- `cassandra`: **`db.collection.name` (the addressed table) is now a metric label**
  on `db.client.operation.duration` and `cassandra.query.attempts`, on by
  default. semconv v1.39.0 marks it *Conditionally Required* on the duration
  histogram — *"if readily available and if a database call is performed on a
  single collection"* — and both conditions hold for CQL, so its previous
  absence was a conformance gap. This makes per-table latency and error rate
  answerable from metrics rather than from sampled traces, gives
  `cassandra.query.attempts` a per-table view of client-side round trips
  (retries, speculative executions, paging), and lets the client-side series
  join the server-side `cassandra-exporter`, whose table-level metrics are
  labeled by `keyspace`/`table`. Note that `cassandra.query.attempts` counts
  round trips rather than retries — one attempt is recorded per observer
  callback alongside one duration sample, so their ratio is identically `1`.
  See ADR 0019's 2026-07-29 amendment.
- `cassandra`: `WithCollectionMetricLabel(bool)` opts a session out of the new
  metric label (spans keep `db.collection.name` either way).
- `o11y`: `WithMaxUniqueCollections(n)` / `DefaultMaxUniqueCollections` (200)
  cap distinct `db.collection.name` values on the Cassandra metrics at the
  export boundary, collapsing overflow to `"other"` — the same mechanism
  `WithMaxUniqueRoutes` applies to `http.route`. Because a Cassandra schema is
  DDL-fixed, an `"other"` bucket here signals that the SDK's CQL tokenizer
  mis-read a statement shape rather than that the schema grew.

### Changed

- **Series-count impact**: services using the `cassandra` integration will see
  `db.client.operation.duration` and `cassandra.query.attempts` gain a per-table
  dimension (roughly ×5 on a ten-table keyspace, since verbs and tables are
  strongly correlated rather than a full cross-product). Pass
  `cassandra.WithCollectionMetricLabel(false)` to `NewSession` to keep the
  previous label set.

### Fixed

- `internal/metricscap`: a real label value equal to the overflow sentinel
  (`"other"`) no longer bypasses the cardinality budget. It previously
  short-circuited before the budget check, so a cap of N could export N+1
  distinct values — reproducible on both `db.collection.name` (a Cassandra table
  may be named `other`) and `http.route`. It now consumes a slot like any other
  value; no exported label value changes, because `"other"` is what was returned
  in either case. A real `other` still merges with the overflow bucket once the
  cap is reached — that half needs an out-of-band sentinel and is tracked
  separately.

### Notes

- ADR 0002 §7 gained a 2026-07-29 addendum defining when a *schema-level* label
  (table, collection, bucket, topic) is admissible as a metric label, so this is
  a cross-integration rule rather than a Cassandra one-off.
- MongoDB parity is **upstream-blocked** and deliberately not shipped: its
  `db.client.operation.duration` is emitted by contrib `otelmongo`, and views
  can filter attributes but never add them. Upstream also omits `db.namespace`
  on the failure path while emitting it on success, so widening the SDK
  allowlist alone would break error-rate grouping. See ADR 0014's 2026-07-29
  amendment.

---

## [0.9.0] - 2026-07-28

### Changed

- `nats`: upgraded the underlying instrumentation from
  `github.com/Marz32onE/instrumentation-go/otel-nats` v0.2.11 to
  `github.com/akira-core/instrumentation-go/otel-nats` **v0.7.0** (upstream
  renamed its module path to the `akira-core` org in v0.6.0). The v0.5.x line
  was skipped; see `docs/upstream-otel-nats.md` and ADR 0004's 2026-07-09 and
  2026-07-16 amendments for the re-audits. v0.7.0 cleared almost the entire
  upstream backlog — see the dedicated items below.
- `nats`: **NATS tracing is now on by default.** `o11ynats.Connect` passes
  `otelnats.WithTracingEnabled(true)` (new in upstream v0.7.0), so tracing
  follows the SDK's own tracer toggle instead of the two process-wide env vars
  (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` + `OTEL_NATS_TRACING_ENABLED`) the
  upstream previously required, default off. With a real TracerProvider you get
  spans and W3C context propagation; with the noop provider (SDK tracing
  disabled) NATS spans are noop and, because there is no recording span, no
  active trace context is propagated while the pillar is off — correlation is
  inactive, not broken, and resumes when the trace pillar is enabled. No env
  vars are needed for the NATS examples or tests anymore.
- `nats`: **span kinds corrected** (upstream v0.7.0). The reply "receive" span
  and the JetStream pull-**receive** spans (`Next`/`Messages`/`Fetch`/
  `FetchBytes`/`FetchNoWait`) are now `CLIENT` (were `CONSUMER`); `publish`
  stays `PRODUCER`. The JetStream `Consume` callback span and the core Subscribe
  handler span keep `process` semantics and stay `CONSUMER` (they did **not**
  change). Pull-receive spans also carry `messaging.operation.type=receive`.
- `nats`: JetStream consumer spans now attach the consumer/durable name under
  the semconv v1.39.0 key `messaging.consumer.group.name` (was the non-semconv
  literal `messaging.consumer.name`), resolving the last deviation in
  `docs/semconv.md`.
- `nats`: the requester-side reply "receive" span is now recorded by upstream
  `recordReply` instead of the facade. Its topology changed accordingly — it
  is named for the reply **inbox** (`receive {inbox}`, not `receive
  {subject}`) and parents under the **responder's** trace with a link back to
  the request, rather than as a child of the caller's ctx. The two sides are
  now two linked traces, not one. It is also emitted (without a link) for
  replies that carry no trace context. See `docs/semconv.md` and
  `docs/guide.md`.

### Added

- `nats`: `Conn.RequestMsg(ctx, msg, timeout)` — a ctx-first shadow of the
  embedded upstream `RequestMsg(msg, timeout)`, whose ctx-less signature
  parents its producer span to `context.Background()` and orphans the trace.
  Use it for requests that must carry headers.
- `nats`: `Consumer.Next(ctx, opts...)` is now wrapped (upstream v0.6.0 fixed
  it to return the local receive-span context).
- `nats`: `ConsumeContext` now exposes `Drain()` and `Closed()` in addition to
  `Stop()` (upstream now mirrors the native `jetstream.ConsumeContext`).
- `nats`: `MessageBatch.Stop()` abandons an in-flight `Fetch`/`FetchBytes`
  batch, releasing the facade's forwarding goroutine and — by cancelling the
  fetch context — the upstream goroutine and its NATS pull subscription.

### Fixed

- `nats`: an abandoned, still-waiting `Fetch`/`FetchBytes` batch no longer
  leaves a forwarding goroutine and NATS pull subscription parked until the
  pull expires; `MessageBatch.Stop()` now cancels the fetch context to drain
  them promptly. `MessageBatch.Error()` no longer reports the
  `context.Canceled` that Stop itself triggers (a caller-ctx cancellation
  without Stop is still surfaced). Upstream v0.7.0 additionally fixes the
  forwarding goroutine to observe `Stop` while parked *receiving* from the
  native batch (not just while sending), so the facade's fetch-context
  workaround is belt-and-braces rather than load-bearing.
- `nats`: JetStream trace-context extraction now links messages whose trace
  headers were written under a canonicalized key (e.g. `Traceparent` rather
  than lowercase `traceparent`). Upstream v0.7.0's `HeaderCarrier` implements
  `propagation.ValuesGetter` and falls back to MIME-canonical and case-folded
  lookups, resolving the header-casing limitation previously documented in
  `nats/jetstream.go` (ADR 0022 2026-07-03 amendment).
- `nats`: `Consumer.Next` now honors live context cancellation (upstream wires
  `jetstream.FetchContext`), so cancelling a deadline-less ctx mid-wait aborts
  the pull promptly instead of blocking for the ~30s default max wait.
- `nats`: request/reply "send" (CLIENT) spans no longer overwrite
  `messaging.message.body.size` with the reply payload size after the round
  trip — it now always reports the request size. Core request/reply send spans
  additionally carry `messaging.message.conversation_id` (the reply inbox),
  joining the requester's send/receive spans and the responder's process span
  by attribute query, not just by span link.

### Breaking Changes (Migration Guide)

- `nats.Conn.Request`'s trailing variadic `attrs ...attribute.KeyValue`
  parameter (added in 0.8.0) was **removed**. The reply "receive" span is now
  created inside otel-nats v0.6.0, which offers no caller-attribute injection
  point. Call sites passing `attrs` no longer compile: drop the extra
  arguments and attach domain identifiers (request/correlation IDs, room/site
  IDs) to your own ambient span instead. Interfaces/variables typed against
  the 0.8.0 variadic signature revert to the non-variadic
  `func(context.Context, string, []byte, time.Duration) (*nats.Msg, error)`.
- `nats.ConsumeContext` gained `Drain()` and `Closed() <-chan struct{}`. Any
  test double/fake implementing this interface must add those two methods.
- `nats.MessageBatch` gained `Stop()`, and `nats.Consumer` gained
  `Next(ctx, opts...)`. Test doubles implementing either interface must add
  the new method(s).
- The import path for the NATS instrumentation dependency changed from
  `github.com/Marz32onE/...` to `github.com/akira-core/...`. Services import
  `github.com/flywindy/o11y/nats` (not the upstream directly), so this is
  transparent; only code that imported the upstream package directly (against
  policy) is affected.
- **Span kinds changed (v0.7.0).** The reply "receive" span and the JetStream
  pull-**receive** spans (`Next`/`Messages`/`Fetch`/`FetchBytes`/`FetchNoWait`)
  moved from `CONSUMER` to `CLIENT`. The JetStream `Consume` callback span and
  the core Subscribe handler span stay `CONSUMER` (they are `process` spans and
  did not change) — do not move their dashboard filters. Update any
  Tempo/dashboard queries or span-metrics rules that filter the pull-receive /
  reply spans by `SpanKind`.
- **`messaging.consumer.name` → `messaging.consumer.group.name` (v0.7.0).**
  The consumer/durable name now lives under the semconv v1.39.0 key. Update
  dashboards/queries keyed on the old attribute.
- **Batch / `Messages` receive-span durations shortened (v0.7.0).** These
  spans now end at handover (before the message reaches your loop body) instead
  of when the next message arrives. Any per-message enrichment via
  `trace.SpanFromContext(m.Ctx).SetAttributes(...)` was already a no-op and
  stays one — start your own child span with `tracer.Start(m.Ctx, ...)` for
  per-message work. Latency panels reading these span durations will show
  shorter values (receive-to-handover, not receive-to-processing).
- **`Consumer.Next` + `FetchMaxWait` (v0.7.0).** Calling `Next` with a
  cancelable ctx (`WithCancel`/`WithTimeout`/`WithDeadline`) *and* a
  caller-supplied `jetstream.FetchMaxWait` opt now returns
  `jetstream.ErrInvalidOption` (upstream rejects `FetchContext` + `FetchMaxWait`
  together). Migration: express the bound via the ctx deadline
  (`context.WithTimeout`) instead of a separate `FetchMaxWait`;
  `context.Background()` + `FetchMaxWait` keeps working unchanged.
- **Deliver spans removed (v0.7.0).** The upstream synthetic "deliver" span
  (an implicit `OTEL_EXPORTER_OTLP_ENDPOINT`-gated second exporter with no
  sampler) is gone, and the package no longer reads that env var for span
  emission. Deployments that had set the env var to opt into deliver spans (or
  had avoided setting it to suppress them) will no longer see the Grafana
  service-graph broker node those spans produced.

---

## [0.8.0] - 2026-07-03

### Added

- `nats`: JetStream `Consumer.Fetch` / `FetchBytes` / `FetchNoWait` are now
  wrapped (ADR 0022 amendment, 2026-07-01), closing the largest remaining
  chat-integration gap — batch pull consumers previously had to import the
  upstream `oteljetstream` package directly to get trace context, or got none
  at all. Each delivered message arrives as a `FetchedMessage{Ctx, Msg}` on
  the new `MessageBatch.Messages()` channel, mirroring the `(ctx, msg)` shape
  `Consume`/`Messages` already deliver. The forwarding channel is buffered to
  each request's own message-count bound where one exists (`batch` for
  `Fetch`/`FetchNoWait`), so abandoning `Messages()` before reading everything
  is safe for those two — the forwarding goroutine always drains the whole
  batch and exits on its own. `FetchBytes` has no message-count bound, so its
  buffer is a fixed best-effort size; see the new `examples/jetstream/fetch-worker`
  for the full drain pattern and `docs/guide.md` for the full caveat. `Fetch`
  and `FetchBytes` also plumb `ctx` into the native pull request via
  `jetstream.FetchContext`, so cancelling `ctx` after the call returns ends
  the fetch early instead of running to the default/`FetchMaxWait` timeout
  (falls back to the caller's own `FetchMaxWait`, unmodified, when one is set
  explicitly — the two are mutually exclusive upstream); `FetchNoWait` has no
  `FetchOpt` to plumb `ctx` into, so it stays a registration-time guard only,
  same as `Consume`/`Messages`. The fallback is scoped precisely to that one
  collision (not any `jetstream.ErrInvalidOption`): pairing a tight ctx
  deadline with an explicit `jetstream.FetchHeartbeat`, for example, triggers
  a different native rejection, and is surfaced as an error rather than
  silently falling back to an unbounded fetch that ignores the caller's
  deadline. Documented caveat (no code change, upstream behavior): buffering
  the forwarding channel means a batch message's receive span (`m.Ctx`) may
  already be ended by the time your loop body runs for messages after the
  first — `trace.SpanFromContext(m.Ctx).SetAttributes(...)` is unreliable
  there; use log correlation or a child span instead, as
  `examples/jetstream/fetch-worker` does.
- `nats`: `Conn.Request` now closes the requester-side half of the
  request/reply round trip. When the reply carries a trace context (the
  responder replied via `Conn.Respond`), `Request` starts a `receive
  {subject}` span, as a child of the caller's ctx, linking back to the
  responder's reply-send span — previously the handler → requester leg was
  invisible in Grafana Tempo. `Request` also takes an optional variadic
  `attrs ...attribute.KeyValue` attached to that span, for domain identifiers
  (a request ID, a room/site ID) the SDK cannot infer on its own; a supplied
  attr that reuses one of the span's own base keys is dropped in favor of the
  base value, matching the `redis.WithAttributes` / `cassandra.WithAttributes`
  "built-in wins" precedent elsewhere in the SDK. Known limitation (documented,
  not code-worked-around): the reply-link span only lands in the same trace
  as the call's own send span when `ctx` already carries an active span at
  the `Request` call site — a bare `ctx` (e.g. a background worker's own
  top-level request loop) gets two disconnected root traces tied together
  only by a Link. Open a span before calling `Request` if that round trip
  needs to appear as one trace; see `docs/guide.md`'s Request section.
- `examples/nats-ws-browser`: extracted the inline browser receive-span logic
  into a reusable `receiveWithSpan(msg, { name, attributes }, callback)`
  helper in `src/tracing.js`, documented as the pattern for any nats.ws
  consumer that needs to appear correlated with a Go backend's distributed
  trace. Same "built-in wins" collision protection as `Conn.Request` above.
- `redis`: command-noise filtering (ADR 0013 amendment). The wrapper now
  unconditionally skips connection-lifecycle commands the go-redis client
  issues itself (`AUTH`, `HELLO`, `SELECT`, `READONLY`, and the auto-issued
  `CLIENT SETINFO` / `CLIENT SETNAME`) in addition to the existing Pub/Sub
  filter — these are never application units of work, and skipping `AUTH` also
  keeps credentials out of `db.query.text`. Two new options let applications
  suppress preference-based noise: `redis.WithIgnoredCommands(names ...string)`
  drops named commands (e.g. health-check `PING`, `INFO`) by verb,
  case-insensitively; `redis.WithRequireParentSpan(true)` drops commands issued
  without an active parent span (background probes, keepalive, topology
  refreshes). Both suppress the span and the `db.client.operation.duration`
  sample. Deliberate `CLIENT` subcommands such as `LIST` remain instrumented.

### Fixed

- `nats`: `Inject`/`Extract` (and the new `Conn.Request` reply-link
  extraction) previously used `propagation.HeaderCarrier`, which
  canonicalizes header keys to MIME form (`"traceparent"` →
  `"Traceparent"`). `nats.Header.Get`/`Set` is case-sensitive with no
  canonicalization, so this silently failed to read back headers written by
  `otel-nats` itself (or by any other W3C-compliant writer using the literal
  lowercase key) — the bug was self-masked as long as both `Inject` and
  `Extract` were only ever used against each other. Both now use a
  case-sensitive carrier backed directly by `nats.Header`.
- `nats`: the new case-sensitive header carrier didn't implement
  `propagation.ValuesGetter` (`propagation.HeaderCarrier`, the type it
  replaced, does), so `propagation.Baggage.Extract` silently fell back to
  single-value extraction and dropped every baggage member after the first
  whenever a message carried baggage split across more than one repeated
  `baggage` header instance. Fixed by adding a `Values` method.
- `examples/jetstream/fetch-worker`: removed a dead `ctx.Err() != nil` check
  after a failed `Fetch` — `ctx` is only cancelled by `main`'s deferred
  `cancel()`, which runs after the loop has already returned via `<-quit`, so
  the branch could never fire; its misleading comment claimed otherwise.
  Shutdown was already handled correctly by the `<-quit` cases elsewhere in
  the loop, so this is a dead-code removal, not a behavior change.
- `nats`: `Conn.Request`'s reply-link span was missing
  `messaging.message.conversation_id` (the request/reply inbox), even though
  `docs/semconv.md` already documented it as part of this span's attribute
  set and `otel-nats`'s own send span emits it via `msg.Reply`. Without it,
  the reply-link span couldn't be correlated or filtered by inbox alongside
  the send/process spans for the same request/reply exchange. Fixed by
  passing the reply's own `Subject` (the inbox it was delivered to) through
  to `replyAttrs`.
- `nats`: documented (not fixed — outside this package's control) that
  `Consume`/`Messages`/`Fetch`/`FetchBytes`/`FetchNoWait` extract per-message
  trace context entirely inside the vendored `oteljetstream` package, whose
  own header carrier has no fallback for a canonicalized key ("Traceparent"),
  unlike this package's own `Extract`. A message written by a
  pre-`headerCarrier`-fix version of this SDK (or by any other canonicalizing
  producer) arrives unlinked from its producer's trace when consumed through
  those five methods. Self-resolves once such messages drain from any durable
  streams; see `nats/jetstream.go`'s package doc comment and the ADR 0022
  amendment for the full explanation of why this can't be fixed from this
  facade without reimplementing `oteljetstream`'s consumer-span creation.

### Breaking Changes (Migration Guide)

- `nats.Conn.Request` gained a trailing variadic `attrs ...attribute.KeyValue`
  parameter. Direct calls are unaffected (the parameter is optional), but a
  variadic method has a different method type than a non-variadic one in Go:
  code that assigns `conn.Request` to a `func(context.Context, string, []byte,
  time.Duration) (*nats.Msg, error)`-shaped variable, or asserts `*nats.Conn`
  against a hand-written interface with that exact method signature (a common
  pattern for mocking in tests), needs the interface/variable type updated to
  include the variadic parameter.
- `nats.Consumer` gained three new methods (`Fetch`, `FetchBytes`,
  `FetchNoWait`). Any test double/fake that implements `nats.Consumer` for
  JetStream worker tests needs those three methods added to keep satisfying
  the interface, even if the test doesn't exercise batch pull.

---

## [0.7.1] - 2026-06-23

### Changed

- Upgraded the core OpenTelemetry module set
  (`otel` / `metric` / `trace` / `sdk` / `sdk/metric`) from the
  `v1.43.1-0.20260521080857-e5bdc311108b` pre-release pseudo-version to the
  tagged `v1.44.0` release, and aligned the stable OTLP HTTP exporters
  (`otlpmetrichttp` / `otlptracehttp`) to `v1.44.0`. The pseudo-version set was
  only pulled in transitively because the official contrib `otelmongo`
  (mongo-driver/v2) commit required an unreleased core; the newer `otelmongo`
  commit now depends on tagged `v1.44.0`, so downstream consumers no longer
  inherit a pre-release core. The contrib `otelmongo` module itself remains a
  `v0.0.0-…` pseudo-version because upstream has not tagged that module path yet
  (bumped to `v0.0.0-20260622212340-49857026d46e`, which also moves
  `go.mongodb.org/mongo-driver/v2` to `v2.7.0`).

---

## [0.7.0] - 2026-06-23

### Added

- `nats.Conn.JetStream()` now returns an o11y-owned JetStream facade
  (`JetStream` / `Stream` / `Consumer` interfaces) so callers import only
  `github.com/flywindy/o11y/nats`, never the upstream instrumentation package,
  while keeping its trace propagation and consumer span-links. Covers stream and
  consumer management, `Publish` (with `jetstream.WithMsgID` dedup pass-through),
  and the pull consume modes `Consume` / `Messages`; handlers receive the native
  `jetstream.Msg` plus a trace-carrying `ctx` (the same `(ctx, msg)` shape as
  core `Subscribe`). `Consume` and `Messages` take a registration-time `ctx`
  (up-front guard, consistent with `Subscribe`; not a trace carrier and not a
  running-loop cancel). See ADR 0022 (Phase 2) and its amendment.
- ADR 0019: `cassandra` package — T3 SDK-owned observers over
  `github.com/gocql/gocql`. `cassandra.NewSession` builds an instrumented
  `*gocql.Session` from a caller-supplied `*gocql.ClusterConfig`, wiring the
  driver's query and connect observers to explicit SDK tracer/meter providers
  (no OpenTelemetry globals). Emits CLIENT spans (one per attempt and per page,
  semconv v1.39.0 attributes), the `db.client.operation.duration` histogram, a
  Cassandra-unique `cassandra.query.attempts` retry/speculative counter, and
  optional connect metrics (labeled with `db.client.connection.pool.name`,
  synthesized from the contact point or set via `cassandra.WithPoolName`). CQL
  statement text is opt-in via `cassandra.WithQueryText(true)`; the
  contacted-coordinator host topology (`network.peer.*` /
  `cassandra.coordinator.*`) is opt-in via `cassandra.WithHostAttributes(true)`.
  `cassandra.ExecuteBatch` (plus `cassandra.ExecuteBatchCAS` /
  `cassandra.MapExecuteBatchCAS` for conditional batches) is the SDK-owned batch
  seam (one span per logical batch with `db.operation.batch.size`).
  `cassandra.MetricViews` is composed into `o11y.Init`.
- ADR 0020: `elasticsearch` package — a T2 facade (`NewClient`,
  `NewTypedClient`, `WithSearchBody`) that wires the SDK `TracerProvider` into
  `github.com/elastic/go-elasticsearch/v8`'s first-party OpenTelemetry
  instrumentation without touching OTel globals. Trace-only in v1 (no
  `MeterProvider` / propagator parameters); search-body capture is opt-in and
  off by default. The pinned `elastic-transport-go/v8 v8.8.0` emits legacy
  semconv keys (`db.system`, `db.operation`, `db.statement`,
  `db.elasticsearch.*`), documented in `docs/semconv.md` and pinned by a
  compatibility test. The facade adds two thin response-side normalizations the
  bare upstream lacks: it guards search-body capture against a nil body, and it
  records `http.response.status_code` and marks span status = Error for ES HTTP
  error responses (status > 299, the client's own `IsError` boundary, which it
  otherwise returns as a non-error). The status is decided at `Close` from the
  final attempt, so a retried 5xx→2xx stays successful and a product-check
  failure on a 200 stays Error. Span names follow the cross-package
  `{system.name}.{operation} {target}` convention (ADR 0023), e.g.
  `elasticsearch.search my-index`, rewritten from the upstream's bare endpoint id.
- k8s infrastructure: monitor stack (`base/monitor/`) and per-datastore
  Kustomize Components (`base/components/{nats,mongodb,redis,minio}`) split
  so the monitor stack is always deployed together while datastores are
  applied on-demand only when running the relevant example.

### Changed

- **Breaking:** `nats.Conn.JetStream()` returns the o11y `nats.JetStream`
  interface instead of `oteljetstream.JetStream`, and JetStream config / consume
  callbacks now use `github.com/nats-io/nats.go/jetstream` types rather than
  `oteljetstream` aliases. Update JetStream call sites accordingly — the
  `examples/jetstream/*` programs show the new shape. Deferred (not wrapped yet):
  single-message `Consumer.Next`, `Fetch`/`FetchBytes`/`FetchNoWait`, push
  consumers, and ordered consumers.

---

## [0.6.0] - 2026-06-14

### Added

- ADR 0024: `obsctx` package (`Detach`, `DetachWithTimeout`, `Go`) for carrying
  observability/trace context into background work that outlives a request,
  without inheriting the request's cancelation or deadline. Prevents the
  `context canceled` / `connection reset by peer` failures caused by threading a
  request-scoped context (or `*gin.Context`) into post-response / fire-and-forget
  work.
- `o11ytest` package (`CanceledRequestContext`, `RequireNotCanceled`) — test
  helpers that turn the above into a deterministic, traffic-independent
  regression test.
- `tools/lint/o11y-context.yml` — a semgrep ruleset to self-audit for
  request-scoped contexts captured by goroutines.
- `gin.WithSkipPaths` — convenience option that excludes common Kubernetes probe
  and metrics-scrape endpoints (`/health`, `/healthz`, `/livez`, `/readyz`,
  `/metrics`, `/ping`, `/ready`, `/live`) from tracing without hand-writing a
  `WithFilter`. `WithSkipPathPrefixes` opts in to prefix-based skipping (e.g.
  `/health/` to cover `/health/probe`). `DefaultSkipPaths()` returns the
  built-in list for inspection or composition.

### Changed

### Deprecated

### Removed

### Fixed

### Security

### Breaking Changes (Migration Guide)

---

## [0.5.0] - 2026-06-11

### Added

- ADR 0023: cross-package span-naming convention
  `{system.name}.{operation} {target}` for data-store integrations.
- Added `nats.Conn.Respond(ctx, msg, data)`, a traced reply primitive for
  request/reply handlers. It validates the message (non-nil, non-empty reply
  subject) and routes the reply through the traced publish path so the response
  carries trace context in its headers — unlike raw `msg.Respond`, which skips
  header injection and breaks the distributed trace (ADR 0004 §5, ADR 0022).

### Changed

- **Span names** for the `mongo` and `minio` integrations now follow the
  unified convention `{system.name}.{operation} {target}` (ADR 0023):
  - `mongo`: `users.find` → `mongodb.find users` (commands with no
    collection, e.g. `ping`, are `mongodb.ping`).
  - `minio`: `PutObject media` → `s3.PutObject media`.
  - `redis` was already `redis.GET` / `redis.pipeline` and is unchanged.
  Span/metric **attributes** are unchanged; only the human-facing span name
  moved. For `minio`, which exposes a public `WithSpanNameFormatter`, a
  caller-supplied formatter is still used verbatim (the system prefix is part
  of the default name only); `mongo` wires its formatter into the otelmongo
  monitor and does not accept a user-provided one.

### Deprecated

### Removed

### Fixed

### Security

### Breaking Changes (Migration Guide)

- **Data-store span names changed (`mongo`, `minio`).** Any trace-search
  query (e.g. Tempo TraceQL `{ name = "users.find" }`), saved Jaeger
  operation filter, or `spanmetrics` connector keyed on the old span names
  must be updated:
  - `"<collection>.<command>"` → `"mongodb.<command> <collection>"`
  - `"<Operation> <bucket>"` → `"s3.<Operation> <bucket>"`
  `redis` span names are unchanged. Span/metric attributes are unchanged, so
  any dashboard aggregating on `db.operation.name`, `db.collection.name`,
  `object_store.operation.name`, etc. is unaffected.

---

## [0.4.0] - 2026-06-08

### Added

- Added `WithSamplingRatio` and `WithTraceSampler` so services can configure
  SDK-side head sampling explicitly while preserving OpenTelemetry
  `OTEL_TRACES_SAMPLER` / `OTEL_TRACES_SAMPLER_ARG` support when no typed
  sampler is set.
- Documented trace sampling guidance, including the head-vs-tail sampling split,
  high-throughput producer recommendations, and `OTEL_BSP_*` batch span
  processor environment variables.
- Added explicit user identity helpers: `SetUser(ctx, name)` records
  `user.name` on the current span, and `UserName(name)` returns a `slog.Attr`
  for adding the same semantic-convention key to log records. These Phase 1
  helpers are in-process only and do not propagate usernames across service
  boundaries.
- Added opt-in user identity baggage propagation: `ContextWithUser(ctx, name)`
  stores `user.name` in W3C Baggage, and `WithUserBaggage()` materializes the
  whitelisted value onto this service's spans and SDK log records. The feature
  is off by default because `user.name` is PII and can cross HTTP/NATS
  boundaries via baggage propagation.
- Added MongoDB connection-pool metrics for ADR 0014 Phase 2:
  `db.client.connection.count`, `db.client.connection.max`,
  `db.client.connection.idle.min`, `db.client.connection.pending_requests`,
  `db.client.connection.timeouts`, and `db.client.connection.create_time`.
  The pool gauges and timeout counter use the synchronous instrument kinds
  defined by OpenTelemetry semantic conventions v1.39.0.
- Added `mongo.WithPoolName` to set `db.client.connection.pool.name` on
  SDK-owned MongoDB pool metrics.

### Changed

- MongoDB instrumentation now uses the official contrib `otelmongo`
  `CommandMonitor` for both command spans and `db.client.operation.duration`
  metrics. Command spans are always-on and controlled by the SDK sampler; the
  old Marz `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` /
  `OTEL_MONGO_TRACING_ENABLED` gates are no longer read by the o11y MongoDB
  facade. Span shape changes from Marz logical operation spans to contrib wire
  command spans (for example, `insert orders` becomes `orders.insert`, address
  attributes move from `server.*` to `network.peer.*`, and cursor reads may add
  `getMore` spans).

### Deprecated

### Removed

- Removed the Marz MongoDB wrapper dependency and `_oteltrace` document
  propagation support from the o11y MongoDB package.

### Fixed

### Security

### Breaking Changes (Migration Guide)

- `mongo.Connect` now returns a plain
  `*go.mongodb.org/mongo-driver/v2/mongo.Client` instead of the o11y wrapper
  client. Standard driver `Database`, `Collection`, and result types flow
  through application code without wrapper types.
- `mongo.WithDocumentTracePropagation` was removed. The SDK no longer writes
  `_oteltrace` into persisted business documents; model async trace continuity
  on an outbox/event envelope and message headers instead.
- Services that build their own `*options.ClientOptions` should call
  `mongo.Instrument(opts, obs.TracerProvider(), obs.MeterProvider(),
  obs.Propagator)` before the driver's `mongo.Connect(opts)`. The returned
  cleanup function disables SDK-owned MongoDB pool event handling and should be
  deferred near the client's `Disconnect`, after the final metrics flush when a
  last zero-value pool snapshot is required.

---

## [0.3.0] - 2026-06-01

### Added

- New `redis/` package: an SDK-owned T3 wrapper over
  `github.com/redis/go-redis/v9` that emits OTel semconv v1.39.0 spans and pool
  metrics for single, Cluster, and Ring topologies. Public surface is
  `Wrap` / `Unwrap` / `MetricViews` / `WithCommandTextEnabled` /
  `WithAttributes` / `WithPoolName`. `Wrap` is idempotent and installs its hook
  before any caller hooks so the span encloses them; `WithAttributes` cannot
  override the SDK's built-in semconv keys. See ADR 0013.
- `internal/metrics.Config.ExtraViews` lets integration packages contribute
  metric views at `MeterProvider` construction time; `o11y.Init` now composes
  `o11yredis.MetricViews()` automatically so Redis pool-metric labels stay
  within the SDK cardinality contract.
- MongoDB operation-duration metrics via the official contrib `otelmongo`
  CommandMonitor in metrics-only mode. The `mongo` wrapper now emits
  `db.client.operation.duration` with bounded labels and keeps existing Marz
  tracing behavior unchanged. See ADR 0014.
- Added the official MongoDB contrib instrumentation dependency, which requires
  `go.mongodb.org/mongo-driver/v2 v2.6.0` and the corresponding OTel
  `v1.43.1-0.20260521080857-e5bdc311108b` pseudo-version set.

### Changed

- `db.client.operation.duration` from both the `mongo` and `redis` wrappers now
  honors the SDK's configured histogram boundaries (`WithHistogramBuckets`),
  matching the HTTP duration histograms. Previously the MongoDB metric inherited
  the contrib instrument's baked-in boundaries and the Redis metric inherited
  the SDK default boundaries. `mongo.MetricViews` and `redis.MetricViews` now
  take a `[]float64` buckets argument. Each view is also scoped to its own
  instrumentation so the two `db.client.operation.duration` views no longer
  match each other's instrument (which produced a conflicting stream with the
  wrong attribute filter when both wrappers were active in one process).

### Security

- Bumped indirect dependencies `golang.org/x/crypto` to `v0.52.0` and
  `golang.org/x/net` to `v0.55.0` to clear the GO-2026-50xx advisory set. Both
  are transitive (the SDK does not import them directly) and the affected
  `ssh`/`html`/`idna` paths are unused, but the pins are raised to the fixed
  versions to keep vulnerability scanners clean.

### Breaking Changes (Migration Guide)

- `mongo.Connect` now requires an explicit `metric.MeterProvider` argument:
  pass `obs.MeterProvider()` between `obs.TracerProvider()` and
  `obs.Propagator`. This preserves the SDK's no-global-provider policy while
  enabling MongoDB operation-duration metrics.

---

## [0.2.0] - 2026-05-22

### Added

- `WithExemplars(bool)` controls OpenMetrics content negotiation on the
  Prometheus pull `/metrics` handler. Defaults to `true` so per-bucket
  exemplars carrying `trace_id` / `span_id` flow through to Grafana / Tempo
  by default. Set `false` only as a temporary mitigation for services
  whose dashboards / recording rules / alert rules hardcode integer
  histogram bucket boundaries — see the Breaking Changes entry below.

### Changed

### Deprecated

### Removed

### Breaking Changes (Migration Guide)

- The default Prometheus pull handler now content-negotiates to the
  OpenMetrics exposition format when scrapers advertise it (which real
  Prometheus does on every scrape). OpenMetrics is the only format
  otelprom emits per-bucket exemplars in, so this is what makes
  trace-to-metric linkage actually work — previously the SDK built
  exemplars in memory and discarded them at serialization time, leaving
  Prometheus's exemplar store empty regardless of
  `--enable-feature=exemplar-storage`.

  The renegotiation also normalises integer histogram bucket boundaries
  on the rendered `le` label. The underlying `float64` values are
  unchanged, but the text differs:

  | Prometheus text format (pre-v0.2) | OpenMetrics (v0.2+, default) |
  |---|---|
  | `le="1"` | `le="1.0"` |
  | `le="5"` | `le="5.0"` |
  | `le="10"` | `le="10.0"` |

  Queries that hardcode the integer form silently stop matching:

  - `http_server_request_duration_seconds_bucket{le="1"}` → no series
  - `histogram_quantile(0.95, sum by (le) (rate(..._bucket[5m])))` →
    unaffected (aggregates across all buckets)

  **Migration**: audit dashboards, recording rules, and alert rules for
  literal integer `le` values that match the SDK's default bucket set
  (`1`, `5`, `10`). Update to the `.0` form. If you cannot stage the
  update before rolling the SDK, set `o11y.WithExemplars(false)` on the
  affected service to keep the handler on plain Prometheus format and
  preserve the integer `le` labels; trace-to-metric exemplars stay
  disabled until the option is removed.

### Fixed

- Enable OpenMetrics format on the Prometheus pull `/metrics` handler so
  exemplars actually reach scrapers. `promhttp.HandlerFor` used the
  default `HandlerOpts{}`, which leaves `EnableOpenMetrics` off; content
  negotiation then always returned the plain Prometheus exposition
  format, and that format has no syntax for per-bucket exemplars. As a
  result, even with Prometheus running
  `--enable-feature=exemplar-storage` and a sampled trace context
  attached to the measurement, exemplar storage stayed empty and
  Grafana's histogram-to-trace links were dead. The new
  `WithExemplars(bool)` option (default `true`) wires
  `EnableOpenMetrics` so SDK-managed HTTP histograms and caller-defined
  histograms both gain working exemplars; the bucket-boundary
  serialisation change is documented under Breaking Changes.

### Security

---

## [0.1.0] - 2026-05-22

First tagged release. Subsequent minor versions may still introduce breaking
changes per the pre-1.0 policy noted above.

### Added

- **`WithExtraHTTPServerAttributeKeys(keys ...string)`** to promote
  caller-controlled attributes (e.g. `app_name`, `bot_name`) onto the
  SDK-managed `http.server.request.duration` series. By default that view
  keeps only `http.request.method`, `http.route`, and
  `http.response.status_code` to bound cardinality; any other attributes
  attached via `o11ygin.WithMetricAttributesFn` / `otelhttp` were silently
  dropped from the exported series. The option appends user-supplied keys
  to the view's allow-list so they participate in PromQL aggregations.
  Keys are checked at startup against the full Prometheus label-name
  normalization the otelprom exporter applies (non-alphanumeric →
  `_`, runs collapsed, leading digits prefixed with `key_`). The SDK
  drops — with a structured `WARN` log — any key that, after
  normalization, collides with a built-in SDK label (e.g.
  `"http_route"`, `"http.route"`, `"http-route"`, `"http__route"` all
  shadow the existing `http_route`), collides with another
  caller-supplied key (e.g. `"app.name"` + `"app_name"`), or
  normalizes to an invalid label name. Accepting either kind of
  collision would silently merge two attribute values into one
  exported label and corrupt PromQL grouping for that dimension.
  Cardinality is the caller's responsibility — prefer enumerable
  values with bounded keyspaces. Has no effect when
  `WithDisableDefaultViews` is set.

- **Per-pillar feature toggles** for progressive SDK adoption:
  `WithTraceEnabled(bool)`, `WithMetricsEnabled(bool)`, `WithLogEnabled(bool)`,
  and `WithProfilingEnabled(bool)`. Trace, metrics, and log default to `true`
  (no change to existing behaviour); profiling defaults to `false` because it
  is an opt-in fourth signal. When a pillar is disabled the SDK returns a
  no-op provider for that signal while keeping everything else fully
  operational. All four toggles are also controllable without code changes
  via the `O11Y_TRACE_ENABLED`, `O11Y_METRICS_ENABLED`, `O11Y_LOG_ENABLED`,
  and `O11Y_PROFILING_ENABLED` environment variables (same defaults as the
  code options); explicit option calls take precedence. `sdk.Toggles`
  (`FeatureToggles{Trace, Metrics, Log, Profiling}`) reports the active state
  at runtime for health-check endpoints and startup logging. Notable
  per-pillar behaviour: Trace-disabled still parses and forwards W3C
  `traceparent` headers; Metrics-disabled does not start the Prometheus HTTP
  server; Log-disabled falls back to stdout-only JSON output. Profiling is
  doubly gated: it starts only when both `WithProfilingEnabled(true)` AND a
  non-empty `WithProfilingEndpoint` are set — either alone is insufficient,
  and the SDK emits a startup `WARN` when only one of the two is configured.
  `Toggles.Profiling` reflects whether the SDK actually started a profiler.
  The `otel-profiling-go` trace-to-profile wrapper is now installed only
  after `pyroscope.Start` returns successfully, so spans no longer carry
  `pyroscope.profile.id` when the profiler failed to start.

- Continuous profiling integration via Pyroscope:
  `WithProfilingEndpoint(url)` enables Pyroscope-compatible profile pushes,
  `WithProfilingAuthHeaders(map[string]string)` forwards auth / tenant headers,
  Grafana Alloy receives profiles on `:4040`, and the infrastructure stack now
  provisions Pyroscope plus Grafana trace-to-profile links. Includes
  `examples/profiling` for end-to-end local validation.
- `WithOTLPHeaders(map[string]string)` attaches arbitrary headers to every
  OTLP/HTTP request the SDK emits across traces, logs, and OTLP-push metrics.
  Use it to authenticate against managed observability backends like Grafana
  Cloud (`Authorization: Basic ...`), Honeycomb (`X-Honeycomb-Team`), New Relic
  (`Api-Key`), Datadog (`DD-API-KEY`), or to route through a multi-tenant
  Collector (`X-Scope-OrgID`).
- `SDK.Shutdown` is idempotent: a `sync.Once` gates the closer loop and the
  cached error is returned on subsequent calls. Safe to register both in a
  `defer` and inside a signal handler without double-flushing exporters.
- `http.NewServerHandler` and `http.NewTransport` wrap `otelhttp` while
  threading the SDK TracerProvider, MeterProvider, and Propagator explicitly.
- `gin.Middleware` wraps `otelgin` while threading the SDK TracerProvider,
  MeterProvider, and Propagator explicitly, and records typed `gin.error.type`
  span events for errors pushed through `c.Error` / `c.AbortWithError`.
- `WithDisableDefaultViews()` and `WithMaxUniqueRoutes(int)` configure the
  SDK-owned HTTP metric label governance added during the `otelhttp`
  migration.
- ADR 0008 CI gate (`make adr-check` and `.github/workflows/adr-check.yml`)
  validates approved instrumentation imports, package tier annotations, and
  absence of direct OTel global setter calls.
- `nats.Conn.Subscribe` and `QueueSubscribe` reject empty `subject` (and
  empty `queue` for the queue variant) up front. An empty NATS subject
  silently matches no messages and was almost always a programming error.
- `internal/testutil` package consolidates duplicated test fixtures
  (`FakeOTLPServer`, `NewCapturingOTLPServer`, `FreeAddr`, `ScrapeMetrics`,
  `TryScrapeMetrics`, `MustShutdown`).
- ADR `0007-otlp-authentication.md` documents the `WithOTLPHeaders` design
  and rejected alternatives.
- README **Logging Guidelines** section covering PII handling, log injection,
  and attribute payload size limits.
- GitHub Actions CI: race-detector tests with coverage, golangci-lint v2,
  govulncheck, and a build of every example program.
- `Makefile` mirroring CI targets for local parity (`test`, `lint`, `vuln`,
  `examples`, `bench`, `cover`, `fmt`, `tidy`).

### Changed

- `http/` is now a Tier-2 facade over `otelhttp`; inbound HTTP requests create
  server spans, extract `traceparent`, and emit standard OTel HTTP metrics.
- `o11y.Init` registers default HTTP metric views that keep
  `http.server.request.duration` labels to method, route, and status code.
- `SDK.TracerProvider()` now returns the `trace.TracerProvider` interface and
  `SDK.MeterProvider()` now returns the `metric.MeterProvider` interface, so
  profiling and future provider wrappers can stay internal to the SDK.

### Validated

- `Init` rejects empty / non-positive / NaN / `+Inf` / unsorted histogram
  bucket lists at start-up rather than allowing the OTel SDK to emit
  silently broken histograms.

### Breaking Changes (Migration Guide)

Pre-1.0 these are technically allowed without a major-version bump, but
adopters should be aware:

#### `DefaultLatencyBuckets` is now a function

```go
// Before: package-level []float64 variable
buckets := o11y.DefaultLatencyBuckets
n      := len(o11y.DefaultLatencyBuckets)
o11y.WithHistogramBuckets(o11y.DefaultLatencyBuckets)

// After: function returning a defensive copy on each call
buckets := o11y.DefaultLatencyBuckets()
n      := len(o11y.DefaultLatencyBuckets())
o11y.WithHistogramBuckets(o11y.DefaultLatencyBuckets())
```

The motivation: the old exported slice could be mutated by any caller:
`o11y.DefaultLatencyBuckets[0] = 0.999` would silently corrupt every later
SDK initialization in the process. The function returns a fresh copy each
time, so callers can safely modify the returned slice.

#### `DefaultMetricsAddr` is now a `const` (was `var`)

```go
// Before
addr := &o11y.DefaultMetricsAddr // legal: taking address of a var
o11y.DefaultMetricsAddr = ":9090" // legal: mutation

// After
addr := o11y.DefaultMetricsAddr   // copy the const value
// Taking the address or assigning to it now fails to compile.
```

If you need to override the listen address, use `o11y.WithMetricsAddr(":9090")`
which has been the supported path since the option was added.

#### `http.New` was replaced by `http.NewServerHandler`

```go
// Before
handler := o11yhttp.New(ctx, obs.Meter("svc"))(mux)

// After
handler := o11yhttp.NewServerHandler(
    mux,
    obs.TracerProvider(),
    obs.MeterProvider(),
    obs.Propagator,
)
```

Use Go 1.22+ `http.ServeMux` patterns or router-native route patterns to keep
`http.route` bounded. The old `WithPathNormalizer` callback was removed.

#### Provider accessors now return interfaces

```go
// Before
var tp *sdktrace.TracerProvider = obs.TracerProvider()
var mp *sdkmetric.MeterProvider = obs.MeterProvider()

// After
var tp trace.TracerProvider = obs.TracerProvider()
var mp metric.MeterProvider = obs.MeterProvider()
```

Instrumentation libraries already accept these interfaces. Lifecycle methods
such as `Shutdown` and concrete-only mutation hooks remain owned by
`SDK.Shutdown` and SDK initialization.

### Fixed

- Suppress `exemplar labels have N runes, exceeding the limit of 128` noise
  on the SDK-managed HTTP histograms. The OTel SDK routes attributes
  dropped by a view's `AttributeFilter` into the exemplar's
  `FilteredAttributes`; the Prometheus exporter then asks `client_golang`
  to build an exemplar from `trace_id` + `span_id` plus every filtered
  attribute, and `client_golang` rejects exemplars whose combined label
  runes exceed the OpenMetrics cap of 128. With typical otelhttp /
  otelgin default attributes (`server.address`, `url.scheme`,
  `network.protocol.*`, `user_agent.original`) the rejection fired on
  every scrape and surfaced via `otel.Handle` as a recurring error log.
  The SDK now wires a thin `ExemplarReservoirProviderSelector` wrapper on
  `http.server.request.duration` and `http.client.request.duration` that
  drops `FilteredAttributes` before the exemplar is offered: exemplars
  retain `trace_id` + `span_id` (so trace-to-metric linking in Tempo /
  Grafana still works) but no longer carry the verbose attribute payload
  that overflows the rune cap. Custom user views and AP-defined metrics
  are unaffected.
- Test sleep removed: `internal/metrics` `TestInitMeter_HappyPath` no longer
  uses `time.Sleep(100ms)` for runtime-metrics readiness; it polls via
  `assert.Eventually` instead.
- `WithOTLPEndpoint` godoc now warns about the `http://` default and points
  to `WithOTLPHeaders` for managed-backend authentication.

---

## [0.x] - historical

The project does not maintain release tags prior to this changelog. See
`git log` for earlier history.

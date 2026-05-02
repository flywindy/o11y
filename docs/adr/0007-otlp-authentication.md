# ADR 0007: OTLP Authentication via `WithOTLPHeaders`

**Status**: Accepted
**Date**: 2026-04-27

---

## Context

The SDK initially had only `WithOTLPEndpoint` for configuring the OTLP/HTTP
collector. That works against a self-hosted OTel Collector on the same network,
but every managed observability backend requires *some* per-request
authentication header:

| Backend            | Required header                              |
|--------------------|----------------------------------------------|
| Grafana Cloud      | `Authorization: Basic <base64(instanceID:token)>` |
| Honeycomb          | `X-Honeycomb-Team: <api-key>`                |
| New Relic          | `Api-Key: <license-key>`                     |
| Datadog            | `DD-API-KEY: <api-key>`                      |
| Multi-tenant Mimir | `X-Scope-OrgID: <tenant>`                    |

Without first-class support, callers were forced to set
`OTEL_EXPORTER_OTLP_HEADERS` as a process-wide env var. That bypasses the
SDK's option-based configuration entirely, leaks credentials into the
process environment (visible to any subprocess), and applies the same value
to traces, metrics, *and* logs even when the desired backends differ.

## Decision

Expose a single functional option, `WithOTLPHeaders(map[string]string)`, that
attaches the given headers to every OTLP/HTTP request the SDK emits — for
traces, metrics (OTLP push path), and logs.

```go
obs, _ := o11y.Init(ctx,
    o11y.WithServiceName("checkout-svc"),
    // ... required identity options ...
    o11y.WithOTLPEndpoint("https://otlp-gateway-prod.grafana.net/otlp"),
    o11y.WithOTLPHeaders(map[string]string{
        "Authorization": "Basic " + base64Token,
    }),
)
```

The headers are plumbed through `internal/trace`, `internal/log`, and
`internal/metrics` to each exporter's `WithHeaders(...)` constructor. The map
is treated as additive — multiple `WithOTLPHeaders` calls merge rather than
replace, with later calls overwriting same-key entries. Empty maps are no-ops.

## Why a single bag of headers, not per-signal options

We considered adding `WithTraceOTLPHeaders`, `WithLogOTLPHeaders`, and
`WithMetricsOTLPHeaders` separately. Rejected for three reasons:

1. **Most users send all three signals to the same backend.** Forcing them to
   set the same header three times is verbose and error-prone (typos on the
   token).
2. **Tenant routing headers are inherently process-global.** `X-Scope-OrgID`
   identifies the *application*, not the signal type, so per-signal granularity
   solves a problem that does not exist.
3. **Per-signal endpoints are also not yet exposed.** When we add
   `WithLogOTLPEndpoint` / `WithMetricsOTLPEndpoint`-style per-signal
   routing (open question — see below), per-signal headers can land
   alongside them as a pair. Adding split header options now would
   commit us to an awkward intermediate API.

If a real use-case for per-signal headers emerges, we can add the narrower
options later without breaking `WithOTLPHeaders` — they would simply override
on a per-signal basis.

## Alternatives Considered

### Alternative A — Read `OTEL_EXPORTER_OTLP_HEADERS`

The OTel SDK already honours `OTEL_EXPORTER_OTLP_HEADERS` (comma-separated
`key=value` pairs). We could do nothing and tell users to set the env var.

Rejected:
- Credentials in process env leak to every child process and to anyone with
  `ps -ef` access.
- The env var format is awkward: values containing `=` or `,` need URL
  escaping, which is easy to get wrong.
- Setting it from Go code requires `os.Setenv` *before* SDK init, which
  pollutes the parent process for the rest of its lifetime.

### Alternative B — Accept an `*http.Client`

Let the caller supply a fully configured `http.Client` whose `Transport`
injects headers. Rejected because it requires the caller to construct an OTel
client wrapper they did not previously need, and exposes the entire transport
surface (timeouts, TLS config, proxies) for what is in 95% of cases just an
auth header.

### Alternative C — Accept a `func() map[string]string` for token rotation

Tokens that rotate (Grafana Cloud short-lived API keys, AWS SigV4) need a
per-request callback. Rejected for v1: the OTel HTTP exporter does not expose
a per-request header callback; it only accepts a static map at construction
time. Token rotation is therefore a feature gap in the underlying exporter,
not in our wrapper.

When the exporter adds support, we will add `WithOTLPHeaderProvider(fn)` as
a separate option without breaking `WithOTLPHeaders`. Until then, callers
needing rotation should put a sidecar OTel Collector in front of their
service and rotate credentials on the Collector side.

## Security Considerations

- Header values are **never logged**. The SDK logs nothing about headers —
  not at init, not on send, not on shutdown.
- Defining the option as a regular Go function (not env var) keeps tokens
  inside the process's heap, which is never written to disk by the SDK.
- TLS responsibility is left to `WithOTLPEndpoint`: in production the
  endpoint must be `https://`. The godoc on both options reinforces this.

## Consequences

- Adopters can now drop `OTEL_EXPORTER_OTLP_HEADERS` from their deployment
  manifests and Helm charts.
- Multi-tenant deployments routing through a shared Collector can set
  `X-Scope-OrgID` per service via Go code instead of env wiring.
- The internal `Init*` signatures of trace / log / metrics now take a
  `headers map[string]string` parameter. This is `internal/`, so external
  consumers are unaffected.

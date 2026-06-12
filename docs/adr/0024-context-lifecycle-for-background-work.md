# ADR 0024 — Context lifecycle for background work (`obsctx`)

**Status**: Accepted — implemented
**Date**: 2026-06-06
**Relates to**: ADR 0003 (global-state policy), ADR 0021 (MongoDB instrumentation
— asynchronous trace context belongs on an envelope, not a load-bearing path)

---

## Context

When services adopt the SDK and start propagating context to stitch traces
together, a recurring and severe failure mode appears: a **request-scoped
context** (`http.Request.Context()`, or `*gin.Context`) gets threaded into work
that **outlives the request** — background goroutines, fire-and-forget writes,
post-response processing. The request context is canceled the moment the handler
returns (or the client disconnects), so that background work is aborted
mid-flight. For MongoDB this surfaces as `context canceled` and dropped
connections (`connection reset by peer`); the driver also tears the in-flight
connection, causing pool churn.

The failure is **timing dependent**, which makes it especially dangerous:

- Locally (docker-compose, localhost DB latency <1ms, one request at a time) the
  background work almost always finishes before the cancelation lands, so it
  passes.
- In production/k8s, higher DB latency, sidecars/load balancers closing
  connections, real clients disconnecting, and rolling-deploy `SIGTERM` widen
  the race window, so it fails — often taking the service down.

A real incident followed exactly this shape: a well-intentioned observability
change (threading context everywhere to connect traces) caused outages that were
invisible in the test environment and hard to find case-by-case.

The root cause is a category error: **observability was made load-bearing.**
Stitching a trace changed the program's cancelation/lifetime semantics. Trace
context should ride *alongside* business logic, never alter its control flow —
the same principle ADR 0021 applied to MongoDB (trace context belongs on an
envelope, not on a path that changes business behavior).

## Decision

1. **Provide `obsctx`** (`github.com/flywindy/o11y/obsctx`) with:
   - `Detach(ctx) context.Context` — keeps ctx's values (span, baggage) but
     drops cancelation and deadline (`context.WithoutCancel`).
   - `DetachWithTimeout(ctx, d) (context.Context, context.CancelFunc)` — Detach
     plus an independent deadline so detached work cannot hang forever.
   - `Go(reqCtx, timeout, fn)` — the safe primitive for starting background work
     from a handler: detaches, bounds with a timeout, recovers+logs panics.
2. **Establish the rule** (documented in package docs, README, and AGENTS.md):
   - Synchronous, in-request work → use the request context directly.
   - Background / post-response / fire-and-forget work → `obsctx.Detach`/`Go`
     plus a timeout. Never thread a cancelable request context (or `*gin.Context`)
     into work that outlives the request; use `c.Copy()` for the gin side.
3. **Make the failure deterministically testable** without traffic or a real DB,
   via `o11ytest` (`github.com/flywindy/o11y/o11ytest`):
   - `CanceledRequestContext() (ctx, endRequest)` mimics an in-flight request and
     its end.
   - `RequireNotCanceled(ctx, t)` asserts background work was detached.
   This turns the production heisenbug into a CI-friendly red/green test.
4. **Ship a self-audit lint ruleset** (`tools/lint/o11y-context.yml`, semgrep)
   that flags goroutines capturing a request context or `*gin.Context`. Intended
   workflow: run it to find offending sites, fix with `obsctx`, re-run until
   clean; adopt in CI afterwards.
5. **Principle, recorded for future integrations**: telemetry must be
   non-load-bearing. Any instrumentation change that alters cancelation,
   deadlines, or lifetimes is a smell; prefer span links and detached contexts.

## Consequences

**Positive**
- The fix at each call site is small and local (change the *source* of the
  context at background spawn points; downstream `func(ctx)` signatures are
  unchanged). The lint enumerates the exact sites, so the change set is bounded.
- `obsctx.Go` centralizes background safety (detach + timeout + panic recovery)
  in one audited place; call sites become a one-line wrap.
- The deterministic `o11ytest` assertion removes the reliance on production
  traffic to discover the bug, and gives adopters a copyable regression test.

**Negative / trade-offs**
- Detached work loses the request's deadline by design; callers must supply a
  timeout (`DetachWithTimeout`/`Go` make this the default path).
- The semgrep rules are heuristics: they catch the common shapes, not every
  semantic case, so they complement — do not replace — the deterministic test.
- A new public surface (`obsctx`, `o11ytest`) to maintain; both depend only on
  the standard library, so there is no new third-party/global-state exposure
  (ADR 0003 unaffected).

## Alternatives considered

- **Document the pitfall only.** Rejected: guidance without a blessed helper and
  an automated check leaves every adopter to rediscover the trap (whack-a-mole),
  which is how the incident happened.
- **A custom `WithoutCancel` reimplementation.** Unnecessary since Go 1.21
  ships `context.WithoutCancel`; `obsctx` wraps it with the timeout + goroutine
  ergonomics and the project's naming.

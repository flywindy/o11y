# Handoff for the next session (2026-09-25)

> Scope: what is left of the work planned around the 2026-09-14 full review,
> after v0.13.0 shipped.
> Audience: the next agent or engineer picking this up. Paste the whole file as
> an opening prompt, or read it top to bottom — it is written to be enough on
> its own.
> Status: current as of `6cd73e6` on `main`.

---

## 1. Where things stand

- **`v0.13.0` is released.** Tag pushed, pointing at `2ec943b`
  (`Merge pull request #108`), five CI checks green on it.
- Wave A is complete and merged: **#104** (A1, exemplar and reserved-key
  guard), **#105** (A2, mongo pool readiness), **#106** (A3, credential
  paths), **#107** (A4, docs), **#108** (A5, the release cut).
- **#109** landed after the tag: R-14, the last four literal semconv keys in
  `redis`.
- No open PRs, no unresolved review threads.
- The backlog these tasks refer to is
  **`docs/reviews/2026-09-14-sdk-full-review.md`** — 45 findings, `R-1` … `R-45`,
  each verified against the code or the pinned upstream before it was written
  down. **Read it first.**

### Read this before anything else

That report is **not on `main`**. It exists only on the working branch
`claude/o11y-newchat-integration-priority-sbwb9s`, together with
`docs/reviews/2026-09-06-newchat-launch-readiness-review.md`. Those two files
are the *entire* content difference between that branch and `main` —
everything else on it has been merged.

```bash
git show origin/claude/o11y-newchat-integration-priority-sbwb9s:docs/reviews/2026-09-14-sdk-full-review.md
git diff --stat origin/main origin/claude/o11y-newchat-integration-priority-sbwb9s   # 2 files
```

**Task 0 (do this first, it is ten minutes):** open a small docs PR that brings
both review reports onto `main`, so the backlog is discoverable by anyone who
clones the repo — and so it does not disappear with the branch. Once it merges,
the working branch holds nothing unique and can be reset from `main` or
retired; ask the user which.

---

## 2. Wave B — the next six PRs (target v0.13.x)

Independent of each other; can be opened in parallel. Each is one package plus
a test, except B1.

| PR | Title | Findings | Notes |
|---|---|---|---|
| B1 | `fix(o11y): honour OTEL_EXPORTER_OTLP_*ENDPOINT when the option is not set` | R-4, R-21 | Behaviour change → needs a patch-level Migration note. Scope it to (a) let the env var win only when the option was not set, and (b) require a scheme and a host. **Do not** change path semantics — that is C2. |
| B2 | `fix(gin): stop double-recording errors` | R-10 + ADR 0010 amendment | `ErrorRecorder` and otelgin v0.68 each call `RecordError`, so every error produces two `exception` events. Exception-event volume halves on rollout — say so in the entry. |
| B3 | `fix(nats): report a request timeout as nats.ErrTimeout` | R-11 | `Conn.Request` returns `context.DeadlineExceeded`; a service migrated from raw nats.go has a retry branch that silently never fires. Use `fmt.Errorf("%w: %w", ...)` so both `errors.Is` checks hold. |
| B4 | `feat(o11y): add SDK.ForceFlush` | R-12 | The OTLP push path is advertised for serverless but has no flush short of `Shutdown`. Reuse `Shutdown`'s even deadline-sharing. |
| B5 | `fix(redis): stabilise the default pool name and the used gauge` | R-18, R-19 | Default pool name is a heap address → new series on every restart. `used` gauge can wrap to 4.29e9 (`uint32` subtraction before widening). Renaming the pool renames existing series: Migration note. |
| B6 | `test: cover the untested public options` | R-13 | 16 exported functions have no test reference, which breaks AGENTS.md's test rule. Zero behaviour change, easiest to parallelise. |

**R-14 is done** (#109, `d7fc378`). It was listed before the tag in the
review's §6, moved into B5 when the PRs were planned, and shipped in neither,
so it landed on its own afterwards. B5 is now R-18 and R-19 only.

---

## 3. Wave C — after v0.13.x (design first, then code)

- **C1** `fix(redis): observe cluster and ring pools without reloading the topology` — R-7, R-8, plus an ADR 0013 amendment. **Design before coding**: go-redis exposes no public API to enumerate a down shard, and `ForEachShard` unconditionally calls Reload, so every scrape currently costs a `CLUSTER SLOTS` round trip.
- **C2** `WithOTLPEndpoint` as a base URL — R-22. **Breaking**, so it rides a minor version. Grafana Cloud's documented `https://otlp-gateway.example/otlp` form currently 404s for traces and logs.
- **C3** custom-metrics guide + second-scale bucket views — R-20 with SDK-19.
- **C4** seal `http.Option` as ADR 0009 promised — R-32. Today it is an alias for `otelhttp.Option`, so a caller can append `otelhttp.WithMeterProvider(otel.GetMeterProvider())` and silently replace the SDK's provider.
- **P3 sweep** — R-24 … R-45, packaged into two or three tidy-up PRs by package. R-45 alone is a long documentation-drift list.
- **Carry-overs** — PR4 (`WithLinkConsistentSampling`, SDK-2, deferred because production does not sample yet) and SDK-10, 11, 12, 16, 17, 18 from the 2026-09-06 review.

---

## 4. Found during the v0.13.0 release review, not yet filed

Neither blocks anything; both are worth a line in the backlog.

1. **The `Makefile` gosec rationale block is stale.** It documents 12
   pre-existing findings for `make sast-gosec` (not a CI gate by design);
   the tree now reports **32**. The new classes are all benign — `G124` on
   `http.Cookie` test fixtures whose purpose is to be redacted, `G710` on an
   `httptest` server that redirects on purpose, and `G304`/`G703` in
   `diagnostics.go` for reading the cert and key files the operator configured
   (the same files the OTLP exporter reads). Either retriage and update the
   comment, or annotate the intentional ones. Related to R-44.
2. **`G115` in `internal/redact`.** A `\U` escape carries eight hex digits, so
   `rune(r)` in `goUnescaped` can exceed the rune range. Every out-of-range
   value renders as `RuneError` whether it overflowed or not, and both the raw
   text and every decoded view are scanned, so there is **no detection gap** —
   but a bounds check against `utf8.MaxRune` would remove the finding.

### Worth considering: gate the rule, not the instance

R-14 existed because `docs/semconv.md` Enforcement Rule #1 ("No string literals
for SDK-owned semconv keys") was written down but never checked. Nothing stops
the next literal. A check in the shape of `scripts/check_credential_directives.go`
would close the class: scan non-test, non-example Go files for a key string that
the pinned semconv package also defines as a constant, and fail naming the
constant to use. SDK-owned keys such as `redis.error.kind` pass automatically
because the package does not define them, so no allowlist is needed.

---

## 5. newchat-side follow-ups (separate repo, separate session)

From the v0.13.0 upgrade impact analysis of `hmchangw/newchat` (at `e48a37f`,
still on SDK v0.12.0). **That repo is not in the o11y session's GitHub scope** —
it needs its own session, or `add_repo`.

1. **`OTEL_TRACES_SAMPLER` becomes fatal.** `pkg/obs/obs.go:155`
   `samplerOptions` deliberately warns and falls back to 100 % on an unknown
   value. v0.13.0's `Init` now rejects it outright
   (`diagnostics.go:394 checkSamplerName`, only the six OTel spec names), so an
   operator typo crash-loops the service. No deployment file in the repo sets
   the variable today, so nothing breaks on day one — **but the production env
   values live outside the repo and must be checked.** If the forgiving
   behaviour is wanted, normalise it in `pkg/obs` before calling `o11y.Init`.
2. **Upgrade the SDK to v0.13.0.** The concrete win, measured on the import
   graph: **34 of 48 binaries shed at least one driver.** gocql 37 → 6,
   minio-go 34 → 3, go-redis 36 → 18, mongo-driver 45 → 41. The largest
   (`outbox-worker`, `push-notification-service`) drop ~133 packages each.
   `go.mod` itself does not change — the win is per binary.
3. **Install the new handlers.** `initSDK` does not call
   `otel.SetErrorHandler(sdk.ErrorHandler())` / `otel.SetLogger(sdk.Logr())`,
   so OTel's internal export errors are invisible today.
4. **Alert on `o11y_export_failures_total{otel_component_type}`** — new in
   v0.13.0, needs no code change.
5. **`docs/specs/o11y/storage-dependency-metrics.md:27`** lists
   `network_peer_address` as a label of `db_client_operation_duration_seconds`;
   its value changes from `host:port[-n]` to `host` in v0.13.0.

Not applicable to newchat, checked and confirmed: no userinfo in NATS URLs, no
`OTEL_EXPORTER_OTLP_HEADERS`, profiling off, no `o11y/resty` usage, no
dashboard reads the dropped `target_info` labels, and
`OTEL_RESOURCE_ATTRIBUTES=site.id=...` survives the new guard.

---

## 6. Working conventions — follow these exactly

- **Reply to the user in Traditional Chinese (繁體中文).**
- **The user decides when to merge.** Never merge or approve.
- Branch per PR from `main`, named `claude/<topic>`. Also cherry-pick every
  commit onto `claude/o11y-newchat-integration-priority-sbwb9s` — but see
  Task 0: settle that branch's fate with the user first.
- Conventional Commits for both the commit and the PR title.
- Commit trailers: `Co-Authored-By:` and the `Claude-Session:` URL. PR bodies
  end with the Claude Code footer. Every GitHub comment ends with `---` +
  `_Generated by [Claude Code](https://claude.ai/code)_`.
  **Never put a model identifier in any repository artifact.**
- Local gates before every push — all must be clean:
  ```bash
  export PATH="$HOME/go/bin:$HOME/.local/bin:$PATH"
  gofmt -l . && go mod tidy && go build ./... && go vet ./...
  go test -race -count=1 ./...
  golangci-lint run ./...
  make adr-check
  make sast          # takes >2 min, raise the tool timeout
  ```
- **Mutation-verify every fix**: write the test, then confirm it *fails* under
  `go test -overlay=<json>` with a copy of the file that removes just that fix.
  A test that passes for the wrong reason is worse than no test.
- **Before writing or defending any attribute-key string** — in code, an ADR or
  `docs/semconv.md` — use the `verify-semconv-attributes` skill and resolve the
  key against the pinned package source on disk. Keys move between namespaces
  across versions; never answer from memory.
- Fixture credentials need `// #nosec G101 -- reason` **and**
  `// nosemgrep: hardcoded-credential-literal,gosec.G101-1`, placed directly
  above the construct gosec flags (the composite literal, not the line before).
  `scripts/check_credential_directives.go` enforces the pairing.
- **GraphQL is blocked in this environment.** Read review threads with the CCR
  REST endpoints:
  ```bash
  curl -sS -H "Authorization: bearer $GITHUB_TOKEN" \
    https://api.github.com/repos/flywindy/o11y/pulls/<n>/ccr/review_threads?per_page=100
  curl -sS -X POST -H "Authorization: bearer $GITHUB_TOKEN" -H "Content-Type: application/json" \
    https://api.github.com/repos/flywindy/o11y/pulls/<n>/ccr/comments/<comment_id>/resolve
  ```
  A POST without `Content-Type: application/json` is rejected.
- Bot cadence: CodeRabbit gives **one included review per hour** and auto-pauses
  on an active branch (`@coderabbitai review` resumes it); a refused retry does
  not reset the window. Codex has been at its account usage limit for code
  reviews. Plan pushes around that rather than burning the window.
- For each review round: reproduce the finding first, fix it, run the gates,
  push, cherry-pick, **reply and resolve the thread**, then report to the user
  in Traditional Chinese. A finding posted in a review body with no inline
  thread gets an issue comment instead — there is nothing to resolve.
- **Do not create scheduled triggers.** The user removed them and wants none.

---

## 7. Suggested order

1. Task 0 — the two review reports onto `main`, and settle the working branch.
2. B6 (tests only, zero risk) and B2/B3 in parallel — each is small and local.
3. B5, then B1 (B1 carries a Migration note and touches the endpoint contract).
4. B4.
5. Re-read the review report before starting wave C; C1 and C2 both want a
   design pass first.

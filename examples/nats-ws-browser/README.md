# nats-ws-browser

Demonstrates **end-to-end distributed tracing** between a browser frontend and a Go backend, connected via NATS Core over WebSocket.

```
Browser (nats.ws + OTel JS)
  └─ publish → [traceparent header] → NATS Server (ws://localhost:4223)
                                             │
                                    Go backend subscriber
                                    (otel-nats extracts traceparent → child span)
                                             │
                                    publish reply → [traceparent header] → Browser
                                                                  └─ receive span (linked to backend span)
```

Both services export spans to the same OTel Collector → Tempo. In Grafana Tempo you see a single distributed trace spanning the browser publish, the backend processing, and the browser receive.

## Prerequisites

- [kind](https://kind.sigs.k8s.io/) + [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [Node.js](https://nodejs.org/) ≥ 20 + npm
- Go ≥ 1.21

## 1 — Start the infrastructure

```sh
# From the repo root
kind create cluster --config kind-config.yaml
kubectl apply -k k8s/infrastructure/base/
```

The kind cluster maps:
- `localhost:4318` → OTel Collector HTTP (traces/logs/metrics)
- `localhost:4223` → NATS WebSocket

Wait for pods to be ready:
```sh
kubectl get pods -n infra -w
```

## 2 — Start the Go backend subscriber

```sh
# From the repo root
go run ./examples/nats-ws-browser/backend/
```

The backend listens on `demo.frontend.events` and replies to `demo.frontend.replies`.
Its Prometheus metrics are on `:2114` (override with `METRICS_ADDR`).

## 3 — Start the frontend

```sh
cd examples/nats-ws-browser
npm install
npm run dev
```

Open <http://localhost:5173> in your browser.

## 4 — Send a message

1. Wait for the status badge to show **Connected**.
2. Type a payload (or keep the default) and click **Publish**.
3. The browser publishes to NATS with a `traceparent` header.
4. The Go backend receives it, logs it, and replies.
5. The browser receives the reply and logs it.

## 5 — View the trace in Grafana

Open <http://localhost:3000> → **Explore** → select **Tempo** as the data source.

Search by service name `nats-ws-browser` or `nats-ws-browser-backend`. You will see a trace with three spans:

| Span | Service | Description |
|------|---------|-------------|
| `nats.publish` | nats-ws-browser (browser) | Root span created when clicking Publish |
| `process-frontend-event` | nats-ws-browser-backend (Go) | Child span; parent is the browser publish span |
| `nats.receive` | nats-ws-browser (browser) | Receive span linked to the backend reply span |

## How trace propagation works

The browser uses the OTel JS SDK with the **W3C TraceContext** propagator (matching the Go backend's composite propagator). On publish:

1. `propagation.inject(ctx, carrier)` writes `traceparent` (and optionally `tracestate`) into a plain JS object.
2. Each key is copied to the NATS `MsgHdrs` object and sent with the message.
3. The Go `otel-nats` layer calls `nats.Extract(ctx, msg)` on the subscriber side, reading those headers and creating a child span with the browser span as its parent.

On reply the backend uses `conn.Publish(msgCtx, repSubject, data)` (not `msg.Respond`) so the reply also carries the backend's `traceparent` header for the browser to extract.

## Port reference

| Port | Service |
|------|---------|
| `4222` | NATS TCP (Go clients) |
| `4223` | NATS WebSocket (browser) |
| `4318` | OTel Collector HTTP |
| `2114` | Prometheus metrics (backend, overridable via `METRICS_ADDR`) |
| `5173` | Vite dev server (frontend) |

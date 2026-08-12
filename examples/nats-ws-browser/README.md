# nats-ws-browser

Demonstrates end-to-end distributed tracing between a browser frontend and a Go backend, connected via NATS Core over WebSocket.

```text
Browser (nats.ws + OTel JS)
  publish -> traceparent header -> NATS WebSocket
      -> Go backend extracts traceparent
      -> backend process span
      -> reply with backend traceparent
      -> browser receive span
```

Both services export spans to the same OTel Collector and Tempo. In Grafana Tempo you should see one distributed trace that spans the browser publish, backend processing, backend reply publish, and browser receive.

## Prerequisites

- kind + kubectl
- Node.js 20+ and npm
- Go 1.25+

## 1. Start the infrastructure

```sh
# From the repo root
kind create cluster --config kind-config.yaml
kubectl apply -k k8s/infrastructure/base/monitor
kubectl apply -k k8s/infrastructure/base/components/nats
```

The kind cluster maps:

- `localhost:4318`: OTel Collector HTTP for traces, logs, and metrics
- `localhost:4223`: NATS WebSocket

The browser connects directly to `ws://localhost:4223`. That host port is exposed by three pieces working together:

1. `k8s/infrastructure/base/components/nats/nats.yaml` enables NATS WebSocket on container port `4223`.
2. The `nats-websocket` Kubernetes `NodePort` service exposes that port as node port `30001`.
3. `kind-config.yaml` maps host port `4223` to kind node port `30001`.

If you created the kind cluster before this mapping existed, recreate it so kind picks up the port mapping:

```sh
kind delete cluster
kind create cluster --config kind-config.yaml
kubectl apply -k k8s/infrastructure/base/monitor
kubectl apply -k k8s/infrastructure/base/components/nats
```

Wait for pods to be ready:

```sh
kubectl get pods -n infra -w
```

## 2. Open local ports

The browser connects to NATS WebSocket through `localhost:4223`, which is mapped by kind. The Go backend also needs a local NATS TCP port, and Grafana needs a local UI port:

```sh
# Terminal 1: NATS TCP for the Go backend
kubectl port-forward -n infra svc/nats 4222:4222

# Terminal 2: Grafana UI for Tempo verification
kubectl port-forward -n infra svc/grafana 3000:3000
```

Keep both port-forward commands running while you use the example. If another tool such as k9s already owns one of these ports, reuse that port-forward instead of starting a duplicate.

## 3. Start the Go backend subscriber

```sh
# From the repo root
go run ./examples/nats-ws-browser/backend/
```

The backend listens on `demo.frontend.events` and replies to `demo.frontend.replies`. Its Prometheus metrics are on `:2114` (override with `METRICS_ADDR`).

## 4. Start the frontend

```sh
cd examples/nats-ws-browser
npm install
npm run dev
```

Open <http://localhost:5173> in your browser.

Use `localhost`, not `127.0.0.1`, for the frontend URL. The OTel Collector CORS config allows `http://localhost:5173` so browser spans can export to `http://localhost:4318/v1/traces`.

## 5. Send a message

1. Wait for the status badge to show `Connected`.
2. Type a payload, or keep the default, and click `Publish`.
3. The browser publishes to NATS with a `traceparent` header.
4. The Go backend receives it, logs it, and replies.
5. The browser receives the reply and logs it.

## 6. View the trace in Grafana

Open <http://localhost:3000>, go to Explore, and select Tempo as the data source.

Search by service name `nats-ws-browser` or `nats-ws-browser-backend`. You should see one trace containing these spans:

| Span | Service | Description |
| --- | --- | --- |
| `nats.publish` | `nats-ws-browser` | Root span created when clicking Publish and ended when the reply arrives |
| `process-frontend-event` | `nats-ws-browser-backend` | Child span of the browser publish span |
| `publish demo.frontend.replies` | `nats-ws-browser-backend` | Reply publish span that explicitly injects backend context into NATS headers. Named in the semconv `{operation} {destination}` shape the SDK's own NATS spans use |
| `nats.receive` | `nats-ws-browser` | Child span of the backend reply publish span |

## How trace propagation works

The browser uses the OTel JS SDK with the W3C TraceContext propagator, matching the Go backend's composite propagator.

On publish:

1. `propagation.inject(ctx, carrier)` writes `traceparent` and optional `tracestate` into a plain JS object.
2. Each key is copied to the NATS `MsgHdrs` object and sent with the message.
3. The Go backend extracts those headers with `obs.Propagator.Extract` and starts `process-frontend-event` as a child span.

On reply, the backend starts `publish demo.frontend.replies`, explicitly injects that span context into a NATS message header, and publishes through the raw NATS connection. The browser subscriber extracts that header and starts `nats.receive` as a child span, via the `receiveWithSpan` helper in `src/tracing.js`.

Note: the normal `o11ynats.Subscribe` and `conn.Publish` wrapper methods intentionally follow `otel-nats` behavior, including env-gated NATS instrumentation and OTel messaging correlation through span links. This example uses raw NATS subscribe/publish plus explicit extraction/injection because its purpose is to show one parent-child trace tree in Tempo.

### `receiveWithSpan`: the reusable browser receive-side helper

The o11y Go SDK only instruments Go NATS clients; a browser frontend on `nats.ws` has to extract trace context and start its own consumer span itself. `src/tracing.js` exports `receiveWithSpan(msg, { name, attributes }, callback)` as a small, reusable pattern for that:

1. Extracts the `traceparent`/`tracestate` carried on `msg.headers` (nats.ws `MsgHdrs`) using `propagation.extract`.
2. Starts a `SpanKind.CONSUMER` span named `name`, parented on the extracted context, with `messaging.system` / `messaging.operation.type` / `messaging.operation.name` set plus any caller-supplied `attributes` (e.g. `messaging.destination.name`, or app-specific fields like a room/site ID).
3. Runs `callback(span)` — the actual message-handling / render-dispatch work — inside that span, recording and re-throwing any exception the callback throws, and always ending the span in a `finally`.

`src/main.js`'s `listenForReplies` uses it for every inbound reply: the callback body is exactly the "decode payload, log it to the UI" render dispatch, wrapped by the span. Reuse this helper (or the same three-step pattern) for any other nats.ws consumer in a browser frontend that needs to appear correlated in the same distributed trace as its Go backend.

## Known limitations

- Reply fan-out: the backend always replies to the fixed subject `demo.frontend.replies`. All connected browser tabs receive every reply. Open only one tab at a time, or use distinct reply subjects per client.
- No auto-reconnect: if the NATS WebSocket connection drops, the status badge shows `Disconnected` and you must reload the page.

## Port reference

| Port | Service |
| --- | --- |
| `4222` | NATS TCP for Go clients |
| `4223` | NATS WebSocket for browser clients |
| `4318` | OTel Collector HTTP |
| `2113` | Prometheus metrics for the nats-core subscriber, overridable via `METRICS_ADDR` |
| `2114` | Prometheus metrics for this backend, overridable via `METRICS_ADDR` |
| `2115` | Prometheus metrics for the JetStream subscriber, overridable via `METRICS_ADDR` |
| `5173` | Vite dev server |

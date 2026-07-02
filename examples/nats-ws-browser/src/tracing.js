import { WebTracerProvider } from '@opentelemetry/sdk-trace-web';
import { SimpleSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-http';
import { Resource } from '@opentelemetry/resources';
import { ATTR_SERVICE_NAME } from '@opentelemetry/semantic-conventions';
import { CompositePropagator, W3CTraceContextPropagator, W3CBaggagePropagator } from '@opentelemetry/core';
import { ZoneContextManager } from '@opentelemetry/context-zone';
import { propagation, ROOT_CONTEXT, SpanKind, SpanStatusCode } from '@opentelemetry/api';

const exporter = new OTLPTraceExporter({
  url: 'http://localhost:4318/v1/traces',
});

const provider = new WebTracerProvider({
  resource: new Resource({
    [ATTR_SERVICE_NAME]: 'nats-ws-browser',
    'deployment.environment.name': 'development',
  }),
  spanProcessors: [new SimpleSpanProcessor(exporter)],
});

provider.register({
  contextManager: new ZoneContextManager(),
  // Match the Go backend's composite propagator: W3C TraceContext + Baggage.
  // This ensures traceparent/tracestate headers round-trip correctly through NATS.
  propagator: new CompositePropagator({
    propagators: [
      new W3CTraceContextPropagator(),
      new W3CBaggagePropagator(),
    ],
  }),
});

export const tracer = provider.getTracer('nats-ws-browser');

// extractCarrierFromHeaders adapts a nats.ws MsgHdrs object (from msg.headers)
// to the plain string-keyed object propagation.extract expects.
function extractCarrierFromHeaders(msgHeaders) {
  const carrier = {};
  if (
    msgHeaders &&
    typeof msgHeaders.keys === 'function' &&
    typeof msgHeaders.get === 'function'
  ) {
    for (const key of msgHeaders.keys()) {
      const value = msgHeaders.get(key);
      if (typeof value === 'string') {
        carrier[key] = value;
      }
    }
  }
  return carrier;
}

// receiveWithSpan is the browser-side receive helper item 3 of the o11y NATS
// gap analysis asks for: extract the trace context nats.ws delivered on
// msg.headers, start a CONSUMER span linked into that trace (SpanKind
// CONSUMER, matching how the Go SDK's Subscribe/Consume handlers are
// instrumented server-side), and wrap the callback — the actual
// message-handling / render-dispatch work — inside it. Exceptions thrown by
// callback are recorded on the span and re-thrown; the span always ends.
//
// name is the span name (e.g. "nats.receive"); attributes are merged with
// the fixed messaging.system/operation.type/operation.name (pass
// 'messaging.destination.name' and any app-specific fields — a request ID, a
// room/site ID — that make the span searchable). If attributes reuses one of
// those three fixed keys, the fixed value wins and the supplied one is
// dropped — the fixed keys are placed last in the merge below specifically
// so a collision can never silently corrupt them. callback receives the
// started span so it can read spanContext().traceId for its own correlation
// needs (e.g. matching a locally pending producer span).
//
// callback may be sync or async: if it returns a thenable, the span stays
// open until that promise settles (recording/rethrowing a rejection the same
// way a thrown exception is handled) rather than ending immediately after
// callback returns — otherwise the span would report a near-zero duration
// covering only the synchronous kick-off of async dispatch work, not the
// work itself.
export function receiveWithSpan(msg, { name, attributes = {} }, callback) {
  const parentCtx = propagation.extract(ROOT_CONTEXT, extractCarrierFromHeaders(msg.headers));
  const span = tracer.startSpan(name, {
    kind: SpanKind.CONSUMER,
    attributes: {
      ...attributes,
      'messaging.system': 'nats',
      'messaging.operation.type': 'receive',
      'messaging.operation.name': 'receive',
    },
  }, parentCtx);

  const fail = (err) => {
    const message = err instanceof Error ? err.message : String(err);
    span.recordException(err instanceof Error ? err : new Error(message));
    span.setStatus({ code: SpanStatusCode.ERROR, message });
  };

  let result;
  try {
    result = callback(span);
  } catch (err) {
    fail(err);
    span.end();
    throw err;
  }

  if (result && typeof result.then === 'function') {
    return result.then(
      (value) => {
        span.end();
        return value;
      },
      (err) => {
        fail(err);
        span.end();
        throw err;
      },
    );
  }

  span.end();
  return result;
}

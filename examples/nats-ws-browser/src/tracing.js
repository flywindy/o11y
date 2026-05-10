import { WebTracerProvider } from '@opentelemetry/sdk-trace-web';
import { SimpleSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-http';
import { Resource } from '@opentelemetry/resources';
import { ATTR_SERVICE_NAME } from '@opentelemetry/semantic-conventions';
import { CompositePropagator, W3CTraceContextPropagator, W3CBaggagePropagator } from '@opentelemetry/core';
import { ZoneContextManager } from '@opentelemetry/context-zone';

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

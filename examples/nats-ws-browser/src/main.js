import { connect, StringCodec, headers } from 'nats.ws';
import { propagation, context, trace, SpanKind, SpanStatusCode } from '@opentelemetry/api';
import { tracer, receiveWithSpan } from './tracing.js';

const NATS_WS_URL = 'ws://localhost:4223';
const PUB_SUBJECT = 'demo.frontend.events';
const SUB_SUBJECT = 'demo.frontend.replies';

const codec = StringCodec();

let nc = null;
const pendingPublishSpans = new Map();

const statusEl = document.getElementById('status');
const logEl = document.getElementById('log');
const publishBtn = document.getElementById('publish-btn');
const messageInput = document.getElementById('message-input');

function addLogItem(text, traceId, type) {
  const li = document.createElement('li');
  li.className = type;

  const ts = new Date().toISOString().replace('T', ' ').slice(0, -1);
  const tsSpan = document.createElement('span');
  tsSpan.className = 'ts';
  tsSpan.textContent = ts;
  li.append(tsSpan);
  li.append(document.createTextNode(text));

  if (traceId) {
    const traceSpan = document.createElement('span');
    traceSpan.className = 'trace';
    traceSpan.textContent = `trace: ${traceId}`;
    li.append(traceSpan);
  }

  logEl.prepend(li);
}

function setStatus(text, state) {
  statusEl.textContent = text;
  statusEl.className = state;
  publishBtn.disabled = state !== 'connected';
}

function copyCarrierToHeaders(carrier) {
  const h = headers();
  for (const [key, value] of Object.entries(carrier)) {
    h.set(key, value);
  }
  return h;
}

function publish() {
  if (!nc) return;

  const payload = messageInput.value.trim() || 'hello from browser';

  const span = tracer.startSpan('nats.publish', {
    kind: SpanKind.PRODUCER,
    attributes: {
      'messaging.system': 'nats',
      'messaging.destination.name': PUB_SUBJECT,
      'messaging.operation.type': 'publish',
      'messaging.operation.name': 'publish',
    },
  });
  const ctx = trace.setSpan(context.active(), span);

  const carrier = {};
  propagation.inject(ctx, carrier);

  try {
    nc.publish(PUB_SUBJECT, codec.encode(payload), { headers: copyCarrierToHeaders(carrier) });
    const traceId = span.spanContext().traceId;
    const timeoutId = window.setTimeout(() => {
      const pending = pendingPublishSpans.get(traceId);
      if (!pending) return;

      pending.span.setStatus({
        code: SpanStatusCode.ERROR,
        message: 'reply timeout',
      });
      pending.span.end();
      pendingPublishSpans.delete(traceId);
    }, 15000);

    pendingPublishSpans.set(traceId, { span, timeoutId });
    addLogItem(`sent: "${payload}"`, traceId, 'sent');
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    addLogItem(`publish error: ${message}`, null, 'error');
    span.recordException(err instanceof Error ? err : new Error(message));
    span.setStatus({ code: SpanStatusCode.ERROR, message });
    span.end();
  }
}

async function listenForReplies(sub) {
  for await (const msg of sub) {
    // receiveWithSpan (tracing.js) extracts the traceparent from msg.headers,
    // starts a CONSUMER span linked into that trace, and wraps the callback —
    // here, the render/log dispatch — inside it; it records/rethrows any
    // callback error onto the span and always ends it.
    let traceId;
    try {
      receiveWithSpan(msg, {
        name: 'nats.receive',
        attributes: { 'messaging.destination.name': SUB_SUBJECT },
      }, (span) => {
        traceId = span.spanContext().traceId;
        if (!(msg.data instanceof Uint8Array)) {
          throw new Error('invalid reply payload type');
        }
        addLogItem(`reply: "${codec.decode(msg.data)}"`, traceId, 'received');
      });
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      addLogItem(`reply handling error: ${message}`, traceId, 'error');
    } finally {
      const pending = traceId && pendingPublishSpans.get(traceId);
      if (pending) {
        window.clearTimeout(pending.timeoutId);
        pending.span.end();
        pendingPublishSpans.delete(traceId);
      }
    }
  }
}

async function init() {
  setStatus('Connecting...', 'connecting');
  addLogItem(`Connecting to NATS at ${NATS_WS_URL}`, null, 'info');

  try {
    nc = await connect({ servers: NATS_WS_URL });
  } catch (err) {
    setStatus('Connection failed', 'disconnected');
    const message = err instanceof Error ? err.message : String(err);
    addLogItem(message, null, 'error');
    return;
  }

  setStatus(`Connected to ${NATS_WS_URL}`, 'connected');
  addLogItem('Connected to NATS', null, 'info');

  const sub = nc.subscribe(SUB_SUBJECT);
  void listenForReplies(sub).catch((err) => {
    nc = null;
    const message = err instanceof Error ? err.message : String(err);
    addLogItem(`subscriber error: ${message}`, null, 'error');
    setStatus('Disconnected', 'disconnected');
  });

  publishBtn.addEventListener('click', publish);
  messageInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') publish();
  });

  nc.closed().then((err) => {
    nc = null;
    setStatus('Disconnected', 'disconnected');
    if (err) {
      const message = err instanceof Error ? err.message : String(err);
      addLogItem(`connection closed: ${message}`, null, 'error');
    } else {
      addLogItem('Connection closed', null, 'info');
    }
  });
}

init();

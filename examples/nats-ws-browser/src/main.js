import { connect, StringCodec, headers } from 'nats.ws';
import { propagation, context, trace, ROOT_CONTEXT, SpanKind } from '@opentelemetry/api';
import { tracer } from './tracing.js';

const NATS_WS_URL  = 'ws://localhost:4223';
const PUB_SUBJECT  = 'demo.frontend.events';
const SUB_SUBJECT  = 'demo.frontend.replies';

const codec = StringCodec();

let nc = null;

// ── DOM refs ──────────────────────────────────────────────────────────────────
const statusEl    = document.getElementById('status');
const logEl       = document.getElementById('log');
const publishBtn  = document.getElementById('publish-btn');
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
  statusEl.className = state; // 'connected' | 'disconnected' | 'connecting'
  publishBtn.disabled = state !== 'connected';
}

// ── Publish ───────────────────────────────────────────────────────────────────
function publish() {
  if (!nc) return;

  const payload = messageInput.value.trim() || 'hello from browser';

  // Start a root span for this publish operation.
  const span = tracer.startSpan('nats.publish', {
    kind: SpanKind.PRODUCER,
    attributes: {
      'messaging.system':             'nats',
      'messaging.destination.name':   PUB_SUBJECT,
      'messaging.operation.type':     'publish',
      'messaging.operation.name':     'publish',
    },
  });

  const ctx = trace.setSpan(context.active(), span);

  // Inject W3C traceparent (and tracestate/baggage if present) into a plain
  // object, then copy each key onto the NATS MsgHdrs object.
  const carrier = {};
  propagation.inject(ctx, carrier);

  const h = headers();
  for (const [key, value] of Object.entries(carrier)) {
    h.set(key, value);
  }

  try {
    nc.publish(PUB_SUBJECT, codec.encode(payload), { headers: h });
    addLogItem(`→ sent: "${payload}"`, span.spanContext().traceId, 'sent');
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    addLogItem(`✗ publish error: ${message}`, null, 'error');
    span.recordException(err instanceof Error ? err : new Error(message));
  } finally {
    span.end();
  }
}

// ── Subscribe (replies from the Go backend) ───────────────────────────────────
async function listenForReplies(sub) {
  for await (const msg of sub) {
    // Extract the trace context the backend injected into the reply headers.
    // This links the browser's receive span to the backend's processing span.
    const carrier = {};
    if (
      msg.headers &&
      typeof msg.headers.keys === 'function' &&
      typeof msg.headers.get === 'function'
    ) {
      for (const key of msg.headers.keys()) {
        const value = msg.headers.get(key);
        if (typeof value === 'string') {
          carrier[key] = value;
        }
      }
    }
    const parentCtx = propagation.extract(ROOT_CONTEXT, carrier);

    const span = tracer.startSpan('nats.receive', {
      kind: SpanKind.CONSUMER,
      attributes: {
        'messaging.system':           'nats',
        'messaging.destination.name': SUB_SUBJECT,
        'messaging.operation.type':   'receive',
        'messaging.operation.name':   'receive',
      },
    }, parentCtx);

    try {
      if (!(msg.data instanceof Uint8Array)) {
        throw new Error('invalid reply payload type');
      }
      addLogItem(`← reply: "${codec.decode(msg.data)}"`, span.spanContext().traceId, 'received');
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      addLogItem(`✗ reply handling error: ${message}`, span.spanContext().traceId, 'error');
      span.recordException(err instanceof Error ? err : new Error(message));
    } finally {
      span.end();
    }
  }
}

// ── Bootstrap ─────────────────────────────────────────────────────────────────
async function init() {
  setStatus('Connecting…', 'connecting');
  addLogItem(`Connecting to NATS at ${NATS_WS_URL}`, null, 'info');

  try {
    nc = await connect({ servers: NATS_WS_URL });
  } catch (err) {
    setStatus('Connection failed', 'disconnected');
    const message = err instanceof Error ? err.message : String(err);
    addLogItem(`✗ ${message}`, null, 'error');
    return;
  }

  setStatus(`Connected to ${NATS_WS_URL}`, 'connected');
  addLogItem('Connected to NATS', null, 'info');

  const sub = nc.subscribe(SUB_SUBJECT);
  void listenForReplies(sub).catch((err) => {
    const message = err instanceof Error ? err.message : String(err);
    addLogItem(`✗ subscriber error: ${message}`, null, 'error');
    setStatus('Disconnected', 'disconnected');
  });

  publishBtn.addEventListener('click', publish);
  messageInput.addEventListener('keydown', (e) => { if (e.key === 'Enter') publish(); });

  nc.closed().then(() => {
    setStatus('Disconnected', 'disconnected');
    addLogItem('Connection closed', null, 'info');
  });
}

init();

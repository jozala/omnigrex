import { PassThrough } from 'node:stream';
import { randomUUID } from 'node:crypto';

// The owned runner multiplexes bounded history pages with Pi's unchanged RPC
// reader. History is an append-order snapshot, never model-visible projection.
// Only metadata is transferred; transcript bodies stay in the Session owner.
export function historyInput(runtime, source = process.stdin, write = process.stdout.write.bind(process.stdout)) {
  const input = new PassThrough();
  let snapshot;
  const serve = command => {
    const session = runtime.session;
    if (session.isStreaming || session.isCompacting) throw new Error('history requires idle runtime');
    if (command.snapshot) {
      if (!snapshot || snapshot.id !== command.snapshot) throw new Error('history snapshot mismatch');
    } else {
      const entries = session.sessionManager.getEntries();
      snapshot = { id: randomUUID(), sessionId: session.sessionManager.getSessionId(), entries: entries.map(entry => ({
        id: entry.id, parentId: entry.parentId, type: entry.type, timestamp: entry.timestamp,
        ...(entry.type === 'message' ? { message: { role: entry.message.role, stopReason: entry.message.stopReason, isError: entry.message.isError } } : {}),
      })) };
    }
    let start = 0;
    if (command.since) {
      start = snapshot.entries.findIndex(entry => entry.id === command.since) + 1;
      if (!start) throw new Error('history cursor mismatch');
    }
    const entries = snapshot.entries.slice(start, start + 128);
    const more = start + entries.length < snapshot.entries.length;
    const response = { type: 'response', command: command.type, id: command.id, success: true,
      data: { entries, more, snapshot: snapshot.id, sessionId: snapshot.sessionId } };
    const line = JSON.stringify(response) + '\n';
    if (Buffer.byteLength(line) > 1 << 20) throw new Error('history page exceeds limit');
    if (!more) snapshot = undefined;
    return line;
  };
  // One asynchronous pump preserves LF framing and stream backpressure without
  // blocking Pi's cancellation/response reader behind a history consumer.
  (async () => {
    let buffered = Buffer.alloc(0);
    for await (const chunk of source) {
      buffered = Buffer.concat([buffered, chunk]);
      for (;;) {
        const end = buffered.indexOf(10);
        if (end < 0) break;
        if (end > 1 << 20) throw new Error('RPC frame exceeds limit');
        const line = buffered.subarray(0, end);
        buffered = buffered.subarray(end + 1);
        const command = JSON.parse(line.toString('utf8'));
        if (command.type === 'omnigrex_get_history') {
          let reply;
          try { reply = serve(command); }
          catch { reply = JSON.stringify({ type: 'response', command: command.type, id: command.id, success: false, error: 'History unavailable' }) + '\n'; }
          await new Promise((resolve, reject) => write(reply, error => error ? reject(error) : resolve()));
        } else if (!input.write(Buffer.concat([line, Buffer.from('\n')]))) {
          await new Promise(resolve => input.once('drain', resolve));
        }
      }
      if (buffered.length > 1 << 20) throw new Error('RPC frame exceeds limit');
    }
    if (buffered.length) throw new Error('incomplete RPC frame');
    input.end();
  })().catch(() => input.destroy(new Error('Owned RPC input unavailable')));
  return input;
}

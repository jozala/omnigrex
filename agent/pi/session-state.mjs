// Exact owned files, never Pi's best-effort findById/continueRecent discovery.
import { constants } from 'node:fs';
import { mkdir, realpath, lstat, open, link, unlink } from 'node:fs/promises';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { SessionManager, CURRENT_SESSION_VERSION } from '@earendil-works/pi-coding-agent';
import { validateSessionEntries } from './session-validation.mjs';

const canonicalUUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

async function directory(path, create) {
  if (create) await mkdir(path, { mode: 0o700 }).catch(error => { if (error.code !== 'EEXIST') throw error; });
  if (!(await lstat(path)).isDirectory() || await realpath(path) !== path) throw new Error('invalid Session directory');
}

async function validateFile(file, identity) {
  const handle = await open(file, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const info = await handle.stat();
    if (!info.isFile() || info.size === 0 || info.size > 256 << 20) throw new Error('invalid Session file');
    const bytes = new TextDecoder('utf-8', { fatal: true }).decode(await handle.readFile());
    if (!bytes.endsWith('\n')) throw new Error('incomplete Session file');
    const lines = bytes.slice(0, -1).split('\n');
    const header = JSON.parse(lines.shift());
    if (header.type !== 'session' || header.version !== CURRENT_SESSION_VERSION || header.version !== 3 ||
        header.id !== identity.runtimeSessionId || header.cwd !== '/workspace' || header.parentSession !== undefined ||
        typeof header.timestamp !== 'string' || !Number.isFinite(Date.parse(header.timestamp))) throw new Error('Session header mismatch');
    validateSessionEntries(lines);
  } finally { await handle.close(); }
}

export async function openDurableSession(identity, root = '/state') {
  if (!identity || !canonicalUUID.test(identity.agentSessionId) || identity.runtimeSessionId !== identity.agentSessionId ||
      typeof identity.mustExist !== 'boolean') throw new Error('invalid durable Session identity');
  await directory(root, false);
  const sessions = join(root, 'sessions');
  await directory(sessions, !identity.mustExist);
  const sessionDir = join(sessions, identity.runtimeSessionId);
  await directory(sessionDir, !identity.mustExist);
  const file = join(sessionDir, 'session.jsonl');
  let disposition = identity.mustExist ? 'continued' : 'recovered';
  try { await lstat(file); }
  catch (error) {
    if (error.code !== 'ENOENT' || identity.mustExist) throw error;
    // Publish a synced header atomically. Interrupted staging never becomes an
    // empty/random Session, and a delayed retry cannot overwrite an exact file.
    const temporary = join(sessionDir, `.creating-${randomUUID()}`);
    const handle = await open(temporary, 'wx', 0o600);
    try {
      await handle.writeFile(JSON.stringify({ type: 'session', version: 3, id: identity.runtimeSessionId,
        timestamp: new Date().toISOString(), cwd: '/workspace' }) + '\n');
      await handle.sync();
      try { await link(temporary, file); disposition = 'created'; }
      catch (error) { if (error.code !== 'EEXIST') throw error; }
      const dir = await open(sessionDir, 'r');
      try { await dir.sync(); } finally { await dir.close(); }
    } finally { await handle.close(); await unlink(temporary); }
  }
  await validateFile(file, identity);
  const manager = SessionManager.open(file, sessionDir, '/workspace');
  if (manager.getSessionId() !== identity.runtimeSessionId) throw new Error('runtime Session mismatch');
  return { manager, disposition };
}

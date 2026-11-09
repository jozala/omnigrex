import { statfs } from 'node:fs/promises';

const mounts = [['tmp', '/tmp'], ['run-pi-agent', '/run/pi-agent']];
const number = value => typeof value === 'bigint' && value >= 0n && value <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(value) : null;

// Metadata only: never enumerate the credential directory or read file bodies.
// Sequential async statfs calls bound even a hung syscall to one outstanding job.
export async function temporaryFilesystems(stat = statfs) {
  const filesystems = [];
  for (const [resource, path] of mounts) {
    try {
      const s = await stat(path, { bigint: true });
      const bytes = value => typeof s.bsize === 'bigint' && s.bsize > 0n && typeof value === 'bigint' ? number(s.bsize * value) : null;
      filesystems.push({ resource, status: 'complete', capacity: bytes(s.blocks),
        used: typeof s.blocks === 'bigint' && typeof s.bfree === 'bigint' ? bytes(s.blocks - s.bfree) : null,
        free: bytes(s.bfree), available: bytes(s.bavail), inodes: number(s.files), freeInodes: number(s.ffree) });
    } catch {
      filesystems.push({ resource, status: 'unavailable' });
    }
  }
  return filesystems;
}

export function sampleTemporaryFilesystems(output, { sample = temporaryFilesystems, interval = 60000 } = {}) {
  let stopped = false, pending = false;
  const tick = async () => {
    if (stopped || pending) return;
    pending = true;
    try {
      const filesystems = await sample();
      // One small optional frame at most; never queue behind a slow consumer.
      if (!stopped && !output.destroyed && !output.writableNeedDrain && output.writableLength === 0) {
        const frame = JSON.stringify({ type: 'omnigrex_temporary_filesystems', data: filesystems });
        if (frame.length <= 2048) output.write(frame + '\n');
      }
    } catch { /* Observation failure is not Runtime Process failure. */ }
    finally { pending = false; }
  };
  const timer = setInterval(tick, interval);
  timer.unref();
  void tick();
  return () => { stopped = true; clearInterval(timer); };
}

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { statfs, mkdtemp, writeFile, open, unlink, rm } from 'node:fs/promises';
import { temporaryFilesystems, sampleTemporaryFilesystems } from '../temporary-filesystems.mjs';
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));

test('only two fixed statfs paths, exact block arithmetic, optional numeric bounds', async () => {
  const paths = [];
  const samples = await temporaryFilesystems(async (path, options) => {
    paths.push(path); assert.deepEqual(options, { bigint: true });
    return { bsize: 4096n, blocks: 100n, bfree: 40n, bavail: 30n, files: 20n, ffree: 15n };
  });
  assert.deepEqual(paths, ['/tmp', '/run/pi-agent']);
  for (const sample of samples) assert.deepEqual({ ...sample, resource: 'fixed' }, {
    resource: 'fixed', status: 'complete', capacity: 409600, used: 245760, free: 163840, available: 122880, inodes: 20, freeInodes: 15 });
  const invalid = await temporaryFilesystems(async () => ({ bsize: 4096n, blocks: 1n << 64n, bfree: -1n, bavail: undefined, files: -1n, ffree: undefined }));
  assert.equal(invalid[0].capacity, null); assert.equal(invalid[0].free, null);
  assert.equal(invalid[0].available, null); assert.equal(invalid[0].inodes, null);
});

test('failed and unsupported stats omit values and raw errors', async () => {
  const samples = await temporaryFilesystems(async () => { throw new Error('SENTINEL_SECRET credential-filename'); });
  assert.deepEqual(samples, [{ resource: 'tmp', status: 'unavailable' }, { resource: 'run-pi-agent', status: 'unavailable' }]);
});

test('hung sample is single-flight, cancellation/input timers remain responsive, stop discards late result', async () => {
  let calls = 0, finish;
  const frames = [];
  const stop = sampleTemporaryFilesystems({ writableLength: 0, write: frame => frames.push(frame) }, {
    interval: 2, sample: () => { calls++; return new Promise(resolve => { finish = resolve; }); },
  });
  await delay(25); assert.equal(calls, 1);
  stop(); finish([]); await delay(10);
  assert.equal(calls, 1); assert.equal(frames.length, 0);
});

test('slow transport drops optional frames; failed samples retry without accumulation', async () => {
  let calls = 0;
  const frames = [];
  const output = { writableLength: 1, write: frame => frames.push(frame) };
  const stop = sampleTemporaryFilesystems(output, { interval: 3, sample: async () => { calls++; if (calls === 1) throw new Error('SENTINEL_SECRET'); return []; } });
  await delay(20); assert.ok(calls > 1); assert.equal(frames.length, 0);
  output.writableLength = 0;
  await delay(20); stop();
  assert.ok(frames.length > 0); assert.ok(frames.every(frame => frame === '{"type":"omnigrex_temporary_filesystems","data":[]}\n'));
});

test('live records are actual statfs values, including open-but-unlinked allocation', async () => {
  const root = await mkdtemp('/tmp/omnigrex-statfs-');
  try {
    const path = `${root}/disposable`;
    await writeFile(path, Buffer.alloc(1 << 20));
    const handle = await open(path, 'r');
    try {
      await unlink(path);
      const samples = await temporaryFilesystems();
      const actual = await statfs('/tmp', { bigint: true });
      assert.equal(samples[0].capacity, Number(actual.bsize * actual.blocks));
      assert.ok(samples[0].used >= 1 << 20);
      // The production credential directory may be absent on a workstation.
      assert.ok(['complete', 'unavailable'].includes(samples[1].status));
      if (process.env.OMNIGREX_PI_TEMPORARY_FIXTURE === '1') {
        const run = await statfs('/run/pi-agent', { bigint: true });
        assert.equal(samples[1].status, 'complete');
        assert.equal(samples[1].capacity, Number(run.bsize * run.blocks));
        assert.equal(actual.type, 0x01021994n); // TMPFS_MAGIC, not a host directory.
        assert.equal(run.type, 0x01021994n);
      }
    } finally { await handle.close(); }
  } finally { await rm(root, { recursive: true, force: true }); }
});

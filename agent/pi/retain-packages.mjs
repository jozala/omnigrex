// Build-time collection of integrity-locked archives for installed packages.
import { mkdirSync, readFileSync, existsSync, writeFileSync, copyFileSync } from 'node:fs';
import { join } from 'node:path';
import { archiveName, verifyArchive } from './package-integrity.mjs';

const root = '/opt/omnigrex/pi';
const destination = '/opt/omnigrex/pi-packages';
mkdirSync(destination, { recursive: true });
const lock = JSON.parse(readFileSync(join(root, 'package-lock.json'), 'utf8'));
for (const [name, binding] of Object.entries(lock.packages)) {
  if (!name || !existsSync(join(root, name))) continue;
  const response = await fetch(binding.resolved, { signal: AbortSignal.timeout(30000) });
  if (!response.ok) throw new Error('locked package download failed');
  const bytes = Buffer.from(await response.arrayBuffer());
  verifyArchive(bytes, binding.integrity);
  const path = join(destination, archiveName(binding.integrity));
  writeFileSync(path, bytes, { mode: 0o444 });
  if (name === 'node_modules/@earendil-works/pi-coding-agent') copyFileSync(path, '/opt/omnigrex/pi-runtime.tgz');
}

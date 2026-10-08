import { mkdirSync, writeFileSync, renameSync, chmodSync, symlinkSync } from 'node:fs';

mkdirSync('/workspace/evidence', { recursive: true });
writeFileSync('/workspace/evidence/executable', '#!/bin/sh\nexit 0\n');
chmodSync('/workspace/evidence/executable', 0o755);
symlinkSync('executable', '/workspace/evidence/link');
let generation = 0;
function write() {
  generation++;
  const bytes = Buffer.alloc(1024 * 1024, generation % 251);
  writeFileSync('/workspace/evidence/staged', bytes);
  renameSync('/workspace/evidence/staged', '/workspace/evidence/generation');
}
write();
setInterval(write, 5);

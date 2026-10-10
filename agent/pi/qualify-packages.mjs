import { verifyPackages } from './package-integrity.mjs';

try {
  verifyPackages('/opt/omnigrex/pi', '/opt/omnigrex/pi-packages');
} catch {
  process.stderr.write('Pi locked package inventory mismatch\n');
  process.exit(1);
}

import { execFileSync } from 'node:child_process';
import { appendFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { pageName } from './publication.mjs';

export function affectsWebsite(file) {
  return Boolean(pageName(file))
    || file.startsWith('website/')
    || ['README.md', 'mise.toml', '.github/workflows/website.yaml', '.github/workflows/website_test.go', 'internal/deployment/website_rsync_integration_test.go'].includes(file)
    // A published page may reference an image outside its own section.
    || /\.(png|jpe?g|gif|webp|avif|svg)$/i.test(file);
}

export function websiteChanged({ event, base, head, cwd = process.cwd() }) {
  if (event === 'workflow_dispatch' || !base || /^0+$/.test(base)) return true;
  const files = execFileSync('git', ['diff', '--name-only', '--no-renames', '-z', base, head, '--'], { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).split('\0').filter(Boolean);
  return files.some(affectsWebsite);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const changed = websiteChanged({ event: process.env.EVENT_NAME, base: process.env.BASE_SHA, head: process.env.HEAD_SHA });
  appendFileSync(process.env.GITHUB_OUTPUT, `changed=${changed}\n`);
  console.log(changed ? 'Website inputs changed; build and validate.' : 'No website inputs changed; report the required check without building.');
}

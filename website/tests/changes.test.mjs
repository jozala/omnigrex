import assert from 'node:assert/strict';
import test from 'node:test';
import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { affectsWebsite, websiteChanged } from '../scripts/changes.mjs';

test('current documentation, website code and workflow configuration trigger website qualification', () => {
  for (const file of [
    'docs/users/first-work-item.md',
    'docs/operators/installation.md',
    'docs/reference/commands.md',
    'docs/README.md',
    'CONTEXT.md',
    'CONTRIBUTING.md',
    'README.md',
    'website/package-lock.json',
    'website/src/pages/index.astro',
    '.github/workflows/website.yaml',
    '.github/workflows/website_test.go',
    'internal/deployment/website_rsync_integration_test.go',
    'mise.toml',
  ]) assert.equal(affectsWebsite(file), true, file);
});

test('application-only and excluded historical Markdown changes skip the expensive site build', () => {
  for (const file of [
    'internal/config/config.go',
    'docs/plans/first-iteration.md',
    'docs/decisions/0060-publish-one-astro-website-with-starlight-documentation.md',
    'docs/experiments/pi.md',
    'docs/dogfooding.md',
    'site/index.html',
  ]) assert.equal(affectsWebsite(file), false, file);
});

test('image changes outside the page allowlist conservatively rebuild referenced assets', () => {
  assert.equal(affectsWebsite('assets/install diagram.png'), true);
  assert.equal(affectsWebsite('docs/shared/architecture.svg'), true);
});

function repository(t) {
  const cwd = mkdtempSync(path.join(os.tmpdir(), 'omnigrex-website-changes-'));
  t.after(() => rmSync(cwd, { recursive: true, force: true }));
  const git = (...args) => execFileSync('git', ['-c', 'core.hooksPath=/dev/null', '-c', 'user.name=Website tests', '-c', 'user.email=tests@example.invalid', '-C', cwd, ...args], { encoding: 'utf8' }).trim();
  git('init', '--quiet');
  const write = (file, text) => {
    mkdirSync(path.dirname(path.join(cwd, file)), { recursive: true });
    writeFileSync(path.join(cwd, file), text);
  };
  const commit = () => { git('add', '.'); git('commit', '--quiet', '--no-gpg-sign', '-m', 'Fixture revision'); return git('rev-parse', 'HEAD'); };
  return { cwd, git, write, commit };
}

test('deleted and renamed public pages still trigger a build', t => {
  const fixture = repository(t);
  fixture.write('docs/users/example.md', '# Example\n');
  const base = fixture.commit();
  mkdirSync(path.join(fixture.cwd, 'docs/plans'), { recursive: true });
  renameSync(path.join(fixture.cwd, 'docs/users/example.md'), path.join(fixture.cwd, 'docs/plans/example.md'));
  assert.equal(websiteChanged({ event: 'push', base, head: fixture.commit(), cwd: fixture.cwd }), true);
});

test('an unrelated diff skips the site build; unknown history cannot silently pass', t => {
  const fixture = repository(t);
  fixture.write('docs/plans/example.md', '# Example\n');
  const base = fixture.commit();
  fixture.write('docs/plans/example.md', '# An updated plan\n');
  const head = fixture.commit();
  assert.equal(websiteChanged({ event: 'pull_request', base, head, cwd: fixture.cwd }), false);
  assert.throws(() => websiteChanged({ event: 'push', base: 'missing-revision', head, cwd: fixture.cwd }));
});

test('manual redeploys and initial pushes build even without a comparison revision', () => {
  assert.equal(websiteChanged({ event: 'workflow_dispatch' }), true);
  assert.equal(websiteChanged({ event: 'push', base: '0'.repeat(40) }), true);
});

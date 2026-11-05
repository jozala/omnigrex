import assert from 'node:assert/strict';
import test from 'node:test';
import { pageName, pageURL, resolveLink } from '../scripts/publication.mjs';

test('current guides become documentation routes without changing repository filenames', () => {
  assert.equal(pageName('docs/README.md'), 'index.md');
  assert.equal(pageName('docs/users/README.md'), 'users/index.md');
  assert.equal(pageName('docs/operators/installation.md'), 'operators/installation.md');
  assert.equal(pageName('CONTEXT.md'), 'domain-language.md');
});

test('URLs handle indexes and filename punctuation without slugifying versions away', () => {
  assert.equal(pageURL('index.md'), '/docs/');
  assert.equal(pageURL('users/index.md'), '/docs/users/');
  assert.equal(pageURL('reference/version-1.2.md'), '/docs/reference/version-1.2/');
});

test('root README links go to the homepage and links cannot escape the repository', () => {
  assert.equal(resolveLink('../../README.md', 'docs/users/example.md', {}).url, '/');
  assert.equal(resolveLink('../../README.md#what-works-today', 'docs/users/example.md', {}).url, 'https://github.com/jozala/omnigrex/blob/main/README.md#what-works-today');
  assert.throws(() => resolveLink('../../../private.png', 'docs/users/example.md', {}), /escapes repository/);
});

test('object prototype names do not accidentally become publication entries', () => {
  assert.equal(pageName('constructor'), undefined);
  assert.equal(resolveLink('../../toString', 'docs/users/example.md', {}).url, 'https://github.com/jozala/omnigrex/blob/main/toString');
});

test('history and unrelated files are never published', () => {
  for (const file of [
    'docs/plans/first-iteration.md',
    'docs/decisions/README.md',
    'docs/experiments/pi.md',
    'docs/dogfooding.md',
    'docs/notes.md',
    'docs/blog/first-omnigrex-run.md',
    'AGENTS.md',
    'site/index.html',
    'website/README.md',
    'website/DEPLOYMENT.md',
  ]) assert.equal(pageName(file), undefined, file);
});

import assert from 'node:assert/strict';
import test from 'node:test';
import matter from 'gray-matter';
import { prepareMarkdown, parser, imageDestinations, walk } from '../scripts/markdown.mjs';
import { adaptTree } from '../scripts/remark-repository.mjs';

test('derive a title from Markdown, preserve metadata and the original heading anchor', () => {
  const original = '---\nstatus: accepted\n---\n# Use **your** `repository`\n\n## Setup\n\nKeep this text.\n';
  const prepared = prepareMarkdown(original, 'docs/users/example.md');
  const generated = matter(prepared.markdown);
  assert.equal(generated.data.title, 'Use your repository');
  assert.equal(generated.data.status, 'accepted');
  assert.equal(generated.data.editUrl, 'https://github.com/jozala/omnigrex/edit/main/docs/users/example.md');
  assert.match(generated.content, /<a id="use-your-repository"><\/a>/);
  assert.match(generated.content, /## Setup\n\nKeep this text\./);
  assert.doesNotMatch(generated.content, /^# /m);
  assert.match(generated.data.banner.content, /published release/);
});

test('a fake heading inside a fenced example is not treated as the page title', () => {
  const prepared = prepareMarkdown('```md\n# Not the title\n```\n\n# Actual title\n', 'example.md');
  assert.equal(prepared.title, 'Actual title');
  assert.match(prepared.markdown, /```md\n# Not the title\n```/);
});

test('title frontmatter is supported and a missing title fails clearly', () => {
  assert.equal(prepareMarkdown('---\ntitle: Configuration\n---\nBody.', 'example.md').title, 'Configuration');
  assert.throws(() => prepareMarkdown('No heading.', 'example.md'), /Missing page title: example.md/);
});

function adapt(markdown) {
  const prepared = prepareMarkdown(markdown, 'docs/users/example.md');
  const tree = parser.parse(matter(prepared.markdown).content);
  adaptTree(tree, { source: 'docs/users/example.md', removedHeading: prepared.removedHeading }, {
    'docs/users/README.md': 'users/index.md',
    'docs/operators/installation.md': 'operators/installation.md',
  });
  const nodes = [];
  walk(tree, node => nodes.push(node));
  return nodes;
}

test('published links and their fragments resolve; excluded history is labeled GitHub-only', () => {
  const nodes = adapt('# Example\n\n[Guide](../operators/installation.md#2-stage-deployment-files)\n\n[History](../plans/example.md)\n\n[Vision][vision]\n\n[vision]: ../../project-concept.md\n');
  const links = nodes.filter(node => node.type === 'link');
  assert.equal(links[0].url, '/docs/operators/installation/#2-stage-deployment-files');
  assert.equal(links[1].url, 'https://github.com/jozala/omnigrex/blob/main/docs/plans/example.md');
  assert.equal(links[1].children.at(-1).value, ' (GitHub)');
  assert.equal(nodes.find(node => node.type === 'linkReference').children.at(-1).value, ' (GitHub)');
});

test('heading IDs match GitHub including duplicates after the removed page heading', () => {
  const headings = adapt('# Example\n\n## Example\n\n## 2. Prepare a repository\n\n## Example\n').filter(node => node.type === 'heading');
  assert.deepEqual(headings.map(node => node.data.hProperties.id), ['example-1', '2-prepare-a-repository', 'example-2']);
});

test('images, including reference-style images, use locally published assets', () => {
  const text = '# Example\n\n![Diagram](./diagram.png)\n\n![Another][diagram]\n\n[diagram]: ../shared/another%20diagram.svg\n';
  assert.deepEqual(imageDestinations(parser.parse(text)), ['./diagram.png', '../shared/another%20diagram.svg']);
  const nodes = adapt(text);
  assert.equal(nodes.find(node => node.type === 'image').url, '/docs-assets/docs/users/diagram.png');
  assert.equal(nodes.find(node => node.type === 'definition').url, '/docs-assets/docs/shared/another%20diagram.svg');
});

test('external links, same-page anchors and code examples are unchanged', () => {
  const nodes = adapt('# Example\n\n[Outside](https://example.com/)\n\n[Section](#example)\n\n```md\n[Code](../plans/example.md)\n```\n');
  assert.deepEqual(nodes.filter(node => node.type === 'link').map(node => node.url), ['https://example.com/', '#example']);
  assert.equal(nodes.find(node => node.type === 'code').value, '[Code](../plans/example.md)');
});

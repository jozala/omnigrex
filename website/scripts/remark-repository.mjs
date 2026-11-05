import path from 'node:path';
import { fileURLToPath } from 'node:url';
import GithubSlugger from 'github-slugger';
import { toString } from 'mdast-util-to-string';
import { walk } from './markdown.mjs';
import { repositoryTarget, resolveLink } from './publication.mjs';

const content = fileURLToPath(new URL('../src/content/docs/', import.meta.url));

export function adaptTree(tree, entry, pages) {
  const slugger = new GithubSlugger();
  if (entry.removedHeading) slugger.slug(entry.removedHeading);
  const imageReferences = new Set();
  const definitions = new Map();
  walk(tree, node => {
    if (node.type === 'imageReference') imageReferences.add(node.identifier);
    if (node.type === 'definition') definitions.set(node.identifier, node);
  });
  walk(tree, node => {
    if (node.type === 'heading') {
      node.data ??= {};
      node.data.hProperties ??= {};
      node.data.hProperties.id ??= slugger.slug(toString(node));
    }
    const image = node.type === 'image' || (node.type === 'definition' && imageReferences.has(node.identifier));
    if (node.url) {
      const local = image && repositoryTarget(node.url, entry.source);
      if (local) node.url = '/docs-assets/' + encodeURI(local.target) + local.suffix;
      else {
        const result = resolveLink(node.url, entry.source, pages);
        node.url = result.url;
        if (result.github && node.type === 'link') node.children.push({ type: 'text', value: ' (GitHub)' });
        if (result.github && node.type === 'definition') node.data = { ...node.data, github: true };
      }
    }
  });
  walk(tree, node => {
    if (node.type === 'linkReference' && definitions.get(node.identifier)?.data?.github) node.children.push({ type: 'text', value: ' (GitHub)' });
  });
}

export default function repositoryMarkdown({ pages, entries }) {
  return (tree, file) => {
    const entry = entries[path.relative(content, file.path)];
    if (entry) adaptTree(tree, entry, pages);
  };
}

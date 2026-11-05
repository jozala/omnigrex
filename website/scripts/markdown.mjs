import matter from 'gray-matter';
import { unified } from 'unified';
import remarkParse from 'remark-parse';
import { toString } from 'mdast-util-to-string';
import { slug } from 'github-slugger';

export const parser = unified().use(remarkParse);

export function prepareMarkdown(text, source) {
  const { data, content } = matter(text);
  const tree = parser.parse(content);
  const heading = tree.children.find(node => node.type === 'heading' && node.depth === 1);
  const title = heading ? toString(heading) : data.title;
  if (typeof title !== 'string' || !title.trim()) throw new Error(`Missing page title: ${source}`);
  const body = heading
    ? content.slice(0, heading.position.start.offset) + `<a id="${slug(title)}"></a>` + content.slice(heading.position.end.offset)
    : content;
  return {
    title,
    removedHeading: heading ? title : undefined,
    tree,
    markdown: matter.stringify(body, {
      ...data,
      title,
      editUrl: 'https://github.com/jozala/omnigrex/edit/main/' + source,
      banner: { content: 'Development documentation: these guides follow <a href="https://github.com/jozala/omnigrex/tree/main">main</a> and may describe changes not yet in a published release.' },
    }),
  };
}

export function walk(node, visit) {
  visit(node);
  node.children?.forEach(child => walk(child, visit));
}

export function imageDestinations(tree) {
  const definitions = new Map();
  walk(tree, node => { if (node.type === 'definition') definitions.set(node.identifier, node.url); });
  const images = [];
  walk(tree, node => {
    if (node.type === 'image') images.push(node.url);
    if (node.type === 'imageReference') {
      const url = definitions.get(node.identifier);
      if (url) images.push(url);
    }
  });
  return images;
}

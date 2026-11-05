import path from 'node:path';

// New sections require an intentional publication decision; new tracked pages in
// these sections are included automatically. Never publish all of docs/.
export const sections = ['users', 'repository-owners', 'operators', 'contributors', 'reference'];
const standalone = {
  'docs/README.md': 'index.md',
  'CONTEXT.md': 'domain-language.md',
  'CONTRIBUTING.md': 'contributing.md',
  'docs/observability/application.md': 'observability/application.md',
};

export function pageName(file) {
  if (Object.hasOwn(standalone, file)) return standalone[file];
  if (!sections.some(section => file.startsWith(`docs/${section}/`)) || !file.endsWith('.md')) return;
  return file.slice('docs/'.length).replace(/(^|\/)README\.md$/, '$1index.md');
}

export function pageURL(name) {
  return '/docs/' + name.replace(/(^|\/)index\.md$/, '$1').replace(/\.md$/, '/');
}

export function repositoryTarget(url, source) {
  if (!url || /^(?:[a-z][a-z\d+.-]*:|#|\/)/i.test(url)) return;
  const [, pathname, suffix] = url.match(/^([^?#]*)(.*)$/);
  const target = path.posix.normalize(path.posix.join(path.posix.dirname(source), decodeURIComponent(pathname)));
  if (target === '..' || target.startsWith('../')) throw new Error(`Link escapes repository: ${source}: ${url}`);
  return { target, suffix };
}

export function resolveLink(url, source, pages) {
  const resolved = repositoryTarget(url, source);
  if (!resolved) return { url, github: false };
  const { target, suffix } = resolved;
  const index = `${target.replace(/\/$/, '')}/README.md`;
  const published = Object.hasOwn(pages, target) ? pages[target] : Object.hasOwn(pages, index) ? pages[index] : undefined;
  if (published) return { url: pageURL(published) + suffix, github: false };
  if (target === 'README.md' && !suffix) return { url: '/', github: false };
  return {
    url: 'https://github.com/jozala/omnigrex/blob/main/' + encodeURI(target) + suffix,
    github: true,
  };
}

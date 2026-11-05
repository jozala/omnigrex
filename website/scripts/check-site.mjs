import { readFile, readdir } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { parse } from 'parse5';
import { pageURL } from './publication.mjs';

const website = fileURLToPath(new URL('../', import.meta.url));
const dist = path.join(website, 'dist');
const { pages } = JSON.parse(await readFile(path.join(website, '.generated/publication.json'), 'utf8'));
const html = new Map();
const files = new Set();
async function scan(directory) {
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const filename = path.join(directory, entry.name);
    if (entry.isDirectory()) await scan(filename);
    else files.add(path.relative(dist, filename));
  }
}
await scan(dist);

for (const file of files) {
  if (!file.endsWith('.html')) continue;
  const route = '/' + file.replace(/(^|\/)index\.html$/, '$1');
  const document = parse(await readFile(path.join(dist, file), 'utf8'));
  const page = { ids: new Set(), refs: [], scripts: [] };
  function visit(node) {
    const attrs = Object.fromEntries((node.attrs ?? []).map(({ name, value }) => [name, value]));
    if (attrs.id) page.ids.add(attrs.id);
    for (const attr of ['href', 'src', 'poster']) if (attrs[attr]) page.refs.push(attrs[attr]);
    if (attrs.srcset) page.refs.push(...attrs.srcset.split(',').map(item => item.trim().split(/\s+/)[0]));
    if (node.tagName === 'script' && attrs.src) page.scripts.push(attrs.src);
    node.childNodes?.forEach(visit);
  }
  visit(document);
  html.set(route, page);
}

const errors = [];
for (const [route, page] of html) {
  if (route.startsWith('/docs/') && page.scripts.some(src => src.includes('analytics.aetas.pl'))) errors.push(`Analytics loaded on ${route}`);
  for (const ref of page.refs) {
    if (/^(?:[a-z][a-z\d+.-]*:|\/\/)/i.test(ref)) continue;
    const url = new URL(ref, 'https://local.invalid' + route);
    const filename = decodeURIComponent(url.pathname).slice(1);
    const target = html.get(url.pathname);
    if (!target && !files.has(filename)) errors.push(`${route}: missing ${ref}`);
    if (target && url.hash && !target.ids.has(decodeURIComponent(url.hash.slice(1)))) errors.push(`${route}: missing anchor ${ref}`);
  }
}
for (const name of Object.values(pages)) if (!html.has(pageURL(name))) errors.push(`Missing published page: ${name}`);
for (const route of html.keys()) {
  if (route !== '/' && route !== '/404.html' && !Object.values(pages).some(name => pageURL(name) === route)) errors.push(`Unexpected public page: ${route}`);
}
for (const anchor of ['top', 'idea', 'how-it-works', 'architecture', 'roles', 'principles', 'status']) {
  if (!html.get('/')?.ids.has(anchor)) errors.push(`Missing legacy homepage anchor: ${anchor}`);
}
if (!files.has('pagefind/pagefind.js')) errors.push('Missing search index');
if (!files.has('fonts/montserrat-license.txt')) errors.push('Missing redistributed font license');
if (!html.get('/')?.scripts.some(src => src.includes('analytics.aetas.pl'))) errors.push('Missing existing homepage analytics');
if (errors.length) throw new Error(errors.join('\n'));
console.log(`Verified homepage and ${Object.keys(pages).length} public documentation pages, links, heading anchors, assets, analytics boundary, and search output.`);

import { execFileSync } from 'node:child_process';
import { mkdir, readFile, writeFile, copyFile, rm, realpath } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { pageName, repositoryTarget } from './publication.mjs';
import { prepareMarkdown, imageDestinations } from './markdown.mjs';

const website = fileURLToPath(new URL('../', import.meta.url));
const repository = path.resolve(process.env.REPOSITORY_ROOT ?? path.join(website, '..'));
const content = path.join(website, 'src/content/docs');
const assets = path.join(website, 'public/docs-assets');
const tracked = new Set(execFileSync('git', ['-c', `safe.directory=${repository}`, '-C', repository, 'ls-files', '-z'], { encoding: 'utf8' }).split('\0').filter(Boolean));
const pages = Object.fromEntries([...tracked].filter(file => pageName(file)).map(file => [file, pageName(file)]));

// Both directories are generated, ignored, and owned exclusively by this build.
await rm(content, { recursive: true, force: true });
await rm(assets, { recursive: true, force: true });
const entries = {};
for (const [source, name] of Object.entries(pages)) {
  const prepared = prepareMarkdown(await readFile(path.join(repository, source), 'utf8'), source);
  const generated = `docs/${name}`;
  const destination = path.join(content, generated);
  await mkdir(path.dirname(destination), { recursive: true });
  await writeFile(destination, prepared.markdown);
  entries[generated] = { source, title: prepared.title, removedHeading: prepared.removedHeading };
  for (const url of imageDestinations(prepared.tree)) {
    const local = repositoryTarget(url, source);
    if (!local) continue;
    if (!tracked.has(local.target) || !/\.(png|jpe?g|gif|webp|avif|svg)$/i.test(local.target)) throw new Error(`Image is not a tracked image: ${source}: ${url}`);
    const original = await realpath(path.join(repository, local.target));
    if (!original.startsWith(await realpath(repository) + path.sep)) throw new Error(`Image escapes repository: ${url}`);
    const target = path.join(assets, local.target);
    await mkdir(path.dirname(target), { recursive: true });
    await copyFile(original, target);
  }
}
await mkdir(path.join(website, '.generated'), { recursive: true });
await writeFile(path.join(website, '.generated/publication.json'), JSON.stringify({ pages, entries }, null, 2));
await mkdir(path.join(website, 'public/fonts'), { recursive: true });
await copyFile(path.join(website, 'node_modules/@fontsource/montserrat/LICENSE'), path.join(website, 'public/fonts/montserrat-license.txt'));
console.log(`Prepared ${Object.keys(pages).length} current documentation pages. Historical material is GitHub-only.`);

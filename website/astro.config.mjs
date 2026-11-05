import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import { unified } from '@astrojs/markdown-remark';
import { readFileSync } from 'node:fs';
import repositoryMarkdown from './scripts/remark-repository.mjs';

const publication = JSON.parse(readFileSync(new URL('./.generated/publication.json', import.meta.url), 'utf8'));
const section = (label, directory) => ({
  label,
  collapsed: true,
  items: [{ autogenerate: { directory: `docs/${directory}` } }],
});

export default defineConfig({
  site: 'https://omnigrex.com',
  trailingSlash: 'always',
  markdown: { processor: unified({ remarkPlugins: [[repositoryMarkdown, publication]] }) },
  integrations: [starlight({
    title: 'OMNIGREX',
    favicon: '/favicon.png',
    description: 'A self-hosted software-development team, coordinated through GitHub.',
    customCss: ['./src/styles/brand.css', './src/styles/docs.css'],
    components: { SiteTitle: './src/components/DocsSiteTitle.astro' },
    social: [{ icon: 'github', label: 'GitHub', href: 'https://github.com/jozala/omnigrex' }],
    sidebar: [
      { label: 'Start here', items: [
        { label: 'Documentation', link: '/docs/' },
        { label: 'Domain language', link: '/docs/domain-language/' },
      ] },
      section('Users', 'users'),
      section('Repository owners', 'repository-owners'),
      section('Operators', 'operators'),
      section('Contributors', 'contributors'),
      section('Reference', 'reference'),
      { label: 'Supporting guides', collapsed: true, items: [
        { label: 'Contributing', link: '/docs/contributing/' },
        { label: 'Application observability', link: '/docs/observability/application/' },
      ] },
    ],
    lastUpdated: false,
    credits: false,
  })],
});

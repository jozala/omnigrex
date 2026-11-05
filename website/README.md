# Website prototype

The Astro homepage and Starlight documentation are one static website.
The homepage is a local design proposal based on the existing, untracked `site/index.html`; that file is not modified or published automatically.
Production deployment is intentionally not implemented until the design is approved.

## Preview

From the repository root, with Docker running:

```sh
mise run docs-site-preview
```

Open <http://localhost:4321/> for the homepage and <http://localhost:4321/docs/> for documentation.
The preview is bound to localhost only.
No local Node installation is required.
Use `WEBSITE_PORT=4322 mise run docs-site-preview` if the default port is occupied.

The container tests the adapter, prepares the documentation, builds the static site and search index, and checks the generated links before serving it.
This is a built snapshot, not a live-reloading development server.
After changing website sources or Markdown, rerun `mise run docs-site-preview` to rebuild the snapshot and recreate its container.

Inspect startup errors or stop the preview:

```sh
docker compose --file website/compose.yaml logs preview
mise run docs-site-stop
```

## Publication boundaries

The repository is mounted read-only.
The container reads tracked Markdown from `docs/users/`, `docs/repository-owners/`, `docs/operators/`, `docs/contributors/`, and `docs/reference/`.
The explicit additional pages are the documentation hub, `CONTEXT.md`, `CONTRIBUTING.md`, and the current application-observability guide.
The allowlist lives in `scripts/publication.mjs`.
New tracked pages in an approved section appear automatically; other directories require an explicit publication change.

Plans, decisions, experiments, historical evidence, blog drafts, and local notes are not generated or search-indexed.
Links to repository files outside the publication allowlist point to GitHub and are labeled accordingly.
The root README links to the homepage rather than becoming another generated page.

Title metadata, original heading anchors, section-index routes, and website links are adapted in generated files only.
Referenced local images must be tracked and are copied into the static output.
Generated content is owned by the build and is never an editing location.
Update the repository Markdown instead.

The supplied logo is stored in `src/assets/omnigrex-logo.png` and optimized by Astro.
Montserrat is self-hosted through the pinned `@fontsource/montserrat` dependency; the generated website includes its original font license at `/fonts/montserrat-license.txt`.
The homepage reuses Starlight's theme components and preference storage without importing the documentation layout.
Analytics are restricted to the production homepage hostnames and are absent from documentation pages.

## Verify

```sh
mise run docs-site-check
mise run docs-check
mise run check
```

Website checks cover publication boundaries, title and anchor handling, Markdown and reference-style links, image paths, actual generated routes and assets, preserved homepage anchors, search output, and the analytics boundary.
The normal application gate remains unchanged; the website check is separate.

## Following visual approval

Add a dedicated required Pull Request website check and automatic publication for relevant changes on `main`.
The agreed production mechanism is direct `rsync --delete` over SSH into the existing Apache document root, using a dedicated CI key for the existing login account.
There will be no staging releases, rollback system, backups, or post-deployment checks.
Do not configure production credentials or alter the current website as part of this prototype.

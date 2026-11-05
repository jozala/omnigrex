# Website

The Astro homepage and Starlight documentation are one static website.
The homepage is based on the original, untracked `site/index.html`; that file is not modified or published automatically.
GitHub Actions validates Pull Requests and publishes relevant `main` changes through SSH after the production environment is configured.
The homepage presents the intended product experience, including `omnigrex:run` starting Planner discovery before approved implementation.
That positioning does not change the current operational contract: planning currently uses `omnigrex:plan`, and the implementation handoff is follow-up [#165](https://github.com/jozala/omnigrex/issues/165).
The published user guides continue to document current supported behavior.

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
mise exec -- go test -race ./.github/workflows
mise run check
```

Website checks cover publication boundaries, title and anchor handling, Markdown and reference-style links, image paths, actual generated routes and assets, preserved homepage anchors, search output, and the analytics boundary.
The normal application gate remains unchanged; the website check is separate.
The workflow contract tests also cover required-check outcomes, build/export ordering, direct synchronization, and temporary SSH credential handling without contacting a server.

## CI and publication

The [Website workflow](../.github/workflows/website.yaml) runs change detection on every Pull Request and `main` push, and supports manual redeployment from `main`.
It always reports the stable `Website check` result, including when no website inputs changed.
Relevant changes run repository Markdown checks, workflow contract tests, and the Docker website checks before exporting only the generated `dist/` directory.
Deployment downloads the artifact by the ID returned by its successful build, including when only a failed deployment is rerun, rather than rebuilding it.

The change detector reuses the publication allowlist, includes website and workflow tooling, and conservatively includes image changes anywhere because published guides may reference them.
Deleted or renamed public pages also trigger a build.
Unrelated application changes and excluded historical Markdown skip the expensive build.
Pull Request jobs do not access the production environment or its SSH credentials.

Publication is one direct `rsync --delete` upload into the existing Apache document root.
There are no staging releases, rollback system, backups, or post-deployment checks.
Configure the GitHub environment and required check before merging; see the [website publication procedure](../docs/operators/website.md).

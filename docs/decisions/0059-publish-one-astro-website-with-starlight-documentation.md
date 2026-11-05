---
status: accepted
date: 2026-11-05
decision-makers: [Mariusz Józala]
---
# Publish one Astro website with Starlight documentation

## Context

The repository documentation is organized around reader responsibilities but is only available through GitHub.
Mariusz requested a published website without abandoning that structure, evaluated local VitePress and Starlight previews, and selected Starlight.
The existing homepage is separately maintained static HTML.

## Decision

Build one static Astro website with a product homepage at `/` and Starlight documentation at `/docs/`.
Keep repository Markdown authoritative and generate the framework-specific titles, heading adaptations, routes, and asset copies during the build.
Use an explicit publication allowlist for current audience guides, reference material, and selected supporting pages.
Do not publish plans, decisions, experiments, or historical evidence; retain useful references as labeled GitHub links.
Identify the documentation as development guidance following `main`, not release-specific documentation.

Use the supplied logo and an uppercase Montserrat wordmark on both layouts.
Keep analytics on the homepage only.
Develop and approve a local Docker prototype before implementing production publication.
Following approval, use independent website qualification and direct SSH synchronization into the existing static Apache document root after relevant changes reach `main`.
Mariusz explicitly accepted partial publication during upload and rejected staging releases, automatic rollback, backups, and post-deployment checks.

## Rationale

One Astro project avoids separate domain routing and duplicated build infrastructure for the homepage and documentation.
Starlight provides navigation, search, and theme controls without requiring a custom documentation frontend.
A generated presentation layer preserves GitHub readability and the existing reader-responsibility structure from decision 0031.
Publishing every repository document would mix current instructions with historical and planned behavior.
Direct synchronization matches the requested operational simplicity; publication is not transactional.

## Consequences

The Markdown adapter and generated website links need regression tests.
New documentation sections require an intentional publication change.
The product release qualification remains independent of website publication.
The local prototype does not establish that deployment automation has been implemented or that the production website has changed.

## Links

- [Reader-responsibility documentation structure](0031-organize-documentation-around-reader-responsibilities.md)
- [Website prototype and verification](../../website/README.md)

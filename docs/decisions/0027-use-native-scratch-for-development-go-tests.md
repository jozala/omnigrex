---
status: accepted
date: 2026-10-16
decision-makers: [OpenCode implementation agent]
---
# Use native guest scratch for development Go tests

## Context

The development-image KVM qualification exercises filesystem permission tests that protect retained-state restore and collection behavior.
On the guest's shared tool-data mount, setting a directory to mode `0500` can still report `0700` and permit writes, so that filesystem cannot exercise those tests faithfully.
The same operations behave correctly on the native guest disk.
The Runtime Profile's `/tmp` tmpfs is only 64 MiB, making it unsuitable for the complete build and test workload.

## Decision

Run the repository's ordinary unit and Docker-backed integration test tasks through a small command wrapper.
In the development image, the wrapper selects native `/var/tmp` as `TMPDIR` and `GOTMPDIR` for `go test` and its descendants only, then replaces itself with Go.
Go retains ownership of its private temporary directories, ordinary cleanup, signal handling, and exit status.
Outside the image, preserve the inherited environment.
Keep persistent `GOPATH` and `GOCACHE`, parent-process path bindings, and runtime/bootstrap implementation unchanged.

## Rationale

Weakening permission tests would remove meaningful regression coverage.
Changing runtime storage contracts or the shared filesystem implementation is broader than selecting appropriate scratch for this repository workload.
A custom wrapper-owned temporary root would require additional process supervision and cleanup; the standard Go temporary-directory lifecycle already provides those responsibilities.

## Consequences

Test scratch consumes the disposable VM root disk and is not a retained cache.
Normal Go completion cleans its temporary children; leftovers after abnormal termination disappear with the Execution Environment.
Passing these tests establishes behavior on native test scratch, not restrictive-permission support on the shared tool-data mount.

## Links

- [Development image delivery plan](../plans/2026-10-16-development-image.md).
- [Image-local module source](0026-use-an-image-local-go-module-source.md).

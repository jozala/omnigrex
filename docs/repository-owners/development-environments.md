# Configure a development environment

[Repository-owner guide](README.md) · [Documentation hub](../README.md)

By default, new Participants use the installation's Pi golden image.
Repository tools are installed from the actual working checkout through mise before agent-protocol initialization.
The control plane does not contain mise or execute repository installation scripts.
If the repository needs additional system tools or backend capabilities, publish a prebuilt environment image outside Omnigrex.

## Build and publish an image

Derive from the qualified golden digest using the [Debian-based example](../../agent/environments/pi-example/Dockerfile):

```sh
docker build --platform linux/amd64 --file agent/environments/pi-example/Dockerfile \
  --build-arg OMNIGREX_GOLDEN_IMAGE=ghcr.io/jozala/omnigrex/pi@sha256:<golden-digest> \
  --build-arg EXTRA_DEB_PACKAGE='jq=<approved-exact-Debian-version>' \
  --tag registry.example/team/development:qualified .
docker push registry.example/team/development:qualified
docker image inspect registry.example/team/development:qualified --format '{{index .RepoDigests 0}}'
```

This build example runs from an Omnigrex source checkout or a build context containing the example Dockerfile.
Replace placeholders with actual approved digests and package versions.
Use a controlled package mirror/snapshot when reproducible rebuilding is required.
Preserve Node 24.14.1, Pi 1.4.0 and its locked package/resource inventory, the owned runner/extensions/bootstrap, mise, Git/shell/search resources, and UID/GID 10001.
Labels or image ancestry do not replace executable/resource qualification.
Install extra tools in `/usr/local/bin` or activate them through working-revision mise configuration.

## Declare the environment

Prepare `.omnigrex/environment.yaml` for the default branch:

```yaml
version: 1
image: registry.example/team/development@sha256:<64-lowercase-hex-digest>
platform: linux/amd64
requirements:
  - process-isolation
  - tool-installation
```

Use the resolved untagged registry reference.
Docker accepts `process-isolation` and `tool-installation`.
Microsandbox additionally accepts `hardware-isolation` and `nested-containers` under its Linux/KVM contract.
No backend mounts the host Docker socket into a guest as fallback.
Unknown requirements/fields, mutable tags, and unsupported platforms fail configuration.

## Qualify and promote

1. Ask the operator to provision the exact digest/platform in the selected backend before accepting work.
2. For pre-promotion testing, configure the candidate on the default branch of a separate qualification repository containing the project's build/test workload and required Profiles/skills.
3. Run doctor for that repository, then qualify additional tools and actual build/tests with an isolated Work Item on the selected backend.
4. Promote the verified environment configuration to the managed project's default branch and run its repository preflight before new work.

Doctor does not execute the entire project test suite.
Omnigrex does not build images or replace missing artifacts with tags.
Keep registry artifacts available for native identity verification and nested-container flat-image materialization.
Warm native verification reuses validated persistent registry documents without requests, while first-use verification, flat materialization, and guest pulls can still require registry access.
For agents developing Omnigrex itself, use the qualified [Omnigrex development image](../contributors/development-image.md).

## Understand selection and tool installation

Environment and Profiles use the same trusted default-branch snapshot.
Each Participant retains its selected exact image, platform, and full contract; later Participants can choose newer images even in one Assignment Generation.
Existing Participants never rebind when the file or deployment golden image changes.
An explicit repository image pin is not updated by changing the installation golden image: qualify and promote its replacement separately on the default branch, including for new Participants.
Fresh independent Sessions require no historical source-to-candidate compatibility artifact; this does not replace image/resource validation or the promotion and doctor steps above.
Keep their images available through Session collection and include canonical contracts in PostgreSQL recovery sets.
Source-to-candidate qualification on copied state does not authorize Session rebinding.

Tool installation uses the real Developer/Reviewer working revision, including Change Proposal mise hooks and scripts.
It runs non-root with disk-backed installation scratch removed before protocol startup and separate tool-data attachments for requested caches.
Repository activation cannot replace protected runtime/provider/loader settings, and the agent executable uses its absolute qualified path.
Approved control-plane paths such as `GOCACHE` and `GOPATH` override conflicting mise activation.
The trusted-repository credential model applies to all those scripts and images.

See [repository environment evidence](../plans/2026-10-08-repository-environments.md) and [tool paths](tool-paths-and-caches.md).

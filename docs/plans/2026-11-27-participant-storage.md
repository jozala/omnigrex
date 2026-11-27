# Participant storage collection implementation

Implements the accepted P1 scope of [Work Item 218](https://github.com/jozala/omnigrex/issues/218), including A1's exclusion of historical completed collections.
This design does not amend the first-iteration specification or authorize production host cleanup.

## Ownership map

All `assignment-<UUID>` prefixes use the durable Agent Participant ID, despite their historical naming.
Configured roots are trusted deployment configuration, not paths returned by agent code.

| Storage | Exact ownership | Lifecycle |
| --- | --- | --- |
| Runtime state root / `assignment-ID/runtime-state` | Backend-bound Session state | Existing retained-state collector under irrevocable authorization |
| Runtime state root / `assignment-ID/.restore-<32 hex>` | Participant restore staging | Existing retained-state collector |
| Runtime state root / `assignment-ID/runtime-logs` | Native log archives | Existing native retained-state collector |
| Capture root / `assignment-ID` | Protected captures, seals and capture staging | Participant collection only |
| Workspace root / `assignment-ID/workspace` | Disposable checkout | Execution cleanup plus final Participant collection |
| Workspace root / `assignment-ID/workspace-{staged,retired}-EPOCH-*` | Owned checkout transitions | Epoch cleanup plus final Participant collection |
| Publication root / `assignment-ID/publication` | Disposable trusted checkout | Final Participant collection |
| Publication root / `assignment-ID/.{inspect,corroborate}-*` | Private capture inspection copies | Immediate cleanup plus final Participant collection after interruption |
| Mise root / `assignment-ID/mise` | Tool installations and related local files | Final Participant collection |
| Mise root / `assignment-ID/tool-data` | Dependency/build caches and orphan Turn scratch | Stopped Turn scratch cleanup; final Participant collection |
| Publication root / `.default-snapshot-*` | Installation-owned snapshot acquisition scratch, no Participant identity in acquisition API | Immediate acquisition cleanup; not inferred Participant-owned |
| Historical publication root / `.{inspect,corroborate}-*` | Anonymous staging without durable Participant attribution | Operator-managed, never assigned ownership by filename discovery |
| Workspace root / `.participant-storage-locks/ID` | Installation-owned physical writer exclusion metadata, never unlinked during collection | Retained control metadata, not reclaimed Participant contents |
| Native home / `omnigrex-operations` | Backend dispatch/revocation journals and lock identity | Installation control metadata, retained for safe reconciliation |
| Native home, native image/layer/materialization cache, registry verification documents | Installation-wide shared backend lifecycle | Separate maintenance; all retained when provenance is incomplete |
| Credentials, backups, unrelated host caches, PostgreSQL | Other owners/protected history | Never targeted |

Future inspection and corroboration staging now lives under its exact Participant's publication parent.
Anonymous installation scratch cannot safely be assigned to a Participant after a crash and is not swept.
Configured workspace/publication/mise roots may coincide, as in Compose; known child names are combined once.
Roots inside targeted Participant trees and unknown top-level Participant entries fail closed.

## Removal and recovery

The backend runtime-state collector runs first and verifies backend/namespace and irrevocable collection authority.
Mounted Session/log/restore staging removal shares the bounded no-follow tree primitive; the Docker collector additionally confirms the actual daemon volume absent.
Collection authority additionally refuses unresolved/unrevoked creation reservations, leased human prompts and unconfirmed recovery stops, not merely active Turn rows.
Production creation, publication, capture and tool preparation retain their existing live Turn/active Participant fences.
A shared physical lock now spans host-side writes, including staged Git acquisition and Session initialization before native reservation.
The complete cleaner holds its exclusive counterpart; durable admission is rechecked after lock acquisition.
Trusted Git subprocesses inherit the open lock description, and lock metadata is never unlinked during collection.
See [physical writer exclusion](../decisions/0072-exclude-host-storage-writers-through-physical-lifetime.md).
Collection never acquires heartbeat-dependent database locks through tree removal.
A lease loss cancels the worker but does not revoke COLLECTING authority.
No completed collection is selected or reauthorized.

The expanded filesystem collector opens configured ancestors without following links, validates owner and inode identity, and validates Linux mount identities before traversing or repairing permissions.
Only an exclusively owned tree permits unlinking leaf symlinks; their targets are never followed or chmodded.
Each attempt has a ten-second deadline, 8192-entry work budget, depth limit of 32 and 128-name enumeration buffers.
Removal makes durable progress in the filesystem; interrupted or exhausted attempts return errors, and retry starts from remaining entries.
Empty Participant directory shells are removed only with non-recursive `rmdir` after their required children are absent.
There is no migration, completion-record rewrite or new historical authorization API.
All required category errors propagate before worker finalization.
Depth/ownership/mount errors need operator investigation rather than an unsafe fallback.

Category outcomes report observed allocated blocks for directories and the last observed link of a removed file, never apparent file size.
Hardlinks are counted only at the last unlink, including across categories; externally retained links are not attributed as reclaimed.
Physical release is unknown because open handles, shared extents and host activity remain outside this observation.
Runtime-state collection reports unavailable byte measurement rather than a fabricated zero.

## Native boundary

The bounded CLI inventory includes uncollected ledger roots (including COLLECTING), the current deployment image and repeatable explicit provisioning/pinning roots.
It reports native inventory dimensions as unsupported rather than inferring candidates from absence.
Complete provenance, aliases and shared references remain unknown, so all native resources are retained and apply refuses before backend access.
See the [tracked dependency](../dependencies/microsandbox-safe-image-gc.md) and [policy decision](../decisions/0073-fail-closed-native-image-maintenance.md).

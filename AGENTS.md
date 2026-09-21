# Volume-operator constitution

This file is the constitution for this project. It captures persistent principles, conventions, and context. It is personal and not committed to git.

---

## Project overview

`volume-operator` is a Kubernetes controller (kubebuilder-scaffolded, written in Go) that provisions Azure-backed persistent volumes for Deployments. It is one part of a two-mechanism storage pattern alongside the `ogcapi-operator`.

---

## Full system flow

### The actors

| Repo | Path | Role |
|---|---|---|
| `ogcapi-operator` | `~/Workspace/ogcapi-operator` | Manages OGCAPI CRs; creates and owns the Deployment |
| `volume-operator` | this repo | Hooks into ReplicaSet lifecycle; creates the source PVC and AVP |
| `azure-volume-populator` | `~/Workspace/azure-volume-populator` | Watches AVP CRs; runs a populator pod to fill the PVC with blob data |
| `smooth-operator` | `~/Workspace/smooth-operator` | Shared Go utility library used by all of the above |

### End-to-end sequence

1. A user creates an **OGCAPI CR** with a `volumeOperatorSpec` (blobPrefix, storagCapacity, storageClass).
2. **ogcapi-operator** reconciles the CR and creates a **Deployment** containing:
    - Volume-operator annotations on the Deployment (see Annotations section below)
    - An **ephemeral volume** in the pod spec whose `DataSource` points to a PVC named by a hash
3. Kubernetes creates a **ReplicaSet** from the Deployment.
4. **volume-operator** hooks into the ReplicaSet lifecycle. For the active ReplicaSet (revision matches Deployment), it creates:
    - An **`AzureVolumePopulator` CR** (the AVP)
    - A **source PVC** with `DataSourceRef` pointing to the AVP — both named by the hash
    - Both carry an **`ownerReference` to the ReplicaSet** (see "Cleanup on full deletion" below)
5. **azure-volume-populator** sees the AVP CR and spawns a **populator pod** that copies data from Azure Blob Storage into the source PVC.
6. The source PVC reaches **`Bound`** state.
7. Kubernetes detects that the ephemeral volume's `DataSource` PVC is now Bound and triggers native **volume cloning** — each pod gets its own ephemeral clone.
8. The pod starts with its cloned volume mounted at `/data`.

### Cleanup on rollout

When a Deployment rolls out a new ReplicaSet (new revision), on every reconcile of any ReplicaSet in the set:
- volume-operator deletes the **AVP and PVC** (not the ReplicaSet itself — ReplicaSet lifecycle belongs to Kubernetes/the Deployment controller, not this operator) for old ReplicaSets that are scaled to 0 replicas.
- A resource is only deleted if no other ReplicaSet still references the same `resource-suffix` hash with `replicas > 0` (the hash can be shared across revisions when `blobPrefix`/`volumeMountPath`/`storageCapacity` are unchanged — see "Hash-based deduplication" below).
- **Cleanup runs on a reconcile of *any* ReplicaSet in the set**, not just the current-revision one — an old ReplicaSet scaling to 0 is itself the trigger that makes it eligible. Resource *provisioning* still only happens on the current-revision ReplicaSet (revision match + required annotations); the `else` branch falls through to cleanup.
- **Safety gate:** deletion is skipped unless the ReplicaSet matching the Deployment's *current* revision has `AvailableReplicas > 0` — i.e. the new rollout must actually be serving before old storage is torn down (`currentReplicaSetIsAvailable` in `internal/controller/util.go`). This guards against deleting the old PVC/AVP while the new one is still failing to come up.
- **Requeue while blocked:** when the safety gate holds cleanup back but eligible old resources still exist, `Reconcile` returns `RequeueAfter: cleanupRetryInterval` (1 min) so it converges once the new rollout serves, instead of waiting for an incidental later event. `cleanUpOldReplicaSets` returns `(requeue bool, err error)` for this; `replicaSetHasResources` checks whether the AVP/PVC still exist, and fails safe (reports `true`/still-pending) on any unexpected, non-`NotFound` `Get` error rather than silently treating it as "resource gone".
- **The Deployment's current hash is never deleted:** an old ReplicaSet whose `resource-suffix` equals the Deployment's current one is skipped, regardless of replica counts. `resourceIsUsedByOtherReplicaSet` only counts *other* ReplicaSets with `replicas > 0`, so without this a Deployment scaled to 0 (current ReplicaSet `Spec.Replicas == 0` while `AvailableReplicas` still lags at >0 as pods terminate) would have its live source PVC/AVP deleted and re-populated from blob on scale-up. It also keeps the requeue from firing forever for a scaled-to-0 Deployment.
- The new and old PVCs are **independent** (different names if the hash changed), so there is no conflict during transition.
- Cleanup logic lives in `cleanUpOldReplicaSets` / `deleteResourcesForReplicaSet` / `replicaSetHasResources` in `internal/controller/volume_controller.go`, backed by `hasReplicas`, `resourceIsUsedByOtherReplicaSet`, and `currentReplicaSetIsAvailable` in `internal/controller/util.go`.

### Cleanup on full deletion (owner references)

The rollout cleanup above only fires on a live reconcile of *some* ReplicaSet in the set — but when the whole Deployment (and therefore every one of its ReplicaSets) is deleted outright (e.g. the owning OGCAPI CR itself is deleted), there is no ReplicaSet left to reconcile at all: `Reconcile` sees `Get` return `NotFound` and returns immediately, before `cleanUpOldReplicaSets` ever runs. This used to leave the AVP and PVC permanently orphaned (confirmed live against a test AKS cluster: deploy → delete the OGCAPI → Deployment/ReplicaSet gone in seconds, AVP/PVC left `Bound` indefinitely).

The fix is a safety net independent of any reconcile: `createAvpIfNotExists` / `createPvcIfNotExists` set (or, if the resource already exists via hash-reuse, add to) an `ownerReference` from the AVP/PVC to the owning ReplicaSet, via `ensureOwnerReference` in `internal/controller/volume_controller.go`. Kubernetes' own garbage collector then deletes the AVP/PVC once **every** owning ReplicaSet is gone — no reconcile required.

- Ownership is **additive, not exclusive**: because the resource-suffix hash can be shared across ReplicaSet revisions (see "Hash-based deduplication"), a reused AVP/PVC can carry owner references to more than one live ReplicaSet at once. `ensureOwnerReference` relies on `controllerutil.SetOwnerReference`, which upserts by (group, kind, name) rather than replacing the whole list, so an older revision's ownership survives until that specific ReplicaSet is actually gone.
- `ensureOwnerReference` re-reads the object and sends a **merge patch with an optimistic lock** (`MergeFromWithOptimisticLock`), only when the owner list actually changed, retrying on `Conflict` (`retry.RetryOnConflict`). The lock is required, not cosmetic: a merge patch replaces the whole `ownerReferences` list, so without a `resourceVersion` precondition a stale cache read could drop another ReplicaSet's ownership. Patching rather than `Update` also avoids writing back the whole object, which would prune CRD fields unknown to the vendored AVP types. The client needs `patch` on PVCs and AVPs (this repo has no RBAC markers and `config/rbac/role.yaml` has no PVC rules, so check the ClusterRole where it is actually defined).
- This is a *complement* to the explicit rollout cleanup above, not a replacement — the explicit path still deletes eagerly on scale-to-0 rather than waiting for a ReplicaSet object to eventually disappear (e.g. via `revisionHistoryLimit` trimming).
- `createAvpIfNotExists` / `createPvcIfNotExists` also tolerate `AlreadyExists` on `Create` as a non-error: two near-simultaneous reconciles for the same new ReplicaSet can both read a not-yet-cached AVP/PVC as `NotFound` (informer cache lag) and race to create it — the loser's `Create` legitimately fails with `AlreadyExists`, which isn't a real failure since the desired object already exists. Without this, that race surfaced as noisy `ERROR Reconciler error` log lines (with stack traces) on effectively every normal rollout — confirmed live during the same debugging session.

### Hash-based deduplication

The resource name (used for both the AVP and PVC) is a hash generated by `ogcapi-operator`:

```
hash = GenerateHashFromStrings([]string{blobPrefix, volumeMountPath, storageCapacity})
```

This means:
- If `blobPrefix`, `volumeMountPath`, and `storageCapacity` are unchanged across a new Deployment revision, the hash is identical → volume-operator finds the existing AVP and PVC and skips creation. The same source PVC is reused.
- If any of the three values change (e.g., new blob data), the hash changes → new AVP and PVC are created with the new name, and the old ones are cleaned up after the rollout.

### Why per ReplicaSet?

The Deployment itself is stable (managed by ogcapi-operator), but data can change between versions. Tracking ReplicaSet revisions lets volume-operator detect when a new rollout has happened and decide whether new storage resources need to be provisioned.

---

## Annotations

All annotations use the prefix `volume-operator.pdok.nl` and are read from the **Deployment**:

| Annotation | Required | Default | Description |
|---|---|---|---|
| `volume-operator.pdok.nl/resource-suffix` | yes | — | The hash used as the name for the AVP and PVC |
| `volume-operator.pdok.nl/blob-prefix` | yes | — | Blob prefix in Azure Blob Storage to populate from |
| `volume-operator.pdok.nl/volume-path` | yes | — | Destination path inside the volume |
| `volume-operator.pdok.nl/storage-capacity` | no | `1Gi` | PVC size |
| `volume-operator.pdok.nl/storage-class` | no | `managed-premium-zrs` | Storage class |

### Important: naming is externally controlled

The `resource-suffix` annotation value is a **hash, not a suffix**, and is **not controlled by this operator**. It is generated by `ogcapi-operator` (`addVolumePopulatorToDeployment` in `ogcapi-operator/internal/controller/ogcapi_controller.go`). The name is "suffix" for historical reasons — it was originally a name suffix, but since names can't be specified without hardcoding, it became a standalone hash-derived name.

---

## Technical conventions

### Language & framework

- Go (latest stable)
- [Kubebuilder](https://kubebuilder.io) scaffolding — update via `kubebuilder alpha update --from-branch master`, not manual edits to generated files
- `sigs.k8s.io/controller-runtime` for reconciler patterns

### Testing

- Test framework: Ginkgo v2 + Gomega
- Preferred: **unit tests with a fake client** (`sigs.k8s.io/controller-runtime/pkg/client/fake`)
- Integration tests use `setup-envtest` (`make test`)
- E2E tests use Kind (`make test-e2e`)
- Flag it when a test doesn't exercise real reconciler logic — the existing `volume_controller_test.go` has `TODO(user)` placeholders and tests no actual behavior

### Linting

- `golangci-lint` with extensive rules in `.golangci.yml`
- Run: `make lint` / `make lint-fix`
- Notable rules: max function length 100 lines, max cyclomatic complexity 15, no `fmt.Print*`, no `github.com/pkg/errors`

### Build & CI

- `make build` / `docker build`
- `make test` runs unit + integration tests
- GitHub Actions: builds and publishes Docker image on tag push → Docker Hub as `pdok/volume-operator`
- Versioning: semver tags

### Dependencies

- Dependencies are normally published versions (not local replace directives)
- `go.work` is occasionally used to pull in sibling repos for local development

---

## Git conventions

- Do **not** commit or push.
- Reading git history and diffs is fine.
- Staging files is fine.

---

## Keeping this file current

**Always update `AGENTS.md` after making relevant changes** — annotation renames, new resources created, flow changes, fixed bugs that were documented here, new conventions adopted. This file is the source of truth for future sessions; stale entries cause confusion.
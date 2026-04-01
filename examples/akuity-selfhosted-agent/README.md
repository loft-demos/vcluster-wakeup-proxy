# Akuity-Hosted Phase 1

This directory contains an additive Phase 1 pattern for environments that use:

- Akuity-hosted Kargo
- Akuity-hosted Argo CD
- a self-hosted Kargo agent in the vCluster management cluster

This path does **not** replace the existing watcher-first flow in the rest of this
repository. It exists for hosted control planes where `cmd/watcher` cannot rely on
direct in-cluster access to Argo CD cluster `Secret` resources.

## Shape

1. A `Stage` is pinned to a self-hosted Kargo agent shard with `spec.shard`.
2. The promotion template runs a reusable `wake-vcluster` `ClusterPromotionTask`.
3. That task calls a wake endpoint before `argocd-update`.
4. `argocd-update` runs only after the wake request has been accepted.

The default task example points at `cmd/proxy`:

- `http://vcluster-wakeup-proxy.argocd.svc.cluster.local:8080`

That keeps the existing proxy useful as a readiness-aware wake facade for hosted
Kargo agents and avoids changing the current watcher behavior.

## Files

- [cluster-promotion-task-wake-vcluster.yaml](cluster-promotion-task-wake-vcluster.yaml)
  defines a reusable `ClusterPromotionTask`
- [project-secret-vcluster-platform.yaml](project-secret-vcluster-platform.yaml)
  shows a project-scoped Kargo `Secret` containing a platform token
- [stage-example.yaml](stage-example.yaml) shows a `Stage` pinned to a self-hosted
  shard and invoking the wake task immediately before `argocd-update`

## Notes

- The example `Secret` belongs in the same namespace as the `Stage`.
- The task uses `secret('vcluster-platform').token`, so rename the `Secret` or
  adjust the task if you use a different name.
- If you point the task directly at the vCluster Platform API instead of
  `cmd/proxy`, you will likely also want an explicit readiness poll before
  `argocd-update`.
- This pattern intentionally keeps the current self-hosted watcher solution
  untouched.

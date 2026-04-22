# vcluster-gitops-watcher

`vcluster-gitops-watcher` centers on the `VirtualClusterInstance` watcher for Argo CD `v3.4.0-rc1` or newer.

The recommended architecture is:

- `cmd/watcher` observes `VirtualClusterInstance` state and matching Argo CD `Application` objects
- when Kargo is installed, the watcher can also observe active `Promotion` objects that use `argocd-update`
- the watcher pauses sleeping destinations by patching imported cluster Secrets with `argocd.argoproj.io/skip-reconcile: "true"`
- when a matching Kargo promotion or Argo app sync intent appears, the watcher can trigger the vCluster wake request directly
- once the vCluster is ready again, the watcher removes `skip-reconcile` and hard-refreshes the affected Argo CD apps

For Kargo-driven promotions that end with `argocd-update`, this means Argo CD Notifications and Kargo `http` steps are not needed just to wake sleeping vClusters. The watcher can wake from the Kargo `Promotion` itself before Argo CD has finished creating `Application.operation.sync`.

The optional HTTP proxy remains available as a secondary component if you still want a shared wake facade that treats transient `502` / `504` responses as accepted or waits for readiness before acknowledging the wake request.

## VCI Watcher (Recommended)

The watcher is the main sleep-mode integration point.

It polls `VirtualClusterInstance` objects from the management cluster API and then:

- derives the project from `metadata.labels["loft.sh/project"]` when present, otherwise from `metadata.namespace` using `WATCH_PROJECT_NAMESPACE_PREFIXES`
- finds matching Argo CD `Application` objects by `spec.destination.name`, which should align with the imported cluster Secret name such as `loft-<project>-vcluster-<virtualcluster>`
- when Kargo is available, indexes `Promotion` objects by the Stage named in `kargo.akuity.io/authorized-stage`
- classifies the vCluster as `Sleeping`, `Waking`, `Ready`, or `Unknown`
- patches the imported cluster Secret with `argocd.argoproj.io/skip-reconcile: "true"` while the vCluster is sleeping or waking
- optionally triggers `POST /kubernetes/project/<project>/virtualcluster/<name>` when a matching active Kargo `Promotion` uses `argocd-update`
- still treats `Application.operation.sync` as a fallback wake signal when Kargo is not installed or not in use
- removes `skip-reconcile` and annotates matching apps with `argocd.argoproj.io/refresh: hard` once the vCluster is ready again
- optionally patches non-Kargo `Application.status.health` while sleeping or waking unless `WATCH_PATCH_APPLICATION_HEALTH=false`

Sleep detection prefers the platform-managed annotations `sleepmode.loft.sh/sleeping-since` and `sleepmode.loft.sh/sleep-type`, then falls back to `status.phase`, `status.reason`, `status.message`, and the `VirtualClusterOnline` condition.

### Why Argo CD v3.4.0-rc1 or newer?

The cluster-secret pause path is meant for Argo CD `v3.4.0-rc1` or newer, where cluster-secret `argocd.argoproj.io/skip-reconcile: "true"` is honored by the application controller.

For Kargo, the deterministic trigger is a promotion template that ends with `argocd-update`. When Kargo `Promotion` objects are readable, the watcher wakes sleeping destinations from those active promotions before Argo CD finishes creating `Application.operation.sync`. If Kargo is not installed, or the watcher cannot read `Promotion` objects, it automatically falls back to Argo-only wake detection from `Application.operation.sync`.

Important Kargo caveat: if a destination vCluster is put to sleep while a `Promotion` is still verifying, that verification can fail. The watcher handles the Argo CD sync and wake path, but it does not treat sleep during an active Kargo verification window as success. In practice, sleep should be considered safe only after the Stage is back to `Ready=True` / `Healthy=True`, unless you intentionally plan to override the failed verification.

### Watcher Flow

1. A source change creates new `Freight` in Kargo.
2. A promotion runs and ends with `argocd-update`.
3. The watcher sees the active Kargo `Promotion` for the sleeping destination and triggers the wake request.
4. Argo CD writes `Application.operation.sync` on the target app.
5. The watcher keeps cluster-secret `skip-reconcile=true` while the vCluster is sleeping or waking.
6. When the `VirtualClusterInstance` becomes ready, the watcher removes `skip-reconcile` and hard-refreshes the app.
7. Argo CD performs the real sync, and Kargo can wait on the real Argo `Healthy` state.

With that watcher-first flow, Argo CD Notifications and Kargo `http` steps are not required for wakeup orchestration.

### Watcher Configuration

Ready-to-apply example manifests are included in [deploy/watcher-rbac.yaml](deploy/watcher-rbac.yaml) and [deploy/watcher-deployment.yaml](deploy/watcher-deployment.yaml).

Important watcher settings:

| Variable | Default | Description |
| --- | --- | --- |
| `ARGOCD_CLUSTER_SECRET_NAME_TEMPLATE` | none | Required imported cluster Secret naming template. Supports `{project}` and `{virtualcluster}` |
| `ARGOCD_NAMESPACE` | `argocd` | Default namespace for Argo CD resources |
| `ARGOCD_APPLICATION_NAMESPACE` | `ARGOCD_NAMESPACE` | Namespace where matching `Application` objects live |
| `ARGOCD_CLUSTER_SECRET_NAMESPACE` | `ARGOCD_NAMESPACE` | Namespace where imported cluster Secrets live |
| `WATCH_POLL_INTERVAL` | `15s` | How often to poll `VirtualClusterInstance` objects |
| `WATCH_PROJECT_NAMESPACE_PREFIXES` | `p-,loft-p-` | Namespace prefixes used when no `loft.sh/project` label is present |
| `WATCH_PATCH_APPLICATION_HEALTH` | `true` | When not set to `false`, patches non-Kargo `Application.status.health` to `Suspended` or `Progressing` while Argo is paused |
| `WATCH_SLEEPING_MESSAGE` | `vCluster sleeping` | Health message written when app health patching is enabled |
| `WATCH_WAKING_MESSAGE` | `vCluster waking` | Health message written when app health patching is enabled |
| `WATCH_WAKE_UPSTREAM_BASE` | disabled | Optional base URL used to trigger `POST /kubernetes/project/<project>/virtualcluster/<name>` when a sleeping destination has an active Kargo `Promotion` or `Application.operation.sync` |
| `WATCH_WAKE_TIMEOUT` | `10s` | Timeout for the wake request HTTP client |
| `WATCH_WAKE_SUCCESS_ON` | `502,504` | Comma-separated additional wake response codes treated as accepted, beyond `200` and `202` |
| `WATCH_WAKE_BEARER_TOKEN` | none | Optional bearer token sent with the wake request |
| `WATCH_WAKE_TOKEN_PATH` | none | Optional path to a file containing the bearer token for the wake request |
| `WATCH_WAKE_CA_PATH` | `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` | CA bundle used when `WATCH_WAKE_UPSTREAM_BASE` is `https://...` |
| `WATCH_WAKE_RETRY_INTERVAL` | `30s` | Minimum delay before retrying a wake request while the same sync intent is still present and the vCluster remains asleep |
| `WATCH_KUBERNETES_API` | auto | Optional Kubernetes API base URL. Defaults to the in-cluster API |
| `WATCH_KUBERNETES_TIMEOUT` | `10s` | Timeout for watcher Kubernetes API requests |
| `WATCH_TOKEN_PATH` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | Bearer token for watcher API calls |
| `WATCH_CA_PATH` | `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` | CA bundle for watcher API calls |

The example watcher Deployment uses `WATCH_POLL_INTERVAL=2s` so it can react quickly when a vCluster transitions into sleep.

Application health patching is enabled by default so non-Kargo apps show a helpful `Suspended` or `Progressing` status in Argo CD while their destination vCluster is asleep or waking. Apps annotated with `kargo.akuity.io/authorized-stage` are treated specially: the watcher preserves their last non-watcher health while the destination is dormant and clears stale watcher-managed sleep messages once the destination is ready again. Set `WATCH_PATCH_APPLICATION_HEALTH=false` to disable health patching entirely.

If your cluster does not expose the `applications/status` subresource, the watcher falls back to patching the `Application` resource itself. If that still is not allowed in your cluster, it automatically disables health patching and continues managing cluster-secret pause/unpause plus app refresh.

When `WATCH_WAKE_UPSTREAM_BASE` is set, the watcher treats active Kargo `Promotion`s that use `argocd-update` as the earliest wake signal for sleeping destinations and still uses `Application.operation.sync` as a fallback. If you already run `cmd/proxy`, you can point `WATCH_WAKE_UPSTREAM_BASE` at the proxy service so the watcher reuses the proxy's tolerant wake semantics for transient `502` / `504` responses. If you do not need that behavior, point the watcher directly at the vCluster Platform API instead.

Build the watcher image with [Dockerfile.watcher](Dockerfile.watcher).

## Akuity-Hosted Phase 1 (Optional Alternative)

When Argo CD and/or Kargo are hosted by Akuity, the watcher-first flow above is no
longer the best fit because it assumes direct Kubernetes API access to:

- `VirtualClusterInstance` resources
- Argo CD `Application` resources
- Argo CD imported cluster `Secret` resources
- optional Kargo `Promotion` resources

For hosted control planes, a lower-risk Phase 1 pattern is to keep the current
self-hosted watcher solution unchanged and add a separate Kargo-agent-driven wake
path instead:

- run a self-hosted Kargo agent in the vCluster management cluster
- assign sleeping-vCluster `Stage`s to that agent with `spec.shard`
- add an explicit `wake-vcluster` task immediately before `argocd-update`
- point that task at `cmd/proxy` or another readiness-aware wake facade

That gives Kargo a deterministic pre-sync wake step without requiring this watcher
to mutate hosted Argo CD cluster secrets.

Ready-to-adapt examples live in [examples/akuity-selfhosted-agent](examples/akuity-selfhosted-agent):

- [examples/akuity-selfhosted-agent/cluster-promotion-task-wake-vcluster.yaml](examples/akuity-selfhosted-agent/cluster-promotion-task-wake-vcluster.yaml)
- [examples/akuity-selfhosted-agent/project-secret-vcluster-platform.yaml](examples/akuity-selfhosted-agent/project-secret-vcluster-platform.yaml)
- [examples/akuity-selfhosted-agent/stage-example.yaml](examples/akuity-selfhosted-agent/stage-example.yaml)

This Akuity path is additive. It does not change the existing self-hosted
`cmd/watcher` or optional `cmd/proxy` flow documented above.

## Optional Wake Proxy

`cmd/proxy` is a small HTTP proxy for forwarding requests to a vCluster Platform upstream while handling sleeping virtual cluster wake requests more gracefully.

Its main job is to sit in front of the upstream API and treat the wake-triggering request as "accepted" when the request likely started the wake-up flow, including when the upstream returns `200 OK` or `202 Accepted` with an empty body, a transient `502` or `504`, or a retryable early transport error.

It can also, after a wake request has been accepted, patch the matching Argo CD cluster secret with a fresh `argocd.argoproj.io/refresh` timestamp so Argo invalidates its destination-cluster cache sooner.

This is useful for flows where:

- `POST /kubernetes/project/<project>/virtualcluster/<name>` triggers a wake-up
- the upstream may briefly return a retryable error before the virtual cluster is ready
- callers should not treat that initial wake trigger as a hard failure

The proxy does not blindly hide real problems. Permanent upstream responses such as `401`, `403`, `404`, malformed requests, and non-retryable transport failures are passed through normally.

### Behavior

- Proxies all requests to `UPSTREAM_BASE`
- Exposes `GET /healthz` and `GET /readyz`
- Logs upstream transport errors and upstream response statuses
- Rewrites wake-path upstream `200 OK` and `202 Accepted` responses to a small JSON acknowledgment
- Treats configured retryable statuses as accepted only for wake requests
- Optionally treats retryable wake-path transport errors as accepted when `SUCCESS_ON_ERROR=true`
- Optionally waits for the waking vCluster API to answer before returning the accepted wake response
- Optionally patches an Argo CD cluster secret after an accepted wake request to hint that Argo should refresh its cluster cache

When a wake request is treated as accepted, the proxy returns `200 OK` with a small JSON body like:

```json
{
  "ok": true,
  "accepted": true,
  "note": "wake request likely initiated; retryable upstream status treated as accepted"
}
```

### Wake Request Detection

A request is considered a wake request when both of these are true:

- method is `POST`
- path matches `/kubernetes/project/<project>/virtualcluster/<name>`

Only that request shape gets the special "accepted" handling.

### Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `UPSTREAM_BASE` | none | Required upstream base URL, for example `https://example.platform.dev` |
| `LISTEN_ADDR` | `:8080` | Address for the proxy to listen on |
| `UPSTREAM_TIMEOUT` | `10s` | HTTP client timeout for upstream requests |
| `SUCCESS_ON` | `502,504` | Comma-separated upstream status codes that should be treated as accepted for wake requests |
| `SUCCESS_ON_ERROR` | `false` | When `true`, retryable wake-path transport failures are also treated as accepted |
| `LOG_REQUESTS` | `false` | When `true`, dumps full incoming requests to the log |
| `LOG_REQUESTS_SKIP_USER_AGENTS` | `kube-probe` | Comma-separated User-Agent prefixes that should be excluded from request dump logging |
| `WAKE_READY_TIMEOUT` | disabled | When set to a positive duration, waits up to this long for the woken vCluster API to become reachable before returning the accepted wake response |
| `WAKE_READY_INTERVAL` | `2s` | Poll interval for the readiness check |
| `WAKE_READY_PATH` | `/version` | Relative path checked on the woken vCluster API while waiting for readiness |
| `ARGOCD_CLUSTER_REFRESH_SECRET_NAMESPACE` | none | Enables post-wake Argo cluster refresh and sets the namespace of the target cluster secret |
| `ARGOCD_CLUSTER_REFRESH_SECRET_NAME` | none | Exact Argo cluster secret name to patch after an accepted wake request |
| `ARGOCD_CLUSTER_REFRESH_SECRET_NAME_TEMPLATE` | none | Template for the Argo cluster secret name. Supports `{project}` and `{virtualcluster}` |
| `ARGOCD_CLUSTER_REFRESH_TIMEOUT` | `5s` | Timeout for the Argo cluster secret patch request |
| `ARGOCD_CLUSTER_REFRESH_KUBERNETES_API` | auto | Optional Kubernetes API base URL. Defaults to the in-cluster API from `KUBERNETES_SERVICE_HOST` |
| `ARGOCD_CLUSTER_REFRESH_TOKEN_PATH` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | Bearer token used for the secret patch request |
| `ARGOCD_CLUSTER_REFRESH_CA_PATH` | `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` | Cluster CA bundle used when `ARGOCD_CLUSTER_REFRESH_KUBERNETES_API` is `https://...` |

Supported `SUCCESS_ON` values are `429`, `500`, `502`, and `504`.

To enable the Argo refresh step, set `ARGOCD_CLUSTER_REFRESH_SECRET_NAMESPACE` and exactly one of `ARGOCD_CLUSTER_REFRESH_SECRET_NAME` or `ARGOCD_CLUSTER_REFRESH_SECRET_NAME_TEMPLATE`.

### Example

```yaml
env:
  - name: UPSTREAM_BASE
    value: "https://platform.example.com"
  - name: SUCCESS_ON
    value: "502,504"
  - name: SUCCESS_ON_ERROR
    value: "true"
  - name: WAKE_READY_TIMEOUT
    value: "30s"
  - name: WAKE_READY_INTERVAL
    value: "2s"
  - name: ARGOCD_CLUSTER_REFRESH_SECRET_NAMESPACE
    value: "argocd"
  - name: ARGOCD_CLUSTER_REFRESH_SECRET_NAME_TEMPLATE
    value: "loft-{project}-vcluster-{virtualcluster}"
```

With that configuration, a wake-triggering `POST /kubernetes/project/.../virtualcluster/...` request will be treated as accepted if the upstream responds with `200`, `202`, `502`, or `504`, or if it fails with a retryable early transport error. Before returning the accepted response, the proxy then waits for `GET /version` on the woken vCluster API to succeed, and only after that patches the Argo CD cluster secret annotation `argocd.argoproj.io/refresh` with the current UTC timestamp.

### Argo CD Cache Refresh Notes

- The secret refresh step is best-effort. The proxy still returns the accepted wake response even if the patch fails, and logs the refresh error for debugging.
- `WAKE_READY_TIMEOUT` is the recommended guard if Argo is racing ahead before the vCluster API is truly usable. It reuses the incoming request headers, so the readiness check runs with the same credentials Argo used for the wake request.
- Secret targeting can be static with `ARGOCD_CLUSTER_REFRESH_SECRET_NAME` or derived per request with `ARGOCD_CLUSTER_REFRESH_SECRET_NAME_TEMPLATE`.
- The templated name currently supports `{project}` and `{virtualcluster}` from the wake path `/kubernetes/project/<project>/virtualcluster/<name>`.
- For vCluster Platform-managed Argo cluster secrets, the expected template is typically `loft-{project}-vcluster-{virtualcluster}`. For example, project `demos` plus vCluster `jf-demo` becomes `loft-demos-vcluster-jf-demo`.
- The proxy patches the Kubernetes API directly, which keeps the implementation small and avoids a hard dependency on Argo's API surface.

Ready-to-apply example manifests are included in [deploy/argocd-rbac.yaml](deploy/argocd-rbac.yaml), [deploy/deployment.yaml](deploy/deployment.yaml), and [deploy/service.yaml](deploy/service.yaml).

Update the example Deployment before applying it:

- Set the container `image` to the image you actually publish for this proxy
- Set `UPSTREAM_BASE` to your vCluster Platform endpoint, for example `https://quartz.us.demo.dev`

The service account used by the proxy needs permission to patch the target cluster secret.

That manifest grants `patch` on Secrets in the `argocd` namespace, which is usually the practical choice when secret names are templated like `loft-{project}-vcluster-{virtualcluster}`.

Make sure the proxy Deployment uses that service account:

```yaml
spec:
  template:
    spec:
      serviceAccountName: vcluster-wakeup-proxy
```

If you prefer to hand-roll narrower RBAC for a fixed secret set, a minimal role looks like this:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: vcluster-wakeup-proxy-argocd-refresh
  namespace: argocd
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    resourceNames: ["loft-demos-vcluster-jf-demo"]
    verbs: ["patch"]
```

If you use `ARGOCD_CLUSTER_REFRESH_SECRET_NAME_TEMPLATE`, grant `patch` on the secret set that template can resolve to.

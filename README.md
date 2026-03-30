# vcluster-wakeup-proxy

`vcluster-wakeup-proxy` is a small HTTP proxy for forwarding requests to a vCluster Platform upstream while handling sleeping virtual cluster wake requests more gracefully.

Its main job is to sit in front of the upstream API and treat the wake-triggering request as "accepted" when the request likely started the wake-up flow, including when the upstream returns `200 OK` or `202 Accepted` with an empty body, a transient `502` or `504`, or a retryable early transport error.

It can also, after a wake request has been accepted, patch the matching Argo CD cluster secret with a fresh `argocd.argoproj.io/refresh` timestamp so Argo invalidates its destination-cluster cache sooner.

This is useful for flows where:

- `POST /kubernetes/project/<project>/virtualcluster/<name>` triggers a wake-up
- the upstream may briefly return a retryable error before the virtual cluster is ready
- callers such as Argo should not treat that initial wake trigger as a hard failure

The proxy does not blindly hide real problems. Permanent upstream responses such as `401`, `403`, `404`, malformed requests, and non-retryable transport failures are passed through normally.

## Behavior

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

## Wake Request Detection

A request is considered a wake request when both of these are true:

- method is `POST`
- path matches `/kubernetes/project/<project>/virtualcluster/<name>`

Only that request shape gets the special "accepted" handling.

## Configuration

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

## Example

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

## Argo CD Cache Refresh Notes

- The secret refresh step is best-effort. The proxy still returns the accepted wake response even if the patch fails, and logs the refresh error for debugging.
- `WAKE_READY_TIMEOUT` is the recommended guard if Argo is racing ahead before the vCluster API is truly usable. It reuses the incoming request headers, so the readiness check runs with the same credentials Argo used for the wake request.
- Secret targeting can be static with `ARGOCD_CLUSTER_REFRESH_SECRET_NAME` or derived per request with `ARGOCD_CLUSTER_REFRESH_SECRET_NAME_TEMPLATE`.
- The templated name currently supports `{project}` and `{virtualcluster}` from the wake path `/kubernetes/project/<project>/virtualcluster/<name>`.
- For vCluster Platform-managed Argo cluster secrets, the expected template is typically `loft-{project}-vcluster-{virtualcluster}`. For example, project `demos` plus vCluster `jf-demo` becomes `loft-demos-vcluster-jf-demo`.
- The proxy patches the Kubernetes API directly, which keeps the implementation small and avoids a hard dependency on Argo's API surface.

Ready-to-apply example manifests are included in [deploy/argocd-rbac.yaml](/Users/kmadel/Library%20Mobile%20Documents/com~apple~CloudDocs/projects/loft-demos/vcluster-wakeup-proxy/deploy/argocd-rbac.yaml), [deploy/deployment.yaml](/Users/kmadel/Library%20Mobile%20Documents/com~apple~CloudDocs/projects/loft-demos/vcluster-wakeup-proxy/deploy/deployment.yaml), and [deploy/service.yaml](/Users/kmadel/Library%20Mobile%20Documents/com~apple~CloudDocs/projects/loft-demos/vcluster-wakeup-proxy/deploy/service.yaml).

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

## Optional VCI Watcher

The proxy and the watcher solve different parts of the sleeping-vCluster flow:

- `cmd/proxy` handles the wake-triggering HTTP request and can wait for readiness before returning an accepted response
- `cmd/watcher` continuously reconciles Argo CD behavior from `VirtualClusterInstance` state

The watcher polls `VirtualClusterInstance` objects from the management cluster API and then:

- derives the project from `metadata.labels["loft.sh/project"]` when present, otherwise from `metadata.namespace` using `WATCH_PROJECT_NAMESPACE_PREFIXES`
- finds matching Argo CD `Application` objects by label using `WATCH_APPLICATION_PROJECT_LABEL` and `WATCH_APPLICATION_NAME_LABEL`
- classifies the vCluster as `Sleeping`, `Waking`, `Ready`, or `Unknown`
- patches the imported cluster Secret with `argocd.argoproj.io/skip-reconcile: "true"` while the vCluster is sleeping or waking
- removes `skip-reconcile` and annotates matching apps with `argocd.argoproj.io/refresh: hard` once the vCluster is ready again
- optionally patches `Application.status.health` while sleeping or waking when `WATCH_PATCH_APPLICATION_HEALTH=true`

Sleep detection prefers the platform-managed annotations `sleepmode.loft.sh/sleeping-since` and `sleepmode.loft.sh/sleep-type`, then falls back to `status.phase`, `status.reason`, `status.message`, and the `VirtualClusterOnline` condition.

Ready-to-apply example manifests are included in [deploy/watcher-rbac.yaml](/Users/kmadel/Library%20Mobile%20Documents/com~apple~CloudDocs/projects/loft-demos/vcluster-wakeup-proxy/deploy/watcher-rbac.yaml) and [deploy/watcher-deployment.yaml](/Users/kmadel/Library%20Mobile%20Documents/com~apple~CloudDocs/projects/loft-demos/vcluster-wakeup-proxy/deploy/watcher-deployment.yaml).

Important watcher settings:

| Variable | Default | Description |
| --- | --- | --- |
| `ARGOCD_CLUSTER_SECRET_NAME_TEMPLATE` | none | Required imported cluster Secret naming template. Supports `{project}` and `{virtualcluster}` |
| `ARGOCD_NAMESPACE` | `argocd` | Default namespace for Argo CD resources |
| `ARGOCD_APPLICATION_NAMESPACE` | `ARGOCD_NAMESPACE` | Namespace where matching `Application` objects live |
| `ARGOCD_CLUSTER_SECRET_NAMESPACE` | `ARGOCD_NAMESPACE` | Namespace where imported cluster Secrets live |
| `WATCH_POLL_INTERVAL` | `15s` | How often to poll `VirtualClusterInstance` objects |
| `WATCH_PROJECT_NAMESPACE_PREFIXES` | `p-,loft-p-` | Namespace prefixes used when no `loft.sh/project` label is present |
| `WATCH_APPLICATION_PROJECT_LABEL` | `vclusterProjectId` | Application label key for the project join |
| `WATCH_APPLICATION_NAME_LABEL` | `vclusterName` | Application label key for the vCluster name join |
| `WATCH_PATCH_APPLICATION_HEALTH` | `false` | When `true`, patches `Application.status.health` to `Suspended` or `Progressing` while Argo is paused |
| `WATCH_SLEEPING_MESSAGE` | `vCluster sleeping` | Health message written when app health patching is enabled |
| `WATCH_WAKING_MESSAGE` | `vCluster waking` | Health message written when app health patching is enabled |
| `WATCH_KUBERNETES_API` | auto | Optional Kubernetes API base URL. Defaults to the in-cluster API |
| `WATCH_KUBERNETES_TIMEOUT` | `10s` | Timeout for watcher Kubernetes API requests |
| `WATCH_TOKEN_PATH` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | Bearer token for watcher API calls |
| `WATCH_CA_PATH` | `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` | CA bundle for watcher API calls |

The example watcher Deployment uses `WATCH_POLL_INTERVAL=2s` so it can react quickly when a vCluster transitions into sleep.

The example watcher Deployment enables `WATCH_PATCH_APPLICATION_HEALTH=true` so it can correct the visible Argo CD app health after the vCluster is paused. If your cluster does not expose the `applications/status` subresource, the watcher falls back to patching the `Application` resource itself. If that still is not allowed in your cluster, it automatically disables health patching and continues managing cluster-secret pause/unpause plus app refresh.

Build the watcher image with [Dockerfile.watcher](/Users/kmadel/Library%20Mobile%20Documents/com~apple~CloudDocs/projects/loft-demos/vcluster-wakeup-proxy/Dockerfile.watcher).

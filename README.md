# vcluster-wakeup-proxy

`vcluster-wakeup-proxy` is a small HTTP proxy for forwarding requests to a vCluster Platform upstream while handling sleeping virtual cluster wake requests more gracefully.

Its main job is to sit in front of the upstream API and treat the wake-triggering request as "accepted" when the request likely started the wake-up flow, including when the upstream returns `200 OK` or `202 Accepted` with an empty body, a transient `502` or `504`, or a retryable early transport error.

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

Supported `SUCCESS_ON` values are `429`, `500`, `502`, and `504`.

## Example

```yaml
env:
  - name: UPSTREAM_BASE
    value: "https://platform.example.com"
  - name: SUCCESS_ON
    value: "502,504"
  - name: SUCCESS_ON_ERROR
    value: "true"
```

With that configuration, a wake-triggering `POST /kubernetes/project/.../virtualcluster/...` request will be treated as accepted if the upstream responds with `200`, `202`, `502`, or `504`, or if it fails with a retryable early transport error. Other requests still pass through normally.

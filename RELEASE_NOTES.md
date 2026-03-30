# Release Notes

## Wake Request Handling

This release improves how `vcluster-wakeup-proxy` handles wake-triggering virtual cluster requests.

Previously, the proxy behavior was described mainly in terms of converting upstream `502` and `504` responses into success. With this update, the behavior is more intentional: the proxy now treats the wake-triggering request as accepted when the upstream likely initiated the wake-up flow but returned a transient retryable failure before the virtual cluster was ready.

## What's Changed

- Wake-specific handling now applies only to `POST /kubernetes/project/<project>/virtualcluster/<name>`
- Configured retryable upstream statuses such as `502` and `504` are treated as accepted only for that wake request path
- When `SUCCESS_ON_ERROR=true`, retryable early transport failures on the wake path can also be treated as accepted
- Permanent upstream failures such as `401`, `403`, `404`, malformed requests, and non-retryable transport errors are still returned normally
- The proxy now logs upstream response statuses and upstream transport errors to make debugging easier

## Why It Matters

This better matches the real wake-up behavior of sleeping virtual clusters. A wake request may successfully start the wake-up flow even when the upstream returns a temporary error right away. With this change, callers such as Argo can treat that initial wake trigger as accepted without hiding genuine configuration or authorization problems.

## Validation

- Added test coverage for wake acceptance behavior
- Added test coverage for non-wake pass-through behavior
- Added test coverage for retryable and non-retryable transport failures

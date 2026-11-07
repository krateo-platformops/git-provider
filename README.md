# Git Provider

This is a [Krateo](https://krateo.io) Provider that enables Git operations natively from your Kubernetes cluster.

## Overview

The `git-provider` leverages Krateo [provider-runtime](https://docs.krateo.io/key-concepts/kco/#provider-runtime), a production-grade version of the controller-runtime, to provide automatic reconciliation and Git interactions.

It exposes two distinct Custom Resources (CRs) to handle different use cases:

*   **[Repo](docs/repo.md):** Designed for **Git-to-Git** workflows. It clones an existing Git repository, optionally applies templates ([Mustache](https://mustache.github.io) or [Go templates](https://pkg.go.dev/text/template)) to the files using values from a `ConfigMap`, and pushes the result to a destination repository.
*   **[LocalResource](docs/local-resource.md):** Designed for **K8s-to-Git** workflows. It takes a local source (such as an embedded Kubernetes manifest, a reference to an existing cluster resource, or a raw string), optionally applies placeholder replacements, and commits the result directly to a destination Git repository.

## Installation

```bash
$ helm repo add krateo https://charts.krateo.io
$ helm repo update krateo
$ helm install git-provider krateo/git-provider
```

## Documentation

For detailed configuration, templating rules, and synchronization behavior, please refer to the specific documentation for each Custom Resource:

*   📖 [**Repo CR Documentation**](docs/repo.md)
*   📖 [**LocalResource CR Documentation**](docs/local-resource.md)

## Environment Variables

The provider controller can be configured using the following environment variables:

| Environment Variable | Type | Default Value | Description |
|---------------------|------|---------------|-------------|
| `GIT_PROVIDER_DEBUG` | bool | `false` | Run with debug logging |
| `GIT_PROVIDER_SYNC_PERIOD` | duration | `1h` | Controller manager sync period (e.g., 300ms, 1.5h, or 2h45m) |
| `GIT_PROVIDER_POLL_INTERVAL` | duration | `3m` | Poll interval controls how often an individual resource should be checked for drift |
| `GIT_PROVIDER_MAX_RECONCILE_RATE` | int | `5` | The number of concurrent reconciles for each controller. Maximum number of resources that can be reconciled at the same time |
| `GIT_PROVIDER_LEADER_ELECTION` | bool | `false` | Use leader election for the controller manager |
| `GIT_PROVIDER_MAX_ERROR_RETRY_INTERVAL` | duration | `1m` | The maximum interval between retries when an error occurs. Should be less than half of the poll interval |
| `GIT_PROVIDER_MIN_ERROR_RETRY_INTERVAL` | duration | `1s` | The minimum interval between retries when an error occurs. Should be less than max-error-retry-interval |
| `GIT_PROVIDER_TIMEOUT` | duration | `4m` | The time all the external calls of one reconcile share, clones and pushes included. A call still running at the deadline fails. Raise it for repositories that take longer to clone and push (with the chart: `--set-string env.GIT_PROVIDER_TIMEOUT=10m`) |
| `GIT_PROVIDER_GIT_COMMIT_AUTHOR_NAME` | string | `krateo-git-provider` | The name to use for git commits. |
| `GIT_PROVIDER_GIT_COMMIT_AUTHOR_EMAIL` | string | `contact@krateo.io` | The email to use for git commits. |
| `GIT_PROVIDER_RATE_LIMIT_GOVERNOR` | bool | `false` (the chart sets `true`) | Slow reconciles down per remote and credential when a git server signals a rate limit |

## CRD Reference
To view the generated CR configuration schema, visit [this link](https://doc.crds.dev/github.com/krateoplatformops/git-provider).
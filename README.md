# Kube Actions Runner

A Kubernetes-native webhook scaler for GitHub Actions self-hosted runners. Supports multiple repositories and owners without requiring a GitHub Organization by using webhook-triggered ephemeral runners with JIT (Just-In-Time) configuration.

## Features

- **Multi-repo support without GitHub Organization** - Single deployment handles webhooks from any number of repositories
- **Ephemeral runners with JIT configuration** - Runners register on-demand and auto-deregister after job completion
- **Four security modes** - standard, userns, dind, dind-rootless
- **Auto-cleanup via Kubernetes Job TTL** - Completed jobs automatically deleted after 5 minutes
- **Zero idle cost** - Runners scale to zero when not in use

## Quick Start

### Prerequisites

- **Kubernetes 1.33+** for user namespace modes (userns, dind-rootless), or any version for standard/dind modes
- **GitHub fine-grained PAT** with the following repository permissions:
  - Actions: Read and write
  - Administration: Read and write
- **Ingress** - Cloudflare Tunnel recommended (or any ingress that can route to the kube-actions-runner service)

### Installation

#### 1. Create namespace

```bash
kubectl create namespace github-runners
```

#### 2. Create secrets

```bash
# Generate a random webhook secret
WEBHOOK_SECRET=$(openssl rand -hex 32)

# Create the secret (replace YOUR_GITHUB_TOKEN with your fine-grained PAT)
kubectl create secret generic kube-actions-runner-secrets \
  --namespace github-runners \
  --from-literal=webhook-secret="${WEBHOOK_SECRET}" \
  --from-literal=github-token="YOUR_GITHUB_TOKEN"

# Save the webhook secret for GitHub configuration
echo "Webhook secret: ${WEBHOOK_SECRET}"
```

#### 3. Apply manifests

```bash
kubectl apply -f kubernetes/kube-actions-runner/rbac.yaml -n github-runners
kubectl apply -f kubernetes/kube-actions-runner/deployment.yaml -n github-runners
kubectl apply -f kubernetes/kube-actions-runner/service.yaml -n github-runners
```

#### 4. Configure ingress

If using Cloudflare Tunnel, add to your tunnel config:

```yaml
ingress:
  - hostname: kube-actions-runner.yourdomain.com
    service: http://kube-actions-runner.github-runners.svc.cluster.local:80
```

#### 5. Configure GitHub webhook

See [GitHub Webhook Setup](#github-webhook-setup) below.

### Configuration

#### Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `WEBHOOK_SECRET` | Yes | - | HMAC-SHA256 secret for webhook validation |
| `GITHUB_TOKEN` | Yes | - | Fine-grained PAT with Actions + Administration permissions |
| `RUNNER_MODE` | No | `dind-rootless` | Runner mode: `standard`, `userns`, `dind`, or `dind-rootless` |
| `RUNNER_IMAGE` | No | Mode-dependent | Override default runner image |
| `DIND_IMAGE` | No | `docker:dind` | Docker-in-Docker sidecar image (dind mode only) |
| `NAMESPACE` | No | `default` | Namespace for runner Jobs |
| `LABEL_MATCHERS` | No | - | Label matching rules (see below) |
| `PORT` | No | `8080` | HTTP server port |

#### Label Matchers

Control which jobs the scaler handles using `LABEL_MATCHERS`:

```bash
# Handle any job with "linux" OR "x64" label
LABEL_MATCHERS="linux,x64"

# Handle jobs with BOTH "linux" AND "x64" labels (exact match)
LABEL_MATCHERS="linux+x64"

# Multiple matchers (semicolon-separated)
LABEL_MATCHERS="linux+x64;arm64"
```

If `LABEL_MATCHERS` is not set, the scaler handles all jobs with the `self-hosted` label.

#### Default Runner Images

| Mode | Default Image |
|------|---------------|
| `standard` | `ghcr.io/actions/actions-runner:latest` |
| `userns` | `ghcr.io/actions/actions-runner:latest` |
| `dind` | `ghcr.io/actions/actions-runner:latest` |
| `dind-rootless` | `ghcr.io/actions/actions-runner-dind-rootless:latest` |

### GitHub Webhook Setup

1. Go to your repository **Settings > Webhooks**
2. Click **Add webhook**
3. Configure:
   - **Payload URL**: `https://kube-actions-runner.yourdomain.com/webhook`
   - **Content type**: `application/json`
   - **Secret**: The webhook secret from step 2 of installation
4. Under "Which events would you like to trigger this webhook?":
   - Select **Let me select individual events**
   - Check only **Workflow jobs**
5. Click **Add webhook**

Repeat for each repository you want to use self-hosted runners.

### Workflow Configuration

In your GitHub Actions workflows, use:

```yaml
jobs:
  build:
    runs-on: [self-hosted, linux, x64]  # Labels must include "self-hosted"
    steps:
      - uses: actions/checkout@v4
      # ... your steps
```

## Architecture

The scaler receives `workflow_job` webhooks from GitHub, validates the HMAC signature, generates JIT runner configuration via the GitHub API, and creates ephemeral Kubernetes Jobs.

```
GitHub webhook -> Cloudflare Tunnel -> Kube Actions Runner -> Kubernetes Job -> Runner Pod
```

Key design decisions:
- **No message queue** - GitHub's webhook retry mechanism provides persistence
- **Synchronous processing** - Returns 200 only after Job is created
- **TTL cleanup** - Jobs auto-delete 5 minutes after completion

See [ARCHITECTURE.md](ARCHITECTURE.md) for detailed design documentation.

## Runner Modes

| Mode | Docker Support | Security | Requirements |
|------|----------------|----------|--------------|
| `standard` | No | High | Any Kubernetes |
| `userns` | No | High | K8s 1.33+, Kernel 6.3+, containerd 2.0+ |
| `dind` | Yes | Low (privileged) | Any Kubernetes |
| `dind-rootless` | Yes | Medium-High | K8s 1.33+, Kernel 6.3+, containerd 2.0+ |

**Recommended**: `dind-rootless` provides Docker support with user namespace isolation, containing privilege escalation within the user namespace.

See [ARCHITECTURE.md#runner-modes](ARCHITECTURE.md#runner-modes) for detailed mode descriptions.

## Monitoring

### Prometheus Metrics

Metrics are exposed at `/metrics`:

| Metric | Type | Description |
|--------|------|-------------|
| `webhook_requests_total` | Counter | Total webhook requests by status, owner, repo |
| `webhook_latency_seconds` | Histogram | Webhook processing latency |
| `runner_jobs_created_total` | Counter | Runner jobs created by owner, repo |
| `runner_jobs_active` | Gauge | Currently active runner jobs |
| `github_api_requests_total` | Counter | GitHub API requests by endpoint, status |
| `github_api_rate_limit_remaining` | Gauge | Remaining GitHub API rate limit |

### Health Endpoints

- `GET /health` - Liveness probe
- `GET /ready` - Readiness probe

### ServiceMonitor (Prometheus Operator)

For clusters using [Prometheus Operator](https://github.com/prometheus-operator/prometheus-operator), a ServiceMonitor resource is provided that scrapes the `/metrics` endpoint every 30 seconds:

```bash
kubectl apply -f kubernetes/kube-actions-runner/servicemonitor.yaml
```

## Troubleshooting

### Webhook returns 401 Unauthorized

The HMAC signature validation failed. Verify that:
- The `WEBHOOK_SECRET` in the Kubernetes secret matches the GitHub webhook secret exactly
- There are no extra whitespace characters in the secret

### Jobs are not being created

Check the scaler logs:
```bash
kubectl logs -n github-runners deployment/kube-actions-runner
```

Common issues:
- **"job labels do not match"** - Configure `LABEL_MATCHERS` or ensure your workflow uses `runs-on: [self-hosted, ...]`
- **"job is no longer queued"** - The job was picked up by another runner or cancelled
- **"failed to generate JIT config"** - GitHub token may lack required permissions

### Runner pod fails to start

Check the runner pod logs:
```bash
kubectl logs -n github-runners job/runner-<job-id>
```

Common issues:
- **Image pull errors** - Verify the runner image is accessible
- **User namespace errors** (userns/dind-rootless) - Ensure Kubernetes 1.33+ with user namespace support enabled

### Jobs stuck in pending

Check for:
- Insufficient cluster resources (CPU/memory)
- Node selector or affinity rules preventing scheduling
- Pod security policies blocking the runner configuration

### Orphaned jobs not cleaning up

Jobs automatically delete 5 minutes after completion via TTL. For stuck jobs:
```bash
# List runner jobs
kubectl get jobs -n github-runners -l app=github-runner

# Delete a specific job
kubectl delete job -n github-runners runner-<job-id>
```

## License

MIT

# Cloudflare Tunnel Setup for Webhook Scaler

Cloudflare Tunnel provides secure ingress to the webhook-scaler without exposing ports to the internet.

## Prerequisites

- [cloudflared CLI](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/)
- Cloudflare account with a domain
- kubectl configured for your cluster

## Setup Steps

### 1. Create a Tunnel

```bash
# Authenticate with Cloudflare
cloudflared tunnel login

# Create the tunnel
cloudflared tunnel create github-webhooks
```

This creates a credentials file at `~/.cloudflared/<tunnel-id>.json`. Note the tunnel ID.

### 2. Configure DNS

```bash
# Create a CNAME record pointing to the tunnel
cloudflared tunnel route dns github-webhooks github-webhooks.yourdomain.com
```

Or manually add a CNAME record in Cloudflare DNS:
- Name: `github-webhooks`
- Target: `<tunnel-id>.cfargotunnel.com`

### 3. Create Kubernetes Resources

```bash
# Create the namespace if it doesn't exist
kubectl create namespace github-runners

# Create the secret from your credentials file
kubectl create secret generic cloudflared-credentials \
  --namespace=github-runners \
  --from-file=credentials.json=$HOME/.cloudflared/<tunnel-id>.json

# Copy and edit the configmap
cp configmap.yaml.example configmap.yaml
# Edit configmap.yaml: replace tunnel ID and hostname

# Apply the resources
kubectl apply -f configmap.yaml
kubectl apply -f deployment.yaml
```

### 4. Configure GitHub Webhook

In your GitHub repository settings:
1. Go to Settings > Webhooks > Add webhook
2. Payload URL: `https://github-webhooks.yourdomain.com/webhook`
3. Content type: `application/json`
4. Secret: (your webhook secret)
5. Events: Select "Workflow jobs"

### 5. Verify

```bash
# Check tunnel is running
kubectl logs -n github-runners -l app=cloudflared

# Test the endpoint
curl https://github-webhooks.yourdomain.com/health
```

## Troubleshooting

### Tunnel not connecting
- Verify credentials secret is correct
- Check cloudflared logs: `kubectl logs -n github-runners -l app=cloudflared`

### 502 errors
- Ensure webhook-scaler service is running
- Verify service name in config matches actual service

### DNS not resolving
- Confirm CNAME record exists in Cloudflare DNS
- Wait for DNS propagation (usually instant with Cloudflare)

## References

- [Cloudflare Tunnel Documentation](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/)
- [Run cloudflared in Kubernetes](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/deploy-tunnels/deployment-guides/kubernetes/)

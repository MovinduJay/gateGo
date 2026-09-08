# Kubernetes deployment

1. Build/push `gatego` and `gatego-demo` images.
2. Replace `ghcr.io/YOUR_GITHUB/...` in `all.yaml`.
3. Replace development secrets.
4. Apply:

```bash
kubectl apply -f deploy/k8s/all.yaml
kubectl apply -f deploy/k8s/hpa.yaml   # optional; requires metrics-server
```

The gateway runs with two replicas and uses Redis for a shared rate-limit state. The `LoadBalancer` service is appropriate for managed Kubernetes; for Minikube/kind, switch it to `NodePort` or use port-forwarding.

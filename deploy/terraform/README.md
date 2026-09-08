# Google Cloud Run via Terraform

This deploys three scale-to-zero demo backends and one public GateGo service. The gateway configuration is generated from the backend Cloud Run URLs and passed through `GATEGO_CONFIG_JSON`.

```bash
cd deploy/terraform
cp terraform.tfvars.example terraform.tfvars
terraform init
terraform plan
terraform apply
```

Redis is optional. Without it, GateGo automatically uses its in-process token bucket. For multiple gateway replicas, provide a Redis endpoint (`redis://` or TLS `rediss://`) so rate-limit state is shared.

Cloud Run cold starts can make the first request slower because the demo deliberately uses `min_instance_count = 0` to stay friendly to free/low-cost usage.

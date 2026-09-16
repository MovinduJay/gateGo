variable "project_id" {
  type = string
}

variable "region" {
  type    = string
  default = "asia-southeast1"
}

variable "gateway_image" {
  type        = string
  description = "Container image for cmd/gatego"
}

variable "demo_image" {
  type        = string
  description = "Container image for cmd/demo-backend"
}

variable "jwt_secret" {
  type      = string
  sensitive = true
}

variable "admin_key" {
  type      = string
  sensitive = true
}

variable "redis_addr" {
  type        = string
  default     = ""
  description = "Optional redis:// or rediss:// endpoint"
}

variable "redis_password" {
  type      = string
  default   = ""
  sensitive = true
}

variable "otel_exporter_otlp_endpoint" {
  type        = string
  default     = ""
  description = "Optional OTLP/HTTP collector endpoint"
}

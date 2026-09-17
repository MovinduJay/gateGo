provider "google" {
  project = var.project_id
  region  = var.region
}

resource "google_project_service" "run" {
  service            = "run.googleapis.com"
  disable_on_destroy = false
}

locals {
  backends = {
    a = "backend-a"
    b = "backend-b"
    c = "backend-c"
  }
}

resource "google_cloud_run_v2_service" "backend" {
  for_each            = local.backends
  name                = "gatego-${each.value}"
  location            = var.region
  deletion_protection = false
  depends_on          = [google_project_service.run]

  template {
    scaling {
      min_instance_count = 0
      max_instance_count = 1
    }

    containers {
      image = var.demo_image

      env {
        name  = "BACKEND_NAME"
        value = each.value
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "128Mi"
        }
        cpu_idle = true
      }
    }
  }
}

resource "google_cloud_run_v2_service_iam_member" "backend_public" {
  for_each = google_cloud_run_v2_service.backend
  project  = var.project_id
  location = each.value.location
  name     = each.value.name
  role     = "roles/run.invoker"
  member   = "allUsers"
}

locals {
  gateway_config = jsonencode({
    server = {
      listen                 = ":8080"
      read_header_timeout    = "5s"
      idle_timeout           = "60s"
      shutdown_timeout       = "10s"
      max_request_body_bytes = 2097152
    }
    health = {
      interval          = "5s"
      timeout           = "2s"
      failure_threshold = 2
      success_threshold = 1
    }
    observability = {
      service_name = "gatego"
    }
    routes = [
      {
        name          = "public-api"
        path_prefix   = "/public"
        methods       = ["GET", "POST"]
        auth_required = false
        rate_limit = {
          requests_per_second = 25
          burst               = 50
        }
        resilience = {
          circuit_failure_threshold = 20
          circuit_cooldown           = "10s"
          max_concurrent_per_backend = 16
          max_concurrent_retries     = 8
        }
        timeout = "8s"
        retries = 1
        backends = [
          {
            url         = google_cloud_run_v2_service.backend["a"].uri
            weight      = 3
            health_path = "/healthz"
          },
          {
            url         = google_cloud_run_v2_service.backend["b"].uri
            weight      = 1
            health_path = "/healthz"
          }
        ]
      },
      {
        name          = "protected-api"
        path_prefix   = "/api"
        methods       = ["GET", "POST", "PUT", "PATCH", "DELETE"]
        auth_required = true
        rate_limit = {
          requests_per_second = 50
          burst               = 100
        }
        timeout = "8s"
        retries = 1
        backends = [
          {
            url         = google_cloud_run_v2_service.backend["a"].uri
            weight      = 2
            health_path = "/healthz"
          },
          {
            url         = google_cloud_run_v2_service.backend["b"].uri
            weight      = 1
            health_path = "/healthz"
          },
          {
            url         = google_cloud_run_v2_service.backend["c"].uri
            weight      = 1
            health_path = "/healthz"
          }
        ]
      }
    ]
  })
}

resource "google_cloud_run_v2_service" "gateway" {
  name                = "gatego"
  location            = var.region
  deletion_protection = false
  depends_on          = [google_cloud_run_v2_service_iam_member.backend_public]

  template {
    scaling {
      min_instance_count = 0
      max_instance_count = 2
    }

    containers {
      image = var.gateway_image

      env {
        name  = "GATEGO_CONFIG_JSON"
        value = local.gateway_config
      }
      env {
        name  = "GATEGO_JWT_SECRET"
        value = var.jwt_secret
      }
      env {
        name  = "GATEGO_ADMIN_KEY"
        value = var.admin_key
      }
      env {
        name  = "GATEGO_REDIS_ADDR"
        value = var.redis_addr
      }
      env {
        name  = "GATEGO_REDIS_PASSWORD"
        value = var.redis_password
      }
      env {
        name  = "OTEL_EXPORTER_OTLP_ENDPOINT"
        value = var.otel_exporter_otlp_endpoint
      }
      env {
        name  = "OTEL_EXPORTER_OTLP_PROTOCOL"
        value = "http/protobuf"
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "256Mi"
        }
        cpu_idle = true
      }
    }
  }
}

resource "google_cloud_run_v2_service_iam_member" "gateway_public" {
  project  = var.project_id
  location = google_cloud_run_v2_service.gateway.location
  name     = google_cloud_run_v2_service.gateway.name
  role     = "roles/run.invoker"
  member   = "allUsers"
}

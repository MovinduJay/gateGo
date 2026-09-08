output "gateway_url" {
  value = google_cloud_run_v2_service.gateway.uri
}

output "backend_urls" {
  value = { for k, v in google_cloud_run_v2_service.backend : k => v.uri }
}

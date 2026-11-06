resource "virtscale_s3_bucket" "logs" {
  name           = "example-logs"
  versioning     = true
  max_size_bytes = 107374182400 # 100 GiB
  max_objects    = 1000000
}

data "virtscale_compute_limits" "grid" {}

# The grid a size must sit on; the instance's memory_mb is memory_gb times 1024.
output "largest_size" {
  value = {
    vcpus     = max(data.virtscale_compute_limits.grid.vcpus_steps...)
    memory_mb = data.virtscale_compute_limits.grid.memory_gb.max * 1024
    disk_gb   = data.virtscale_compute_limits.grid.disk_gb.max
  }
}

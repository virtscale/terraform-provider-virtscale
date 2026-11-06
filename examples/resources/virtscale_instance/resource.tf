data "virtscale_images" "debian" {
  region = "gro"
}

resource "virtscale_instance" "web" {
  name            = "web-01"
  hostname        = "web-01.example.com"
  region          = "gro"
  image           = [for i in data.virtscale_images.debian.images : i.id if startswith(i.name, "Debian 12")][0]
  vcpus           = 1
  memory_mb       = 2048
  disk_gb         = 40
  storage_pool    = "gro-ceph"
  network_profile = "gro-tenant-default"
  ssh_keys        = ["laptop"] # a virtscale_ssh_key, taken by name
}

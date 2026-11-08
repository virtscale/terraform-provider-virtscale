data "virtscale_images" "debian" {
  region = "gro"
}

# The size is given as vcpus, memory_mb, disk_gb and a storage_pool (the catalogue of instance types is empty in
# production). vcpus is 1, 2, 4, 8, 16 or 32; memory is whole gigabytes from 1 to 128, in megabytes (GB x 1024); the disk
# is 10 to 2000 GB in steps of 10. network_profile may be left out: the platform picks the first one your organization may
# use in the region (gro-tenant-default in gro).
resource "virtscale_instance" "web" {
  name            = "web-01"
  hostname        = "web-01.example.com"
  region          = "gro"
  image           = [for i in data.virtscale_images.debian.images : i.id if startswith(i.name, "Debian 12")][0]
  vcpus           = 2
  memory_mb       = 4096 # 4 GB
  disk_gb         = 20
  storage_pool    = "gro-ceph"
  network_profile = "gro-tenant-default"
  ssh_keys        = ["laptop"] # a virtscale_ssh_key, taken by name
}

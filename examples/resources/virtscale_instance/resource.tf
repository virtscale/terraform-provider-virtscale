data "virtscale_images" "debian" {
  region = "gro"
}

# The first VM needs no ids: an instance type from the virtscale_instance_types data source sets the sizes and the
# platform picks the storage pool and the network profile. Name a storage_pool or network_profile only to override
# its choice.
resource "virtscale_instance" "web" {
  name          = "web-01"
  hostname      = "web-01.example.com"
  region        = "gro"
  image         = [for i in data.virtscale_images.debian.images : i.id if startswith(i.name, "Debian 12")][0]
  instance_type = "g1-small"
  ssh_keys      = ["laptop"] # a virtscale_ssh_key, taken by name
}

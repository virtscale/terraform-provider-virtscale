terraform {
  required_providers {
    virtscale = {
      source = "virtscale/virtscale"
    }
  }
}

# No credentials here: set VIRTSCALE_API_KEY and VIRTSCALE_ORGANIZATION (the
# organization's UUID) in the environment instead. The key needs the
# compute:write scope (instances, images and SSH keys), and network:write as
# well for the public IP below.
provider "virtscale" {}

data "virtscale_images" "gro" {
  region = "gro"
}

locals {
  # Pick the Debian 12 image out of the catalogue by name prefix.
  debian_12_image_id = [
    for img in data.virtscale_images.gro.images : img.id
    if startswith(img.name, "Debian 12")
  ][0]
}

# The public key you log in with, imported once and taken by name below.
# cloud-init installs it for the image's default user (debian on the Debian
# images) when the instance first boots. file() keeps the comment and the
# trailing newline of the .pub file: Terraform compares the key itself, so they
# never show up as a difference. Deleting this key later does not change the
# instance, which keeps the key it booted with.
resource "virtscale_ssh_key" "example" {
  name       = "example"
  public_key = file("~/.ssh/id_ed25519.pub")
}

resource "virtscale_instance" "example" {
  name            = "example"
  hostname        = "example.example.com"
  region          = "gro"
  image           = local.debian_12_image_id
  vcpus           = 1
  memory_mb       = 2048
  disk_gb         = 40
  storage_pool    = "gro-ceph"
  network_profile = "gro-tenant-default"
  ssh_keys        = [virtscale_ssh_key.example.name]
}

output "ssh_key_fingerprint" {
  value = virtscale_ssh_key.example.fingerprint
}

output "id" {
  value = virtscale_instance.example.id
}

output "state" {
  value = virtscale_instance.example.state
}

output "hostname" {
  value = virtscale_instance.example.hostname
}

output "private_ipv4" {
  value = virtscale_instance.example.private_ipv4
}

# A customer organization's public IP quota is one per region. Uncomment to allocate
# one and associate it with the instance above.
#
# resource "virtscale_public_ip" "example" {
#   region      = "gro"
#   instance_id = virtscale_instance.example.id
# }
#
# output "public_ip_address" {
#   value = virtscale_public_ip.example.address
# }

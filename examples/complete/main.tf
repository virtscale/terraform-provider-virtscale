terraform {
  required_providers {
    virtscale = {
      source = "virtscale/virtscale"
    }
  }
}

# No credentials here: set VIRTSCALE_API_KEY and VIRTSCALE_ORGANIZATION (the organization's UUID) in the
# environment instead. The key needs the write scope (the project), compute:write (instances, images, SSH keys),
# network:write (VPC, firewall, public IP) and storage:write (S3), or just write.
provider "virtscale" {}

variable "region" {
  type        = string
  description = "The region to build in; see the virtscale_regions data source."
  default     = "gro"
}

variable "public_key_path" {
  type        = string
  description = "The OpenSSH public key you log in with."
  default     = "~/.ssh/id_ed25519.pub"
}

data "virtscale_caller" "me" {}

data "virtscale_images" "region" {
  region = var.region
}

locals {
  # Pick the Debian 12 image out of the catalogue by name prefix.
  debian_12_image_id = [
    for img in data.virtscale_images.region.images : img.id
    if startswith(img.name, "Debian 12")
  ][0]
}

# A project groups what belongs together; the VPC lives in it.
resource "virtscale_project" "example" {
  name        = "example"
  description = "The example project"
  labels      = { env = "example" }
}

resource "virtscale_vpc" "example" {
  project_id = virtscale_project.example.id
  name       = "example"
  region     = var.region
}

# The public key you log in with, imported once and taken by name below. cloud-init installs it for the image's
# default user (debian on the Debian images) when the instance first boots.
resource "virtscale_ssh_key" "example" {
  name       = "example"
  public_key = file(var.public_key_path)
}

# An instance type sets the sizes; the platform chooses the storage pool and the network profile.
resource "virtscale_instance" "example" {
  name          = "example"
  hostname      = "example.example.com"
  region        = var.region
  image         = local.debian_12_image_id
  instance_type = "g1-small"
  ssh_keys      = [virtscale_ssh_key.example.name]
}

# The whole rule set of the instance; ssh from one network only.
resource "virtscale_instance_firewall" "example" {
  instance_id = virtscale_instance.example.id

  rule {
    direction   = "ingress"
    protocol    = "tcp"
    port_min    = 22
    port_max    = 22
    remote_cidr = "203.0.113.0/24"
    description = "ssh from the office"
  }

  rule {
    direction   = "ingress"
    protocol    = "icmp"
    remote_cidr = "0.0.0.0/0"
  }
}

# Object storage: a bucket with a ceiling, and a key for an application. The key's secret is in the state, sensitive.
resource "virtscale_s3_bucket" "example" {
  name           = "example-bucket"
  max_size_bytes = 10737418240 # 10 GiB
}

resource "virtscale_s3_access_key" "example" {}

output "organization_id" {
  value = data.virtscale_caller.me.organization_id
}

output "id" {
  value = virtscale_instance.example.id
}

output "state" {
  value = virtscale_instance.example.state
}

output "private_ipv4" {
  value = virtscale_instance.example.private_ipv4
}

output "vpc_cidr" {
  value = virtscale_vpc.example.cidr
}

output "s3_access_key_id" {
  value = virtscale_s3_access_key.example.access_key_id
}

output "s3_secret_access_key" {
  value     = virtscale_s3_access_key.example.secret_access_key
  sensitive = true
}

# A customer organization may hold one public IP per region by default (its quota decides). Uncomment to allocate one
# and associate it with the instance above; the resource is validated on its own in examples/resources/virtscale_public_ip.
#
# resource "virtscale_public_ip" "example" {
#   region      = var.region
#   instance_id = virtscale_instance.example.id
# }

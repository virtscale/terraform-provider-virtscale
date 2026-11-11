data "virtscale_images" "debian" {
  region = "gro"
}

# The size is given as vcpus, memory_mb, disk_gb and a storage_pool (the catalogue of instance types is empty in
# production). vcpus is 1, 2, 4, 8, 16 or 32; memory is whole gigabytes from 1 to 128, in megabytes (GB x 1024); the disk
# is 10 to 2000 GB in steps of 10. network_profile may be left out: the platform picks the first one your organization may
# use in the region (gro-tenant-default in gro). A change of vcpus or memory_mb later resizes the instance in place (it
# restarts); a change of disk_gb replaces it.
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

  # cloud-init for the first boot, plain text (sensitive; a different value replaces the instance).
  user_data = <<-EOT
    #cloud-config
    packages:
      - htop
  EOT

  # While true, deleting the instance (a destroy, or a replacement) is refused with 409 vm_protected: set it to false and
  # apply first. Changing it is an in-place update.
  disable_api_termination = true
}

# Internal organizations only (VirtScale's own): any other organization is refused with 422 host_placement_not_allowed
# (host_id) or network_interface_not_allowed (network_interface).
# The instance is placed on a named host (host_id, not case sensitive) and boots from a root volume on that host's local
# storage: root_block_device is another spelling of storage_pool and disk_gb (volume_type is the storage pool,
# volume_size the disk in GB), and a host-local pool such as ams1-local needs host_id. network_interface block i sets
# interface i of the network profile: a fixed private_ip and mac_address, and secondary_private_ips (the shared address
# of a VRRP or keepalived pair; they need source_dest_check = true, the default). They are set at create: a different
# block replaces the instance. An instance with a host_id is never resized in place either: a different vcpus or
# memory_mb replaces it.
data "virtscale_images" "debian_ams" {
  region = "ams"
}

resource "virtscale_instance" "dns" {
  name            = "dns-01"
  region          = "ams"
  image           = [for i in data.virtscale_images.debian_ams.images : i.id if startswith(i.name, "Debian 12")][0]
  vcpus           = 2
  memory_mb       = 2048
  host_id         = "IKVM-AMS1-01"
  network_profile = "<profile id>" # no default on the AMS1 hosts: list GET /v1/orgs/{org}/compute/network-profiles and name one

  root_block_device {
    volume_type = "ams1-local"
    volume_size = 40
  }

  network_interface {
    private_ip            = "10.20.30.40" # an address of that network's subnet
    mac_address           = "fa:16:3e:aa:bb:cc"
    secondary_private_ips = ["10.20.30.41"]
  }
}

---
page_title: "virtscale_instance Resource - terraform-provider-virtscale"
subcategory: ""
description: |-
  A VirtScale compute instance (VM). An instance resized outside Terraform keeps its instance_type in the API. If the configuration names only instance_type, the next refresh shows the new vcpus and memory_mb in state but plans no change. To manage the sizes, set vcpus, memory_mb, disk_gb and storage_pool in the configuration instead of instance_type: a vcpus or memory_mb that then differs resizes the instance in place (it restarts), a disk_gb that differs plans a replacement. Size an instance with vcpus, memory_mb, disk_gb and storage_pool. The API takes this grid: vcpus is 1, 2, 4, 8, 16 or 32; memory is whole gigabytes from 1 to 128, given in megabytes (gigabytes times 1024, so memory_mb is 1024 to 131072); disk_gb is 10 to 2000 in steps of 10. A size off the grid is refused with 422 invalid_spec, which lists every violation. Where to look the values up today: the API at GET /v1/orgs/{org}/compute/limits, /compute/storage-pools and /compute/network-profiles. The catalogue of instance types is empty in production, so there instance_type is refused with 422 instance_type_unpriced. The min_disk_gb and min_ram_mb of an image are 0 today: they are no guide to a size. The provider does not manage power state. An instance stopped through the API stays stopped and the plan stays empty; state shows stopped after the next refresh. A create takes about 30 to 60 seconds and a destroy about 15 seconds (apply waits up to 20 minutes for a create and 10 for a destroy).
---

# virtscale_instance (Resource)

A VirtScale compute instance (VM).

An instance resized outside Terraform keeps its instance_type in the API. If the configuration names only instance_type, the next refresh shows the new vcpus and memory_mb in state but plans no change. To manage the sizes, set vcpus, memory_mb, disk_gb and storage_pool in the configuration instead of instance_type: a vcpus or memory_mb that then differs resizes the instance in place (it restarts), a disk_gb that differs plans a replacement.

Size an instance with vcpus, memory_mb, disk_gb and storage_pool. The API takes this grid: vcpus is 1, 2, 4, 8, 16 or 32; memory is whole gigabytes from 1 to 128, given in megabytes (gigabytes times 1024, so memory_mb is 1024 to 131072); disk_gb is 10 to 2000 in steps of 10. A size off the grid is refused with 422 invalid_spec, which lists every violation. Where to look the values up today: the API at GET /v1/orgs/{org}/compute/limits, /compute/storage-pools and /compute/network-profiles. The catalogue of instance types is empty in production, so there instance_type is refused with 422 instance_type_unpriced. The min_disk_gb and min_ram_mb of an image are 0 today: they are no guide to a size.

The provider does not manage power state. An instance stopped through the API stays stopped and the plan stays empty; state shows stopped after the next refresh. A create takes about 30 to 60 seconds and a destroy about 15 seconds (apply waits up to 20 minutes for a create and 10 for a destroy).

## Example Usage

```terraform
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
  network_profile = "ams-ssd-infra" # AMS1's k8s-geneve network, for internal organizations

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
```

## Schema

### Required

- `image` (String) The image id to boot from, as listed by the images catalogue. A create-time input only: neither Create nor Read ever overwrites it from the API, so it always reflects what was configured (import is the one exception, seeding it once from the observed value since there is no prior state to preserve). There is no image-drift detection in v0 -- see image_name for what the instance is actually running. **Forces replacement.**
- `name` (String) 1-63 lowercase letters, digits or hyphens; must start and end alphanumeric. Unique among live instances in the same region -- a deleted instance's name is reusable. **Forces replacement.** Validation: 1-63 lowercase letters, digits or hyphens, starting and ending with a letter or digit.
- `region` (String) The region id to create the instance in. **Forces replacement.**

### Optional

- `disable_api_termination` (Boolean) Protect the instance against deletion, like AWS's DisableApiTermination: while it is true, deleting the instance is refused with 409 vm_protected (a destroy, or a replacement, then fails until you set it to false and apply). Defaults to what the API reports, false. Changing it is an in-place update; a create sets it once the instance runs. Changing it needs the compute.instance.update permission. Removing the line keeps the current value.
- `disk_gb` (Number) Boot disk size, in gigabytes: 10 to 2000, in steps of 10. Taken from instance_type when one is named. A different value replaces the instance: the API resizes vCPUs and memory only. **Forces replacement.**
- `host_id` (String) Place the instance on this compute host, like AWS's Dedicated Host host_id (for example IKVM-AMS1-01): one plain host name, letters, digits and hyphens, not case sensitive. Only for VirtScale's own organizations (kind internal): any other organization is refused with 422 host_placement_not_allowed. A host that cannot take the instance is 422 host_unavailable (one sentence for a host that does not exist, is in another region or is out of service), and a storage pool that keeps the disk on the host the instance runs on needs it (422 host_required). The instance is built on that host in one attempt. Read back from the API, null for an instance on no named host. Naming another host replaces the instance (a different case of the same host does not), and so does a different vcpus or memory_mb: an instance with a host is never resized in place. Removing the line keeps the host. **Forces replacement.** Validation: one plain host name: letters, digits and hyphens, at most 63 characters, starting with a letter or digit.
- `hostname` (String) The lower-case FQDN the instance is known by in your organization; unique among the organization's live instances (a duplicate is 409 hostname_taken). It is a handle only: no DNS record is created. Changing it updates the instance in place; it is read back from the API on every refresh, so a change made elsewhere shows as a difference. Removing the attribute from the configuration keeps the current hostname (this provider does not remove one). Import fills it. Validation: a lower-case fully qualified name such as web.example.com, at most 253 characters.
- `instance_type` (String) The catalogue instance type to create the instance from, for example g1-small (the virtscale_instance_types data source lists them). It sets vcpus, memory_mb and disk_gb: name it or give those three, not both. With a type, storage_pool and network_profile need not be set. Read back from the API, so it is null for an instance the API reports no type for; an instance that has one keeps it when the line is removed. A different type replaces the instance. The platform may require a type, in which case it refuses vcpus, memory_mb and disk_gb with 422 instance_type_required. Production has no orderable types yet: a type is refused there with 422 instance_type_unpriced, so give the sizes instead. **Forces replacement.** Validation: string length must be at least 1.
- `memory_mb` (Number) Memory, in megabytes: whole gigabytes from 1 to 128, so gigabytes times 1024 (1024 for 1 GB, 131072 for 128 GB). Taken from instance_type when one is named. Changing it resizes the instance in place: THE INSTANCE RESTARTS, and apply waits until it reports the new size. Resizing may be switched off for your organization (503 not_ready). An instance with a host_id is never resized in place: a different value replaces it.
- `network_profile` (String) The network profile id to attach the instance to, for example gro-tenant-default in gro (the API lists them at GET /v1/orgs/{org}/compute/network-profiles). Left out, the API picks the first profile your organization may use in the region on the hosts the instance is placed on. State holds the profile the instance is on. Naming a different profile replaces the instance; removing the line keeps the profile. **Forces replacement.** Validation: string length must be at least 1.
- `ssh_keys` (List of String) Names of SSH keys imported into the organization (see virtscale_ssh_key), at most 10, each listed once; no key material is stored here. cloud-init installs them for the image's default user (for example debian or ubuntu) at first boot, and the instance keeps them if a key is deleted or replaced later. The API refuses a name that was never imported with 422 ssh_key_not_found, and a 503 not_ready means SSH keys are not switched on for this API yet. Changing the list replaces the instance. Importing an instance reads its key names back from the API. **Forces replacement.** Validation: at most 10 SSH keys; no SSH key name listed more than once.
- `storage_pool` (String) The storage pool id to place the instance's disk on, for example gro-ceph in gro (the API lists the organization's pools at GET /v1/orgs/{org}/compute/storage-pools). Left out with instance_type, the API uses the type's own pool; with vcpus, memory_mb and disk_gb it is required. State holds the pool the instance is on. Naming a different pool replaces the instance; removing the line keeps the pool. **Forces replacement.** Validation: string length must be at least 1.
- `user_data` (String) Plain cloud-init text for the first boot: a #cloud-config document, a script that starts with #!, or a MIME multipart document, at most 32 KiB (checked at plan time). The provider sends it as standard base64 and the API applies it after the platform's own part (the SSH keys of ssh_keys). It is read at the first boot only, so a different value replaces the instance. The API never returns it: state keeps the configured value, an imported instance has none, and a configuration that sets it after an import plans a replacement (use ignore_changes to avoid that). Marked sensitive, because cloud-init is where bootstrap secrets go; the state holds it as text. A 503 not_ready means cloud-init is not switched on for this API yet; a payload it cannot deliver is 422 user_data_invalid. **Forces replacement.** **Sensitive.** Validation: string length must be at least 1; at most 32 KiB of cloud-init text.
- `vcpus` (Number) Number of virtual CPUs: 1, 2, 4, 8, 16 or 32. Taken from instance_type when one is named; otherwise give vcpus, memory_mb, disk_gb and storage_pool together. Changing it resizes the instance in place: THE INSTANCE RESTARTS, and apply waits until it reports the new size. Resizing may be switched off for your organization (503 not_ready). An instance with a host_id is never resized in place: a different value replaces it.
- `workload_type` (String) Which hardware the instance runs on: customer (the API's default) or infra, VirtScale's own pools. Only an organization of kind internal may ask for infra; any other gets 422 workload_not_allowed. Read back from the API. A different value replaces the instance; removing the line keeps the value. **Forces replacement.** Validation: value must be one of: ['customer' 'infra'].
- `network_interface` (Block List) The instance's network interfaces, for organizations of kind internal only (any other is refused with 422 network_interface_not_allowed): like AWS's private_ip, secondary_private_ips and source_dest_check, and a fixed MAC. Block i describes interface i of the network profile, in order, and there can be no more blocks than the profile has interfaces (422 invalid_spec, which also names a bad address or MAC, one used twice and more than 8 secondary addresses). An address or MAC already in use on the network fails the create. They are set at create and read back as asked (what a block leaves out is the platform's choice and stays null): a different block replaces the instance, and so do blocks added to an instance that has none; taking them out keeps the instance. State has the blocks only when the configuration did, so an imported instance has none.
- `root_block_device` (Block List) The root volume, like AWS's root_block_device (at most one block): another spelling of storage_pool and disk_gb, so it does not go with them, and volume_size does not go with instance_type (volume_type does, as storage_pool does). A storage pool that keeps the disk on a host (for example ams1-local) is a volume type here, and needs host_id. Any change of the block replaces the instance. It is read back (and into storage_pool and disk_gb) only when the configuration or the state has it: an imported instance has no block, so a configuration that names the same pool and size as a block adopts it with an update in place. Validation: list must contain at most 1 elements.

### Read-Only

- `created_at` (String) When the instance was created, RFC 3339.
- `fqdn` (String) The instance's fully-qualified domain name. Null for a customer account.
- `id` (String) The instance's id (a UUID), assigned by the API.
- `image_name` (String) The human-readable name of the image the instance is currently running, as reported by the API. Informational only -- it does not participate in planning and never forces a replacement.
- `private_ipv4` (String) The instance's private IPv4 address, as reported by the platform; empty until it is known. An associated public IP is not the private address.
- `state` (String) The instance's current lifecycle state, as reported by the API.
- `status` (String) The instance's status word, as the API reports it.

### Nested Schema for `network_interface`

#### Optional

- `mac_address` (String) The MAC address the interface gets: a unicast address, six pairs of hex digits split by colons (aa:bb:cc:dd:ee:ff). The API stores it in lower case and state keeps the spelling of the configuration, so a configuration in upper case plans no difference; a change of case alone is an update in place that changes nothing, never a replacement. Validation: six pairs of hex digits split by colons, aa:bb:cc:dd:ee:ff.
- `private_ip` (String) The IPv4 address the interface gets, instead of one the platform picks. It must belong to the network the interface is on. Validation: string length must be at least 1.
- `secondary_private_ips` (Set of String) Up to 8 more IPv4 addresses the interface may use besides private_ip, for example the shared address of a VRRP or keepalived pair (allowed address pairs). None twice, none equal to private_ip. Needs source_dest_check true. A set: the order means nothing. Validation: set must contain at most 8 elements.
- `source_dest_check` (Boolean) True (the default): the interface only sends and receives traffic for its own addresses, and the organization's firewall applies to it. False: it is not filtered and carries no firewall, as a router or a VRRP pair needs; it takes no secondary_private_ips.

### Nested Schema for `root_block_device`

#### Optional

- `volume_size` (Number) The size of the root volume, in gigabytes: what disk_gb is. Does not go with instance_type.
- `volume_type` (String) The storage pool id the root volume is on, for example ams1-local: what storage_pool is. With an instance_type, left out, it is the type's own pool. Validation: string length must be at least 1.

#### Read-Only

- `delete_on_termination` (Boolean) Always true: the root volume is deleted with the instance.

## Import

Import is supported using the following syntax:

```shell
tofu import virtscale_instance.web <instance id>
```

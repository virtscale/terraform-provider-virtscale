---
page_title: "virtscale_instance Resource - terraform-provider-virtscale"
subcategory: ""
description: |-
  A VirtScale compute instance (VM). An instance resized outside Terraform keeps its instance_type in the API. If the configuration names only instance_type, the next refresh shows the new vcpus and memory_mb in state but plans no change. To manage the sizes, set vcpus, memory_mb, disk_gb and storage_pool in the configuration instead of instance_type; a size that then differs from the configuration plans a replacement. Size an instance with vcpus, memory_mb, disk_gb and storage_pool. The API takes this grid: vcpus is 1, 2, 4, 8, 16 or 32; memory is whole gigabytes from 1 to 128, given in megabytes (gigabytes times 1024, so memory_mb is 1024 to 131072); disk_gb is 10 to 2000 in steps of 10. A size off the grid is refused with 422 invalid_spec, which lists every violation. Where to look the values up today: the API at GET /v1/orgs/{org}/compute/limits, /compute/storage-pools and /compute/network-profiles. The catalogue of instance types is empty in production, so there instance_type is refused with 422 instance_type_unpriced. The min_disk_gb and min_ram_mb of an image are 0 today: they are no guide to a size. The provider does not manage power state. An instance stopped through the API stays stopped and the plan stays empty; state shows stopped after the next refresh. A create takes about 30 to 60 seconds and a destroy about 15 seconds (apply waits up to 20 minutes for a create and 10 for a destroy).
---

# virtscale_instance (Resource)

A VirtScale compute instance (VM).

An instance resized outside Terraform keeps its instance_type in the API. If the configuration names only instance_type, the next refresh shows the new vcpus and memory_mb in state but plans no change. To manage the sizes, set vcpus, memory_mb, disk_gb and storage_pool in the configuration instead of instance_type; a size that then differs from the configuration plans a replacement.

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
```

## Schema

### Required

- `image` (String) The image id to boot from, as listed by the images catalogue. A create-time input only: neither Create nor Read ever overwrites it from the API, so it always reflects what was configured (import is the one exception, seeding it once from the observed value since there is no prior state to preserve). There is no image-drift detection in v0 -- see image_name for what the instance is actually running. **Forces replacement.**
- `name` (String) 1-63 lowercase letters, digits or hyphens; must start and end alphanumeric. Unique among live instances in the same region -- a deleted instance's name is reusable. **Forces replacement.** Validation: 1-63 lowercase letters, digits or hyphens, starting and ending with a letter or digit.
- `region` (String) The region id to create the instance in. **Forces replacement.**

### Optional

- `disk_gb` (Number) Boot disk size, in gigabytes: 10 to 2000, in steps of 10. Taken from instance_type when one is named. **Forces replacement.**
- `hostname` (String) The lower-case FQDN the instance is known by in your organization; unique among the organization's live instances (a duplicate is 409 hostname_taken). It is a handle only: no DNS record is created. Changing it updates the instance in place; it is read back from the API on every refresh, so a change made elsewhere shows as a difference. Removing the attribute from the configuration keeps the current hostname (this provider does not remove one). Import fills it. Validation: a lower-case fully qualified name such as web.example.com, at most 253 characters.
- `instance_type` (String) The catalogue instance type to create the instance from, for example g1-small (the virtscale_instance_types data source lists them). It sets vcpus, memory_mb and disk_gb: name it or give those three, not both. With a type, storage_pool and network_profile need not be set. Read back from the API, so it is null for an instance the API reports no type for; an instance that has one keeps it when the line is removed. A different type replaces the instance. The platform may require a type, in which case it refuses vcpus, memory_mb and disk_gb with 422 instance_type_required. Production has no orderable types yet: a type is refused there with 422 instance_type_unpriced, so give the sizes instead. **Forces replacement.** Validation: string length must be at least 1.
- `memory_mb` (Number) Memory, in megabytes: whole gigabytes from 1 to 128, so gigabytes times 1024 (1024 for 1 GB, 131072 for 128 GB). Taken from instance_type when one is named. **Forces replacement.**
- `network_profile` (String) The network profile id to attach the instance to, for example gro-tenant-default in gro (the API lists them at GET /v1/orgs/{org}/compute/network-profiles). Left out, the API picks the first profile your organization may use in the region on the hosts the instance is placed on. State holds the profile the instance is on. Naming a different profile replaces the instance; removing the line keeps the profile. **Forces replacement.** Validation: string length must be at least 1.
- `ssh_keys` (List of String) Names of SSH keys imported into the organization (see virtscale_ssh_key), at most 10, each listed once; no key material is stored here. cloud-init installs them for the image's default user (for example debian or ubuntu) at first boot, and the instance keeps them if a key is deleted or replaced later. The API refuses a name that was never imported with 422 ssh_key_not_found, and a 503 not_ready means SSH keys are not switched on for this API yet. Changing the list replaces the instance. Importing an instance reads its key names back from the API. **Forces replacement.** Validation: at most 10 SSH keys; no SSH key name listed more than once.
- `storage_pool` (String) The storage pool id to place the instance's disk on, for example gro-ceph in gro (the API lists the organization's pools at GET /v1/orgs/{org}/compute/storage-pools). Left out with instance_type, the API uses the type's own pool; with vcpus, memory_mb and disk_gb it is required. State holds the pool the instance is on. Naming a different pool replaces the instance; removing the line keeps the pool. **Forces replacement.** Validation: string length must be at least 1.
- `vcpus` (Number) Number of virtual CPUs: 1, 2, 4, 8, 16 or 32. Taken from instance_type when one is named; otherwise give vcpus, memory_mb, disk_gb and storage_pool together. **Forces replacement.**

### Read-Only

- `created_at` (String) When the instance was created, RFC 3339.
- `fqdn` (String) The instance's fully-qualified domain name. Null for a customer account.
- `id` (String) The instance's id (a UUID), assigned by the API.
- `image_name` (String) The human-readable name of the image the instance is currently running, as reported by the API. Informational only -- it does not participate in planning and never forces a replacement.
- `private_ipv4` (String) The instance's private IPv4 address, as reported by the platform; empty until it is known. An associated public IP is not the private address.
- `state` (String) The instance's current lifecycle state, as reported by the API.
- `status` (String) The instance's status word, as the API reports it.

## Import

Import is supported using the following syntax:

```shell
tofu import virtscale_instance.web <instance id>
```

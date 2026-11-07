---
page_title: "virtscale_instance Resource - terraform-provider-virtscale"
subcategory: ""
description: |-
  A VirtScale compute instance (VM).
---

# virtscale_instance (Resource)

A VirtScale compute instance (VM).

## Example Usage

```terraform
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
```

## Schema

### Required

- `disk_gb` (Number) Boot disk size, in gigabytes. **Forces replacement.**
- `image` (String) The image id to boot from, as listed by the images catalogue. A create-time input only: neither Create nor Read ever overwrites it from the API, so it always reflects what was configured (import is the one exception, seeding it once from the observed value since there is no prior state to preserve). There is no image-drift detection in v0 -- see image_name for what the instance is actually running. **Forces replacement.**
- `memory_mb` (Number) Memory, in megabytes. **Forces replacement.**
- `name` (String) 1-63 lowercase letters, digits or hyphens; must start and end alphanumeric. Unique among live instances in the same region -- a deleted instance's name is reusable. **Forces replacement.** Validation: 1-63 lowercase letters, digits or hyphens, starting and ending with a letter or digit.
- `network_profile` (String) The network profile id to attach the instance to. **Forces replacement.**
- `region` (String) The region id to create the instance in. **Forces replacement.**
- `storage_pool` (String) The storage pool id to place the instance's disk on. **Forces replacement.**
- `vcpus` (Number) Number of virtual CPUs. **Forces replacement.**

### Optional

- `hostname` (String) The lower-case FQDN the instance is known by in your organization; unique among the organization's live instances (a duplicate is 409 hostname_taken). It is a handle only: no DNS record is created. Changing it updates the instance in place; it is read back from the API on every refresh, so a change made elsewhere shows as a difference. Removing the attribute from the configuration keeps the current hostname (this provider does not remove one). Import fills it. Validation: a lower-case fully qualified name such as web.example.com, at most 253 characters.
- `ssh_keys` (List of String) Names of SSH keys imported into the organization (see virtscale_ssh_key), at most 10, each listed once; no key material is stored here. cloud-init installs them for the image's default user (for example debian or ubuntu) at first boot, and the instance keeps them if a key is deleted or replaced later. The API refuses a name that was never imported with 422 ssh_key_not_found, and a 503 not_ready means SSH keys are not switched on for this API yet. Changing the list replaces the instance. Importing an instance reads its key names back from the API. **Forces replacement.** Validation: at most 10 SSH keys; no SSH key name listed more than once.

### Read-Only

- `created_at` (String) When the instance was created, RFC 3339.
- `fqdn` (String) The instance's fully-qualified domain name. Null for a customer account.
- `id` (String) The instance's id (a UUID), assigned by the API.
- `image_name` (String) The human-readable name of the image the instance is currently running, as reported by the API. Informational only -- it does not participate in planning and never forces a replacement.
- `instance_type` (String) The catalogue instance type the instance was created from; null when the API does not report one.
- `private_ipv4` (String) The instance's private IPv4 address, as reported by the platform; empty until it is known. An associated public IP is not the private address.
- `state` (String) The instance's current lifecycle state, as reported by the API.
- `status` (String) The instance's status word, as the API reports it.

## Import

Import is supported using the following syntax:

```shell
tofu import virtscale_instance.web <instance id>
```

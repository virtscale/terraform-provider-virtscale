---
page_title: "Getting started - terraform-provider-virtscale"
subcategory: ""
description: |-
  Build a first VM end to end with the virtscale provider: an SSH key, an instance, a firewall and a public IP, then plan, apply, connect and destroy.
---

# Getting started

This guide builds one VM you can log in to: an SSH key, an instance, a firewall that lets in SSH from your own address, and a public IP. It ends with `destroy`, which removes everything again.

## Before you start

- The provider is installed (the Installation guide) and `VIRTSCALE_API_KEY` and `VIRTSCALE_ORGANIZATION` are set (the Authentication guide). The key needs **Manage compute** and **Manage network** (`compute:write` and `network:write`), or **Full access**.
- You have an OpenSSH key pair. The public key, `~/.ssh/id_ed25519.pub` in this guide, is imported into your organization; the private key never leaves your machine. Supported types: `ssh-ed25519`, `ecdsa-sha2-nistp256`, `ecdsa-sha2-nistp384`, `ecdsa-sha2-nistp521` and `ssh-rsa` of 2048 to 16384 bits.
- You know the public address you will log in from. The firewall below lets in SSH from that address only.

## Find the region, image and instance type

An instance is built from three choices: a region, an image and an instance type. The type sets the vCPUs, the memory and the boot disk; the platform picks the storage pool and the network profile itself. All three have data sources. Put this in an empty directory as `main.tf` and run `tofu init` and `tofu plan` (or `terraform init` and `terraform plan`); the outputs list what your organization may use.

```terraform
terraform {
  required_providers {
    virtscale = {
      source  = "virtscale/virtscale"
      version = "~> 0.1"
    }
  }
}

provider "virtscale" {}

data "virtscale_regions" "all" {}

data "virtscale_images" "gro" {
  region = "gro"
}

output "regions" {
  value = [for r in data.virtscale_regions.all.regions : "${r.id}: ${r.status}, entitled ${r.entitled}"]
}

data "virtscale_instance_types" "gro" {
  region = "gro"
}

output "images" {
  value = [for i in data.virtscale_images.gro.images : "${i.name} (${i.id}), at least ${i.min_ram_mb} MB and ${i.min_disk_gb} GB"]
}

output "instance_types" {
  value = [for t in data.virtscale_instance_types.gro.instance_types : "${t.id}: ${t.vcpus} vCPU, ${t.memory_mb} MB, ${t.disk_gb} GB disk"]
}
```

Take a region with status `active` and `entitled` true. The examples in this guide use `gro`; use a region from your own list. Take an instance type that is listed for that region, and one whose memory and disk are not below the image's minimum. The guide uses `g1-small`; use a type from your own list.

You never need a storage pool or a network profile id. Left out, the platform uses the type's own storage pool and the first network profile your organization may use in the region. `virtscale_instance` has the optional `storage_pool` and `network_profile` attributes for the rare case where you want to name one; the state holds the pool and the profile the instance is on either way.

## The configuration

Replace `main.tf` with the whole configuration. The variables for the region and the instance type have to match what you found above; `ssh_cidr` has no default, so that a run never opens SSH to an address you did not choose.

```terraform
terraform {
  required_providers {
    virtscale = {
      source  = "virtscale/virtscale"
      version = "~> 0.1"
    }
  }
}

provider "virtscale" {}

variable "region" {
  type        = string
  description = "A region from the virtscale_regions data source."
  default     = "gro"
}

variable "instance_type" {
  type        = string
  description = "An instance type listed for that region by the virtscale_instance_types data source."
  default     = "g1-small"
}

variable "ssh_cidr" {
  type        = string
  description = "The network you log in from, as an IPv4 CIDR with no host bits set: your address followed by /32 is one address."
}

variable "public_key_path" {
  type        = string
  description = "The OpenSSH public key you log in with."
  default     = "~/.ssh/id_ed25519.pub"
}

# The project a server lands in when it names none: the organization's default one.
data "virtscale_project" "default" {}

data "virtscale_images" "region" {
  region = var.region
}

locals {
  # The Debian 12 image of the region, picked by name prefix. If your region has none, the plan stops with
  # "Invalid index": choose another image from the list above.
  image_id = [for i in data.virtscale_images.region.images : i.id if startswith(i.name, "Debian 12")][0]
}

# Your public key, imported once and taken by name by the instance.
resource "virtscale_ssh_key" "me" {
  name       = "getting-started"
  public_key = file(var.public_key_path)
}

resource "virtscale_instance" "first" {
  name          = "first-vm"
  region        = var.region
  image         = local.image_id
  instance_type = var.instance_type
  ssh_keys      = [virtscale_ssh_key.me.name]
}

# The whole rule set of the instance: SSH in from your own network.
resource "virtscale_instance_firewall" "first" {
  instance_id = virtscale_instance.first.id

  rule {
    direction   = "ingress"
    protocol    = "tcp"
    port_min    = 22
    port_max    = 22
    remote_cidr = var.ssh_cidr
    description = "ssh from my network"
  }
}

# An address from the internet, tied to the instance. By default an organization may hold one per region.
resource "virtscale_public_ip" "first" {
  region      = var.region
  instance_id = virtscale_instance.first.id
}

output "default_project_id" {
  value = data.virtscale_project.default.id
}

output "instance_state" {
  value = virtscale_instance.first.state
}

output "private_ipv4" {
  value = virtscale_instance.first.private_ipv4
}

output "public_ipv4" {
  value = virtscale_public_ip.first.address
}

output "ssh" {
  value = "ssh debian@${virtscale_public_ip.first.address}"
}
```

What each part does:

- `virtscale_ssh_key` imports the public key under a name. The instance takes the key by that name, and cloud-init installs it for the image's default user (`debian` on the Debian images) the first time the instance boots.
- `virtscale_instance` is the VM. `instance_type` sets its vCPUs, memory and disk; the type's memory and disk must not be below the image's `min_ram_mb` and `min_disk_gb`, and your organization's quota must have room for them. The storage pool and the network profile are the platform's choice and are read back into the state. Its project is the default project, which `data.virtscale_project.default` reads.
- `virtscale_instance_firewall` owns the whole rule set of the instance: a rule you leave out of it is removed, and destroying the resource clears the rules.
- `virtscale_public_ip` allocates an address in the region and associates it with the instance. Take `instance_id` out and the address stays allocated without an instance.

## Plan and apply

Give your address as `ssh_cidr`. A `/32` is one address, so `203.0.113.7/32` below stands for yours:

```shell
export TF_VAR_ssh_cidr="203.0.113.7/32"
tofu plan
```

The plan ends with `Plan: 4 to add, 0 to change, 0 to destroy.`: the key, the instance, the firewall and the public IP. A plan only checks the shape of what you wrote (names, ports, CIDRs) and reads the data sources. Whether the region is open to you and whether your quota has room is decided by the API when you apply.

```shell
tofu apply
```

Answer `yes`. What happens, in order:

1. The key is imported.
2. The instance is created. The API answers at once with an operation, and the platform builds the VM in the background. The provider asks for the instance every 5 seconds and waits, for up to 20 minutes, until its state is `running`. If the build ends in state `error`, apply stops with `Instance Failed to Build` and the platform's `error_class` and `error_detail`; the instance stays in the state as failed, and the next `apply` replaces it.
3. The firewall and the public IP are created at the same time. The firewall is applied asynchronously, so apply waits until its state is `applied`; the public IP waits up to 10 minutes for each step.
4. The outputs are printed.

A throttled request (`429`) is retried automatically with a growing pause, and so is a request that failed in a way that is safe to send again; the rest of the errors stop the run (see below).

## Connect

```shell
ssh debian@203.0.113.50      # the address of the output "public_ipv4"; or run the output "ssh"
```

If the connection times out, check that `ssh_cidr` is the address you connect from now: a laptop behind a changing address needs the firewall rule changed. `tofu apply -var ssh_cidr=...` updates the rule in place.

## Change and destroy

Most attributes of an instance cannot be changed in place: a different `name`, `region`, `image`, `instance_type` or `ssh_keys` replaces the instance, which is destroyed and built again. So does a different `storage_pool`, `network_profile` or size, if you set one in the configuration; a value you leave out is the platform's choice and is never a change. The plan shows that as `must be replaced`: read it before you apply. The `hostname`, the firewall rules and the public IP's `instance_id` change in place.

```shell
tofu destroy
```

Destroy removes everything in reverse order. Deleting the instance waits up to 10 minutes. The firewall rules are cleared, the public IP is released and the key is removed.

## Common errors

Every error shows the API's status, its code and its message, then your request id: `virtscale API 422 quota_exceeded: ... (request ...)`. Quote the request id when you ask VirtScale for help.

| What you see | What it means | What to do |
| --- | --- | --- |
| `401 unauthorized` | The key is wrong, revoked or expired. | Check `VIRTSCALE_API_KEY`, or create a new key. |
| `403 permission_denied` | The key's scopes do not cover the call. The message names the permission. | Create a key with the scope the provider names in its hint (`compute:write` or `network:write`, or Full access). |
| `403 source_address_not_allowed` | The key is limited to networks and this is not one. | Run from an allowed address. |
| `404 not_found`, `no organization matching that identifier` | `VIRTSCALE_ORGANIZATION` is wrong, or the key belongs to another organization. | Check both. |
| `409 compute_not_enabled`, `the organization is not enrolled in the compute service` | Compute is not switched on for your organization. | Ask VirtScale to enable it, then apply again. |
| `409 region_not_ready` | Your project in this region is still being prepared. | Wait a while and apply again. |
| `422 region_unavailable`, `region '...' is not accepting instances` | The region does not exist, is not active, or is restricted and your organization has no access. | Use a region that `virtscale_regions` shows as `active` and `entitled`. Access to a restricted region is granted by VirtScale. |
| `422 quota_exceeded`, for example `instances: 1 in use plus 1 requested exceeds the limit of 1 for this account` | Your organization's limit is reached. The text names what: `instances`, `vcpus`, `ram_mb`, `disk_gb`, or `public IPs` for an address (the limit is one per region by default). | Delete what you no longer use, ask VirtScale for a higher limit, or ask for less. The same request is refused again until then. |
| `422 instance_type_unavailable`, `instance type '...' is not available in gro` | The type does not exist, is not offered to your organization, or cannot be ordered in that region. The API gives the same answer for all three. | Take a type from `virtscale_instance_types` with `region` set: its `regions` list shows where it can be ordered. |
| `422 instance_type_unpriced` | The type is sold in the region, but its price is not published yet. | Take another type, or order this one once VirtScale has published its price. |
| `422 instance_type_required` | This deployment of the platform only creates instances from an instance type, and the configuration gave `vcpus`, `memory_mb` and `disk_gb`. | Set `instance_type` and remove the three sizes. |
| `422 public_ips_not_offered`, `region '...' attaches instances straight to a public network, so it has no public IPs to allocate` | A `virtscale_public_ip` in a region that attaches its instances straight to a public network, for example `fsn`: it has no public IPs to allocate. | Remove the `virtscale_public_ip`, or build in a region that offers public IPs (`gro` in this guide). |
| `422 image_not_usable` | The image is not offered in that region. | Take an image from `virtscale_images` for the same region. |
| `422 invalid_spec` | The instance is smaller than the image allows (`<image> needs at least N GB of disk and M MB of memory`), or the request mixes things the API does not take together: an instance type with sizes, or sizes without a storage pool. The message says which. | Take an instance type whose memory and disk reach `min_ram_mb` and `min_disk_gb` of the image, or another image. The provider already refuses the mixed requests at plan time. |
| `409 name_taken`, `409 hostname_taken` | A live instance already has that name (names are unique per region) or hostname (unique per organization). | Choose another. The name of a deleted instance can be reused. |
| `422 ssh_key_not_found`, `422 invalid_public_key` | The instance names a key that was never imported, or the key is not one the platform accepts. | Import a supported key (see above) and reference the `virtscale_ssh_key` by its name. |
| `422 billing_state_blocks_provisioning` | The billing account is not in a state that allows new resources. Existing ones stay readable and deletable. | Settle the billing account, then apply again. |
| `503 not_ready` | The feature behind the call, for example SSH keys, is not switched on in this deployment yet. | Try again later: the provider does not retry it. |
| `Instance Failed to Build`, `error_class=no_valid_host` | No host in the pool could take that size right now. | Ask for less, or build in another region, then apply again. |
| `Instance Failed to Build`, `error_class=capacity`, `internal` or `openstack_unreachable` | The platform could not finish the build: no capacity left, or an unclassified or temporary fault. | Apply again, which replaces the failed instance. If it fails the same way, report the request id and the `error_detail`. |

## Next

The State, secrets and import guide says what the state holds and how to bring an existing resource under management. Each resource and data source has its own page, with every attribute.

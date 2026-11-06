---
page_title: "Provider: virtscale"
subcategory: ""
description: |-
  Interact with the VirtScale customer API (compute instances, and more) as Terraform resources.
---

# virtscale Provider

Interact with the VirtScale customer API (compute instances, and more) as Terraform resources.

## Example Usage

```terraform
terraform {
  required_providers {
    virtscale = {
      source = "virtscale/virtscale"
    }
  }
}

# No credentials here: set VIRTSCALE_API_KEY and VIRTSCALE_ORGANIZATION (the organization's UUID) in the
# environment. VIRTSCALE_ENDPOINT overrides the default https://api.virtscale.nl.
provider "virtscale" {}
```

## Schema

### Optional

- `api_key` (String) The VirtScale API key used to authenticate. Defaults to the VIRTSCALE_API_KEY environment variable. A value set here takes precedence over the environment variable. **Sensitive.**
- `endpoint` (String) The base URL of the VirtScale API. Defaults to the VIRTSCALE_ENDPOINT environment variable, or https://api.virtscale.nl when neither is set. A value set here takes precedence over the environment variable.
- `organization` (String) The id (a UUID) of the organization to operate in. Defaults to the VIRTSCALE_ORGANIZATION environment variable. A value set here takes precedence over the environment variable.

## Resources

- [`virtscale_instance`](resources/instance.md): A VirtScale compute instance (VM).
- [`virtscale_instance_firewall`](resources/instance_firewall.md): The firewall rules of one instance.
- [`virtscale_project`](resources/project.md): A project: it groups an organization's servers, public IPs and networks.
- [`virtscale_public_ip`](resources/public_ip.md): A VirtScale public IP address.
- [`virtscale_s3_access_key`](resources/s3_access_key.md): An S3 access key for the organization's storage.
- [`virtscale_s3_bucket`](resources/s3_bucket.md): An object-storage bucket.
- [`virtscale_ssh_key`](resources/ssh_key.md): An SSH public key imported into the organization, which instances take by name (virtscale_instance.ssh_keys).
- [`virtscale_vpc`](resources/vpc.md): A VPC: a private address space in one project and one region.

## Data Sources

- [`virtscale_caller`](data-sources/caller.md): Who the API key (or other credential) is, from GET /v1/me: its organization, scopes and permissions.
- [`virtscale_images`](data-sources/images.md): The catalogue of images available to boot a virtscale_instance from, in a given region.
- [`virtscale_instance_types`](data-sources/instance_types.md): The public instance types, in the order the API lists them (by sort order, then id).
- [`virtscale_plans`](data-sources/plans.md): The Managed VPS plans of the price book, cheapest first.
- [`virtscale_project`](data-sources/project.md): A project of the organization, by name; without a name, the organization's default project.
- [`virtscale_regions`](data-sources/regions.md): The regions this organization may see, in the order the API lists them.

See the [guide on state, secrets and import](guides/state-secrets-and-import.md).

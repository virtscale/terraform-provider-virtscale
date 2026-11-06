---
page_title: "State, secrets and import - terraform-provider-virtscale"
subcategory: ""
description: |-
  What the state of the virtscale provider holds, which attributes are sensitive, and how to import each resource.
---

# State, secrets and import

## Versions

The provider speaks protocol 6 and runs with OpenTofu 1.x and Terraform 1.x; it is tested with OpenTofu 1.12.5. No attribute is write-only, so no feature of a recent minimum version is needed.

## The state holds credentials

A sensitive attribute is hidden in plan and apply output, but it is stored in the state in plain text, as every provider does. Keep the state in an encrypted backend (or use OpenTofu's state encryption) and do not print a sensitive value in an output.

The sensitive attributes of this provider are:

- `provider.api_key`
- `virtscale_s3_access_key.secret_access_key`

The provider's `api_key` is better kept out of configuration files: set `VIRTSCALE_API_KEY` in the environment. `virtscale_s3_access_key.secret_access_key` is returned by the API exactly once, by the request that creates the key, so it is in the state and nowhere else; an imported key has none. `user_data` is not supported by the API, so no instance data is stored. The provider has no resource for IAM: members, invitations and API keys are managed by people, because no API key scope holds `iam.*` (a key cannot mint keys, so a leaked key cannot widen its reach).

## Import

Every resource is imported by an id. Attributes the API cannot return are left empty; the resource page says which.

| Resource | Import id |
| --- | --- |
| `virtscale_instance` | `id>` |
| `virtscale_instance_firewall` | `id>` |
| `virtscale_project` | `id>` |
| `virtscale_public_ip` | `id>` |
| `virtscale_s3_access_key` | `id>` |
| `virtscale_s3_bucket` | `id>` |
| `virtscale_ssh_key` | `id>` |
| `virtscale_vpc` | `id>` |

---
page_title: "State, secrets and import - terraform-provider-virtscale"
subcategory: ""
description: |-
  What the state of the virtscale provider holds, which attributes are sensitive, and how to import each resource.
---

# State, secrets and import

## Versions

The provider speaks protocol 6 and is built and tested with OpenTofu 1.12.5. Terraform 1.x speaks the same protocol but is not tested. No attribute is write-only, so no feature of a recent minimum version is needed.

## Sensitive attributes

A sensitive attribute is hidden in plan and apply output. The sensitive attributes of this provider are:

- `provider.api_key`
- `virtscale_s3_access_key.secret_access_key`

## Where a secret ends up

- **The state.** The sensitive attributes of a resource are stored in the state in plain text, as every provider does. Keep the state in an encrypted backend (or use OpenTofu's state encryption, OpenTofu 1.7 or newer) and do not print a sensitive value in an output without `sensitive = true`. `virtscale_s3_access_key.secret_access_key` is returned by the API exactly once, by the request that creates the key, so it is in the state and nowhere else; an imported key has none.
- **Plans and JSON output.** A saved plan (`tofu plan -out`) holds every planned value and the values of the input variables in plain text, and `tofu output -json` and `tofu show -json` print sensitive values in clear text: keep them as safe as the state.
- **The provider configuration.** `api_key` is not stored in the state, but it is better kept out of configuration files and variables: set `VIRTSCALE_API_KEY` in the environment.
- **Instance data.** The provider has no `user_data` attribute, so no instance data is stored.
- **IAM.** The provider has no resource for IAM: members, invitations and API keys are managed by people, because no API key scope holds `iam.*` (a key cannot mint keys, so a leaked key cannot widen its reach).

## Import

Every resource is imported by the id shown below; a VPC is addressed through its project, so its id has two parts. Attributes the API cannot return are left empty; the resource page says which.

| Resource | Import id |
| --- | --- |
| `virtscale_instance` | `<instance id>` |
| `virtscale_instance_firewall` | `<instance id>` |
| `virtscale_project` | `<project id>` |
| `virtscale_public_ip` | `<public ip id>` |
| `virtscale_s3_access_key` | `<access key uuid>` |
| `virtscale_s3_bucket` | `<bucket id>` |
| `virtscale_ssh_key` | `<ssh key id>` |
| `virtscale_vpc` | `<project id>/<vpc id>` |

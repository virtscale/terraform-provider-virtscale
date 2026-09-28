# terraform-provider-virtscale

Terraform/OpenTofu provider for the [VirtScale customer API](https://learn.virtscale.nl) (`virtscale/virtscale`).

## Status

Pre-release. This repository lives on GitLab (`virtscale/virtscale-platform/terraform-provider-virtscale`)
and is used via `dev_overrides` only -- it is not yet published to a Terraform registry. It will mirror to
`github.com/virtscale` once that organization exists; registry publication (goreleaser, GPG signing) follows
from there.

## What it provides

- Provider `virtscale`: configured with an API key and an organization id.
- Resource `virtscale_instance`: a VirtScale compute instance (VM).
- Resource `virtscale_public_ip`: a VirtScale public IP address, optionally associated with an instance.
  A customer organization's quota is one public IP.
- Data source `virtscale_images`: the image catalogue for a region.

See [examples/main.tf](examples/main.tf) for a complete, runnable configuration.

## Build

```bash
go build -o ~/.terraform.d/plugins/terraform-provider-virtscale .
```

or, to install into `$(go env GOBIN)` (or `$(go env GOPATH)/bin` when `GOBIN` is unset):

```bash
go install .
```

## Local use (dev_overrides)

OpenTofu resolves `source = "virtscale/virtscale"` against `registry.opentofu.org`; a plain Terraform CLI
resolves the same source string against `registry.terraform.io`. A `dev_overrides` block must list **both**
hostnames, pointing at the same local build directory, or you'll see `Inconsistent dependency lock file`.
With `dev_overrides` in effect, `tofu init` / `terraform init` is skipped entirely for that provider.

Create a CLI config file, e.g. `~/.tfrc.d/dev.tfrc`:

```hcl
provider_installation {
  dev_overrides {
    "registry.opentofu.org/virtscale/virtscale"  = "/path/to/GOBIN"
    "registry.terraform.io/virtscale/virtscale"  = "/path/to/GOBIN"
  }
  direct {}
}
```

Point at it and run from `examples/`:

```bash
export TF_CLI_CONFIG_FILE=~/.tfrc.d/dev.tfrc
export VIRTSCALE_API_KEY=...
export VIRTSCALE_ORGANIZATION=...
tofu plan
```

## Authentication

The provider is configured via the `api_key` / `organization` attributes, or -- preferred, so a key never
lands in a `.tf` file -- the environment:

| Attribute      | Environment variable       |
| -------------- | --------------------------- |
| `api_key`      | `VIRTSCALE_API_KEY`         |
| `organization` | `VIRTSCALE_ORGANIZATION`    |
| `endpoint`     | `VIRTSCALE_ENDPOINT` (defaults to `https://api.virtscale.nl`) |

An attribute set in configuration takes precedence over the matching environment variable. A missing
`api_key` or `organization` (after both are checked) fails `Configure` with a diagnostic naming the missing
attribute and its environment variable -- never a panic.

## Resources and data sources

### `virtscale_instance`

| Attribute         | Type   | Required/Computed | Notes |
| ----------------- | ------ | ------------------ | ----- |
| `name`            | string | Required, forces replacement | |
| `region`          | string | Required, forces replacement | |
| `image`           | string | Required, forces replacement | Image id from `virtscale_images`. |
| `vcpus`           | number | Required, forces replacement | |
| `memory_mb`       | number | Required, forces replacement | |
| `disk_gb`         | number | Required, forces replacement | |
| `storage_pool`    | string | Required, forces replacement | |
| `network_profile` | string | Required, forces replacement | |
| `ssh_keys`        | list(string) | Optional, forces replacement | |
| `id`              | string | Computed | |
| `state`           | string | Computed | |
| `fqdn`            | string | Computed | Null for a customer account. |
| `private_ipv4`    | string | Computed | Empty until observed. |
| `created_at`      | string | Computed | RFC 3339. |

### `virtscale_public_ip`

| Attribute     | Type   | Required/Computed | Notes |
| ------------- | ------ | ------------------ | ----- |
| `region`      | string | Required, forces replacement | |
| `instance_id` | string | Optional | Associates/disassociates in place; never forces replacement. |
| `id`          | string | Computed | |
| `address`     | string | Computed | |
| `state`       | string | Computed | |

### `virtscale_images` (data source)

| Attribute | Type   | Required/Computed | Notes |
| --------- | ------ | ------------------ | ----- |
| `region`  | string | Required | |
| `id`      | string | Computed | Set to `region`. |
| `images`  | list(object) | Computed | Sorted by `name`. Each object: `id`, `name`, `os_distro` (nullable), `os_version` (nullable), `min_disk_gb`, `min_ram_mb`. |

## Testing

Unit tests (client package) run with plain `go test`. Provider and resource tests are Terraform acceptance
tests (`terraform-plugin-testing`) against an in-process fake of the customer API; they only run with
`TF_ACC=1` and `tofu` on `PATH`:

```bash
TF_ACC=1 \
TF_ACC_TERRAFORM_PATH=$(command -v tofu) \
TF_ACC_PROVIDER_NAMESPACE=virtscale \
TF_ACC_PROVIDER_HOST=registry.terraform.io \
go test ./... -count=1 -v
```

## Links

- API reference: <https://learn.virtscale.nl>

# terraform-provider-virtscale

Terraform/OpenTofu provider for the [VirtScale customer API](https://learn.virtscale.nl) (`virtscale/virtscale`).

## Status

Version 0.2.0 is released and published on registry.terraform.io, and it installs with Terraform and with OpenTofu when
the source carries the registry host: `registry.terraform.io/virtscale/virtscale`. registry.opentofu.org does not list
it yet, so the short source `virtscale/virtscale` fails in OpenTofu until the requests `opentofu/registry` #6388 and
#6396 are merged. This repository lives on GitLab (`virtscale/virtscale-platform/terraform-provider-virtscale`) and is the
source of truth. GitLab push-mirrors to `github.com/virtscale/terraform-provider-virtscale`, where GitHub Actions builds,
signs and publishes a release when a `v*` tag arrives; see [RELEASING.md](RELEASING.md).

## What it provides

- Provider `virtscale`: configured with an API key and an organization id.
- Resource `virtscale_instance`: a VirtScale compute instance (VM). VirtScale's own (internal) organizations can also place it on a named host (`host_id`), give it a host-local root volume (`root_block_device`) and fix its network interfaces (`network_interface`).
- Resource `virtscale_ssh_key`: an SSH public key imported into the organization, which instances take by name.
- Resource `virtscale_project`: a project, which groups servers, public IPs and networks. Data source
  `virtscale_project` finds one by name, or the organization's default project.
- Resource `virtscale_instance_firewall`: the firewall rules of one instance.
- Resources `virtscale_s3_bucket` (with its own size and object ceilings) and `virtscale_s3_access_key`: object
  storage.
- Resource `virtscale_vpc`: a VPC, a private address space in one project and one region.
- Resource `virtscale_public_ip`: a VirtScale public IP address, optionally associated with an instance.
  The default customer quota is one public IP per region; addresses in state `error` do not count against it.
- Data source `virtscale_images`: the image catalogue for a region.
- Data sources `virtscale_regions`, `virtscale_instance_types` and `virtscale_plans`: the catalogues a configuration
  chooses from.
- Data source `virtscale_compute_limits`: the size grid of an instance (vCPU counts, memory and disk ranges with their
  steps), which is where to look up what `vcpus`, `memory_mb` and `disk_gb` may be.
- Data source `virtscale_caller`: who the API key is (`GET /v1/me`): its organization, scopes and permissions.

See [examples/complete/main.tf](examples/complete/main.tf) for a complete, runnable configuration, and [docs/](docs/) for one page per resource and data source.

## Documentation

`docs/` has a page per resource and data source, a provider page and four guides: [installation](docs/guides/installation.md),
[authentication](docs/guides/authentication.md) (create an API key, scopes, where a key works), [getting
started](docs/guides/getting-started.md) (a first VM end to end) and [state, secrets and
import](docs/guides/state-secrets-and-import.md). The pages are **generated** from the schemas and from
`examples/` (`go generate ./...` rewrites them), so they cannot drift: a test regenerates them in memory and fails on a
difference. `examples/` holds one example per resource and data source (`examples/resources/<name>/resource.tf` with
its `import.sh`, `examples/data-sources/<name>/data-source.tf`), the provider block and a complete configuration. The
tests `tofu validate` every one of them against the provider built from the tree (so an example that does not match a
schema fails CI), check that a broken example is refused, and check that no example sets `api_key`, `secret`, `token` or
`password` to a literal or holds a `vsk_` key or a private key (a heuristic, not a scanner: gitleaks runs in CI as well).
The import ids on the pages are checked against what each resource's `ImportState` takes. The pages also say what the
validators of an attribute accept.

## Install from the registry

Write the full source address, with the registry host. It works in Terraform and in OpenTofu, because both fetch the
provider from registry.terraform.io when the source names that host:

```hcl
terraform {
  required_providers {
    virtscale = {
      source  = "registry.terraform.io/virtscale/virtscale"
      version = "~> 0.1"
    }
  }
}
```

The short source `virtscale/virtscale` means the CLI's own default registry. For OpenTofu that is registry.opentofu.org,
which answers 404 for this provider until it is added there, so `tofu init` fails with `provider registry
registry.opentofu.org does not have a provider named registry.opentofu.org/virtscale/virtscale`. Terraform CLI users
may shorten the source to `virtscale/virtscale`; every example and guide here keeps the long form so that one
configuration serves both CLIs.

Pin a release with `version = "..."` (the registry page lists them). Both CLIs verify the release's checksums and their
GPG signature against the key the registry holds. `dev_overrides` stays the way to run a local build while developing.

## Build

```bash
go build -o ~/.terraform.d/plugins/terraform-provider-virtscale .
```

or, to install into `$(go env GOBIN)` (or `$(go env GOPATH)/bin` when `GOBIN` is unset):

```bash
go install .
```

## Local use (dev_overrides)

A configuration with the full source `registry.terraform.io/virtscale/virtscale` (as in every example here) needs only
the `registry.terraform.io` line below. OpenTofu resolves the short source `virtscale/virtscale` against
`registry.opentofu.org` and a plain Terraform CLI against `registry.terraform.io`, so list **both** hostnames, pointing
at the same local build directory, when a configuration may use the short source or both CLIs: a block that names only
one fails with `Inconsistent dependency lock file` under the other. With `dev_overrides` in effect, `tofu init` /
`terraform init` is skipped entirely for that provider.

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

Point at it and run from `examples/complete/`:

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

`organization` is the organization's id, a UUID. Anything else is refused by the API with
`400 invalid_request`, and the error names the field (`org: Input should be a valid UUID`).

## API key scopes

An API key carries scopes, and the API refuses a call the key's scopes do not cover with
`403 permission_denied`. What the provider needs:

| You use | The key needs |
| ------- | ------------- |
| `virtscale_instance` (create, resize, protect, destroy, refresh) | `compute:write` |
| `virtscale_ssh_key` (import, destroy, refresh) | `compute:write` |
| `virtscale_public_ip` (allocate, associate, disassociate, release, refresh) | `network:write` |
| `virtscale_vpc` (create, destroy, refresh) | `network:write`, and `compute:read` to find its project with the `virtscale_project` data source |
| `virtscale_instance_firewall` (set, clear, refresh) | `network:write` |
| `virtscale_s3_bucket`, `virtscale_s3_access_key` (create, change, destroy, refresh) | `storage:write` (or the older `write`) |
| `virtscale_project` (create, rename, destroy) | `write`: `compute:write` only reads projects |
| `virtscale_project` (data source, refresh) | `compute:read` (`compute:write` covers it too) |
| `virtscale_images`, `virtscale_regions`, `virtscale_instance_types`, `virtscale_plans` (data sources) | `compute:read` (`compute:write` covers it too) |
| `virtscale_caller` (data source) | any key: `GET /v1/me` asks for no permission |
| `plan` and refresh only, no `apply` | the read halves: `compute:read` for instances, SSH keys, images and projects, `network:read` for public IPs and VPCs |

A configuration with an instance and a public IP associated to it needs both `compute:write` and
`network:write`.

`compute:write` covers every `compute.*` and `operation.*` permission, reads included (SSH keys are
`compute.ssh_key.*`), and reading projects (`project.list`, `project.read`); `network:write` covers every
`network.*` and `edge.*` permission (public IPs are `edge.public_ip.*`, VPCs `network.vpc.*`); `storage:write` covers
every `storage.*` permission (buckets, their ceilings and access keys). Creating, renaming and
deleting projects (`project.create`, `project.update`, `project.delete`) is covered by the older `write` scope only. The `:read` scopes cover the `.read` and `.list` permissions of their family. The older
all-family scopes also work: `write` for everything above, `read` for plan-only use.

A key can also carry network limits (allowed CIDRs). A request from an address outside them is refused with
`403 source_address_not_allowed`.

## Retries

The client retries like the AWS SDK's standard mode, within what this API makes safe:

- `429` (`rate_limited`) is retried for every request: the rate limiter refuses a request before it runs.
- `502`, `503`, `504` and transport errors (a dropped connection, a timeout) may have been applied, so they
  are retried only for requests that are safe to send twice: reads, deletes, and creating an instance or
  importing an SSH key, which carry an `Idempotency-Key` (the API answers a repeat of the same request with
  the first answer, so one create never builds two instances and one import never stores a key twice).
- A certificate that does not verify, and an endpoint whose scheme is not `http` or `https`, fail at once and
  are not retried: trying again cannot fix them.
- Allocating, associating and disassociating a public IP carry no `Idempotency-Key`, so they are never
  retried after a `5xx` or a lost connection. If the API processed one but its answer was lost, the error
  reaches you and the address may exist without being in state: check the panel or `GET .../compute/public-ips`
  and `tofu import` it.
- A `503` with code `not_ready` (the feature is switched off, for example SSH keys that are not switched on
  yet) is never retried.
- A `409` with code `idempotency_in_progress` is retried for creating an instance or importing an SSH key: the
  first attempt with the same `Idempotency-Key` is still being processed, and the key returns its answer
  once the API has stored it. Every other `409` is returned at once.
- The wait is the `Retry-After` header (seconds or an HTTP date) when the API sends one, otherwise exponential
  backoff with full jitter: a random wait between zero and a ceiling that starts at 1 second, doubles with
  each retry and is capped at 20 seconds. A `Retry-After` above 60 seconds is not waited for: the error is
  returned at once.
- At most 5 attempts in all, and never a wait that would end after the context's deadline: the last error is
  returned instead. Cancelling the run (Ctrl-C) ends a wait at once.

When it gives up, the error is still the API's own (status, code, detail, request id) followed by
`gave up after N attempts`.

When creating an instance or importing an SSH key gives up after a lost answer, a gateway error, a duplicate
that is still being processed or a lost connection, the first request may have been processed anyway, so the
resource may exist without being in state. The error says so and says what to do: if the next apply reports
`name_taken`, find the resource in the panel or with the API and bring it under management with `tofu import`
(`tofu import virtscale_instance.<name> <id>`, `tofu import virtscale_ssh_key.<name> <id>`). A `429`, a
`not_ready`, a refusal of the API and a certificate error carry no such hint: nothing was created.

Waiting for a result is separate from retrying a request: creating an instance waits up to 20 minutes for
state `running` (in practice 30 to 60 seconds) and fails at once, with the API's `error_class` and `error_detail`, if it reaches `error`;
resizing one waits up to 20 minutes for the new size;
destroying waits up to 10 minutes (an instance is gone in about 15 seconds); each public IP operation waits up to 10 minutes. Waits poll every 5 seconds.

## Errors

Every error carries the API's own message and its request id (quote the request id when you ask for help),
including the field list the API sends with a `400`. Where the fix is on your side, a hint follows:

| Response | Hint |
| -------- | ---- |
| `401 unauthorized` | check `VIRTSCALE_API_KEY`: the key may be wrong, revoked or expired |
| `403 permission_denied` | the scope that grants the permission it names: `compute:write` or `compute:read` for `compute.*` (instances, SSH keys, images) and `operation.*`, `network:write` or `network:read` for `edge.*` and `network.*`, `write` for creating, renaming and deleting a project |
| `403 source_address_not_allowed` | the key's network limits do not include this address |
| `404` about the organization | check `VIRTSCALE_ORGANIZATION`, or that the key belongs to that organization |
| `412 generation_mismatch` | the resource was changed by something else while the change was made: apply again |
| `409 vm_protected` | the instance is protected against deletion: set `disable_api_termination = false` and apply first, then destroy or replace it |
| `422 host_placement_not_allowed`, `422 network_interface_not_allowed` | `host_id`, `network_interface` and internal instance types are for VirtScale's internal organizations |
| `422 host_unavailable` | check `host_id`: the host must exist, be in the instance's region and be in service |
| `422 host_required` | the storage pool keeps the disk on one host: set `host_id` to the host the volume lives on |
| `409 resize_not_available` | an instance with `host_id` or of an internal instance type is replaced, not resized: the provider plans a replacement when `host_id` is set, and `tofu apply -replace` replaces any other |

Only a `404` that says the resource itself is gone is read as "gone": its code is `not_found` and its detail is
the API's sentence for that kind of resource (an instance, a public IP or an SSH key, as asked about). Every other
`404` is an error with the API's own detail, and the resource stays in state: a `404` about the organization
(pointing the provider at the wrong organization), about the compute service or a region, an unknown route (a
mistyped `endpoint`: a bare `404 not_found: Not Found`, with no hint) and a proxy's error page. Dropping a live resource on a wrong `404` would orphan it, and a
destroy that waits for "gone" would report a deletion nobody saw.

Error text that came from the response has your API key replaced by `[redacted]`, in case a proxy in front of
the API prints the request.

## Resources and data sources

### `virtscale_instance`

| Attribute         | Type   | Required/Computed | Notes |
| ----------------- | ------ | ------------------ | ----- |
| `name`            | string | Required, forces replacement | 1-63 lowercase letters, digits or hyphens, starting and ending with a letter or digit; checked at plan time (the API lower-cases names and strips a trailing `.virtscale.nl`, so any other form would come back different). Unique among live instances in the same region; a deleted instance's name is reusable. |
| `region`          | string | Required, forces replacement | |
| `image`           | string | Required, forces replacement | Image id from `virtscale_images`. Set at creation only; the provider does not detect image drift in v0. |
| `instance_type`   | string | Optional, Computed, forces replacement when set to a different value | A catalogue type from `virtscale_instance_types`, for example `g1-small`. It sets the three sizes; name it **or** give `vcpus`, `memory_mb` and `disk_gb`, not both. **Production has no orderable types today** (`virtscale_instance_types` returns none, and a type is refused with `422 instance_type_unpriced`): give the sizes instead. Read back from the API (null for an instance it reports no type for). A platform that only sells types refuses raw sizes with `422 instance_type_required`. |
| `vcpus`           | number | Optional, Computed, changes in place | 1, 2, 4, 8, 16 or 32. Taken from `instance_type` when one is named; otherwise `vcpus`, `memory_mb`, `disk_gb` and `storage_pool` are given together. A different value resizes the instance in place (below): **it restarts**. |
| `memory_mb`       | number | Optional, Computed, changes in place | Whole gigabytes from 1 to 128, written in megabytes: gigabytes times 1024 (1024 for 1 GB, 4096 for 4 GB, 131072 for 128 GB). Given with `vcpus`, as above. A different value resizes the instance in place (below): **it restarts**. |
| `disk_gb`         | number | Optional, Computed, forces replacement when set to a different value | 10 to 2000, in steps of 10. Given with `vcpus`, as above. The API resizes vCPUs and memory only, so a different disk is a new instance. |
| `storage_pool`    | string | Optional, Computed, forces replacement when set to a different value | For example `gro-ceph` in `gro`. Left out with `instance_type`, the API uses the type's own pool; with raw sizes it is required. State holds the pool the instance is on. |
| `network_profile` | string | Optional, Computed, forces replacement when set to a different value | For example `gro-tenant-default` in `gro`. Left out, the API picks the first profile your organization may use in the region on the hosts the instance is placed on. State holds the profile the instance is on. |
| `ssh_keys`        | list(string) | Optional, forces replacement | Names of keys imported into the organization with `virtscale_ssh_key`, at most 10, each listed once (both checked at plan time, with the API's own wording). cloud-init installs them for the image's default user (for example `debian` or `ubuntu`) at first boot, and the instance keeps them if a key is deleted or replaced later. A name that was never imported is `422 ssh_key_not_found`, and a `503 not_ready` means SSH keys are not switched on for this API yet. Read back from the API on import, so an imported instance does not plan a replacement for its keys. |
| `user_data`       | string | Optional, Sensitive, forces replacement | Plain cloud-init text for the first boot: a `#cloud-config` document, a script that starts with `#!`, or a MIME multipart document, at most 32 KiB (checked at plan time; an empty value is refused too). The provider sends it as standard base64 and the API applies it after the platform's own part (the keys of `ssh_keys`). It is read at the first boot only, so a different value replaces the instance. **Never read back:** state keeps the value you configured, and an imported instance has none, so a configuration that sets `user_data` after `tofu import` plans a replacement (put `lifecycle { ignore_changes = [user_data] }` on it to avoid that). Sensitive, so plans hide it, but the state holds it as text. `503 not_ready` means cloud-init is not switched on for this API yet; a payload the API cannot deliver is `422 user_data_invalid`. |
| `workload_type`   | string | Optional, Computed, forces replacement when set to a different value | `customer` (the API's default) or `infra`, VirtScale's own hardware pools; checked at plan time. Only an organization of kind internal may ask for `infra`: any other gets `422 workload_not_allowed`. Read back from the API (VmOut has `workload_type`). |
| `disable_api_termination` | bool | Optional, Computed, changes in place | Protect the instance against deletion, like AWS's `DisableApiTermination`. Defaults to what the API reports, `false`. A create sets it once the instance runs; a change is one `PUT .../protection`, never a replacement; a refresh reads it back (`protected`). While it is `true`, a destroy or a replacement is refused with `409 vm_protected` and the provider adds "set disable_api_termination = false and apply first": it never lifts the protection for you. Changing it needs the `compute.instance.update` permission (`compute:write`). |
| `host_id`         | string | Optional, Computed, forces replacement when set to a different host | Internal organizations only: place the instance on this compute host, like AWS's Dedicated Host `host_id` (for example `IKVM-AMS1-01`; one plain host name, not case sensitive: the API answers in its own spelling and a change of case alone is no change). Any other organization is refused with `422 host_placement_not_allowed`; a host that cannot take the instance is `422 host_unavailable` (one sentence for a host that does not exist, is in another region or is out of service); a storage pool that keeps the disk on the host the instance runs on needs it (`422 host_required`). Built on that host in one attempt. Read back from the API (null for an instance on no named host), so an import fills it. **An instance with a `host_id` is never resized in place:** a different `vcpus` or `memory_mb` plans a replacement (the API refuses the resize with `409 resize_not_available`). Taking the line out keeps the host. |
| `root_block_device` | block (at most one) | Optional, forces replacement when it names a different pool or size | The root volume, like AWS's `root_block_device`: another spelling of `storage_pool` and `disk_gb`, with `volume_type` (the storage pool id, for example `ams1-local`) and `volume_size` (GB), and `delete_on_termination`, which can only be `true` (the default, as `aws_instance` accepts it): the platform always deletes the root volume with the instance, so `false` is refused at plan time. It does not go with `storage_pool` or `disk_gb`, and `volume_size` does not go with `instance_type` (`volume_type` does, as `storage_pool` does): refused at plan time. A host-local pool such as `ams1-local` is a volume type here and needs `host_id`. Read back (also into `storage_pool` and `disk_gb`) only when the configuration or the state has the block, so a configuration that uses `storage_pool` and `disk_gb` sees no change, and neither does an imported instance; naming the same pool and size as a block on an instance that has none adopts it with an update in place, and taking the block out for the same two values changes nothing either. |
| `network_interface` | block list | Optional, forces replacement when a block differs | Internal organizations only: the instance's network interfaces, like AWS's `private_ip`, `secondary_private_ips` and `source_dest_check`, plus a fixed MAC. Block *i* describes interface *i* of the network profile, in order (for example one block for `ams-ssd-infra`, AMS1's k8s-geneve network). Each block has `private_ip`, `mac_address` (six pairs of hex digits split by colons; the API stores it in lower case and state keeps your spelling, so upper case plans no difference, and a change of case alone is an update in place that changes nothing), `secondary_private_ips` (a set of at most 8, for example the shared address of a VRRP pair; they need `source_dest_check = true`, which is the default, checked at plan time) and `source_dest_check` (`false`: the interface is not filtered and carries no firewall). Any other organization is refused with `422 network_interface_not_allowed`; more blocks than the profile has interfaces, a bad address or MAC, or one used twice is `422 invalid_spec` naming the entry; an address or MAC already in use on the network fails the create. Read back as asked (what a block leaves out the platform chooses, and it stays null). An import reads the interfaces back from the API, so a configuration that names the same ones plans "No changes"; if its configuration has no blocks it plans no replacement either, only one update in place that forgets the interfaces in state, and later reads do not bring them back. Blocks added to an instance whose state has none replace it (the interfaces are set at create); taking the blocks out keeps the instance. |
| `hostname`        | string | Optional, Computed | The lower-case fully qualified name the instance is known by in your organization (for example `web.example.com`); checked at plan time (labels of lowercase letters, digits and hyphens, at least two labels, at most 253 characters). Unique among the organization's live instances: a duplicate is `409 hostname_taken`; another organization may use the same name. It is a handle only: the platform creates no DNS record for it. Changing it is an in-place update, not a replacement. It is read back on every refresh, so a change made elsewhere shows as a difference, and `tofu import` fills it. Removing the attribute from the configuration keeps the current hostname (this provider does not remove one). |
| `id`              | string | Computed | |
| `state`           | string | Computed | |
| `status`          | string | Computed | The instance's status as the API reports it, for example `running` or `stopped`. |
| `fqdn`            | string | Computed | Null for a customer account. |
| `private_ipv4`    | string | Computed | The instance's private IPv4 address as the platform reports it; empty until it is known. An associated public IP is not the private address. Kept across plans, so an in-place change does not plan it as known after apply. |
| `image_name`      | string | Computed | The name of the image the instance was built from, as reported by the API. Kept across plans, like `private_ipv4`. |
| `created_at`      | string | Computed | RFC 3339. |

The optional attributes follow the API's own rules: a create names an `instance_type` or all of `vcpus`, `memory_mb`,
`disk_gb` and `storage_pool`, and a mix, none of them, or a partial set is refused at plan time with the API's
wording. What the API decides it still decides, and its answer is shown as it comes: `422 instance_type_unavailable` for
a type it does not sell in the region, `422 storage_pool_unavailable`, `422 network_profile_unavailable`. A value
left out of the configuration is the API's choice, read back into state: naming a different one later replaces the
instance, and taking the line out again keeps what the instance has, so the next plan is empty.
The API checks `vcpus`, `memory_mb` and `disk_gb` against a size grid: `vcpus` is 1, 2, 4, 8, 16 or 32, memory is whole
gigabytes from 1 to 128 (`memory_mb` is gigabytes times 1024) and `disk_gb` is 10 to 2000 in steps of 10; a size off
the grid is refused with a list of every violation (`422 invalid_spec`). The organization's quota must have room for it
as well. Until the instance-type catalogue has orderable types, look the values up in the API: `GET
/v1/orgs/{org}/compute/limits`, `GET /v1/orgs/{org}/compute/storage-pools` and `GET
/v1/orgs/{org}/compute/network-profiles`. The `min_disk_gb` and `min_ram_mb` of an image are 0 today and are no guide to
a size. Destroying an instance waits until it is gone.

The provider does not manage power state. An instance stopped through the API stays stopped, and the plan stays empty
(`state` reads `stopped` after the next refresh). An instance create takes about 30 to 60 seconds and a destroy about
15 seconds; the waits below are ceilings, not what to expect. Four attributes change in place: `hostname` (one PATCH,
guarded by the instance's current generation read just before it, so a change made at the same moment by someone else is
refused with `412 generation_mismatch` and the next run picks it up), `disable_api_termination` (one PUT) and `vcpus` and
`memory_mb` (a resize, below). Every other attribute forces replacement. The in-place changes of one apply are made in
that order: hostname, protection, resize.

**Resize.** A different `vcpus` or `memory_mb` is one `POST .../resize` of the same instance, with an `Idempotency-Key`,
and **the instance restarts** (a cold resize: the platform stops it, moves it to the new size and starts it again; a
stopped instance stays stopped). Apply waits (up to 20 minutes) until the instance reports the new `vcpus` and
`memory_mb` and is `running` or `stopped` again. A resize the platform cannot complete is reverted: the instance keeps
its size and apply fails with the operation's `error_class` and `error_detail`. The disk cannot change (a different
`disk_gb` replaces the instance), and so does a different `instance_type`. Resizing is a feature that may be switched off
for an organization: the API then answers `503 not_ready`, which is shown as it comes and not retried. An instance with a `host_id` is never resized: the plan
replaces it (above). A resize of an
instance whose `instance_type` is set keeps that type in the API; to manage the sizes, give `vcpus`, `memory_mb`,
`disk_gb` and `storage_pool` instead of the type.

### `virtscale_ssh_key`

An SSH public key imported into the organization, like AWS's `ImportKeyPair`. Instances take keys by name
(`virtscale_instance.ssh_keys`), and cloud-init installs them for the image's default user when an instance first
boots. Only the public key is stored.

| Attribute     | Type   | Required/Computed | Notes |
| ------------- | ------ | ------------------ | ----- |
| `name`        | string | Required, forces replacement | Unique within the organization and case-sensitive: 1 to 64 characters from letters, digits, `.`, `_`, `@` and `-`, starting with a letter or digit. A name that is taken is `409 name_taken`; one that breaks the rule is `422 invalid_name`. |
| `public_key`  | string | Required | One OpenSSH line, `<type> <base64> [comment]`, for example `file("~/.ssh/id_ed25519.pub")`. Types: `ssh-ed25519`, `ecdsa-sha2-nistp256`, `ecdsa-sha2-nistp384`, `ecdsa-sha2-nistp521`, and `ssh-rsa` of 2048 to 16384 bits. A key the API does not take is `422 invalid_public_key`, and its detail says why. A different key forces replacement; a change that leaves the key alone does not (below). |
| `id`          | string | Computed | |
| `key_type`    | string | Computed | The type the API recognised, for example `ssh-ed25519`. |
| `fingerprint` | string | Computed | `SHA256:` and the unpadded base64 of the SHA-256 of the key, as `ssh-keygen -lf` prints it. |
| `created_at`  | string | Computed | RFC 3339. |

```hcl
resource "virtscale_ssh_key" "laptop" {
  name       = "laptop"
  public_key = file("~/.ssh/id_ed25519.pub")
}

resource "virtscale_instance" "web" {
  # ...
  ssh_keys = [virtscale_ssh_key.laptop.name]
}
```

**One key, one meaning, whatever the text.** The API stores the key in canonical form, `<type> <base64>`, with the
comment dropped. The provider compares the key type and the decoded key bytes, not the text. So
`file("~/.ssh/id_ed25519.pub")`, with its comment and trailing newline, is the same key as what the API stores;
surrounding whitespace and CRLF line endings do not matter; and two spellings of one base64 string (spare bits in
its last character) are one key. The text is still shown: a plan that only changes the text of the same key, such as an
edit of the comment, shows an in-place `~ public_key` change that stores your text and makes no API call. That is also
what you see in the first plan after `tofu import`, where the state holds the API's canonical form and your
configuration holds the file with its comment. It never replaces the key. A different key
does: the old key is destroyed and the new one is created (the API's import, not `tofu import`). A text that does not parse (a bad key) is compared exactly, so it still reaches the API,
which refuses it with `422 invalid_public_key`.

**Keys are a launch-time snapshot.** Deleting or replacing a key does not change instances that were created with
it: they keep the key they booted with, and `virtscale_instance.ssh_keys` is not touched. Changing an instance's
`ssh_keys` replaces the instance. The organization has a quota of keys (`422 quota_exceeded` beyond it), and a
`503 not_ready` on an import, or on creating an instance that names keys, means SSH keys are not switched on for
this API yet; it is not retried.

Import by id: `tofu import virtscale_ssh_key.laptop <id>`.

### `virtscale_project`

A project groups an organization's servers, public IPs and networks. Every organization has a default project, which
is never deleted (read it with the `virtscale_project` data source, or `tofu import` it to read it as a resource;
destroying it is refused with `409 default_project`).

| Attribute     | Type        | Required/Computed | Notes |
| ------------- | ----------- | ----------------- | ----- |
| `name`        | string      | Required | 1-63 lower-case letters, digits or hyphens, not starting or ending with a hyphen (checked at plan time). Unique among the organization's projects (`409 name_taken`). Changing it renames the project in place. |
| `description` | string      | Optional, default `""` | At most 255 characters. In place. |
| `labels`      | map(string) | Optional, default `{}` | At most 64; keys of 1-63 characters from letters, digits and `_ . : / -`, values of at most 255. Setting labels replaces the whole set. In place. |
| `id`, `vrn`   | string      | Computed | `vrn:<organization>:project:<id>`. |
| `is_default`  | bool        | Computed | |
| `created_at`, `updated_at` | string | Computed | RFC 3339. |

A change is one `PATCH` under the generation of a fresh read (`If-Match`): a change made at the same moment by someone
else is refused with `412 generation_mismatch` and the next run picks it up. A project that still holds a server, a
public IP or an address allocation cannot be deleted (`409 project_not_empty`); a VPC is not counted, so delete a
project's VPCs before the project (Terraform does, when the VPC refers to the project). The organization has a quota of 100 projects
(`422 quota_exceeded`). Import by id: `tofu import virtscale_project.web <id>`.

The data source `virtscale_project` takes an optional `name` and returns `id`, `vrn`, `description`, `labels`,
`is_default` and `created_at`; without a `name` it is the default project.

### `virtscale_vpc`

A private address space in one project and one region, built and deleted asynchronously: apply waits (up to 20
minutes) until the VPC is `active` and fails with the platform's reason if it ends in `error`; destroy waits (up to 10
minutes) until it is gone. A VPC has no update call, so a change to any attribute replaces it. VPCs are switched on per
deployment: where they are not, every call is `503 not_ready` and the error says so. **Production does not switch them
on yet:** a plan succeeds there, and only the apply fails, at once, with `503 not_ready`.

| Attribute         | Type   | Required/Computed | Notes |
| ----------------- | ------ | ----------------- | ----- |
| `project_id`      | string | Required, forces replacement | The project's id, from `virtscale_project`. |
| `name`            | string | Required, forces replacement | 1-63 lower-case letters, digits or hyphens. Unique among the project's VPCs in one region (`409 name_taken`). |
| `region`          | string | Required, forces replacement | A region id from `virtscale_regions`; one that is not available to you is `422 region_unavailable`. |
| `description`     | string | Optional, default `""`, forces replacement | At most 255 characters. |
| `prefix_length`   | number | Optional, Computed, forces replacement when set to another value | 20 to 24. Leave it out for the address plan's default; it is read back from `cidr`. |
| `id`, `vrn`       | string | Computed | |
| `cidr`            | string | Computed | The private address block the platform gave it. |
| `status`          | string | Computed | `pending`, `creating`, `active`, `deleting`, `deleted` or `error`. |
| `is_default`, `internet_egress` | bool | Computed | |
| `created_at`      | string | Computed | RFC 3339. |

The organization has a quota of VPCs (`422 quota_exceeded`). A VPC that still has subnets cannot be deleted
(`409 vpc_not_empty`). A delete that the platform cannot carry out ends the VPC in status `error`; destroy says so with the
platform's reason. `internet_egress` is true only for the project's default VPC. An imported VPC that has a description
needs the same `description` in the configuration, or the plan replaces it. Import by `<project id>/<vpc id>`: `tofu import virtscale_vpc.net <project id>/<vpc id>`.

### `virtscale_instance_firewall`

The whole firewall rule set of one instance. The API replaces the set as one and applies it asynchronously, so the
resource owns all of it: a rule added in the panel shows up as a difference and is removed by the next apply. Apply
waits (up to 20 minutes) until the API says `applied` and fails with its text if the set ends in `error`. The first
`error` it sees ends the wait, although the platform retries a few times and may succeed later, so a failure can be a
transient one: the next plan reads the state as it is. A set that ended in `error` may not be fully in force, so the
resource then reports no confirmed rules: the next plan shows the difference, and the next apply of an update sets the
rules again (a create that failed is tainted and replaced). A change the platform's worker gave up on (a precondition
such as a missing network binding) can stay `pending` for good: apply then ends at the 20 minutes with `last seen: state
pending`, and a later apply or the destroy sets the rules again, because the API only refuses a new change while an
operation really runs. Destroying the resource asks for the empty rule set (the rules this API lets you set; the
platform's own base rules are not part of the set) at once, asks again while another change is still being applied
(`409 operation_in_flight`), and then waits until the empty set is applied; asking and waiting share one budget of 10
minutes. An instance that is gone
has no rules to clear. An instance the platform does not let you change rules for (a region policy) is refused with
`409 vm_action_not_allowed`.

**The first apply replaces whatever rules the instance already holds** (set in the panel or at creation), without a
preview of them. To see them first, import the instance's firewall (`tofu import`, below) and run `tofu plan`: the
difference to your configuration is what the apply would change.

| Attribute     | Type   | Required/Computed | Notes |
| ------------- | ------ | ----------------- | ----- |
| `instance_id` | string | Required, forces replacement | The instance's id. |
| `rule`        | set of blocks | Optional, at most 50 | The order does not matter. See below. |
| `id`          | string | Computed | The instance's id. |
| `state`       | string | Computed | `pending` while the API applies a change, `applied`, or `error`. |

A `rule` block has `direction` (`ingress` or `egress`), `protocol` (`tcp`, `udp`, `icmp` or `any`), `port_min` and
`port_max` (1 to 65535; required for `tcp` and `udp`, not allowed for `icmp` and `any`), `remote_cidr` (an IPv4 CIDR
with no host bits set: `10.0.0.0/24`, not `10.0.0.5/24`, because the API would store it masked) and an optional
`description` (1 to 80 characters). The rules are checked at plan time against the API's limits (its sentences for the
ports and the description; the direction, protocol and CIDR sentences are the provider's own); whatever else the API
refuses comes back as `422 invalid_rule` with the field of every rule it refused (`422 too_many_rules` past 50).

```hcl
resource "virtscale_instance_firewall" "web" {
  instance_id = virtscale_instance.web.id

  rule {
    direction   = "ingress"
    protocol    = "tcp"
    port_min    = 22
    port_max    = 22
    remote_cidr = "203.0.113.0/24"
    description = "ssh from the office"
  }
}
```

Import by the instance's id: `tofu import virtscale_instance_firewall.web <instance id>`.

### `virtscale_s3_bucket`

An object-storage bucket. It is created, resized and deleted asynchronously: apply waits (up to 20 minutes) until the
bucket is `active` and fails if it ends in `provision_failed`, `degraded` or `suspended`. The organization needs the S3
service switched on (the panel does it), and has a limit of buckets (`422 bucket_limit_reached`).

| Attribute        | Type   | Required/Computed | Notes |
| ---------------- | ------ | ----------------- | ----- |
| `name`           | string | Required, forces replacement | 3-63 lower-case letters, digits and hyphens, starting and ending with a letter or digit, no dots, no two hyphens in a row, and not a name (`admin`, `api`, `www`, `internal`, `system`, `backup`, `billing`, `metrics`, `health`) or a prefix (`virtscale-`, `vs-`, `s3-`, `rgw-`, `admin-`, `internal-`, `system-`) the platform keeps: checked at plan time with the API's sentences (`400 bucket_name_invalid`, `400 bucket_name_reserved`). Unique within the organization (`409 bucket_already_exists`). |
| `versioning`     | bool   | Optional, default `false` | Keep previous versions of overwritten and deleted objects. Fixed at creation: a change is refused at plan time, because replacing the bucket would soft-delete it and its name stays taken (below). |
| `max_size_bytes` | number | Optional | The most bytes this bucket may hold, inside the organization's ceiling (`422 bucket_quota_exceeds_plan` above it). Leave it out for no per-bucket cap. Changes in place; removing it clears the cap. |
| `max_objects`    | number | Optional | The most objects. Same rules. |
| `force_destroy`  | bool   | Optional, default `false` | Acknowledge on destroy that the bucket may hold objects. Without it, a bucket whose last metering sample saw objects or bytes is `409 bucket_not_empty` (what the API calls empty is below); a young bucket that has no sample yet is not (below). The provider's own: the API does not store it, so import leaves it `false`. |
| `id`, `state`, `created_at` | string | Computed | |

Deleting is a **soft delete**: the API marks the bucket `deleting_soft` and answers `202` at once, its data is kept for
a retention window (7 days), and the plan's bucket slot is freed. Access to the bucket is cut **afterwards, by a worker**:
the cut is not instant and not guaranteed (the worker retries and can give up, and an access key of the organization can
undo it until the platform's reconciler writes it again), so a bucket that was just destroyed can stay reachable over S3
for a while. A bucket in that state reads as gone, but **its name stays taken** until it is purged (`409
bucket_already_exists` for a new bucket of that name; the panel can restore a soft-deleted bucket). **The platform's
purge worker is on in production** (since 2026-10-27, `S3CP_BUCKET_PURGE_ENABLED=true`, not a dry run), so a soft-deleted
bucket is erased when its 7 days are up and its name is free again; a deployment that ships the worker off (the API's
default) never erases one. Until then the bucket keeps a retention row: the API limits the buckets an organization holds,
live and soft-deleted, to twice its bucket limit (`422 bucket_retention_limit_reached`, which names the oldest erasure
date), so do not create and destroy buckets in a loop. A destroy of a bucket that is already being deleted (`409
bucket_already_deleting`) is done; one in a state that cannot be deleted (`409 bucket_not_deletable`, while it is still
being built or erased) is an error with the API's sentence. The ceilings are applied by the object store after the API
accepts them, and a change right after another operation of the bucket can meet `409 operation_in_flight`, which the
provider repeats for about a minute.

What counts as empty is the API's judgement from the **last metering sample**, not a fresh look at the object store: the
sampler records every bucket's object and byte count every 10 minutes (the API's default), so the judgement can be that
old in both directions, and it has no age limit. A bucket whose last sample saw objects or bytes (bytes alone count:
incomplete multipart uploads) is `409 bucket_not_empty` unless `force_destroy` is set; the refusal quotes the sample's time
and counts, and on a versioned bucket it says that the sample counts previous versions a plain object listing does not
show, so deleting every object you can see does not make it empty. **When there is no sample the API lets the delete
through** without `force_destroy`: none for the organization, none for this bucket, or only one older than the bucket
(a bucket filled or created since the last sample is let through the same way). So a young bucket, one that has had no
sample yet (samples come every 10 minutes), is soft-deleted with `force_destroy = false` even when it holds objects. It
is recoverable: the objects are kept for the 7 days of the retention window and the panel can restore the bucket. A
bucket you have just emptied can still be refused until the next sample; set `force_destroy = true`, apply, and
destroy again. Nothing is erased either way and the retention window is unchanged.

If a create fails after the bucket was made (the API refuses a ceiling, `422 bucket_quota_exceeds_plan`; the bucket ends
in `provision_failed`), the bucket is in the state, tainted by the error, and the next apply would replace it, which
cannot work because the soft-deleted bucket keeps the name. After a refused ceiling, fix it in the configuration, run
`tofu untaint` on the bucket and apply again; after a failed build, give the bucket another name. To destroy a bucket that
holds objects, set `force_destroy = true`, run `tofu apply` so that the setting is stored, and destroy again. Import by
id: `tofu import virtscale_s3_bucket.logs <id>`.

### `virtscale_s3_access_key`

An S3 access key for the organization's storage. There is nothing to configure. An organization holds at most 5 active
access keys: a sixth is `422 access_key_limit_reached` until one is revoked. **The secret is returned exactly once,
by the request that creates the key**, so the provider keeps `secret_access_key` in the state, marked sensitive. That
means the state holds a credential: use an encrypted backend (or OpenTofu's state encryption), and do not print the
attribute in an output. A key's secret cannot be read back, so an imported key has none (`secret_access_key` is null).
Destroying the resource revokes the key and waits until it reads revoked (a key that is already revoked, `409
access_key_already_revoked`, is done, and one that is being revoked, `409 access_key_already_revoking`, is waited for;
a key that cannot be revoked yet, `409 access_key_not_revocable`, is an error with the API's sentence). To rotate, replace the resource
(`tofu apply -replace=virtscale_s3_access_key.app`, or `replace_triggered_by`). The key is a key on the organization's
own storage owner: it can delete the organization's buckets and objects directly over S3, bypassing the soft-delete
window.

If a create is repeated after a lost answer (a gateway error), the API answers the repeat with the stored result, which
never holds the secret. The key exists then, so the provider keeps it in the state and fails with "Access Key Secret Not
Returned": run `tofu destroy` (or apply again, which replaces it) to revoke it and get a new one. If the provider gives up
before it gets any answer, or the answer to the create cannot be read (a 201 whose body is not valid JSON), the key may
exist with its secret lost: look at the organization's access keys (at most 5) in the panel or with the API
(`GET /v1/orgs/{org}/s3/access-keys`), and revoke any you did not receive (`DELETE /v1/orgs/{org}/s3/access-keys/{id}`). Destroying the resource revokes the key; a revoke that fails
leaves it `degraded`, which the destroy reports at once.

| Attribute           | Type   | Required/Computed | Notes |
| ------------------- | ------ | ----------------- | ----- |
| `id`                | string | Computed | |
| `access_key_id`     | string | Computed | The public half, to use as the S3 access key id. An organization holds at most 5 keys (`422 access_key_limit_reached`). |
| `secret_access_key` | string | Computed, sensitive | Known only after the create here. |
| `state`, `created_at` | string | Computed | |

Import by id: `tofu import virtscale_s3_access_key.app <id>`.

### What the provider does not manage: IAM

Members, invitations and API keys are managed by people, not by API keys: the `iam.*` permissions are in no key scope
(no scope map of an API key holds them, so a key cannot mint or revoke keys or invite, change or remove members; for
people the writes also need a fresh second factor, where the deployment enforces it). That is deliberate, so a leaked
key cannot widen its own reach, and the provider has no resource for them. Use the panel for members and keys;
`virtscale_caller` shows what the key you run Terraform with can do.

### `virtscale_public_ip`

| Attribute     | Type   | Required/Computed | Notes |
| ------------- | ------ | ------------------ | ----- |
| `region`      | string | Required, forces replacement | |
| `instance_id` | string | Optional | Associates/disassociates in place; never forces replacement. |
| `id`          | string | Computed | |
| `address`     | string | Computed | |
| `state`       | string | Computed | |

Associating needs the instance to be `running` (otherwise `409 vm_not_in_a_state_for_this`) and in the same
region as the address (`422 region_mismatch`), and an instance carries one public IP at a time. Allocating
beyond the quota is `422 quota_exceeded`. Destroying disassociates first when the address is associated, then
releases it.

### `virtscale_caller` (data source)

Who the API key is, from `GET /v1/me`. It fails early, with the organization named, when the key does not belong to
the provider's `organization`, which is easier to read than the `404` of the first resource.

| Attribute              | Type        | Notes |
| ---------------------- | ----------- | ----- |
| `id`, `subject`        | string      | `api-token:<key id>` for an API key. |
| `realm`, `credential`  | string      | `credential` is `api_token` for an API key. |
| `organization_id`      | string      | The provider's `organization`, confirmed to be one the credential names. |
| `permissions_enforced` | bool        | Whether the deployment refuses a request that lacks a permission. |
| `scopes`               | set(string) | The key's scopes; empty when the credential is not an API key. |
| `permissions`          | set(string) | Every permission the credential holds in the organization. |
| `role`                 | string      | Null for an API key. |
| `api_key_id`           | string      | Null when the credential is not an API key. |
| `project_id`           | string      | Set when the key is restricted to one project. Such a key is refused on `/v1/orgs/{org}/...`. |
| `expires_at`           | string      | RFC 3339; null when the key never expires. |

### `virtscale_regions`, `virtscale_instance_types`, `virtscale_plans` (data sources)

Read-only catalogues, in the order the API gives them. `virtscale_regions.regions` lists `id`, `name`, `datacenter`,
`status` (`active`, `coming_soon`, `maintenance`, `retired`), `access`, `entitled` (whether this organization may use
a restricted region), `default` and `disk_types` (`disk_type` and `status`). `virtscale_instance_types.instance_types`
lists `id`, `vrn`, `name`, `family`, `cpu_kind`, `vcpus`, `memory_mb`, `disk_gb`, `regions` (where it can be ordered
now) and `status`; the optional `region` keeps the types that can be ordered there. Production has no orderable types today, so the list is empty there. `virtscale_plans.plans` lists the
Managed VPS plans with `id`, `name`, `vcpus`, `ram_mb`, `disk_gb`, `monthly_price_cents`, `currency`, `status`,
`management_level` and `backup_policy`.

### `virtscale_images` (data source)

| Attribute | Type   | Required/Computed | Notes |
| --------- | ------ | ------------------ | ----- |
| `region`  | string | Required | |
| `id`      | string | Computed | Set to `region`. |
| `images`  | list(object) | Computed | Sorted by `name`. Each object: `id`, `name`, `os_distro` (nullable), `os_version` (nullable), `min_disk_gb`, `min_ram_mb` (both 0 for every image today: not a guide to a size). |

## Testing

Unit tests (client package) run with plain `go test`. Provider and resource tests are Terraform acceptance
tests (`terraform-plugin-testing`) against an in-process fake of the customer API; they only run with
`TF_ACC=1` and `tofu` on `PATH`. The fake is written from the deployed API's spec and handlers
(s3-control-plane `94f5021f`, and `87607618` for the S3 buckets and access keys: wire shapes, error envelope, state
machines, the codes, sentences and order of the refusals, key scopes), not from the client's
own types, so a wrong field name or status in the client fails a test instead of being mirrored back. On top of
that, every request the client sends the fake and every answer it gets back is checked against the OpenAPI document
the provider was written against, `internal/contract/customer-v1.json` (`SOURCE` names its s3-control-plane commit and says
how to update it): a route the contract does not have, a deprecated `/compute/vms` route, a request field it does
not know and an answer that lacks a required field all fail the test. This stands in for a generated client until
`sdk-go` can be used (decision D12 in `docs/plans/terraform-provider-virtscale.md`). Its SSH-key
routes follow the contract of the SSH-key release of s3-control-plane, which is not deployed yet; its key
parsing and fingerprints are its own, checked against what `ssh-keygen -lf` prints for keys OpenSSH made. Its host
placement (`host_id`, the host-local pool `ams1-local`, the refusals) and its network interfaces follow s3-control-plane
`75d63a00` (main, with the host placement and the internal instance types), which is not deployed yet. Run
everything with:

```bash
TF_ACC=1 \
TF_ACC_TERRAFORM_PATH=$(command -v tofu) \
TF_ACC_PROVIDER_NAMESPACE=virtscale \
TF_ACC_PROVIDER_HOST=registry.terraform.io \
go test ./... -count=1 -v
```

### Live acceptance (staging)

The live tests run the same lifecycle proof as the fake-backed ones against a real API: apply creates the objects, a
second plan is empty, an in-place update works where the resource allows one, import by id works, and destroy removes
them. One test per family:

| Test | Proves |
| ---- | ------ |
| `TestLiveSweepLeftovers` | not a proof: it removes the instances named `tf-live-*` that an earlier run left behind (protection lifted first, then deleted, waited until gone) and passes at once when there are none. The CI job runs it first of all, so a protected leftover cannot block the clean-start check or take the staging host's one server slot |
| `TestLiveSshKeyResource_LifecycleProof` | `virtscale_ssh_key` |
| `TestLiveCatalogueProof` | `virtscale_caller` (its organization is the configured one), `virtscale_regions` (lists the live region), `virtscale_instance_types`, `virtscale_plans`, `virtscale_project` |
| `TestLiveProjectResource_LifecycleProof` | `virtscale_project`: create, an empty second plan, description and labels in place, import |
| `TestLiveVpcResource_LifecycleProof` | `virtscale_vpc` in the default project: waits for `active`, empty second plan, import as `<project>/<vpc>`, destroy waits until it is gone |
| `TestLiveS3Resources_LifecycleProof` | `virtscale_s3_bucket` (a ceiling changed in place; destroy is a soft delete) and `virtscale_s3_access_key` (the secret is in the state; destroy revokes). **Opt-in** (`VIRTSCALE_LIVE_S3=1`): it needs an object store behind the API, which staging has not, and the bucket it leaves soft-deleted may never be purged |
| `TestLiveRegistryRelease_SshKeyProof` | the published release (exactly `registryReleaseVersion`) installed from registry.terraform.io, checked against its signed SHA256SUMS: `virtscale_ssh_key` create, an empty second plan, destroy. It has no twin against the fake: it needs the registry and GitHub |
| `TestLiveInstanceFirewallResource_LifecycleProof` | `virtscale_instance` and `virtscale_instance_firewall` on the smallest size: rules applied, one rule added in place, import |
| `TestLiveInstanceAttributesProof` | `virtscale_instance` on the smallest size, created with `user_data` and `disable_api_termination = true`: the destroy is refused while protected, the protection is lifted in place, the destroy works. With `VIRTSCALE_LIVE_RESIZE=1` a resize 1/1024 to 1/2048 happens in place too (the instance restarts) between the lift and the destroy. Needs cloud-init (`user_data`) and protection switched on for the staging org. **The resize is opt-in**: staging is a fenced deployment (`settings.compute_os_fenced`: a fixed table of flavors) and refuses every resize by design, so the in-place resize is proven by the fake-API acceptance tests and, live, by a non-fenced region with `VIRTSCALE_LIVE_RESIZE=1`. The test removes its own instance by name when it ends, whatever step failed |

Each has a twin against the fake (`TestAcc...AgainstTheFake`) that proves the test code in CI. A test is skipped
unless `TF_ACC=1`, `VIRTSCALE_LIVE=1`, `VIRTSCALE_API_KEY`, `VIRTSCALE_ORGANIZATION` and `VIRTSCALE_ENDPOINT` are all
set. It creates only objects named `tf-live-<random>` and deletes them again; the instance tests also remove their instance when a step fails, and `TestLiveSweepLeftovers` removes what a killed run left. The API key needs the `write` scope
(projects), `network:write` and `compute:write` (VPCs, instances, firewalls) and `storage:write` or `write` (S3). The
place defaults to staging's (`VIRTSCALE_LIVE_REGION=stg`, `VIRTSCALE_LIVE_STORAGE_POOL=stg-local`,
`VIRTSCALE_LIVE_NETWORK_PROFILE=stg-tenant`; set them to run elsewhere). The staging API is cluster-internal, so run
it from somewhere that reaches the endpoint. Staging has room for one small server at a time. A soft-deleted bucket is
not purged while the platform's purge worker is off: it keeps its name and one of the organization's retention rows
(twice its bucket limit in all), so do not loop the S3 test. Allow a long run: the waits for a server and a firewall are
minutes (`-timeout 60m` below).

```bash
TF_ACC=1 \
TF_ACC_TERRAFORM_PATH=$(command -v tofu) \
TF_ACC_PROVIDER_NAMESPACE=virtscale \
TF_ACC_PROVIDER_HOST=registry.terraform.io \
VIRTSCALE_LIVE=1 VIRTSCALE_API_KEY=... VIRTSCALE_ORGANIZATION=... VIRTSCALE_ENDPOINT=... \
go test ./internal/provider -run TestLive -count=1 -v -timeout 60m
```

A run counts only when `-v` shows `--- PASS` for the test (a skip prints `ok` too). The tests refuse the production endpoint unless `VIRTSCALE_LIVE_ALLOW_PRODUCTION=1`, and fail at once, instead of
skipping, when a live run is requested and `tofu` cannot be found.

In CI the job `provider-staging-live` does this for you, without anyone holding the key. It exists only in a pipeline
started through the GitLab API on `main` with the variable `PROVIDER_STAGING_LIVE=1`; a push, a merge request or a web
click never runs it. The job logs in to OpenBao with the JWT role `provider-staging-live` (this project, protected
`main`, API pipelines), reads the staging test org's key, and runs the tests above one after another (a failed one does
not stop the next, so a run shows which families work; the S3 one only when the pipeline is started with
`PROVIDER_STAGING_LIVE_S3=1`) against `http://s3-staging-customer-api.s3-staging.svc.cluster.local`, and revokes the
token. The first test it runs is `TestLiveSweepLeftovers`, which removes leftover `tf-live-*` instances. It then refuses
to start if the org still holds `tf-live-*` objects (SSH keys, projects, instances, buckets that are not soft-deleted,
VPCs), withholds a test's log if it contains the key (checked after every test, with the artifact
updated each time), fails unless the log shows `--- PASS` for every test, and counts again afterwards: the run must leave
nothing behind. Each test has 30 minutes (`-timeout 30m`) and the job 90 (the sum of the ceilings is larger, but a run
that needs more than 90 minutes in all is stuck). A job that is killed can leave `tf-live-*` objects behind: instances are removed by the sweeper at the start of the next
run; for any other kind the run refuses to start and says how many of which kind, so delete them (VPCs first, then
projects, then SSH keys; buckets are only soft-deleted) before running again. The lists are read as one page (the API's default: 50 buckets
or VPCs, 100 instances; SSH keys and projects are not paged), which is plenty for a test org that holds nothing else of
that name. The part of the script that counts is tested in CI against a
local API (`internal/provider/live_script_test.go`). The script re-executes itself under `env -i` with a short allowlist of
variables, so nothing a trigger sets (proxy, Go, curl, apt, `PATH`, ...) reaches it; it uses its own pinned and
checksum-verified `tofu`, the public OpenBao CA in `ci/bao-ca.pem`, and runs without a cache. Only users with the
Maintainer role can start a pipeline with variables in this project.

```bash
printf 'header = "PRIVATE-TOKEN: %s"\n' "$GITLAB_TOKEN" | curl -sS -K - -X POST -H "Content-Type: application/json" \
  -d '{"ref":"main","variables":[{"key":"PROVIDER_STAGING_LIVE","value":"1"}]}' \
  "$GITLAB_URL/api/v4/projects/225/pipeline"
```

## Links

- API reference: <https://learn.virtscale.nl>
- Releasing: [RELEASING.md](RELEASING.md)

## License

License: MPL-2.0 (see [LICENSE](LICENSE)).

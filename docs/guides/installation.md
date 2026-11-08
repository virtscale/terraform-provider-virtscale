---
page_title: "Installation - terraform-provider-virtscale"
subcategory: ""
description: |-
  Install the virtscale provider with Terraform or OpenTofu, pin and lock its version, verify the release signature, or build it locally.
---

# Installation

The VirtScale provider is published as `virtscale/virtscale` on [registry.terraform.io](https://registry.terraform.io/providers/virtscale/virtscale). Terraform and OpenTofu both download it from there when you run `init`; there is nothing to build or copy by hand. registry.opentofu.org does not list it yet, so write the source with the registry host, as below.

## Requirements

- Terraform 1.0 or newer, or OpenTofu 1.0 or newer: the provider speaks protocol 6. It is tested with OpenTofu 1.12.5; Terraform 1.x speaks the same protocol but is not part of the provider's tests.
- Network access from the machine that runs `init` to the registry and to github.com, where the release files are kept.
- For `plan` and `apply`, but not for `init`: an API key and your organization's id. See the Authentication guide.

## Install from the registry

Declare the provider in `required_providers` and run `init`. Terraform and OpenTofu use the same block, with the full source address `registry.terraform.io/virtscale/virtscale`.

Write the registry host in the source. A short source, `virtscale/virtscale`, means the default registry of the CLI, and for OpenTofu that is registry.opentofu.org, which answers 404 until the provider is added there (the requests are opentofu/registry #6388 and #6396): `tofu init` then stops with `provider registry registry.opentofu.org does not have a provider named registry.opentofu.org/virtscale/virtscale`. With the host written out, `tofu init` fetches the provider from registry.terraform.io, and Terraform does the same. If you only use the Terraform CLI you may shorten the source to `virtscale/virtscale`; this guide and the other pages keep the long form so that one configuration works with both CLIs.

```terraform
terraform {
  required_providers {
    virtscale = {
      source  = "registry.terraform.io/virtscale/virtscale"
      version = "~> 0.1"
    }
  }
}

provider "virtscale" {}
```

```shell
# Terraform
terraform init

# OpenTofu
tofu init
```

`init` downloads the release for your platform, checks its checksum against the signed checksum file, and writes the lock file `.terraform.lock.hcl`. The line it prints for the provider names the key that signed the release: `E06A22C6849F6314`, the last 16 characters of the fingerprint [below](#verify-the-release-signature).

The block holds no credentials: the API key and the organization come from the environment (see the Authentication guide).

## Pin a version and keep the lock file

`version` is a constraint, not a single release:

| Constraint | Accepts |
| --- | --- |
| `"~> 0.1"` | 0.1.0 and every later 0.x release, never 1.0 |
| `"~> 0.1.0"` | patch releases of 0.1 only |
| `"0.1.0"` | exactly that release |
| `">= 0.1.0, < 0.3.0"` | a range you choose |

Before 1.0, semantic versioning allows a minor release to change behavior, so `"~> 0.1.0"` is the safer choice for production. Raise it on purpose and run `plan` before `apply`.

`init` records the release it installed, and the checksums of its files, in `.terraform.lock.hcl` next to your configuration. Commit that file: it makes every machine and every CI run use the same release, and refuse a file whose checksum does not match.

```hcl
provider "registry.terraform.io/virtscale/virtscale" {
  version     = "0.1.0"
  constraints = "~> 0.1"
  hashes = [
    "h1:...",
    "zh:...",
  ]
}
```

With the full source address both CLIs record the provider under `registry.terraform.io`, so one lock file serves both.

```shell
# Move to the newest release the constraint allows, and rewrite the lock file.
tofu init -upgrade

# Add the checksums of other platforms, so that a lock file made on a laptop also passes on the CI runner.
tofu providers lock -platform=linux_amd64 -platform=linux_arm64 -platform=darwin_arm64 -platform=windows_amd64
```

Releases are built for Linux, FreeBSD, Windows and macOS (darwin), on amd64 and arm64, and for 386 and arm where the system supports them: 13 zip files in all.

## Verify the release signature

`init` already does this: each release has a checksum file (`terraform-provider-virtscale_<version>_SHA256SUMS`) signed with VirtScale's release key, and both CLIs refuse a release whose signature does not verify against a key the registry lists for the `virtscale` namespace. The key that signs VirtScale releases:

```
VirtScale Release Signing <noreply@virtscale.nl>
4505 CC01 E081 7FF4 F56A CC48 E06A 22C6 849F 6314
```

To check a release by hand, for an audit or before you put it in a mirror, you need `gpg`, `curl` and `jq`. The registry answers with the files of one platform and the public key it lists; the commands below use a throwaway keyring, so nothing is added to yours:

```shell
VERSION=0.1.0
REGISTRY=registry.terraform.io   # registry.opentofu.org answers 404 for this provider until it is added there
mkdir virtscale-verify && cd virtscale-verify

curl -sSf "https://$REGISTRY/v1/providers/virtscale/virtscale/$VERSION/download/linux/amd64" -o download.json
fetch() { curl -sSfL -o "$1" "$(jq -r "$2" download.json)"; }
fetch "terraform-provider-virtscale_${VERSION}_SHA256SUMS" .shasums_url
fetch "terraform-provider-virtscale_${VERSION}_SHA256SUMS.sig" .shasums_signature_url
fetch "terraform-provider-virtscale_${VERSION}_linux_amd64.zip" .download_url
jq -r '.signing_keys.gpg_public_keys[].ascii_armor' download.json > signing-key.asc

export GNUPGHOME="$(mktemp -d)"
gpg --import signing-key.asc
gpg --verify "terraform-provider-virtscale_${VERSION}_SHA256SUMS.sig" "terraform-provider-virtscale_${VERSION}_SHA256SUMS"
sha256sum --check --ignore-missing "terraform-provider-virtscale_${VERSION}_SHA256SUMS"
```

What to look for:

- `gpg --verify` says `Good signature from "VirtScale Release Signing <noreply@virtscale.nl>"` and prints `Primary key fingerprint:` with the fingerprint above (gpg puts a wider gap in the middle). Compare the fingerprint, not the name: that is what ties the key to VirtScale. gpg also warns that the key is not certified with a trusted signature; that only means you have not signed the key yourself.
- `sha256sum` says `terraform-provider-virtscale_<version>_linux_amd64.zip: OK`. The checksum file lists every platform's zip, and `--ignore-missing` skips the ones you did not download.
- If the registry lists more than one key, all of them are imported; the fingerprint on the `Primary key fingerprint` line tells you which one signed.

If VirtScale replaces the signing key, this page lists the new fingerprint. The registries keep the old public key, so releases signed with it still verify.

## Build the provider locally

For contributors, and for trying a change that is not released yet. You need Go (the version `go.mod` names) and the source from [github.com/virtscale/terraform-provider-virtscale](https://github.com/virtscale/terraform-provider-virtscale).

```shell
git clone https://github.com/virtscale/terraform-provider-virtscale.git
cd terraform-provider-virtscale
go install .
```

`go install` puts `terraform-provider-virtscale` in `$(go env GOBIN)`, or in `$(go env GOPATH)/bin` when `GOBIN` is empty. Tell the CLI to use that directory instead of the registry with `dev_overrides` in a CLI configuration file, for example `~/.tfrc.d/dev.tfrc`:

```hcl
provider_installation {
  dev_overrides {
    "registry.opentofu.org/virtscale/virtscale" = "/home/you/go/bin"
    "registry.terraform.io/virtscale/virtscale" = "/home/you/go/bin"
  }
  direct {}
}
```

A configuration with the full source address needs only the `registry.terraform.io` line. List both hostnames, pointing at the same directory, when some configuration uses the short source `virtscale/virtscale`: OpenTofu resolves that against registry.opentofu.org and Terraform against registry.terraform.io, and a block that names only one fails with `Inconsistent dependency lock file` when you use the other CLI. `direct {}` leaves every other provider on the registry.

Point the CLI at the file and run your configuration; the `examples/complete` directory of the repository is a complete one:

```shell
export TF_CLI_CONFIG_FILE="$HOME/.tfrc.d/dev.tfrc"
export VIRTSCALE_API_KEY=...         # see the Authentication guide
export VIRTSCALE_ORGANIZATION=...
cd examples/complete
tofu plan
```

With the override in effect:

- `init` is skipped for this provider, and every command warns `Provider development overrides are in effect`.
- The `version` constraint is ignored, and the build reports its version as `dev`.
- Rebuild with `go install .` after a change; there is nothing to reinstall.

Unset `TF_CLI_CONFIG_FILE` to go back to the release from the registry.

## Next

The Authentication guide creates the API key the provider needs, and the Getting started guide builds a first VM with it.

---
page_title: "Authentication - terraform-provider-virtscale"
subcategory: ""
description: |-
  Create an organization API key in the customer panel, choose its scopes, give it to the provider through the environment, and rotate it.
---

# Authentication

The provider works inside one VirtScale organization and proves who it is with an API key of that organization. Both are given through the environment, so that no key is ever written into a `.tf` file.

## What the provider needs

| Setting | Environment variable | Provider attribute | Where it comes from |
| --- | --- | --- | --- |
| API key | `VIRTSCALE_API_KEY` | `api_key` | Created in the customer panel (below) |
| Organization | `VIRTSCALE_ORGANIZATION` | `organization` | The organization's id, a UUID (below) |
| API endpoint | `VIRTSCALE_ENDPOINT` | `endpoint` | Defaults to `https://api.virtscale.nl`; set it only for another deployment of the API |

An attribute in the configuration wins over its environment variable. A missing key or organization stops the run with a message that names the attribute and the variable, before any request is made.

## Create an API key

An API key is created for the organization, not for a person's login: it expires on the date you choose and can be revoked without touching anyone's password.

1. Sign in to the customer panel at [panel.virtscale.nl](https://panel.virtscale.nl). You need the owner or admin role; with another role the panel answers `This account's role cannot manage API keys.`
2. Open **Settings**. On the **API keys** card, choose **Create API key**.
3. **Name** the key after what will use it, for example `terraform-ci`. The description is optional.
4. Under **Access**, choose what the key may do (see the scopes below). The dialog starts on **Read-only**: for Terraform that can create things, choose **Full access** or **Custom**.
5. Under **Expiry**, keep **90 days**, the default, or choose **On a date**. **Never** is possible, but then you have to notice a leaked key yourself.
6. Under **Source**, choose where the key may be used from (see "Limit where a key works" below). **Default** is shown only for a key that can write, and it limits the key to the address you create it from. For CI or any other machine than the one you are on, choose **Only these networks** and list one IPv4 or IPv6 CIDR per line, or **Any address**.
7. Choose **Create key**. The dialog **API key created** shows the secret once: copy it into your secret store now. It cannot be shown again; if it is lost, create a new key and revoke the old one.

Creating a key can ask for a recent sign-in with a second factor (`Creating an API key needs a recent sign-in with a second factor.`); sign in again and repeat. A key can never create or revoke keys or manage members, so Terraform cannot mint keys: that stays with people.

## Choose the scopes

A key carries scopes; the API refuses what they do not cover with `403 permission_denied`. The panel's **Access** choices set them: **Read-only** is the scope `read`, **Full access** is `write` (everything a key can do except managing members and keys), and **Custom** lets you pick scopes one by one. A key cannot hold more than the role of the person who creates it.

| You manage | Scope to pick under **Custom** |
| --- | --- |
| `virtscale_instance`, `virtscale_ssh_key`, and the data sources `virtscale_images`, `virtscale_regions`, `virtscale_instance_types`, `virtscale_plans`, `virtscale_project` | Manage compute (`compute:write`) |
| `virtscale_public_ip`, `virtscale_vpc`, `virtscale_instance_firewall` | Manage network (`network:write`) |
| `virtscale_s3_bucket`, `virtscale_s3_access_key` | Manage storage (`storage:write`) |
| `virtscale_project` (create, rename, delete) | **Full access** (`write`): `compute:write` only reads projects |
| `plan` and refresh only | View compute (`compute:read`), View network (`network:read`) and View storage (`storage:read`), or **Read-only** |
| `virtscale_caller` | any key |

A configuration with an instance and a public IP needs both Manage compute and Manage network. The billing and audit scopes are not used by the provider. A key restricted to a single project is refused by the provider, which works on the organization's own paths (`403 project_restricted_key`).

## Give the key to the provider

Set both variables in the shell that runs `init`, `plan` and `apply`, or in your CI system's secret variables. The provider block stays empty:

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
# Paste the key and press Enter: it is not echoed and stays out of the shell history.
read -rs VIRTSCALE_API_KEY && export VIRTSCALE_API_KEY

# The organization id is not a secret. The API tells it to the key that belongs to it:
export VIRTSCALE_ORGANIZATION="$(curl -sSf -H "Authorization: Bearer $VIRTSCALE_API_KEY" https://api.virtscale.nl/v1/me | jq -r '.organizations[0].org_id')"
```

Check that the key works, and what it may do, with the `virtscale_caller` data source. It reads `GET /v1/me`, and it fails if the credential does not name the organization you configured:

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

data "virtscale_caller" "me" {}

output "organization_id" {
  value = data.virtscale_caller.me.organization_id
}

output "scopes" {
  value = data.virtscale_caller.me.scopes
}

output "api_key_expires_at" {
  value = data.virtscale_caller.me.expires_at # null when the key never expires
}
```

`tofu plan` prints the three outputs. Keep an eye on `api_key_expires_at`: a key that has expired answers `401`.

## Keep the key out of your files

- Do not write the key as `api_key = "..."` in a `.tf` file, a `.tfvars` file or a module call: it would end up in version control and in every copy of the repository.
- If your tooling can only pass inputs and not environment variables, give the provider a sensitive variable and set it with `TF_VAR_virtscale_api_key`:

```terraform
terraform {
  required_providers {
    virtscale = {
      source  = "registry.terraform.io/virtscale/virtscale"
      version = "~> 0.1"
    }
  }
}

variable "virtscale_api_key" {
  type      = string
  sensitive = true
}

provider "virtscale" {
  api_key = var.virtscale_api_key
}
```

- The environment variable is the safer way. A saved plan (`plan -out`) holds the values of input variables in plain text, so a key passed as a variable lands in the plan file; a key passed as `VIRTSCALE_API_KEY` does not. The provider does not store `api_key` in the state either way.
- Error messages of the provider replace your key with `[redacted]`, in case a proxy in front of the API echoes the request. Still, do not paste debug output into a ticket without reading it first.
- The state does hold the secrets of the resources you create, for example the secret of a `virtscale_s3_access_key`. See the State, secrets and import guide for what is sensitive and how to protect the state.

## Limit where a key works

A key can be limited to the addresses it is used from; a request from any other address is refused. Under **Source** the panel offers:

| Choice | What it does |
| --- | --- |
| **Default** | Shown only for a key that can write (**Full access**, or **Custom** with any scope ending in `:write`). The key is pinned to the address you create it from: one address (`/32`), or a `/64` network for IPv6. It is the address the edge sees, so behind a NAT it is the NAT's public address. If VirtScale cannot tell the address, the panel asks you to choose networks or any address instead. |
| **Only these networks** | One IPv4 or IPv6 CIDR per line, at most 20. |
| **Any address** | No limit. For a key that can write, the panel warns that changes can be made from any address. |

A key that only reads has no **Default**: it works from any address unless you list networks. The key's **Source** is shown once it is created, and in the list of keys.

This matters for Terraform. A key that can write and was created with **Default** works only from the address you were on when you created it: run from a CI runner or another machine, Terraform stops with `403 source_address_not_allowed` and the hint that the key's network limits do not include this address. For CI or another machine, create the key with **Only these networks** and the addresses it runs from, or, if those are not fixed, with **Any address**. A key cannot be edited afterwards: to change its source, create a new key and revoke the old one (see below).

## Rotate a key

Rotate before the expiry date, and at once if a key may have leaked. The old key keeps working until you revoke it, so nothing stops in between.

1. In **Settings**, **API keys**, create a new key with the same access.
2. Put it where the old one was: the secret store, the CI variable, your shell.
3. Run `tofu plan`. It should show no changes. `virtscale_caller` shows `api_key_id` of the key in use, so you can see that the new one is the one answering.
4. Revoke the old key: choose **Revoke** on its row and type its name to confirm. Anything still using it loses access at once, and a revoked key cannot be restored.

If a key leaked, revoke it first and create the replacement afterwards.

## When a request is refused

| What you see | What it means |
| --- | --- |
| `Missing API Key`, `Missing Organization` | The variable is not set in the shell that runs the command. |
| `401 unauthorized` | The key is wrong, revoked or expired; the API does not say which. Check `VIRTSCALE_API_KEY`. |
| `403 permission_denied` | The key's scopes do not cover the call. The message names the permission, and the provider adds the scope that grants it, for example `the API key needs the 'compute:write' scope`. |
| `403 source_address_not_allowed` | The key is limited to networks and you are not on one of them. |
| `404 not_found` with `no organization matching that identifier` | `VIRTSCALE_ORGANIZATION` is wrong, or the key belongs to another organization. |
| `400 invalid_request` with `org: Input should be a valid UUID` | `VIRTSCALE_ORGANIZATION` is not a UUID: use the id, not the organization's name. |

Every error carries the API's own message and a request id: quote the request id when you ask VirtScale for help.

## Next

The Getting started guide builds a first VM with this key.

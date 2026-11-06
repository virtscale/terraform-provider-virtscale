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

See the [guide on state, secrets and import](guides/state-secrets-and-import.md).

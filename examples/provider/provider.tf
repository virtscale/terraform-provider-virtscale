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

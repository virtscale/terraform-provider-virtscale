---
page_title: "virtscale_images Data Source - terraform-provider-virtscale"
subcategory: ""
description: |-
  The catalogue of images available to boot a virtscale_instance from, in a given region.
---

# virtscale_images (Data Source)

The catalogue of images available to boot a virtscale_instance from, in a given region.

## Example Usage

```terraform
data "virtscale_images" "gro" {
  region = "gro"
}

output "image_names" {
  value = [for i in data.virtscale_images.gro.images : i.name]
}
```

## Schema

### Required

- `region` (String) The region id to list images for.

### Read-Only

- `id` (String) Set to region.
- `images` (Attributes List) The images available in region, ordered by name.

### Nested Schema for `images`

#### Read-Only

- `id` (String) The image id, for use as a virtscale_instance's image attribute.
- `min_disk_gb` (Number) The minimum boot disk size, in gigabytes, this image requires. 0 today for every image: not a guide to choosing a size.
- `min_ram_mb` (Number) The minimum memory, in megabytes, this image requires. 0 today for every image: not a guide to choosing a size.
- `name` (String) The image's display name.
- `os_distro` (String) The OS distribution, e.g. debian. Null when unknown.
- `os_version` (String) The OS version, e.g. 12. Null when unknown.


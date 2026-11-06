---
page_title: "virtscale_s3_bucket Resource - terraform-provider-virtscale"
subcategory: ""
description: |-
  An object-storage bucket. It is created, resized and deleted asynchronously: apply waits until the bucket is active. Deleting a bucket is a soft delete: it is cut off at once, its data is kept for a retention window (7 days) and then purged, and the organization's bucket slot is freed. Needs an API key with the storage:write (or write) scope.
---

# virtscale_s3_bucket (Resource)

An object-storage bucket. It is created, resized and deleted asynchronously: apply waits until the bucket is active. Deleting a bucket is a soft delete: it is cut off at once, its data is kept for a retention window (7 days) and then purged, and the organization's bucket slot is freed. Needs an API key with the storage:write (or write) scope.

## Example Usage

```terraform
resource "virtscale_s3_bucket" "logs" {
  name           = "example-logs"
  versioning     = true
  max_size_bytes = 107374182400 # 100 GiB
  max_objects    = 1000000
}
```

## Schema

### Required

- `name` (String) The bucket name, unique within the organization: 3 to 63 lower-case letters, digits and hyphens, starting and ending with a letter or digit. Changing it replaces the bucket. **Forces replacement.**

### Optional

- `force_destroy` (Boolean) Acknowledge, when the bucket is destroyed, that it may still hold objects (the API refuses a non-empty bucket with 409 bucket_not_empty otherwise). The provider's own: not stored by the API, so import leaves it false.
- `max_objects` (Number) The most objects this bucket may hold. Leave it out for no per-bucket cap. Changes in place.
- `max_size_bytes` (Number) The most bytes this bucket may hold, inside the organization's ceiling. Leave it out for no per-bucket cap (the organization's ceiling still applies). Changes in place.
- `versioning` (Boolean) Keep previous versions of overwritten and deleted objects. Fixed when the bucket is created: changing it is refused at plan time, because replacing the bucket would soft-delete it and its name stays taken for the retention window.

### Read-Only

- `created_at` (String) When the bucket was created, RFC 3339.
- `id` (String) The bucket's id (a UUID), assigned by the API.
- `state` (String) requested, provisioning, active, provision_failed, suspended, degraded, or deleting_soft once deleted.

## Import

Import is supported using the following syntax:

```shell
tofu import virtscale_s3_bucket.logs <bucket id>
```

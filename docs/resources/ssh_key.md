---
page_title: "virtscale_ssh_key Resource - terraform-provider-virtscale"
subcategory: ""
description: |-
  An SSH public key imported into the organization, which instances take by name (virtscale_instance.ssh_keys). Only the public key is stored. cloud-init installs it for the image's default user when an instance first boots, and an instance keeps the keys it was created with when a key is deleted or replaced later.
---

# virtscale_ssh_key (Resource)

An SSH public key imported into the organization, which instances take by name (virtscale_instance.ssh_keys). Only the public key is stored. cloud-init installs it for the image's default user when an instance first boots, and an instance keeps the keys it was created with when a key is deleted or replaced later.

## Example Usage

```terraform
resource "virtscale_ssh_key" "laptop" {
  name       = "laptop"
  public_key = file(var.public_key_path)
}

variable "public_key_path" {
  type        = string
  description = "The OpenSSH public key to import."
  default     = "~/.ssh/id_ed25519.pub"
}
```

## Schema

### Required

- `name` (String) The key's name, unique within the organization and case-sensitive: 1 to 64 characters from letters, digits, '.', '_', '@' and '-', starting with a letter or digit. Instances name the key by this. Changing it replaces the key. **Forces replacement.**
- `public_key` (String) The OpenSSH public key: one line, "<type> <base64> [comment]", for example file("~/.ssh/id_ed25519.pub"). Types: ssh-ed25519, ecdsa-sha2-nistp256, ecdsa-sha2-nistp384, ecdsa-sha2-nistp521, and ssh-rsa of 2048 to 16384 bits. The API stores the key without its comment. Two texts are the same key when the key type and the key bytes are the same, so a comment, surrounding whitespace, a trailing newline or CRLF never replace the resource. They do show up as an in-place change of public_key: editing only the comment does, and so does the first plan after an import, where the state holds the API's comment-less text and the configuration holds the file. Applying it stores your text and makes no API call. A different key replaces the resource. **Forces replacement.**

### Read-Only

- `created_at` (String) When the key was imported, RFC 3339.
- `fingerprint` (String) The key's SHA-256 fingerprint as ssh-keygen -lf prints it: SHA256: and the unpadded base64 of the SHA-256 of the key.
- `id` (String) The key's id (a UUID), assigned by the API.
- `key_type` (String) The key type the API recognised, for example ssh-ed25519.

## Import

Import is supported using the following syntax:

```shell
tofu import virtscale_ssh_key.laptop <ssh key id>
```

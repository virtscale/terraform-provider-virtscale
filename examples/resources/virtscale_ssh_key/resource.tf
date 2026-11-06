resource "virtscale_ssh_key" "laptop" {
  name       = "laptop"
  public_key = file(var.public_key_path)
}

variable "public_key_path" {
  type        = string
  description = "The OpenSSH public key to import."
  default     = "~/.ssh/id_ed25519.pub"
}

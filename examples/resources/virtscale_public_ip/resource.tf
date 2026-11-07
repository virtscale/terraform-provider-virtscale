resource "virtscale_public_ip" "web" {
  region      = "gro"
  instance_id = var.instance_id # leave it out to allocate an address that is not associated yet
}

variable "instance_id" {
  type        = string
  description = "The instance to associate the address with."
}

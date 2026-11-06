resource "virtscale_instance_firewall" "web" {
  instance_id = var.instance_id

  rule {
    direction   = "ingress"
    protocol    = "tcp"
    port_min    = 22
    port_max    = 22
    remote_cidr = "203.0.113.0/24"
    description = "ssh from the office"
  }

  rule {
    direction   = "ingress"
    protocol    = "tcp"
    port_min    = 443
    port_max    = 443
    remote_cidr = "0.0.0.0/0"
  }

  rule {
    direction   = "ingress"
    protocol    = "icmp"
    remote_cidr = "0.0.0.0/0"
  }
}

variable "instance_id" {
  type        = string
  description = "The instance whose rules these are."
}

data "virtscale_instance_types" "gro" {
  region = "gro" # leave it out for every type
}

output "instance_types" {
  value = [for t in data.virtscale_instance_types.gro.instance_types : "${t.id}: ${t.vcpus} vCPU, ${t.memory_mb} MB"]
}

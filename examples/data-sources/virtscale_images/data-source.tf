data "virtscale_images" "gro" {
  region = "gro"
}

output "image_names" {
  value = [for i in data.virtscale_images.gro.images : i.name]
}

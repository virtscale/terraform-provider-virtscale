# Without a name: the organization's default project.
data "virtscale_project" "default" {}

data "virtscale_project" "web" {
  name = "web"
}

output "project_ids" {
  value = [data.virtscale_project.default.id, data.virtscale_project.web.id]
}

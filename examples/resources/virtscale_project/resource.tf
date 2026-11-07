resource "virtscale_project" "web" {
  name        = "web"
  description = "The web tier"
  labels = {
    team = "platform"
    env  = "production"
  }
}

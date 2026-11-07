// Command terraform-provider-virtscale is the virtscale Terraform/OpenTofu
// provider binary.
package main

//go:generate go run ./tools/gendocs

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/provider"
)

// version is set at build/release time via -ldflags; "dev" locally.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/virtscale/virtscale",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}

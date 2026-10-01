// Command terraform-provider-nelm is a Terraform provider for Nelm
// (https://github.com/werf/nelm) release management.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
	"github.com/infrabay/terraform-provider-nelm/internal/provider"
)

// version is set via -ldflags during release builds; "dev" for local builds.
var version = "dev"

func main() {
	var debug bool

	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers like delve")
	flag.Parse()

	opts := providerserver.ServeOpts{
		Address: "registry.terraform.io/infrabay/nelm",
		Debug:   debug,
	}

	err := providerserver.Serve(context.Background(), provider.New(version), opts)

	// Serve returns once Terraform has shut the provider down: remove the
	// per-process nelm temp root before exiting (log.Fatal skips defers).
	nelmclient.Shutdown()

	if err != nil {
		log.Fatal(err.Error())
	}
}

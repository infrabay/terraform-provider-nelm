terraform {
  required_providers {
    nelm = {
      source = "infrabay/nelm"
    }
  }
}

# This example is meant to be run via dev_overrides (see docs/DEVELOPMENT.md);
# `terraform init` is intentionally never run against it.
provider "nelm" {
  kube_context = "orbstack"
}

resource "nelm_release" "basic" {
  name      = "basic-example"
  namespace = "tf-nelm-basic-example"

  # ${path.module} makes this absolute regardless of the working directory
  # Terraform is invoked from, so it survives dev_overrides workflows just as
  # well as a plain relative path like "../../testdata/charts/basic" would
  # when run from this directory.
  chart = "${path.module}/../../testdata/charts/basic"

  values = [
    <<-YAML
    replicaCount: 1
    configMap:
      message: "hello from examples/basic"
    YAML
  ]
}

output "release_id" {
  value = nelm_release.basic.id
}

output "release_status" {
  value = nelm_release.basic.status
}

output "release_resources" {
  value = nelm_release.basic.resources
}

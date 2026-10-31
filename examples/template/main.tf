terraform {
  required_providers {
    nelm = {
      source = "infrabay/nelm"
    }
  }
}

# This example is meant to be run via dev_overrides (see DEVELOPMENT.md);
# `terraform init` is intentionally never run against it.
#
# The provider block names no cluster: an offline render never contacts one,
# so `terraform plan` works on any machine. A nelm_release in this
# configuration would fail with "No Kubernetes connection configured".
provider "nelm" {}

data "nelm_template" "template" {
  name      = "template-example"
  namespace = "tf-nelm-template-example"

  # ${path.module} makes this absolute regardless of the working directory
  # Terraform is invoked from.
  chart = "${path.module}/../../testdata/charts/template"

  values = [
    <<-YAML
    message: "hello from examples/template"
    YAML
  ]

  # The chart's Secret renders this value. It is masked in the plan's view of
  # this argument only: manifest, manifests and crds carry it in cleartext.
  set_sensitive = [
    { name = "secret.password", value = "example-only" },
  ]

  # What the templates see as .Capabilities in an offline render.
  kube_version = "1.31.0"
  api_versions = ["example.com/v1"]

  # Leave the chart's helm test Pod out of manifest and manifests.
  skip_tests = true
}

output "template_id" {
  value = data.nelm_template.template.id
}

output "template_files" {
  value = keys(data.nelm_template.template.manifests)
}

# The chart's ConfigMap template writes no namespace; the rendered object
# carries metadata.namespace = "tf-nelm-template-example" all the same, the
# namespace a nelm_release of this chart would create it in.
output "template_configmap" {
  value = data.nelm_template.template.manifests["templates/configmap.yaml"]
}

# manifest holds the rendered Secret: mark any output of it sensitive.
output "template_manifest" {
  value     = data.nelm_template.template.manifest
  sensitive = true
}

output "template_crds" {
  value = data.nelm_template.template.crds
}

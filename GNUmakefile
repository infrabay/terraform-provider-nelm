BINARY_NAME := terraform-provider-nelm
VERSION     := dev

GOBIN := $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN := $(shell go env GOPATH)/bin
endif

# Strictly-local acceptance test guard: TF_ACC=1 opts into
# running acceptance tests at all; NELM_TEST_KUBE_CONTEXT pins the ONLY
# context test code is allowed to touch (default: orbstack; CI overrides it
# to the kind cluster's context). testAccPreCheck (provider_test.go)
# additionally asserts that context exists in ~/.kube/config (the one file
# the provider, kubectl and helm are all pointed at) and its server host is
# 127.0.0.1/::1/localhost — never a cloud endpoint — before any test runs.
NELM_TEST_KUBE_CONTEXT ?= orbstack
.PHONY: build
build:
	go build -o bin/$(BINARY_NAME) -ldflags "-X main.version=$(VERSION)" .

.PHONY: install
install: build
	mkdir -p $(GOBIN)
	cp bin/$(BINARY_NAME) $(GOBIN)/$(BINARY_NAME)

.PHONY: test
test:
	./gates.sh repo

.PHONY: testacc
testacc:
	TF_ACC=1 NELM_TEST_KUBE_CONTEXT=$(NELM_TEST_KUBE_CONTEXT) \
		go test -race -count=1 -timeout 30m ./internal/provider/...

# e2e: manual, dev_overrides workflow against examples/basic — never run
# unattended by an agent or CI. See DEVELOPMENT.md for the full script
# (build -> dev_overrides .tfrc -> plan/apply/import/destroy on orbstack,
# terraform init intentionally skipped).
.PHONY: e2e
e2e: install
	@echo "e2e is a manual workflow — see DEVELOPMENT.md, section 'Dev overrides'."
	@echo "Summary: export TF_CLI_CONFIG_FILE to a dev_overrides .tfrc pointing"
	@echo "at $(GOBIN), then from examples/basic run 'terraform plan|apply|destroy'"
	@echo "(no 'terraform init') against kube_context=\"orbstack\"."

.PHONY: fmt
fmt:
	gofmt -l -w .

.PHONY: clean
clean:
	rm -rf bin/

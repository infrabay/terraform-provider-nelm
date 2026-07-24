BINARY_NAME := terraform-provider-nelm
VERSION     := dev

GOBIN := $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN := $(shell go env GOPATH)/bin
endif

# Strictly-orbstack acceptance test guard (design §6): TF_ACC=1 opts into
# running acceptance tests at all; NELM_TEST_KUBE_CONTEXT pins the ONLY
# context test code is allowed to touch. testAccPreCheck (Phase C,
# provider_test.go) additionally asserts the "orbstack" kubeconfig context
# exists and its cluster.server is 127.0.0.1/localhost — never a remote
# endpoint — before any test runs.
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
	TF_ACC=1 NELM_TEST_KUBE_CONTEXT=orbstack \
		go test -race -count=1 -timeout 30m ./internal/provider/...

# e2e: manual, dev_overrides workflow against examples/basic — never run
# unattended by an agent or CI. See docs/DEVELOPMENT.md for the full script
# (build -> dev_overrides .tfrc -> plan/apply/import/destroy on orbstack,
# terraform init intentionally skipped).
.PHONY: e2e
e2e: install
	@echo "e2e is a manual workflow — see docs/DEVELOPMENT.md 'Manual e2e' section."
	@echo "Summary: export TF_CLI_CONFIG_FILE to a dev_overrides .tfrc pointing"
	@echo "at $(GOBIN), then from examples/basic run 'terraform plan|apply|destroy'"
	@echo "(no 'terraform init') against kube_context=\"orbstack\"."

.PHONY: fmt
fmt:
	gofmt -l -w .

.PHONY: clean
clean:
	rm -rf bin/

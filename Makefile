.PHONY: build test vet tidy

# BUILD_FLAGS passes extra go build flags. Default (prod) build fails closed
# on a missing group_id. For LOCAL DEV ONLY, add the group_id fallback tag:
#   make build BUILD_FLAGS="-tags dev"
# which compiles the config default_group_id fallback (devgroup_dev.go). The
# untagged prod build excludes it entirely — never ship a dev-tagged binary.
#
# BINARY_NAME is honored so the host's github-fetch build (CloneAndBuild runs
# `make build BINARY_NAME=<path>`) can place the binary where Core expects —
# required because main lives in ./cmd/opentalon-agents, not the repo root.
BINARY_NAME ?= bin/opentalon-agents
build:
	go build $(BUILD_FLAGS) -o $(BINARY_NAME) ./cmd/opentalon-agents

test:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy

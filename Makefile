.PHONY: build test vet tidy

# BUILD_FLAGS passes extra go build flags. Default (prod) build fails closed
# on a missing group_id. For LOCAL DEV ONLY, add the group_id fallback tag:
#   make build BUILD_FLAGS="-tags dev"
# which compiles the config default_group_id fallback (devgroup_dev.go). The
# untagged prod build excludes it entirely — never ship a dev-tagged binary.
build:
	go build $(BUILD_FLAGS) -o bin/opentalon-agents ./cmd/opentalon-agents

test:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy

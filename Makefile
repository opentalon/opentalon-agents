.PHONY: build test vet tidy

build:
	go build -o bin/opentalon-agents ./cmd/opentalon-agents

test:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy

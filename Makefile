.PHONY: build test vet
build:
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -o bin/router ./cmd/router

test:
	CGO_ENABLED=0 go test ./...

vet:
	CGO_ENABLED=0 go vet ./...

.PHONY: fmt vet test race build ci e2e

fmt:
	gofmt -w $(shell find cmd internal -name '*.go')

vet:
	go vet ./...

test:
	go test ./...

race:
	go test -race ./...

build:
	go build -o bin/stacklane ./cmd/stacklane

ci: vet race build
	git diff --check
	test -z "$$(gofmt -l cmd internal)"

e2e:
	E2E=1 go test -race -tags=e2e ./internal/app -count=1

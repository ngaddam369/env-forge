BINARY      := forge
MODULE      := github.com/ngaddam369/env-forge
GOTOOLCHAIN := go1.26.1

export GOTOOLCHAIN

.PHONY: build fmt lint test verify clean tidy vet

## build: compile the forge binary
build:
	go build -o bin/$(BINARY) ./cmd/forge

## fmt: format all Go source files in place
fmt:
	gofmt -w .

## vet: run go vet
vet:
	go vet ./...

## lint: run golangci-lint
lint:
	golangci-lint run ./...

## test: run all tests with race detector and show coverage summary
test:
	go test -v -race -count=1 -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | grep -E "^total|^github"

## verify: run the full checklist (fmt → build → vet → lint → test)
verify: fmt build vet lint test
	go mod verify

## clean: remove build artifacts
clean:
	rm -rf bin/ coverage.out

## tidy: tidy and verify go modules
tidy:
	go mod tidy
	go mod verify

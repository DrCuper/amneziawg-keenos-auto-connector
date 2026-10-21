BIN := awgkeen
PKG := ./...

.PHONY: build test lint vet tidy run-help

build:
	go build -o bin/$(BIN) ./cmd/awgkeen

test:
	go test -race -count=1 $(PKG)

vet:
	go vet $(PKG)

lint:
	golangci-lint run $(PKG)

tidy:
	go mod tidy

run-help: build
	./bin/$(BIN) --help

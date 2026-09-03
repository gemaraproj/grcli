.PHONY: build test testcov lint vet fmt fmtcheck tidy tidycheck ci-local clean

BIN     := bin/grcli
PKG     := github.com/gemaraproj/grcli
VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)
LDFLAGS := -X $(PKG)/cmd.version=$(VERSION)

build:
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o $(BIN) .

test:
	go test ./...

testcov:
	go test -coverprofile=coverage.out ./...

lint:
	golangci-lint run ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

fmtcheck:
	@diff=$$(gofmt -s -d .); \
	if [ -n "$$diff" ]; then \
		echo "gofmt diff:"; echo "$$diff"; exit 1; \
	fi

tidy:
	go mod tidy

tidycheck:
	@cp go.mod go.mod.bak; cp go.sum go.sum.bak; \
	go mod tidy; \
	diff=$$(diff go.mod go.mod.bak; diff go.sum go.sum.bak); \
	mv go.mod.bak go.mod; mv go.sum.bak go.sum; \
	if [ -n "$$diff" ]; then echo "go mod tidy would change go.mod/go.sum"; exit 1; fi

ci-local: fmtcheck vet lint tidycheck testcov

clean:
	rm -rf bin coverage.out grcli-out

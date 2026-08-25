BINARY  := rackctl
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/rackctl/rackctl/cmd.Version=$(VERSION)

.PHONY: build test cover vet fmt install clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

test:
	go test -race ./...

# The coverage floors, enforced. `test` proves the suite passes; this proves it still
# covers enough of the tree — and 100% of the functions that decide what gets destroyed.
cover:
	./scripts/coverage.sh

vet:
	go vet ./...

fmt:
	gofmt -w .

install:
	go install -ldflags "$(LDFLAGS)" .

clean:
	rm -f $(BINARY)

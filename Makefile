BINARY  := rackctl
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/rackctl/rackctl/cmd.Version=$(VERSION)

.PHONY: build test cover gates vet fmt install clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

test:
	go test -race ./...

# The coverage floors, enforced. `test` proves the suite passes; this proves it still
# covers enough of the tree — and 100% of the functions that decide what gets destroyed.
cover:
	./scripts/coverage.sh

# Every gate proves it can reject, then runs. A check that cannot fail reports success.
# An absent tool exits 127 with "command not found", which is indistinguishable from a
# gate rejecting the tree — a missing binary would read as a finding. Each tool is asserted
# to be present AND to run before any verdict is taken from it, and a failure here exits 2
# with a sentence naming what to install.
tools:
	@missing=""; \
	for t in shellcheck python3 sh go; do \
	  command -v "$$t" >/dev/null 2>&1 || missing="$$missing $$t"; \
	done; \
	if [ -n "$$missing" ]; then \
	  echo "gates: not run. missing tool(s):$$missing" >&2; \
	  echo "gates: an absent tool exits 127, which would otherwise read as a rejection" >&2; \
	  exit 2; \
	fi; \
	shellcheck --version >/dev/null 2>&1 || { echo "gates: shellcheck is present but does not run" >&2; exit 2; }

gates: tools
	shellcheck scripts/*.sh
	sh ./scripts/install_test.sh
	python3 ./scripts/floor.py
	python3 ./scripts/pins.py
	python3 ./scripts/prose.py

vet:
	go vet ./...

fmt:
	gofmt -w .

install:
	go install -ldflags "$(LDFLAGS)" .

clean:
	rm -f $(BINARY)

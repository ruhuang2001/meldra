APP := meldra
DIST_DIR := dist
COVERAGE_FILE := coverage.out
COVERAGE_MIN := 75.0
# Local builds should identify the source they were built from just like
# release artifacts do. VERSION may be overridden for reproducible builds.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
VERSION_LDFLAGS := -X main.version=$(VERSION)

.DEFAULT_GOAL := check

.PHONY: fmt vet test test-race test-coverage benchmark benchmark-sanity benchmark-swe-bench build check release-check release-snapshot install-hooks clean

fmt:
	gofmt -w $$(go list -f '{{.Dir}}' ./...)

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

test-coverage:
	go test -race -coverprofile=$(COVERAGE_FILE) ./...
	@coverage="$$(go tool cover -func=$(COVERAGE_FILE) | awk '/^total:/ { print $$3 }' | tr -d '%')"; \
		awk -v coverage="$$coverage" -v minimum="$(COVERAGE_MIN)" 'BEGIN { if (coverage < minimum) { printf "coverage %.1f%% is below %.1f%%\n", coverage, minimum; exit 1 }; printf "coverage %.1f%% meets %.1f%% minimum\n", coverage, minimum }'

benchmark:
	go test -run='^$$' -bench=. -benchmem ./...

benchmark-sanity: build
	@if ! command -v sanity >/dev/null 2>&1; then \
		echo "sanity CLI not found. Install it from https://github.com/lemon07r/SanityHarness"; \
		echo "  git clone https://github.com/lemon07r/sanityharness.git"; \
		echo "  cd sanityharness && make build && cp sanity ~/.local/bin/"; \
		exit 1; \
	fi
	@mkdir -p benchmarks/sanityharness/bin
	@cp $(DIST_DIR)/$(APP) benchmarks/sanityharness/bin/$(APP)
	PATH="$(CURDIR)/benchmarks/sanityharness/bin:$${PATH}" sanity --config benchmarks/sanityharness/sanity.toml eval --agent meldra --tier core

benchmark-swe-bench: build
	@if [ ! -d benchmarks/swe-bench/.venv ]; then \
		python3 -m venv benchmarks/swe-bench/.venv; \
		benchmarks/swe-bench/.venv/bin/pip install -r benchmarks/swe-bench/requirements.txt; \
	fi
	PATH="$(CURDIR)/$(DIST_DIR):$${PATH}" benchmarks/swe-bench/.venv/bin/python benchmarks/swe-bench/run.py

build:
	mkdir -p $(DIST_DIR)
	go build -trimpath -ldflags "$(VERSION_LDFLAGS)" -o $(DIST_DIR)/$(APP) .

check:
	test -z "$$(gofmt -l $$(go list -f '{{.Dir}}' ./...))"
	go vet ./...
	go mod tidy -diff
	$(MAKE) test-coverage
	go build -trimpath -ldflags "$(VERSION_LDFLAGS)" -o /dev/null .

release-check: check
	go run github.com/goreleaser/goreleaser/v2@v2.14.0 check

release-snapshot:
	go run github.com/goreleaser/goreleaser/v2@v2.14.0 release --snapshot --clean

install-hooks:
	git config core.hooksPath .githooks
	@printf 'Git hooks enabled from .githooks\n'

clean:
	rm -rf $(DIST_DIR) $(COVERAGE_FILE)

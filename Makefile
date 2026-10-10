APP := meldra
DIST_DIR := dist
CHECK_DIR := .artifacts/checks
COVERAGE_FILE := coverage.out
COVERAGE_MIN := 75.0
# Local builds should identify the source they were built from just like
# release artifacts do. VERSION may be overridden for reproducible builds.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
VERSION_LDFLAGS := -X main.version=$(VERSION)

.DEFAULT_GOAL := check

.PHONY: fmt vet test test-race test-coverage build check check-ci check-staged check-local-ci release-check release-snapshot install-hooks clean

fmt:
	gofmt -w $$(go list -f '{{.Dir}}' ./...)

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

test-coverage:
	python3 scripts/mcp-check.py --coverage "$(COVERAGE_FILE)" --output-dir "$(CHECK_DIR)"
	@coverage="$$(go tool cover -func=$(COVERAGE_FILE) | awk '/^total:/ { print $$3 }' | tr -d '%')"; \
		awk -v coverage="$$coverage" -v minimum="$(COVERAGE_MIN)" 'BEGIN { if (coverage < minimum) { printf "coverage %.1f%% is below %.1f%%\n", coverage, minimum; exit 1 }; printf "coverage %.1f%% meets %.1f%% minimum\n", coverage, minimum }'

build:
	mkdir -p $(DIST_DIR)
	go build -trimpath -ldflags "$(VERSION_LDFLAGS)" -o $(DIST_DIR)/$(APP) .

check:
	test -z "$$(gofmt -l $$(go list -f '{{.Dir}}' ./...))"
	go vet ./...
	go mod tidy -diff
	$(MAKE) test-coverage
	go build -trimpath -ldflags "$(VERSION_LDFLAGS)" -o /dev/null .

check-ci:
	$(MAKE) test-race
	go build -trimpath -ldflags "$(VERSION_LDFLAGS)" -o /dev/null .

check-staged:
	python3 scripts/check-local-ci.py --quick

check-local-ci:
	python3 scripts/check-local-ci.py --staged

release-check: check
	go run github.com/goreleaser/goreleaser/v2@v2.14.0 check

release-snapshot:
	go run github.com/goreleaser/goreleaser/v2@v2.14.0 release --snapshot --clean

install-hooks:
	git config core.hooksPath .githooks
	@printf 'Git hooks enabled from .githooks\n'

clean:
	rm -rf $(DIST_DIR) $(CHECK_DIR) $(COVERAGE_FILE)

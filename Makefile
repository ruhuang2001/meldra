APP := meldra
DIST_DIR := dist
COVERAGE_FILE := coverage.out
COVERAGE_MIN := 75.0
# Local builds should identify the source they were built from just like
# release artifacts do. VERSION may be overridden for reproducible builds.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
VERSION_LDFLAGS := -X main.version=$(VERSION)

.DEFAULT_GOAL := check

.PHONY: fmt vet test test-race test-coverage benchmark benchmark-report benchmark-eval benchmark-report-test benchmark-compare benchmark-sanity benchmark-sanity-baseline benchmark-swe-bench build check release-check release-snapshot install-hooks clean

fmt:
	gofmt -w $$(go list -f '{{.Dir}}' ./...)

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

test-coverage:
	go test -race -coverpkg=./... -coverprofile=$(COVERAGE_FILE) ./...
	@coverage="$$(go tool cover -func=$(COVERAGE_FILE) | awk '/^total:/ { print $$3 }' | tr -d '%')"; \
		awk -v coverage="$$coverage" -v minimum="$(COVERAGE_MIN)" 'BEGIN { if (coverage < minimum) { printf "coverage %.1f%% is below %.1f%%\n", coverage, minimum; exit 1 }; printf "coverage %.1f%% meets %.1f%% minimum\n", coverage, minimum }'

# Raw Go output is compatible with benchstat. JSON reports retain every sample.
BENCH_COUNT ?= 5
BENCH_TIME ?= 200ms
BENCH_CPU ?= 1
BENCH_OUTPUT ?= benchmarks/results/micro.json
EVAL_OUTPUT ?= benchmarks/results/scenarios.json
EVAL_COUNT ?= 3
EVAL_SUITE ?= benchmarks/suites/offline.json
BENCH_BASELINE ?=
BENCH_CANDIDATE ?=

benchmark:
	go test -run='^$$' -bench=. -benchmem -count=$(BENCH_COUNT) -benchtime=$(BENCH_TIME) -cpu=$(BENCH_CPU) ./...

benchmark-report:
	python3 benchmarks/run.py micro --output "$(BENCH_OUTPUT)" --count $(BENCH_COUNT) --benchtime $(BENCH_TIME) --cpu $(BENCH_CPU)

benchmark-eval:
	python3 benchmarks/run.py scenarios --suite "$(EVAL_SUITE)" --output "$(EVAL_OUTPUT)" --count $(EVAL_COUNT) --cpu $(BENCH_CPU)

benchmark-report-test:
	python3 -m unittest discover -s benchmarks -p 'test_*.py'
	python3 -m unittest discover -s benchmarks/live -p 'test_*.py'

benchmark-compare:
	@test -n "$(BENCH_BASELINE)" || (echo "BENCH_BASELINE is required"; exit 1)
	@test -n "$(BENCH_CANDIDATE)" || (echo "BENCH_CANDIDATE is required"; exit 1)
	python3 benchmarks/run.py compare "$(BENCH_BASELINE)" "$(BENCH_CANDIDATE)"

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

# Record one explicit evaluation as a reviewable release baseline. The report
# rejects duplicate task attempts so stale sessions cannot become a score.
SANITY_RESULTS ?= benchmarks/sanityharness/sessions/*/result.json
SANITY_MODEL ?=
SANITY_PROVIDER ?= unknown
SANITY_TIER ?= core
SANITY_COST ?= unknown
SANITY_BASELINE ?=
benchmark-sanity-baseline:
	@test -n "$(SANITY_MODEL)" || (echo "SANITY_MODEL is required"; exit 1)
	@test -n "$(SANITY_BASELINE)" || (echo "SANITY_BASELINE is required"; exit 1)
	go run ./benchmarks/sanityharness/report --model "$(SANITY_MODEL)" --provider "$(SANITY_PROVIDER)" --tier "$(SANITY_TIER)" --estimated-cost '$(value SANITY_COST)' --meldra-version "$(VERSION)" --output "$(SANITY_BASELINE)" $(SANITY_RESULTS)

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

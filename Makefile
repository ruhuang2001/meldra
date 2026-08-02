APP := meldra
DIST_DIR := dist
COVERAGE_FILE := coverage.out
COVERAGE_MIN := 75.0

.DEFAULT_GOAL := check

.PHONY: fmt vet test test-race test-coverage benchmark build check release-check release-snapshot install-hooks clean

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

build:
	mkdir -p $(DIST_DIR)
	go build -trimpath -o $(DIST_DIR)/$(APP) .

check:
	test -z "$$(gofmt -l $$(go list -f '{{.Dir}}' ./...))"
	go vet ./...
	go mod tidy -diff
	$(MAKE) test-coverage
	go build -trimpath -o /dev/null .

release-check: check
	go run github.com/goreleaser/goreleaser/v2@v2.14.0 check

release-snapshot: release-check
	go run github.com/goreleaser/goreleaser/v2@v2.14.0 release --snapshot --clean

install-hooks:
	git config core.hooksPath .githooks
	@printf 'Git hooks enabled from .githooks\n'

clean:
	rm -rf $(DIST_DIR) $(COVERAGE_FILE)

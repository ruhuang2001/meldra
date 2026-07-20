APP := meldra
DIST_DIR := dist

.DEFAULT_GOAL := check

.PHONY: fmt vet test test-race test-integration test-all build check release-check clean

fmt:
	gofmt -w $$(go list -f '{{.Dir}}' ./...)

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

test-integration:
	go test -tags=integration ./...

test-all: test-race test-integration

build:
	mkdir -p $(DIST_DIR)
	go build -trimpath -o $(DIST_DIR)/$(APP) .

check:
	test -z "$$(gofmt -l $$(go list -f '{{.Dir}}' ./...))"
	go vet ./...
	go mod tidy -diff
	go test -race ./...
	go build -trimpath -o /dev/null .

release-check: check
	go run github.com/goreleaser/goreleaser/v2@v2.14.0 check

clean:
	rm -rf $(DIST_DIR)

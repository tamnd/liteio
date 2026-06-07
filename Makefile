# SPDX-License-Identifier: Apache-2.0

.PHONY: all build test race cover lint vet fmt fmtcheck vuln bench clean install

all: fmt vet lint test build

build:
	go build ./...

# install puts the liteio binary on $GOBIN / $GOPATH/bin.
install:
	go install ./cmd/liteio

test:
	go test ./...

# race mirrors the CI gate: data-race detector + atomic coverage.
race:
	go test -race -covermode=atomic -coverprofile=coverage.out ./...

cover: race
	go tool cover -func=coverage.out | tail -1

vet:
	go vet ./...

# lint runs golangci-lint with the repo config (.golangci.yml).
lint:
	golangci-lint run ./...

fmt:
	gofmt -w .

fmtcheck:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; fi

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# bench runs every benchmark once (smoke); raise -benchtime for real numbers.
bench:
	go test -run '^$$' -bench . -benchtime 1x ./...

clean:
	go clean ./...
	rm -f liteio coverage.out coverage.html

.PHONY: build verifier verifier-test test test-short test-race test-integration lint vuln coverage deps e2e check run

build:
	go build -o mytracker .

verifier:
	go run ./cmd/verifygen -out index.html

verifier-test: verifier
	cd e2e && npx playwright test -c verifier.config.js

test:
	go test ./...

test-short:
	go test -short ./...

test-race:
	go test -short -race ./...

test-integration:
	go test ./internal/tsa/ -count=1 -v

lint:
	go vet ./...
	staticcheck ./...
	golangci-lint run ./...

vuln:
	govulncheck ./...

coverage:
	./scripts/check-coverage.sh

deps:
	./scripts/check-deps.sh

e2e:
	npm --prefix e2e test

check: test-race lint vuln coverage deps e2e

run: build
	./mytracker

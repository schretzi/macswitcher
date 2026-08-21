BINARY      := macswitcher
CMD         := ./cmd/macswitcher
GOLANGCI    := golangci-lint
GOSEC       := gosec
GOVULNCHECK := govulncheck

.PHONY: all fmt lint security test build docs pipeline hooks clean

all: pipeline

fmt:
	@echo "==> checking gofmt"
	@files="$$(gofmt -l .)"; \
	if [ -n "$$files" ]; then \
		echo "gofmt needed on:"; echo "$$files"; exit 1; \
	fi

lint:
	@echo "==> golangci-lint"
	$(GOLANGCI) run ./...

security:
	@echo "==> govulncheck"
	$(GOVULNCHECK) ./...
	@echo "==> gosec"
	$(GOSEC) ./...

test:
	@echo "==> go test"
	go test ./...

build:
	@echo "==> go build"
	go build -o $(BINARY) $(CMD)

docs:
	@echo "==> generating cobra docs"
	go run ./tools/gendocs

pipeline: fmt lint security test build

hooks:
	lefthook install

clean:
	rm -f $(BINARY)
	rm -rf dist

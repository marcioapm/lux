.PHONY: build test unit e2e infra lint clean

GO_LDFLAGS := -s -w -X github.com/marcioapm/lux/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BINARIES := luxd lux-runner lux-shim lux lux-fake

# Static binaries: they run inside Fedora hosts, and the shim runs inside
# arbitrary images.
build:
	@mkdir -p bin
	@for b in $(BINARIES); do \
		CGO_ENABLED=0 go build -trimpath -ldflags '$(GO_LDFLAGS)' -o bin/$$b ./cmd/$$b || exit 1; \
	done

unit:
	LUX_TEST_PG=$${LUX_TEST_PG:-postgres://lux:lux@127.0.0.1:55432/postgres?sslmode=disable} go test ./...

e2e:
	cd tests && uv run python run_tests.py

infra:
	cd tests && uv run python run_tests.py --infra-only

test: unit e2e

lint:
	go vet ./...
	gofmt -l . | (! grep .)

clean:
	rm -rf bin

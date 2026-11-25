.PHONY: test test-e2e run

# Unit tests
test:
	go test ./...

# E2E tests. -count=1 is required: these tests depend on a running
# Metering Service, PostgreSQL, and Redis, none of which are part of
# the test binary's cache key. Without it, `go test` replays a stale
# cached result and never hits the server.
test-e2e:
	go test ./e2e-tests/ -count=1 -v

# Run the service locally against .local/config.yaml
run:
	go run ./cmd/metering-service --config-dir .local --debug

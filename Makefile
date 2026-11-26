.PHONY: fmt test test-rust test-e2e rust-build rust-release run

# Format Go and Rust sources in place. gofmt is a no-op on clean files, so
# `make fmt` is safe to run repeatedly; rustfmt via cargo fmt likewise.
fmt:
	gofmt -w .
	cargo fmt --manifest-path metering-admission/Cargo.toml

# Unit tests
test:
	go test ./...

# Rust admission crate: unit tests (key vectors against the Go
# implementation, fold semantics, CLI harness compile)
test-rust:
	cargo test --manifest-path metering-admission/Cargo.toml

# Rust admission crate: build the library and examples. --examples
# includes the admission-check CLI harness the e2e suite invokes; the
# Go e2e tests also build it themselves (skipping when cargo is
# missing), so this target is a convenience, not a prerequisite.
rust-build:
	cargo build --manifest-path metering-admission/Cargo.toml --examples

# Rust admission crate: optimized release build (library + examples).
# The e2e harness lands in metering-admission/target/release/examples/
# admission-check.
rust-release:
	cargo build --manifest-path metering-admission/Cargo.toml --release --examples

# E2E tests. -count=1 is required: these tests depend on a running
# Metering Service, PostgreSQL, and Redis, none of which are part of
# the test binary's cache key. Without it, `go test` replays a stale
# cached result and never hits the server.
test-e2e:
	go test ./e2e-tests/ -count=1 -v

# Run the service locally against .local/config.yaml
run:
	go run ./cmd/metering-service --config-dir .local --debug

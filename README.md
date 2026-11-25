# Pricetag Metering

Usage metering and budget enforcement for AI Gateway. This repository contains two components:

## Metering Service (Go)

An event-driven accounting service that owns all budget logic: pricing, policy resolution, balance tracking, and admission flag computation. It ingests usage events as CloudEvents from the OTEL pipeline, prices them against rate tables, persists them to PostgreSQL, and writes precomputed admission flags to Redis.

**Key responsibilities:**
- Manage rates, budgets, and budget policies via REST API
- Price usage events against versioned rate tables
- Maintain balance counters and derive admission flags (`allow` / `over` / `deny`)
- Serve usage query and reporting APIs
- Support embedded (self-contained) and federated (external metering platform) modes

## Metering Admission (Rust crate)

A library crate that Praxis (the AI Gateway proxy) links to read admission flags from Redis. The crate owns the Redis key layout, value encoding, and key escaping so that Praxis contains no key names and a layout change does not require a Praxis release.

**Key responsibilities:**
- Read admission flags per (identity, resource) pair from Redis
- Apply most-specific-wins precedence: user > group > organization > tenant
- Return `allow`, `over`, or `deny` to Praxis for enforcement

## REST API

All management endpoints require the `X-Tenant` header.

### Rates

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/v1/rates` | List all rates |
| `GET` | `/api/v1/rates/{id}` | Get a rate by name |
| `POST` | `/api/v1/rates` | Create a rate |
| `PUT` | `/api/v1/rates/{id}` | Update a rate |
| `DELETE` | `/api/v1/rates/{id}` | Delete a rate |

### Budgets

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/v1/budgets` | List all budgets |
| `GET` | `/api/v1/budgets/{id}` | Get a budget by name |
| `POST` | `/api/v1/budgets` | Create a budget |
| `PUT` | `/api/v1/budgets/{id}` | Update a budget |
| `DELETE` | `/api/v1/budgets/{id}` | Delete a budget |

### Budget Policies

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/v1/budget_policies` | List all policies |
| `GET` | `/api/v1/budget_policies/{id}` | Get a policy by ID |
| `POST` | `/api/v1/budget_policies` | Create a policy |
| `PUT` | `/api/v1/budget_policies/{id}` | Update a policy |
| `DELETE` | `/api/v1/budget_policies/{id}` | Delete a policy |

### Usage

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/v1/usage` | Aggregated usage with flexible grouping and time bucketing (admin) |
| `GET` | `/api/v1/usage/me` | Self-scoped usage for the authenticated caller |

### Events

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/v1/events` | Ingest CloudEvent usage events |

## Running

```bash
make run        # start the service against .local/config.yaml
make test       # unit tests
make test-e2e   # e2e tests (requires the service, PostgreSQL, and Redis to be up)
```

> **Note:** always run the e2e suite with `-count=1` (as `make test-e2e` does).
> The tests depend on a running service and live data stores, which are not part
> of the test binary's cache key — a plain `go test ./e2e-tests/` can replay a
> stale cached result and print a pass without sending any requests.

## Architecture

```
Client → Praxis → Redis (admission read via crate)
                     ↑
           Metering Service (sole writer)
                     ↑
           OTEL Collector (CloudEvents)
                     ↑
                  Praxis (usage emission)
```

Praxis reads a single flag per request. The Metering Service is the only writer. Accounting is asynchronous — usage is priced and persisted after the response, and flags are updated when balances change.

## Project Layout

```
├── cmd/                    # Go entrypoints
├── internal/               # Go packages (handlers, store, config, server)
├── migrations/             # PostgreSQL schema migrations
├── deploy/                 # Deployment manifests
├── metering-admission/     # Rust admission crate (linked by Praxis)
│   ├── Cargo.toml
│   └── src/
├── go.mod
├── go.sum
└── Dockerfile
```

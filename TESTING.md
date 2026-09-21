# Integration Testing Strategy

## Test boundaries

| Level | What to test | What to replace |
|---|---|---|
| Unit | Domain invariants; usecase branching and ordering; exact port arguments; metric classification; logging redaction; HTTP response mapping | Ports with behaviorally accurate fakes/recorders. Every fake records calls and arguments so “not called” and contract assertions are explicit. |
| Integration | Real SQL, migrations, constraints, serialization, targeted updates; real HTTP wire format, headers, statuses, and timeouts | Disposable Postgres and WireMock containers only. The adapter under test is always real. |
| E2E | A small number of user-visible flows through HTTP, Order Service, database, Payment, and Inventory: approved, declined, and ambiguous payment/reconciliation | Ideally nothing; otherwise official service sandboxes or process-level test doubles, not in-process Go interface mocks. |

Use both unit and integration coverage when behavior crosses a port. For example, a unit test proves that a payment transport error produces `payment_unknown`; a WireMock test proves that a delayed real HTTP response actually becomes that transport error.

## Unit coverage

`internal/usecase/create_order_test.go` verifies:

- the pending order is persisted before charging;
- exact payment amount, customer/order identifiers, and idempotency key;
- approved, declined, transport-unknown, validation, initial-save failure, and final-save failure paths;
- payment/inventory/repository call counts, including proof that validation and initial-save failures cause no external side effect;
- definitive payment rejection versus ambiguous outcomes, plus retryable inventory failure;
- declined orders count as metric failures and uncontrolled tiers map to `unknown`.

`internal/paymentclient/client_test.go` and `internal/inventoryclient/client_test.go` test request construction, trace-header injection, idempotency headers, non-2xx handling, response validation, and configured timeout behavior without opening a network listener. Logging and metric adapters have focused tests in `observability`, including proof that the authoritative pending query runs at scrape time rather than request time.

## Repository integration test

`testing/integration/order_repo_test.go` starts Postgres with testcontainers and calls `postgres.Migrate`—the exact embedded migration production startup uses. It covers:

- full JSONB/timestamp/status/payment-reference round trips;
- valid SQL, duplicate-insert rejection, and conditional status updates;
- `UpdateStatus` touching only the dirty status field;
- not-found behavior;
- authoritative pending counts;
- real primary-key, status, customer, and JSON-shape constraints.
- checksum ledger population, tamper detection, and repeatable migration startup (the runner also serializes deploys with a Postgres advisory lock).

Using the production migration matters: duplicating `CREATE TABLE` in test setup can make tests green while deployment migrations are broken or stale.

## External API integration tests

`testing/integration/payment_client_test.go` starts WireMock with testcontainers. Stubs match not only method/path but also exact JSON bodies and required headers, including `Idempotency-Key` and `traceparent`. A serialization typo or missing header therefore produces WireMock's unmatched-request response and fails the test.

The payment suite covers approved, declined, timeout, 4xx, and 5xx behavior. `testing/integration/inventory_client_test.go` independently verifies the reservation body, idempotency and trace headers, non-2xx handling, and timeout. Each request has unique matching data, so accumulated WireMock mappings cannot accidentally satisfy a later scenario.

`internal/usecase/reconciler_test.go` drives recovery synchronously with fakes, including a missing provider record, a resolved charge, an inventory retry, CAS conflicts, and bounded scan pagination. The integration suite tests the read-only payment lookup, real Postgres compare-and-swap, and a complete charged-order recovery through Postgres and WireMock. No test races a worker ticker or waits a fixed interval.

Run:

```sh
go test ./...
go test -tags=integration ./testing/integration/...
```

The integration command requires a Docker-compatible daemon.

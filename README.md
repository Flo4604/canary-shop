# Canary Shop

A synthetic commerce API and continuous traffic worker for a shared Unkey
canary workspace. Uses public Unkey APIs only. No database connection,
persistent disk, key recovery feature, or app frontend.

The Unkey dashboard is the interface this app exercises.

## What runs

One Go binary has five commands:

| Command | Purpose |
| --- | --- |
| `init` | Create two API/keyspaces once and print their non-secret IDs |
| `setup` | Ensure 30 identities, three demo namespaces, and a traffic-budget namespace |
| `serve` | Serve products, orders, exports, and `/healthz` |
| `worker` | Run setup, create short-lived keys, and send continuous shop traffic |
| `check` | Check for demo verification and rate-limit events in the last 15 minutes |

The worker creates 64 keys on startup: two keys per customer and four failure
fixtures. Plaintext keys stay in memory. Normal keys expire after 24 hours.
Every 12 hours the worker creates replacements, waits 30 seconds for edge
propagation, and retires its old keys. Startup also soft-deletes expired keys
marked `owner=canary-shop` in the two configured APIs.

Restarts create fresh keys but reuse the same customers and API IDs. Identity
history stays continuous; individual key history changes on renewal. Keys left
by crashes expire and are removed from active listings on a later sweep. Soft
deletion preserves audit data; this is not a fixed database-row count.

## Data produced

Traffic cycles contain normal catalog reads, orders, warehouse exports,
disabled keys, expired keys, exhausted credits, permission failures, invalid
keys, and short bursts. The worker checks response bodies as well as statuses.
Expected denials are logged separately from unexpected failures.

Customer plans have different rate limits. One enterprise customer has an
override. Identity limits are shared by that customer's keys. Shop requests
produce verification and rate-limit analytics. Calling the shop's deployed
gateway URL also produces gateway request logs. Structured app logs produce
runtime logs when the hosting platform collects stdout/stderr.

Orders are synthetic, not stored purchases. The same customer and
`Idempotency-Key` produce the same order ID. No payment service is contacted.

## Configure once

Create a shared canary workspace, invite the team, and enable compute access
outside this app. Preview dashboards must use that same backend. This app does
not configure WorkOS or GitHub callbacks.

Install the pinned Go toolchain and build:

```bash
mise install
mise run build
```

Set `UNKEY_BASE_URL` to the actual canary API origin and `UNKEY_ROOT_KEY` to a
setup root key through your secret manager. There is no production default.
The API hostname must contain a `canary` label separated by dots or hyphens.

Run this once, then save the printed IDs in the worker's environment:

```bash
./bin/canary-shop init
```

`init` is not idempotent. It has no public API for discovering an existing API
by name. If it fails, inspect the dashboard before running it again. It prints
each ID immediately after creation. You can create the two APIs in the
dashboard instead and skip `init`.

The worker needs these variables:

| Variable | Value |
| --- | --- |
| `UNKEY_BASE_URL` | Canary API origin, without `/v2` |
| `UNKEY_ROOT_KEY` | Worker root key |
| `STOREFRONT_API_ID` | First API ID |
| `WAREHOUSE_API_ID` | Second, distinct API ID |
| `SHOP_URL` | Shop gateway origin, not its private container address |
| `SHOP_WORKER_TOKEN` | Random secret of at least 32 characters, also configured on the shop |

The shop needs `UNKEY_BASE_URL`, its own `UNKEY_ROOT_KEY`, and
`SHOP_WORKER_TOKEN`. No files or volume mounts are needed by either process.

## Scope credentials

Use separate root keys, all belonging to the shared canary workspace.
Replace `<api_id>` and `<namespace_id>` with the actual IDs. Namespace-scoped
permissions use the ID, not the namespace name.

| Process | Permissions |
| --- | --- |
| One-time `init` | `api.*.create_api` |
| Worker | `api.<api_id>.read_api`, `read_key`, `create_key`, `delete_key`, and `read_analytics` for both APIs; `identity.*.read_identity`, `identity.*.create_identity`; `ratelimit.*.create_namespace`, `ratelimit.*.limit`, `ratelimit.*.set_override`, `ratelimit.*.read_analytics` |
| Shop | `api.<api_id>.verify_key` for both APIs; `ratelimit.<namespace_id>.limit` for all four namespaces |
| Standalone `check` | `api.<api_id>.read_analytics` for both APIs; `ratelimit.<namespace_id>.read_analytics` for demo namespaces |

The abbreviated API actions in the worker row use the same
`api.<api_id>.<action>` form. The worker's namespace permissions can be narrowed
after the first setup if it will never need to recreate a missing namespace.
Neither runtime key needs API creation, key encryption, or key decryption.
Never give the worker customer production credentials.

## Run on Unkey

Build an OCI image and publish it to a registry your Unkey environment can
pull. Publishing and deployment are separate operator actions.

```bash
docker build -t canary-shop:local .
```

Deploy the same image in two apps:

1. Run the shop with arguments `serve --daily-requests 90000`. Expose port
   8080 and use `/healthz` for liveness. Configure its variables above.
2. Run `setup` once with the worker credentials before sending traffic. The
   worker also runs it on startup.
3. Run exactly one worker with arguments `worker --interval 1s`. Disable
   scale-to-zero for the worker. Set `SHOP_URL` to the shop's gateway origin.
   The worker is a background process and exposes no HTTP port.

The image runs as UID 10001 and supports a read-only root filesystem. Do not
run overlapping worker replicas: they can clean up each other's expired
failure fixtures. Use a stop-then-start deployment strategy for the worker.
The shop can have multiple replicas with the same traffic-limit configuration.

For a finite smoke run:

```bash
./bin/canary-shop worker --count 100
./bin/canary-shop check
```

Allow 30 seconds for key propagation and additional time for analytics
ingestion. A full default traffic cycle takes roughly two minutes during the
day and longer between 00:00 and 06:00 UTC.

## Traffic controls

The default interval is 1 second, with slower overnight traffic and 250 ms
burst intervals. Requests are serial. Unexpected errors back off for 10, 20,
30, and 40 seconds. Five consecutive failures stop the worker with a nonzero
exit status. Use a bounded restart policy or alert on repeated restarts.

Before key verification, the shop checks the public rate-limit API in
`canary-shop-traffic-budget`, using a UTC-date identifier. This counter is in
Unkey, not the container. Budget exhaustion pauses the worker until the next
UTC day. The default admits up to 90,000 shop requests per day, subject to
Unkey's rate-limit consistency semantics. It is a traffic limit, not a strict
financial or globally linearizable quota. Run in one region for predictable
limits, and do not add overrides to the budget namespace.

Each admitted successful request makes three Unkey calls: budget, key
verification, and the scenario rate limit. Denied budget checks still cost an
API call. Setup, key renewal, and analytics queries are additional. Budget for
up to about 270,000 calls/day at the default admission limit, plus these
management calls. Lower the traffic rate or daily limit if needed.

The worker checks analytics every 5 minutes. Missing events or query failures
produce error logs, but do not stop traffic. `/healthz` only checks process
liveness. Monitor worker exits and analytics errors separately.

## Verify changes

Tests use a local fake Unkey API, not shared data or production credentials.

```bash
mise run test
mise run check
mise run build
docker build -t canary-shop:test .
```

Coverage includes restart and TTL behavior, cleanup scope, pagination,
permissions, shared quota behavior, UTC boundaries, malformed responses,
upstream failures, secret boundaries, deterministic order IDs, and shutdown
during propagation. Live canary behavior still needs an operator smoke run.

`--allow-local` permits HTTP loopback origins for local tests. Remote origins
always require HTTPS. Clients do not follow redirects or retry API writes.

## License

MIT. See [LICENSE](LICENSE).

# Canary Shop

A synthetic commerce API and continuous traffic worker for a shared Unkey
canary workspace. Uses public Unkey APIs only. No database connection,
persistent disk, key recovery feature, or app frontend.

All application Unkey calls use the official Go SDK,
`github.com/unkeyed/sdks/api/go/v3` pinned to `v3.0.0`. SDK retries are disabled,
redirects are refused, and logged errors omit response bodies and credentials.
The SDK models required booleans as plain Go `bool`, so a small HTTP response
guard rejects missing or null verification, rate-limit, and pagination
decisions before SDK decoding. The SDK handles endpoint requests, response
types, and statuses, including metadata-only identity deletion. Application
checks still reject unknown verification codes and invalid demo metadata.

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

Each 100-request cycle interleaves 80 normal requests with 10 deliberate key
failures, then sends a ten-request burst. The first deliberate failure is the
ninth shop request. Each of the five static failure types occurs twice.

At startup and once per hour, the worker runs isolated lifecycle checks through
the public Unkey API. These checks create six extra, one-hour keys in the
storefront API. They never modify normal customer keys or identities:

- Assign the named `canary-shop-catalog-reader` role to a key with no direct
  permissions: `catalog.read` changes from denied to allowed. `orders.write`
  stays denied.
- Grant `catalog.read` directly to another key: denied becomes allowed, while
  `orders.write` stays denied.
- Spend two credits, require `USAGE_EXCEEDED`, increment credits by one, spend
  the refill, and require exhaustion again.
- Verify a key, soft-delete it, then require `NOT_FOUND`.
- Attach two keys to one fresh identity with a two-token, one-hour limit.
  Key A spends both tokens. After 30 seconds, key B's first spending request
  and key A's next request must both return `RATE_LIMITED`. A separate per-key
  counter cannot pass this check.

The role is created once and reused. Its permissions must match exactly or the
worker stops without changing it. Lifecycle keys and their identity are removed
after each run, including ordinary failures and cancellation. Cleanup failures
stop the worker. A crash or ambiguous create response can leave an identity for
manual cleanup under the `canary-shop-lifecycle-` prefix. Keys still expire.

Each propagation check permits only its known previous state, with at most
16 probes spaced 2 seconds apart. Readiness and permission probes spend zero
credits and zero identity tokens. Unexpected HTTP responses or verification
codes stop the lifecycle and worker. Credit spending is checked exactly, not
retried until exhaustion. Lifecycle runs have a 10-minute deadline and a separate
90-second cleanup deadline. They pause shop traffic and do not interrupt bursts.

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

Separate root keys are recommended, all in the shared canary workspace. One
root key also works when it has the union of the worker and shop permissions.
Replace `<api_id>` and `<namespace_id>` with the actual IDs. Namespace-scoped
permissions use the ID, not the namespace name.

| Process | Permissions |
| --- | --- |
| One-time `init` | `api.*.create_api` |
| Worker | `api.<api_id>.read_api`, `read_key`, `create_key`, `delete_key`, and `read_analytics` for both APIs; `identity.*.read_identity`, `identity.*.create_identity`; `ratelimit.*.create_namespace`, `ratelimit.*.limit`, `ratelimit.*.set_override`, `ratelimit.*.read_analytics` |
| Worker lifecycle (additional) | `api.<storefront_api_id>.update_key`, `api.<storefront_api_id>.verify_key`; `identity.*.delete_identity`; `rbac.*.read_role`, `rbac.*.create_role`, `rbac.*.create_permission`, `rbac.*.add_permission_to_role`, `rbac.*.add_role_to_key`, `rbac.*.add_permission_to_key` |
| Shop | `api.<api_id>.verify_key` for both APIs; `ratelimit.<namespace_id>.limit` for all four namespaces |
| Standalone `check` | `api.<api_id>.read_analytics` for both APIs; `ratelimit.<namespace_id>.read_analytics` for demo namespaces |

The abbreviated API actions in the worker row use the same
`api.<api_id>.<action>` form. The worker's namespace permissions can be narrowed
after the first setup if it will never need to recreate a missing namespace.
Neither runtime key needs API creation, key encryption, or key decryption.
Never give the worker customer production credentials.

## Deploy from this orb

`./deploy.sh` creates the `canary-shop` project, connects both apps to
`Flo4604/canary-shop`, configures variables, and deploys them on Unkey. It uses
one canary root key for deployment and both apps. The key needs the runtime
permissions above plus project/app creation, environment settings/variables,
and deployment read/create permissions. The Unkey GitHub integration must have
access to the repository. The workspace must have Compute access.

Run the script from Bash with `curl`, `jq`, and `openssl` installed. It prompts
for the root key and the two API IDs if they are not exported. It does not
repeat `init` or retry writes. If it fails, inspect the resulting resources
before rerunning. It deliberately stops on an existing project.

```bash
./deploy.sh
```

For an existing installation, `./deploy.sh --repair-worker-url` finds the
current shop deployment, checks its health, sets only the worker's `SHOP_URL`,
and redeploys the worker. Stop the old worker first to avoid overlapping runs.
`./deploy.sh --diagnose-project` performs only a project lookup.

## Check production and preview routing

After publishing these changes to `main`, run:

```bash
./deploy.sh --check-deployments
```

This opt-in check requires GitHub push access and deployment stop/cancel
permissions. It deploys `main` to the existing shop's production environment,
then pushes a unique `canary-preview-*` branch with a different build marker
and deploys it to the shop's preview environment. It leaves production secrets
and the production worker unchanged. Preview has no worker.

The check requests `/version` on every domain returned for each deployment,
requires the matching build marker, rechecks production after preview is ready,
and verifies that preview did not replace the current production deployment.
It uses explicit deployment API calls, not GitHub webhook auto-deployment;
auto-deployment remains disabled for the shop environments.

The preview is stopped or cancelled on exit when its ID is known. Cleanup waits
for confirmation and reports failure if resources cannot be stopped. An
ambiguous create response can leave a deployment whose ID was not received;
inspect the dashboard before retrying. Preview branches remain on GitHub for
inspection. Production remains running. Each run incurs two builds and temporary
preview compute; this check is not part of the hourly traffic lifecycle.

`GET /version` is public and sends `Cache-Control: no-store`. Its `version` value
defaults to `development`; Docker's `APP_VERSION` build argument overrides it.

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
day and longer between 00:00 and 06:00 UTC. The first cycle also runs lifecycle
checks, including a 30-second identity-counter propagation wait.

## Traffic controls

The default interval is 1 second, with slower overnight traffic. Burst intervals
are one quarter of the requested interval, capped at 250 ms. Every complete
ten-request burst must contain at least one `RATE_LIMITED`/HTTP 429 response;
all-success bursts fail. Requests are serial. Unexpected errors back off for 10, 20,
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

Lifecycle calls go directly to Unkey, outside the shop admission budget. Each
run makes fewer than 250 public API calls, including worst-case propagation
probes and cleanup, and spends three key credits on success. At most one run
starts per hour after the previous run completes, apart from process restarts.
Allow up to 6,000 additional calls/day and use a bounded restart policy.

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
during propagation. Lifecycle tests also inject broken role grants, direct
grants, credit limits/refills, revocation, and identity sharing. They reject
missing throttles and accept bounded propagation of the expected old state.
Live canary behavior still needs an operator smoke run.

`--allow-local` permits HTTP loopback origins for local tests. Remote origins
always require HTTPS. Clients do not follow redirects or retry API writes.

## License

MIT. See [LICENSE](LICENSE).

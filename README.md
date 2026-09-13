# Release Radar

Telegram bot that tells you when an artist you follow puts out something new.

Spotify will happily let you follow an artist and then never reliably tell you
they released anything. This closes that gap: subscribe to an artist, get a
message when a new album, single or EP lands.

**Go · PostgreSQL · ListenBrainz / MusicBrainz**

> Status: **S1 complete** — the bot answers, and CI now gates every change.
> Artist search is next (S2).

---

## Why the design looks like this

Three decisions carry the project, and all three came out of measuring rather
than assuming. The full reasoning, with ~35 verified constraints, is in
[`SPEC.md`](SPEC.md).

**Releases come from ListenBrainz, not Spotify.** Spotify's Web API needs the
app owner to hold an active Premium subscription as of February 2026, and its
Extended Quota tier is closed to individuals. ListenBrainz's `fresh-releases`
endpoint needs no auth and returns the whole ecosystem's window in **one request
per day** — so cost does not grow with the number of tracked artists. Spotify's
developer policy also forbids forwarding Spotify content to another service,
which a Telegram bot plainly does; MusicBrainz data is CC0 and carries no such
restriction.

**Deduplication is a database constraint, not application logic.** A user can
subscribe from the bot and from the Spotify extension; the poller can run twice
over an overlapping window. Neither may produce a duplicate message. So the
guarantee lives in `UNIQUE (user_id, release_id)` and
`PRIMARY KEY (user_id, artist_mbid)`, and every write is
`INSERT … ON CONFLICT DO NOTHING`. There is no read-then-write check anywhere,
because that is a race condition wearing a helpful expression.

**The queue is Postgres `SELECT … FOR UPDATE SKIP LOCKED`, not a broker.** At a
few hundred messages a day, RabbitMQ would add a container, a monitoring
surface and a failure mode while buying nothing. The queue sits behind an
interface, so moving to a broker later is a contained change rather than a
rewrite — and a deliberate one.

## Architecture

One binary, four workers as goroutines. Logical separation, not network
separation.

| Worker | Does |
|---|---|
| `bot` | Telegram long polling, commands, artist search, callbacks |
| `poller` | Daily: ListenBrainz → match against tracked artists → outbox |
| `notifier` | Drains the outbox, paces sends, retries, classifies failures |
| `api` | *(v2)* REST for the Spicetify extension |

Delivery channels are plugins behind a `Notifier` interface. Only Telegram is
implemented, but nothing above that boundary knows what a `chat_id` is — adding
email is an evening, not a refactor.

## Running it

```bash
cp .env.example .env      # then fill in TELEGRAM_BOT_TOKEN and USER_AGENT
docker compose up --build
curl localhost:8080/healthz
```

The database is published on host port **5433**, not 5432, to stay out of the
way of a locally installed PostgreSQL.

```bash
make run        # run against a local Go toolchain instead
make test       # unit tests; database tests skip without TEST_DATABASE_URL
make psql       # psql shell into the container
```

### Tests

The interesting tests assert database invariants, because that is where
correctness lives:

```bash
export TEST_DATABASE_URL='postgres://releaseradar:change-me-locally@localhost:5433/releaseradar?sslmode=disable'
go test ./... -count=1
```

Each one runs in a transaction that is always rolled back, so they leave no rows
behind. Without `TEST_DATABASE_URL` they skip rather than fail, keeping
`go test ./...` green with no Docker running.

## Roadmap

| | |
|---|---|
| ✅ S0 | Skeleton: compose, migrations, `/healthz`, structured logging |
| ✅ S7 | Spike: proved a Spicetify extension can `fetch()` this backend |
| ✅ S1 | Bot answers `/start`, registers commands, tracks blocks |
| ⬜ S2 | Artist search via MusicBrainz, paginated picker card |
| ⬜ S3 | Subscriptions, `/list`, `/stop` |
| ⬜ S4 | Release detection |
| ⬜ S5 | Delivery: outbox drain, pacing, failure classification |
| ⬜ S6 | Production: VPS, backups, metrics — **v1 done** |
| ⬜ S8–S10 | Extension: linking API, subscribe from Spotify, publish |

## Repo layout

```
cmd/releaseradar/      entrypoint: config, migrations, HTTP, shutdown
internal/config/       environment parsing and validation
internal/storage/      pool, migration runner, schema, invariant tests
internal/notify/       the channel boundary — no Telegram types allowed here
internal/httpx/        HTTP surface (health now, extension API in S8)
spike/                 throwaway proof-of-concept; delete after S8
SPEC.md                source of truth: requirements, constraints, decisions
```

`SPEC.md` is updated in the same commit as the code it describes. A spec that
has fallen behind the code is worse than no spec, because it lies with a
straight face.

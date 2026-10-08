# Release Radar

Telegram bot that tells you when an artist you follow puts out something new.

Spotify will happily let you follow an artist and then never reliably tell you
they released anything. This closes that gap: subscribe to an artist, get a
message when a new album, single or EP lands.

**Go · PostgreSQL · ListenBrainz / MusicBrainz**

> Status: **S0–S5 done**, S6 in progress. The bot detects releases, delivers
> them, and reports on itself through metrics, logs, routed alerts and an
> external dead-man's switch. What is left of S6 is configuration rather than
> code — a healthchecks.io URL and a token for the alert bot, both in `.env` —
> plus one deferred decision: there is no VPS, so it runs only while this
> laptop does.

---

## Why the design looks like this

Three decisions carry the project, and all three came out of measuring rather
than assuming. The full reasoning, with ~35 verified constraints, is in
[`SPEC.md`](SPEC.md).

**Releases come from ListenBrainz, not Spotify.** Spotify's Web API needs the
app owner to hold an active Premium subscription as of February 2026, and its
Extended Quota tier is closed to individuals. ListenBrainz's `fresh-releases`
endpoint needs no auth and returns the whole ecosystem's window in **one request
per day**, whether the database tracks ten artists or ten thousand. Only the
*external* cost is flat, to be exact — matching and outbox fan-out still grow
with the artists in the response and the subscribers per release. Both are
linear SQL over a local table, which is why the flat part is the one that
matters. Spotify's
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
curl localhost:8090/healthz
```

The database is published on host port **5433**, not 5432, to stay out of the
way of a locally installed PostgreSQL.

### Observability

```bash
docker compose --profile observability up -d
```

Every published port is bound to the loopback address — Grafana on **3001**,
the app on **8090** (`/metrics`, `/status`, `/v1`) and Postgres on **5433** —
so nothing is reachable from the network without a deliberate proxy. That was
not true until 16.09: both the app and Postgres sat on `0.0.0.0`, and Postgres
accepted the example password. Prometheus, Loki and Alertmanager are not
published at all: `/metrics` carries queue depths and chat volumes, and every
port not published is one fewer chance to lose a container to a bind race on
startup. That is not
hypothetical: Prometheus died that way on 9091 and stayed dead for six days.

Logs go to Loki through Grafana Alloy, read over the Docker API rather than
from `/var/lib/docker/containers`, which on Docker Desktop lives inside a VM.
Only `level` becomes a label; `chat_id`, `release_id` and the rest stay in the
line and are searched with LogQL, because each of them is unbounded and an
unbounded label is how a Loki index falls over.

This exists because `docker compose logs` is wiped by every rebuild. Measured
during one: 90 lines before, 10 after, and the same window still readable in
Loki.

Two collectors read Postgres when Prometheus scrapes rather than tracking a
number in memory. That is not incidental — a gauge set from the drain loop is
only correct at the instant of a drain, so during an incident, when the drain
loop is the thing that stopped, the graph would sit at its last healthy value
and look fine. And an unreadable queue publishes `queue_readable 0` with *no*
depth sample, because a zero depth reads as an empty queue and would silence
the alert that should be firing.

`/status` shows what each worker is doing; `/healthz` stays liveness for the
container.

Alerts are routed by Alertmanager to a **second** Telegram bot, which is the
whole point of the second bot: an alert saying the main one is blocked,
throttled or holding a revoked token cannot be delivered by the main one. Set
`ALERT_BOT_TOKEN` and `ALERT_CHAT_ID` in `.env` — without them the
`alertmanager` container refuses to start, on purpose.

```bash
make alert-chat-id
```

Message the bot once first; Telegram only reports chats that have spoken to it,
and a bot cannot message anyone first. Getting the id this way rather than by
opening `api.telegram.org/bot<TOKEN>/getUpdates` keeps a live credential out of
the browser address bar, and out of the history that syncs with it. The rest of the stack
starts regardless. Alertmanager has exactly one job, and an instance that runs
without a way to reach anybody scrapes green on every dashboard while
delivering nothing.

The app is as strict about its own `HEARTBEAT_URL`: it refuses to start without
one unless `HEARTBEAT_DISABLED=1` says so explicitly, and that state is itself
an alert (`HeartbeatDisabled`). The first version degraded with one warning line
instead, and a misspelled `.env` key switched the dead-man's switch off twice in
one evening without anybody noticing.

Alertmanager's messages carry no parse mode at all. Every other message here can
afford HTML for the formatting; this one cannot, because a stray character in
somebody's alert text would make Telegram answer `400 can't parse entities` and
drop the one message whose subject is that messages are not arriving.

Severity says what you have to do, not how bad it sounds: `critical` means
people are not being notified right now and it will not heal on its own
(repeats hourly); `warning` is a today problem (repeats once a day). Every
firing message ends with the line that mutes it:

```bash
make silence ALERT=BackupStale FOR=24h
make silences
make unsilence ID=<id>
```

A silence expires on its own and survives restarts, which is why it is the
tool for "I know, I'm on it" rather than editing the rule.

```bash
make run        # run against a local Go toolchain instead
make test       # unit tests; database tests skip without TEST_DATABASE_URL
make psql       # psql shell into the container
```

### Proving the dead-man's switch actually works

The heartbeat's value is entirely in what it does **not** send, and that half is
invisible in normal operation — a switch wired to nothing looks exactly like a
healthy one until the day it matters. Verified end to end on 16.09.2026 against
a local receiver, and reproducible:

```bash
docker run -d --name hb-receiver --network music_release_notifier_default python:3-alpine python -m http.server 8000
HEARTBEAT_URL=http://hb-receiver:8000/ HEARTBEAT_INTERVAL=15s docker compose up -d app
docker logs -f hb-receiver          # a GET every 15s, 200
```

Then take away the thing it reports on — **this stops Postgres**, so do it on a
dev machine and not while anything matters:

```bash
docker compose stop db
docker compose logs -f app | grep withholding
```

The pings stop and the app logs `withholding heartbeat: a worker is not healthy`
with `unhealthy: database`. It is alive and could ping; it declines, because it
cannot do its job. `docker compose start db` and the pings resume within
seconds. Clean up with `docker rm -f hb-receiver` and `docker compose up -d app`
to drop the override.

One thing that fell out of doing this: pointing `HEARTBEAT_URL` at a path that
returns 404 is treated as a **failed** ping, not a successful one. A typo in the
URL therefore reads as a dead process rather than as health, which is the safe
direction for it to fail in.

### Backups

A `backup` container dumps the database on a schedule. It is not behind the
observability profile, because metrics are something you look at and backups
decide whether there is anything left to look at.

Three choices in it are worth stating. Every dump is verified by decoding the
**whole** archive before it is renamed into place — `pg_restore --list` was the
obvious check and it is not enough, because the table of contents sits at the
front, so it accepts a dump whose tail has been overwritten. Rotation runs
**only after a successful dump**, so a job that has been failing for a week
cannot be the thing that deletes the last good backup it has. And the dumps go
to `./backups` as a bind mount rather than a named volume, because
`docker compose down -v` removes named volumes and the entire value of a backup
on a laptop is being able to copy it off the laptop.

The schedule counts from the newest dump, not from the container's start, and a
failed attempt is retried in five minutes (doubling up to an hour) rather than a
day later. Both matter on a machine that reboots: Docker restarts `db` and
`backup` at the same moment on boot — `depends_on` is honoured by
`compose up`, not by the daemon's restart policy — so the worker waits for
`pg_isready` before dumping. Before this, the one attempt per boot hit a
database that was still starting, the next attempt was a day away, and from
16.09 to 06.10 no dump succeeded at all.

The app reports on them without making them: `releaseradar_backup_*` is read
from the directory at scrape time. Measuring the files rather than recording the
job is the point — a row saying "backup succeeded at T" goes on saying so after
the directory is deleted, and a listing cannot.

```bash
ls -lh backups/
pg_restore --list backups/releaseradar-<stamp>.dump | head
```

### Tests

The interesting tests assert database invariants, because that is where
correctness lives:

```bash
set -a; . ./.env; set +a      # TEST_DATABASE_URL lives in .env, with the real password
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
| ✅ S2 | Artist search via MusicBrainz, paginated picker card |
| ✅ S3 | Subscriptions, `/list`, `/stop` |
| ✅ S4 | Release detection |
| ✅ S5 | Delivery: outbox drain, pacing, failure classification |
| 🔄 S6 | Production: dead-man's switch ✅, metrics ✅, logs ✅, alert routing ✅, backups ✅, VPS — **v1 done** |
| ❌ S11 | Ranking by metadata completeness — **measured and dropped**: the right artist was already first in 18 of 18 real searches |
| ⬜ S12 | Dead-letter handling for poison messages (with the v2 broker) |
| 🔄 S8 | Linking API: `/v1/link/init`, redemption in the bot, `/v1/me`, CORS |
| ⬜ S9–S10 | Extension: subscribe from Spotify, publish |

## Repo layout

```
cmd/releaseradar/      entrypoint: config, migrations, HTTP, shutdown
internal/config/       environment parsing and validation
internal/storage/      pool, migration runner, schema, invariant tests
internal/notify/       the channel boundary — no Telegram types allowed here
internal/notifier/     drains the outbox; decides what a failure costs
internal/health/       worker liveness and the external dead-man's switch
internal/backup/       reads the dump directory; makes no backups itself
internal/metrics/      Prometheus collectors; two of them read at scrape time
deploy/                Prometheus scrape config, alert rules, Alertmanager routing, Grafana
internal/httpx/        HTTP surface: health, plus the extension API under /v1
spike/                 throwaway proof-of-concept; delete after S8
SPEC.md                source of truth: requirements, constraints, decisions
```

A release notification carries where to listen — Spotify, YouTube, Apple
Music — resolved once per artist and stored in `artists.links`, never looked up
while sending. Same reason `cover_url` works that way: resolving at send time
would mean one lookup per subscriber instead of one per artist, ever.

`SPEC.md` is updated in the same commit as the code it describes. A spec that
has fallen behind the code is worse than no spec, because it lies with a
straight face.

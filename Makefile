.PHONY: run build test test-db lint fmt vet check up down logs psql spike ci-local alert-chat-id silence silences unsilence

# --- development ---
run:      ; go run ./cmd/releaseradar
build:    ; go build -trimpath -o bin/releaseradar ./cmd/releaseradar
fmt:      ; golangci-lint fmt
vet:      ; go vet ./...
lint:     ; golangci-lint run

test:     ; go test ./... -count=1
# Runs the database invariant tests too. Needs `make up` first.
#
# TEST_DATABASE_URL comes from .env, not from a literal here: the password is a
# real one now that compose refuses the example value (review 16.09, Security
# #1). No -race locally either — it needs cgo, which this Windows toolchain
# lacks (CLAUDE.md); CI runs the suite with -race.
test-db:
	@set -a; [ -f .env ] && . ./.env; set +a; \
	[ -n "$$TEST_DATABASE_URL" ] || { echo "TEST_DATABASE_URL is not set - put it in .env"; exit 1; }; \
	go test ./... -count=1

# Everything CI checks, in the order CI checks it. Run before opening a PR.
check: fmt vet lint test-db
	@echo "all checks passed"

# --- docker ---
up:       ; docker compose up -d --build
down:     ; docker compose down

# Prints the value for ALERT_CHAT_ID, so the bot token never goes into a browser
# address bar — and from there into history, into whatever syncs it, and into
# the occasional screenshot. Message the bot once first; Telegram only reports
# chats that have spoken to it.
alert-chat-id:
	@docker run --rm --env-file .env \
		-v "$(CURDIR)/deploy/alertmanager:/etc/alertmanager:ro" \
		--entrypoint sh prom/alertmanager:v0.28.1 /etc/alertmanager/chat-id.sh
# Mutes one alert, e.g. `make silence ALERT=BackupStale FOR=24h`. Every alert
# message ends with the exact line to run. A silence is the tool for "I know,
# I'm on it": it stops the repeats without touching the rule, expires on its
# own so it cannot outlive the incident, and survives restarts because
# Alertmanager keeps it on its volume. Muting by editing the rule does none of
# those. amtool runs inside the container, which keeps the API off the host.
ALERT ?=
FOR ?= 24h
REASON ?= acknowledged
AMTOOL = docker compose exec -T alertmanager amtool --alertmanager.url=http://localhost:9093
silence:
	@[ -n "$(ALERT)" ] || { echo "usage: make silence ALERT=<alertname> [FOR=24h] [REASON=...]"; exit 1; }
	@$(AMTOOL) silence add alertname="$(ALERT)" --duration="$(FOR)" --comment="$(REASON)" --author=owner
silences: ; @$(AMTOOL) silence query
unsilence:
	@[ -n "$(ID)" ] || { echo "usage: make unsilence ID=<id from make silences>"; exit 1; }
	@$(AMTOOL) silence expire "$(ID)"

logs:     ; docker compose logs -f app
psql:     ; docker compose exec db psql -U releaseradar -d releaseradar

# --- spike S7 (throwaway, separate module) ---
spike:    ; cd spike/server && go run .

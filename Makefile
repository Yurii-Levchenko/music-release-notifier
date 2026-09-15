.PHONY: run build test test-db lint fmt vet check up down logs psql spike ci-local alert-chat-id

# --- development ---
run:      ; go run ./cmd/releaseradar
build:    ; go build -trimpath -o bin/releaseradar ./cmd/releaseradar
fmt:      ; golangci-lint fmt
vet:      ; go vet ./...
lint:     ; golangci-lint run

test:     ; go test ./... -count=1
# Runs the database invariant tests too. Needs `make up` first.
test-db:
	TEST_DATABASE_URL='postgres://releaseradar:change-me-locally@localhost:5433/releaseradar?sslmode=disable' \
	go test ./... -race -count=1

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
logs:     ; docker compose logs -f app
psql:     ; docker compose exec db psql -U releaseradar -d releaseradar

# --- spike S7 (throwaway, separate module) ---
spike:    ; cd spike/server && go run .

.PHONY: run build test vet fmt up down logs psql spike

run:      ; go run ./cmd/releaseradar
build:    ; go build -o bin/releaseradar ./cmd/releaseradar
test:     ; go test ./...
vet:      ; go vet ./...
fmt:      ; gofmt -w .

up:       ; docker compose up --build
down:     ; docker compose down
logs:     ; docker compose logs -f app
psql:     ; docker compose exec db psql -U releaseradar -d releaseradar

# Spike S7 — throwaway, see spike/README
spike:    ; cd spike/server && go run .

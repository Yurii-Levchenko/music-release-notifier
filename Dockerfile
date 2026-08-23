# Build
FROM golang:1.26-alpine AS build
WORKDIR /src
# Cache deps separately from source so code edits do not refetch modules.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Static binary: the runtime stage has no libc.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/releaseradar ./cmd/releaseradar

# Run
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/releaseradar /app/releaseradar
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/releaseradar"]

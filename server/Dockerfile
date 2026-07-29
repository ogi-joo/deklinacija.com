# syntax=docker/dockerfile:1
#
# Build from the REPO ROOT so the image can include ../vocative.json:
#   docker build -t deklinacija-api -f server/Dockerfile .
# (docker compose in this folder already sets the correct context.)

# --- Build stage ---
FROM golang:1.22-alpine AS build
WORKDIR /src

# Cache module downloads
COPY server/go.mod server/go.sum ./
RUN go mod download

# Build the static binary
COPY server/ .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/deklinacija-api .

# --- Runtime stage ---
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=build /out/deklinacija-api /app/deklinacija-api
COPY vocative.json /app/vocative.json
COPY server/favicons /app/favicons
COPY server/favicons_dark /app/favicons_dark
COPY server/assets /app/assets

ENV PORT=3009
# Dataset lives next to the binary inside the image (overrides the ../ dev default).
ENV NAMES_PATH=vocative.json
# names.db is rebuilt from vocative.json on every startup, so a volume is optional.
EXPOSE 3009

ENTRYPOINT ["/app/deklinacija-api"]

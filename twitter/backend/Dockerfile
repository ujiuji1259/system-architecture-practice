# syntax=docker/dockerfile:1

# --- build stage ----------------------------------------------------------
FROM golang:1.26 AS build
WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Pure-Go build (modernc.org/sqlite, no CGO) so it runs on a static base.
ARG TARGETOS=linux
ARG TARGETARCH=arm64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# Prepare a data dir owned by the nonroot user for the SQLite file.
RUN mkdir /data

# --- runtime stage --------------------------------------------------------
FROM gcr.io/distroless/static:nonroot
WORKDIR /app

COPY --from=build /out/server /app/server
COPY --from=build --chown=nonroot:nonroot /data /data

# SQLite file lives here; mount a volume for persistence.
ENV DB_PATH=/data/twitter.db
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/server"]

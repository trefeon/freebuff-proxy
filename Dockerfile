FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# VERSION is injected from the build host (docker-compose passes
# `git describe --tags`); the .git dir is excluded from the build context
# so it cannot be derived here. Matches GoReleaser's -X main.version.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/freebuff-proxy ./backend/cmd/freebuff-proxy

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 1000 app \
    && adduser -S -u 1000 -G app app \
    && mkdir -p /app/dump /app/logs \
    && chown -R app:app /app
WORKDIR /app
COPY --from=build /out/freebuff-proxy /usr/local/bin/freebuff-proxy
USER app
EXPOSE 3457
ENTRYPOINT ["/usr/local/bin/freebuff-proxy"]

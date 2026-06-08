# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /liteio ./cmd/liteio

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S liteio && adduser -S -G liteio liteio
COPY --from=build /liteio /usr/local/bin/liteio
USER liteio
EXPOSE 9000 9001
ENTRYPOINT ["/usr/local/bin/liteio"]

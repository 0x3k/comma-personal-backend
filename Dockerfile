FROM golang:1.25-alpine AS build

WORKDIR /src

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/server ./cmd/server

RUN CGO_ENABLED=0 GOOS=linux go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@v4.18.1 \
    && cp "$(go env GOPATH)/bin/migrate" /out/migrate

FROM alpine:3.20

RUN apk add --no-cache ffmpeg ca-certificates tzdata \
    && addgroup -S app && adduser -S app -G app

WORKDIR /app

COPY --from=build /out/server /app/server
COPY --from=build /out/migrate /usr/local/bin/migrate
COPY --from=build /src/sql /app/sql
COPY docker/entrypoint.sh /app/entrypoint.sh

RUN chmod +x /app/entrypoint.sh \
    && mkdir -p /data && chown -R app:app /data /app

USER app

EXPOSE 8080

ENTRYPOINT ["/app/entrypoint.sh"]

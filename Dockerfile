FROM golang:1.26.11-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /lnurl-api ./cmd/lnurl-api

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 app \
    && adduser -D -H -u 10001 -G app app \
    && mkdir -p /var/lib/lnurl-api \
    && chown 10001:10001 /var/lib/lnurl-api
COPY --from=build /lnurl-api /usr/local/bin/lnurl-api
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["lnurl-api"]

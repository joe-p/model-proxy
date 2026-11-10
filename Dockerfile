FROM golang:1.25-alpine AS build

ARG VERSION=dev
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/model-proxy \
    ./cmd/model-proxy

FROM alpine:3.22

RUN apk add --no-cache ca-certificates
COPY --from=build /out/model-proxy /usr/local/bin/model-proxy

USER nobody
EXPOSE 8080
ENTRYPOINT ["model-proxy", "-config", "/etc/model-proxy/config.toml"]

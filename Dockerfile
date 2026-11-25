# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder

WORKDIR /src

ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

# 先复制依赖清单以充分利用层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/comfelle ./cmd/comfelle

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata \
    && mkdir -p /data

WORKDIR /data

COPY --from=builder /out/comfelle /usr/local/bin/comfelle

ENV COMFELLE_DEFAULT_LISTEN=0.0.0.0:8080

VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD sh -c 'addr="${COMFELLE_LISTEN:-${COMFY_PROXY_LISTEN:-8080}}"; wget -q -O /dev/null "http://127.0.0.1:${addr##*:}/panel/login"' || exit 1

ENTRYPOINT ["/usr/local/bin/comfelle"]
CMD ["serve", "-config", "/data/config.yaml"]

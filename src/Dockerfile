# sais2api 多阶段构建。
#
# 构建层：golang:1.24-bookworm，纯静态二进制（CGO_ENABLED=0）；
# -buildvcs=false 因为构建上下文里没有 .git。
FROM golang:1.24-bookworm AS build

ARG VERSION=0.1.0
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath \
    -ldflags "-s -w" \
    -o /out/sais2api .

# 运行层：只带二进制与运行时依赖（CA 证书、时区数据、curl 供健康检查）。
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata curl \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -r -u 10001 -m app
WORKDIR /app
COPY --from=build /out/sais2api /app/sais2api

EXPOSE 3067
USER app

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["curl","-fsS","http://127.0.0.1:3067/healthz"]

ENTRYPOINT ["/app/sais2api","-config","/app/config.json"]

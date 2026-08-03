# ---- 构建阶段 ----
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 纯 Go（modernc sqlite + wazero webp），CGO 关闭即可静态编译
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/shiguang ./cmd/server

# ---- 运行阶段（非 root） ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
 && addgroup -g 10001 shiguang \
 && adduser -D -u 10001 -G shiguang shiguang \
 && mkdir -p /data && chown shiguang:shiguang /data
COPY --from=build /out/shiguang /app/shiguang
USER shiguang
WORKDIR /app
VOLUME /data
ENV SG_ADDR=:8080 \
    SG_DB_DSN=file:/data/shiguang.db \
    SG_BLOB_LOCAL_ROOT=/data/blobs
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/app/shiguang"]

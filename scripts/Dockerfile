# ==============================================================================
# CyberCompanion 多阶段构建镜像
#   构建: docker build -t cybercompanion:latest -f scripts/Dockerfile .
#   运行: docker run -d --name cybercompanion -p 8088:8088 \
#                  -v cybercompanion-data:/data cybercompanion:latest
# ==============================================================================

# ---------- 构建阶段 ----------
FROM golang:1.21-alpine AS builder

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src

# 先拷贝依赖清单，利用层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/cybercompanion ./cmd/cybercompanion

# ---------- 运行阶段 ----------
FROM alpine:3.19

RUN apk --no-cache add ca-certificates tzdata \
 && addgroup -g 10001 -S cyber \
 && adduser -u 10001 -S cyber -G cyber -h /data

WORKDIR /data

COPY --from=builder /out/cybercompanion /usr/local/bin/cybercompanion
COPY --chown=cyber:cyber config.example.json /data/config.example.json

# 运行期数据（config.json / owners.json / exec_audit.log）都落在 /data 卷
VOLUME ["/data"]
EXPOSE 8088

# 以非 root 身份运行
USER cyber

# 容器内没有 curl，用 wget 做健康检查（alpine 自带 busybox wget）
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8088/api/health || exit 1

ENTRYPOINT ["/usr/local/bin/cybercompanion"]
CMD ["-config", "/data/config.json"]

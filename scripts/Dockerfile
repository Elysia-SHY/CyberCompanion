# ==============================================================================
# CyberCompanion 多阶段构建镜像
#   构建: docker build -t cybercompanion:latest -f scripts/Dockerfile .
#   运行: docker run -d --name cybercompanion -p 127.0.0.1:8088:8088 \
#                  -v cybercompanion-data:/data cybercompanion:latest
#
# 运行阶段使用 distroless：镜像里没有 shell、没有包管理器，攻击面最小。
# 因此健康检查不能用 curl/wget，改为调用程序自带的 -health 探针。
# ==============================================================================

# ---------- 构建阶段 ----------
FROM golang:1.22-alpine AS builder

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src

# 先拷贝依赖清单，利用层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# -trimpath 去掉构建机路径（既减小体积，也避免泄露开发机目录结构）
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/cybercompanion ./cmd/cybercompanion

# ---------- 运行阶段 ----------
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /data

COPY --from=builder --chown=65532:65532 /out/cybercompanion /cybercompanion
COPY --chown=65532:65532 config.example.json /data/config.example.json

# 运行期数据（config.json / owners.json / sessions.json / exec_audit.log）都落在 /data 卷
VOLUME ["/data"]
EXPOSE 8088

# distroless 镜像自带非 root 用户 65532
USER 65532:65532

# 用程序自带探针做健康检查（无 shell 依赖）
HEALTHCHECK --interval=30s --timeout=8s --start-period=20s --retries=3 \
  CMD ["/cybercompanion", "-health", "-config", "/data/config.json"]

ENTRYPOINT ["/cybercompanion"]
CMD ["-config", "/data/config.json"]

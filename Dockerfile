# syntax=docker/dockerfile:1
# 多架构镜像：在构建机上直接交叉编译（不需要 QEMU），最终镜像基于 scratch，只有一个静态程序。
#   docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 -t xrayclient-reversepanel .

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN GOARM="${TARGETVARIANT#v}" CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -buildid= -X main.version=$VERSION" -o /out/xrayclient-reversepanel . \
 && mkdir -p /out/data

# 证书和时区数据与架构无关，同样在构建机上取
FROM --platform=$BUILDPLATFORM alpine:3.22 AS assets
RUN apk add --no-cache ca-certificates tzdata

FROM scratch
COPY --from=assets /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=assets /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /out/xrayclient-reversepanel /app/xrayclient-reversepanel
COPY --from=build /out/data /app/data
# 程序与配置同在 /app：配置、证书在 /app/data（挂载这个目录即可持久化/备份）
ENV XRP_CONFIG=/app/data/config.json \
    TZ=Asia/Shanghai
WORKDIR /app/data
VOLUME /app/data
EXPOSE 7400
ENTRYPOINT ["/app/xrayclient-reversepanel"]
CMD ["-init-listen", "0.0.0.0:7400"]
LABEL org.opencontainers.image.source="https://github.com/tcpqueue/xrayclient-reversepanel" \
      org.opencontainers.image.description="只做 VLESS reverse 的精简 Xray 客户端（类似 frpc），带 HTTPS 管理网页" \
      org.opencontainers.image.licenses="MPL-2.0"

# 跟进上游 Xray-core

本项目**不复制上游源码**：Xray-core 作为普通 Go 模块依赖写在 `go.mod` 里。
与上游打交道的代码全部集中在 [`internal/xray/`](internal/xray/)，其他目录（`config`、`app`、`health`、`panel`）不 import xray-core，上游怎么变都不用改。

## 升级步骤

```bash
# 1. 升级依赖（版本号或 commit）
go get github.com/xtls/xray-core@v26.12.1      # 或 @<commit>
go mod tidy

# 2. 编译 + 哨兵测试
go vet ./...
go test ./...            # internal/xray/upstream_test.go 会和官方 infra/conf 逐字段对比

# 3. 确认没有引入多余的上游模块（router/dns/infra/conf 会让体积和内存明显增加）
go list -deps . | grep -E 'xray-core/(infra/conf|app/router|app/dns)$' && echo "引入了不该引入的模块"

# 4. 实际连一次服务端（用新版官方 Xray 做服务端），看网页上服务端变成“在线”
```

测试全部通过、实际能连上，就可以提交并打标签发布。

## 哨兵测试覆盖的耦合点

| 耦合点 | 本项目代码 | 对应上游 | 测试 |
|---|---|---|---|
| VLESS 用户字段（id、flow、testseed、reverse） | `build.go` `buildAccount` | `infra/conf/vless.go` `VLessOutboundConfig.Build` | `TestMatchesUpstreamConfigLoader` |
| VLESS Encryption 字符串解析（模式、0rtt/1rtt、padding、公钥） | `build.go` `parseEncryption` | 同上 | 同上 |
| REALITY 客户端（指纹、公钥、shortId、spiderX → SpiderY、mldsa65） | `build.go` `buildReality` | `infra/conf/transport_security.go` `REALITYConfig.Build` | 同上 |
| reverse 监控任务延迟 2 秒启动（关闭过早会残留） | `instances.go` `reverseStartGuard` | `proxy/vless/outbound/outbound.go` | `TestReverseStartDelay` |
| 反向连接的入站名 `vless-reverse`，freedom 对它默认全拦截 | `tracker.go`、`build.go` 末尾的全拦截规则 | `proxy/vless/outbound/outbound.go`、`proxy/freedom/freedom.go` | `TestReverseInboundName` |
| 反向通道建立的日志 `received request for udp:reverse:0` | `loghook.go` `reverseUpLog` | `common/mux/server.go` | `TestReverseUpLog` |

测试失败时，按提示打开对应的上游文件，看改了什么，再同步修改 `internal/xray/` 里的代码；
如果上游加了新的配置字段（例如新的 encryption 模式），在 `upstream_test.go` 里加一个用例，
保证本项目和官方生成的 protobuf 完全一致。

## 编译期就能发现的耦合点

这些如果上游改名或删除，`go build` 会直接报错：

- `internet.UseAlternativeSystemDialer` / `internet.SystemDialer`：用于统计连接（`tracker.go`）
- `log.RegisterHandler`：接管日志（`loghook.go`）
- `session.InboundFromContext` / `session.OutboundsFromContext`：区分连接属于哪个服务端
- `stat.CounterConnection`：流量计数
- `distro.go` 里注册的模块路径

## 已知的上游问题

- 上游 `infra/conf` 解析 `encryption` 时，若字符串只有 padding 段没有公钥（如 `mlkem768x25519plus.native.0rtt.100-111-1111`）会越界 panic；
  本项目在 `parseEncryption` 中额外检查并返回错误（`TestEncryptionWithoutKey`）。

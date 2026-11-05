# xray-lite

一个只做 **VLESS 反向代理** 的精简 Xray 客户端，用法类似 frpc：装在内网机器上，主动连到公网的**官方 Xray**，把内网服务安全地暴露出去。带一个内嵌的管理网页（拓扑图、实时流量、健康检测）。

> 非官方项目，基于 [Xray-core](https://github.com/XTLS/Xray-core) 作为库构建，与 XTLS / Project X 无关。服务端请直接使用官方 Xray。

```
本地服务 ⇄ [xray-lite] ══ VLESS (REALITY / Encryption) ══▶ [官方 Xray] ⇄ 外部访问者
 (内网)       (客户端，主动连出)                              (公网服务端)
```

## 特性

- **只保留需要的部分**：VLESS + REALITY、VLESS + Encryption（`mlkem768x25519plus`，无 TLS），flow 支持 Vision；不引入 Xray 的 JSON 加载器、router、DNS、geodata 等模块
- **省内存**：空闲常驻内存约 13–17 MB（ARM64 实测），每连接缓冲默认 16 KB（官方 amd64 默认 512 KB），可调 GC / 内存上限 / 线程数
- **多服务端**：可同时连多台服务端，每个本地服务单独指定允许哪些服务端访问；隔离在客户端强制执行
- **默认拒绝**：服务端推下来的连接只能访问放行列表里的目标与协议（TCP / UDP / TCP+UDP）
- **健康检测**：对本地服务做 TCP / UDP / HTTP / HTTPS 检测（HTTPS 可忽略证书），结果显示在拓扑图和流量曲线上
- **管理网页**（Vue 3 + Bootstrap 5，资源全部内嵌，离线可用）：
  - 概览大屏：在线服务端、连接数、实时/累计流量、内存
  - 链路拓扑图：本地服务 → xray-lite → 服务端 → 服务端入口，用颜色区分传输中 / 空闲 / 断线 / 停用 / 未监测，悬停看实时速率
  - 实时流量曲线：带刻度，可按服务端切换或对比，悬停看任意时刻的速率和健康异常
  - 服务端 / 出站管理：改动先暂存，统一“保存并应用”（热重载，失败自动回滚）
  - 分享链接导入/导出（3x-ui / Xray 标准 `vless://`）、UUID / REALITY / VLESS Encryption 密钥生成、服务端配置片段生成
  - 登录页（默认 admin / admin，PBKDF2 存储，失败锁定），可在网页修改密码

## 快速开始

### 1. 编译

需要 Go 1.27（`GOTOOLCHAIN=auto` 会自动下载）。

```bash
git clone https://github.com/tcpqueue/xray-lite.git
cd xray-lite
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o xray-lite .
```

交叉编译示例：

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o xray-lite-linux-arm64 .
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o xray-lite-linux-armv7 .
GOOS=linux GOARCH=mipsle GOMIPS=softfloat CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o xray-lite-linux-mipsle .
```

| 平台 | 体积（`-s -w`） |
|---|---|
| linux/arm64 | 13.7 MB |
| linux/amd64 | 14.8 MB |
| linux/armv7 | 14.3 MB |
| linux/mipsle | 15.9 MB |
| 对比：官方 Xray（linux/amd64） | 32 MB |

### 2. 安装到客户端（以 systemd 为例）

```bash
mkdir -p /usr/local/xray-lite && chmod 700 /usr/local/xray-lite
cp xray-lite /usr/local/xray-lite/
cp examples/xray-lite.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now xray-lite
```

没有配置文件时只会启动管理网页（默认 `127.0.0.1:7400`）。需要从其他机器访问时，用 `-web 0.0.0.0:7400` 启动，或在配置里写 `"web": {"listen": "0.0.0.0:7400"}`。

### 3. 在网页里配置

1. 打开 `http://<客户端IP>:7400`，用 **admin / admin** 登录，然后在“系统”页修改密码
2. **服务端**：点“从分享链接导入”，粘贴服务端的 `vless://` 链接（或手动填写），保存
3. **出站**：添加允许被访问的本地服务（如 `127.0.0.1:22`），可选填“服务端对外端口”，并按需开启健康检测
4. 点顶栏“保存并应用”
5. 在服务端列表点“服务端配置”，把生成的片段合并进官方 Xray 服务端配置并重启服务端

## 服务端怎么配

- 每台 xray-lite 对应服务端 VLESS 入站里的**一个 client**，该 client 带 `"reverse": {"tag": "r-xxx"}`
- 客户端连上后，服务端会**自动生成一个名为 `r-xxx` 的出站**（不需要写进 `outbounds`）
- 在服务端自己创建入站（通常是 `tunnel`），`address:port` 写**客户端那边**要访问的地址，再用路由把这些入站指到 `r-xxx`
- **一个 tag 可以承载任意多个端口**；多台客户端用多个 client、不同的 tag
- 客户端离线时 `r-xxx` 不存在，服务端日志会出现 `non existing outTag`，属正常现象

完整示例见 [examples/server.json](examples/server.json)。

> 客户端本地的 reverse tag **不会发给服务端**（线上只携带“这是反向连接”的命令，服务端使用自己配置里该用户的 tag），所以客户端无需填写，程序会自动生成。

## 客户端配置

完整示例见 [examples/config.json](examples/config.json)。通常不需要手写，网页里的修改会写回这个文件。

### `servers[]`

| 字段 | 说明 |
|---|---|
| `name` | 唯一名称，出站按名称引用 |
| `disabled` | `true` 时暂停该服务端 |
| `server` | 服务端 `host:port` |
| `uuid` | 与服务端 client 的 `id` 一致 |
| `flow` | `xtls-rprx-vision` / `xtls-rprx-vision-udp443` / 空 |
| `testseed` | Vision 填充参数，4 个数，默认 `[900,500,900,256]`；只影响本端发出的填充 |
| `security` | `reality`，或 `none`（此时必须配置 `encryption`） |
| `encryption` | `none`，或 `mlkem768x25519plus.<native\|xorpub\|random>.<0rtt\|1rtt>.<公钥>` |
| `reality` | `serverName`、`password`（公钥）、`shortId`、`fingerprint`、`spiderX`、`mldsa65Verify`（可选） |
| `tag` | 可选，客户端本地 reverse tag，留空自动生成 |

### `allow[]`（出站 / 放行列表）

服务端推下来的连接默认**全部拦截**（Xray 对 `vless-reverse` 的内置行为），只有这里列出的目标、协议和服务端才会被放行。

| 字段 | 说明 |
|---|---|
| `target` | `IP[/前缀]:端口[-端口]`，如 `127.0.0.1:22`、`192.168.1.0/24:8000-8100`；不支持域名 |
| `network` | `tcp`（默认）/ `udp` / `tcp,udp` |
| `servers` | 允许哪些服务端访问（名称列表），空 = 全部 |
| `name` / `remotePort` | 只用于网页显示、拓扑图和生成服务端片段 |
| `health` | 健康检测，见下文 |

### 健康检测 `allow[].health`

检测直接从本机连本地服务（不经过 Xray，不走系统代理）。

| 字段 | 说明 |
|---|---|
| `enabled` | 是否开启 |
| `type` | `tcp`（能否连上）/ `http` / `https`（状态码 < 400 算正常）/ `udp` |
| `target` | 检测地址，默认用出站目标；出站是网段或端口范围时必填 |
| `path` | http(s) 路径，默认 `/` |
| `insecure` | https 忽略证书校验（自签名证书需要） |
| `anyStatus` | http(s) 任何状态码都算正常 |
| `payload` | udp 探测内容，`hex:` 开头为十六进制，默认发一个换行 |
| `expectReply` | udp 必须收到回复；不要求时，只有收到“端口不可达”才算异常（Linux 有效；Windows 上 Go 屏蔽了这个错误） |
| `interval` / `timeout` | 秒，默认 30 / 5，间隔最小 5 |

### `memory`

| 字段 | 默认 | 说明 |
|---|---|---|
| `bufferKB` | 16 | 每连接缓冲 |
| `limitMB` | 0 | Go 软内存上限（GOMEMLIMIT），0 = 不限；太低会频繁 GC，建议 ≥ 32 |
| `gcPercent` | 50 | GOGC |
| `maxProcs` | 0 | CPU 线程数，0 = 全部；设为 1 可再省 1–3 MB |
| `trimSec` | 60 | 定时把空闲内存归还系统，负数关闭 |

环境变量 `GOGC` / `GOMEMLIMIT` / `GOMAXPROCS` 优先于配置。

### `web`

| 字段 | 说明 |
|---|---|
| `listen` | 管理网页监听地址；网页中不能修改 |
| `user` | 默认 `admin` |
| `passwordHash` | 网页里修改密码后写入（PBKDF2-SHA256）；不填则为默认密码 `admin` |

### 命令行

```
xray-lite -c config.json       # 指定配置文件
xray-lite -test -c config.json # 只检查配置
xray-lite -web 0.0.0.0:7400    # 覆盖网页监听地址
xray-lite -memstats 10s        # 定时打印 Go 内存统计（调试用）
xray-lite -version
```

## 设计说明

- **每个服务端一个 Xray 实例**：实例里只有两条出站——连服务端的 VLESS（带 reverse）和一个只放行分配给它的目标的 freedom（作为默认出站）。不需要 Xray 的 router（它会连带引入完整的 DNS 模块，约 5 MB），服务端之间天然隔离
- **配置直接拼成 protobuf**，不经过 `infra/conf`，因此不会引入全部协议
- **状态统计**：替换 Xray 的系统拨号器来区分“隧道连接”和“本地服务连接”，用 Xray 自带的 `stat.CounterConnection` 计数；隧道是否存活按服务端心跳（约 10 秒一次）判断
- **上游竞态的规避**：Xray 的 VLESS reverse 在实例创建 2 秒后才启动监控任务，若在此之前关闭实例，监控任务会残留；热重载时保证实例至少运行 3 秒，并主动关闭旧隧道连接

## 实测

Windows amd64，本机回环，同一服务端：

| | 官方 Xray 作为客户端 | xray-lite |
|---|---|---|
| 空闲工作集 | 23.1 MB | 16–18 MB |
| 20 并发 × 30 MB 下载后 | 61.9 MB | 29 MB |

ARM64（RK3568，Debian 12）空闲 RSS：1 线程约 13 MB，4 线程约 16–17 MB。

泄露测试：多轮“并发下载 + 热重启”后，协程数、Go 堆、隧道连接数均稳定。

## 安全建议

- 管理网页监听在非本机地址时，**第一时间修改默认密码**（未修改时页面顶部会一直提示）
- 写请求要求 JSON 并校验 Origin，会话 Cookie 为 HttpOnly + SameSite=Strict；同一 IP 连续输错 5 次锁定 1 分钟
- 网页基于 HTTP，没有 TLS；如果要跨公网访问管理网页，建议放在 VPN / Tailscale 之后
- 放行列表尽量写具体的 IP 和端口

## 已知限制

- 只支持 RAW（TCP）传输；不支持 XHTTP、gRPC、WebSocket，也不支持普通 TLS
- `testpre`（预连接）未提供：Xray 在启用 reverse 时会忽略它
- 服务端需使用相同或相近版本的 Xray（本项目基于 Xray-core v26.10.31）

## 开发

```bash
go vet ./...
go build -tags pprof -o xray-lite-pprof .   # 带 pprof：XRAY_LITE_PPROF=127.0.0.1:6060 ./xray-lite-pprof
```

升级 Xray-core：`go get github.com/xtls/xray-core@<commit>`，然后对照上游 `infra/conf/vless.go`、`infra/conf/transport_security.go` 检查 `config.go` 中复刻的解析逻辑是否需要同步。

前端在 `web/` 目录，无构建步骤；`web/vendor/` 中是 Vue 3.5.13 和 Bootstrap 5.3.3 的发布文件（MIT，许可证见 [web/vendor/LICENSES.txt](web/vendor/LICENSES.txt)）。

## 许可证

[MPL-2.0](LICENSE)，与 Xray-core 一致。

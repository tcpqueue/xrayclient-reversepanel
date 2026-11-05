# xrayclient-reversepanel

一个只做 **VLESS 反向代理** 的精简 Xray 客户端，用法类似 frpc：装在内网机器上，主动连到公网的**官方 Xray**，把内网服务安全地暴露出去。带一个内嵌的 HTTPS 管理网页（拓扑图、实时流量、健康检测）。

> 非官方项目，基于 [Xray-core](https://github.com/XTLS/Xray-core) 作为库构建，与 XTLS / Project X 无关。服务端请直接使用官方 Xray。

```
本地服务 ⇄ [xrayclient-reversepanel] ══ VLESS (REALITY / Encryption) ══▶ [官方 Xray] ⇄ 外部访问者
 (内网)       (客户端，主动连出)                              (公网服务端)
```

## 特性

- **只保留需要的部分**：VLESS + REALITY、VLESS + Encryption（`mlkem768x25519plus`，无 TLS），flow 支持 Vision；不引入 Xray 的 JSON 加载器、router、DNS、geodata 等模块
- **省内存**：空闲常驻内存约 13–17 MB（ARM64 实测），每连接缓冲默认 16 KB（官方 amd64 默认 512 KB），可调 GC / 内存上限 / 线程数
- **多服务端**：可同时连多台服务端，每个本地服务单独指定允许哪些服务端访问；隔离在客户端强制执行
- **默认拒绝**：服务端推下来的连接只能访问放行列表里的目标与协议（TCP / UDP / TCP+UDP）
- **健康检测**：对本地服务做 TCP / UDP / HTTP / HTTPS 检测（HTTPS 可忽略证书），结果显示在拓扑图和流量曲线上
- **管理网页**（Vue 3 + Bootstrap 5，资源全部内嵌，离线可用），**强制 HTTPS**（自动生成自签名证书）：
  - 概览大屏：在线服务端、连接数、实时/累计流量、内存
  - 链路拓扑图：本地服务 → xrayclient-reversepanel → 服务端 → 服务端入口，用颜色区分传输中 / 空闲 / 断线 / 停用 / 未监测，悬停看实时速率
  - 实时流量曲线：带刻度，可按服务端切换或对比，悬停看任意时刻的速率和健康异常
  - 服务端 / 出站管理：改动先暂存，统一“保存并应用”（热重载，失败自动回滚）
  - 分享链接导入/导出（3x-ui / Xray 标准 `vless://`）、UUID / REALITY / VLESS Encryption 密钥生成、服务端配置片段生成
  - 登录（PBKDF2 存储，失败锁定）、修改密码、修改网页监听地址和端口
- **一键安装脚本**：安装 / 升级 / 卸载 / 查看设置 / 重置密码 / 改监听地址，支持 systemd、OpenWrt、Alpine，支持 GitHub 代理
- **Docker 镜像**：多架构，附 compose 模板

## 安装

### 一键脚本（推荐）

```bash
curl -fsSL https://github.com/tcpqueue/xrayclient-reversepanel/releases/latest/download/install.sh -o install.sh && sh install.sh install
```

访问 GitHub 慢或失败时，用 GitHub 加速代理（脚本本身和后续下载都会走代理，升级时自动沿用）：

```bash
curl -fsSL https://ghfast.top/https://github.com/tcpqueue/xrayclient-reversepanel/releases/latest/download/install.sh -o install.sh && sh install.sh install --proxy https://ghfast.top/
```

没有 curl 的设备（如部分 OpenWrt）用 `wget -O install.sh <同样的地址>`。

安装完成后会打印**登录地址、账号和随机生成的初始密码**。网页默认监听 `0.0.0.0:7400`（可用 `--listen` 修改），只接受 HTTPS；浏览器提示证书不安全是因为证书是自签名的，核对安装输出里的 SHA256 指纹后继续即可。

之后用 `xrpanel` 管理（不带参数打开菜单）：

| 命令 | 作用 |
|---|---|
| `xrpanel` | 打开管理菜单 |
| `xrpanel info` | 查看当前设置：登录地址、账号密码、证书指纹 |
| `xrpanel status` | 运行状态、内存、最近日志 |
| `xrpanel restart` / `start` / `stop` | 重启 / 启动 / 停止客户端 |
| `xrpanel reset-password` | 重置网页账号密码（直接回车随机生成） |
| `xrpanel set-listen 0.0.0.0:7400` | 设置网页监听地址 |
| `xrpanel gen-cert` | 换了 IP / 主机名后重新生成证书 |
| `xrpanel upgrade` | 升级到最新版（失败自动回滚） |
| `xrpanel set-proxy https://ghfast.top/` | 设置 GitHub 代理（`none` 清除） |
| `xrpanel log` | 查看日志 |
| `xrpanel uninstall` | 卸载（`--purge` 同时删除配置） |

脚本选项：`--proxy URL`、`--listen 地址`、`--version v1.0.0`、`--arch 架构`、`-y`（不询问）、`--force`。环境变量 `GH_PROXY` 等同 `--proxy`，`XRP_DIR` 可修改安装目录。

**文件都在一个目录里**，备份只需打包这个目录：

```
/opt/xrayclient-reversepanel/
├── xrayclient-reversepanel   程序
├── config.json               配置（含 UUID、网页密码，权限 600）
├── tls/web.crt, web.key      管理网页证书（自动生成）
├── install.conf              安装设置（GitHub 代理）
└── xrpanel                   管理脚本（/usr/local/bin/xrpanel 链接到这里）
```

另外只会写服务文件：systemd 为 `/etc/systemd/system/xrayclient-reversepanel.service`（带沙箱加固，只允许写上面这个目录），OpenWrt / Alpine 为 `/etc/init.d/xrayclient-reversepanel`。OpenWrt 上会把配置加入 `/etc/sysupgrade.conf`，刷固件时保留。

从旧版（`/usr/local/xray-lite`、`/usr/local/xrayclient-reversepanel`）安装时会自动迁移配置。

### 支持的设备

| 架构（`--arch`） | 常见 SoC / 设备 |
|---|---|
| `amd64` | x86 软路由、PC、服务器（J4125、N100、N5105 …） |
| `386` | 老 32 位 x86 |
| `arm64` | MT7981 / MT7986（AX3000T、RAX3000M 等）、MT7622、IPQ807x / IPQ60xx、RK3328 / RK3399 / RK3568 / RK3588、S905 系列、树莓派 3/4/5（64 位系统）、PiKVM（64 位） |
| `armv7` | IPQ40xx、IPQ806x、BCM47094、Allwinner H3、i.MX6、树莓派 2/3（32 位系统） |
| `armv6` | 树莓派 1 / Zero |
| `armv5` | Kirkwood 等老 ARM NAS |
| `mipsle` | MT7621（K2P、红米 AC2100、新路由3 …）、MT7628 / MT7688、MT7620 |
| `mips` | 高通 Atheros QCA953x / QCA956x / AR9344（TP-Link、GL.iNet 老款等） |
| `mips64` / `mips64le` | Cavium Octeon、龙芯 2/3（MIPS 版） |
| `riscv64` | 昉·星光（JH7110）、全志 D1 等 |
| `loong64` | 龙芯 3A5000 及以上（LoongArch） |

脚本会自动判断架构（OpenWrt 读 `DISTRIB_ARCH`，MIPS 看 ELF 头区分大小端），MIPS 均为软浮点版本（有无 FPU 都能运行）。程序约 14–16 MB，**安装前请确认闪存剩余空间 ≥ 20 MB**（16 MB 闪存的路由器装不下）；下载解压时临时目录需要约 25 MB，`/tmp` 空间不够时可以设置 `TMPDIR`。

PiKVM 根分区是只读的：脚本安装时会自动 `rw`、结束后 `ro`；在网页里保存配置前需要先手动 `rw`。

### Docker

```bash
mkdir xrayclient-reversepanel && cd xrayclient-reversepanel
curl -fsSLO https://raw.githubusercontent.com/tcpqueue/xrayclient-reversepanel/main/docker-compose.yml
docker compose up -d
docker compose logs   # 首次启动会打印初始账号密码
```

镜像 `ghcr.io/tcpqueue/xrayclient-reversepanel`（amd64 / 386 / arm64 / armv7 / armv6 / riscv64）。配置和证书在 `./data`，模板默认使用 **host 网络**，这样容器里的 `127.0.0.1` 就是宿主机，可以直接反代宿主机上的服务。容器内查看设置 / 重置密码：

```bash
docker compose exec xrayclient-reversepanel /app/xrayclient-reversepanel info
docker compose exec xrayclient-reversepanel /app/xrayclient-reversepanel reset-password && docker compose restart
```

### 手动安装

从 [Releases](https://github.com/tcpqueue/xrayclient-reversepanel/releases) 下载对应架构的压缩包，解压后：

```bash
./xrayclient-reversepanel init -c ./config.json -listen 0.0.0.0:7400   # 生成配置和随机密码
./xrayclient-reversepanel -c ./config.json
```

## 在网页里配置

1. 打开 `https://<客户端IP>:7400`，用安装时打印的账号密码登录（忘了就 `xrpanel info` / `xrpanel reset-password`），建议在“系统”页改成自己的密码
2. **服务端**：点“从分享链接导入”，粘贴服务端的 `vless://` 链接（或手动填写），保存
3. **出站**：添加允许被访问的本地服务（如 `127.0.0.1:22`），可选填“服务端对外端口”，并按需开启健康检测
4. 点顶栏“保存并应用”
5. 在服务端列表点“服务端配置”，把生成的片段合并进官方 Xray 服务端配置并重启服务端

## 服务端怎么配

- 每台 xrayclient-reversepanel 对应服务端 VLESS 入站里的**一个 client**，该 client 带 `"reverse": {"tag": "r-xxx"}`
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

服务端推下来的连接默认**全部拦截**，只有这里列出的目标、协议和服务端才会被放行（Xray 对 `vless-reverse` 本身默认全拦截，本程序还会在规则末尾再显式加一条全拦截）。

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
| `listen` | 管理网页监听地址，如 `0.0.0.0:7400`；可在网页“系统”页或 `xrpanel set-listen` 修改 |
| `user` | 用户名，默认 `admin` |
| `password` | 安装 / 重置时生成的随机初始密码（明文，方便 `info` 查看）；在网页里改密码后自动删除 |
| `passwordHash` | 自己设置的密码（PBKDF2-SHA256） |
| `cert` / `key` | 可选，自己的证书和私钥路径；留空使用自动生成的自签名证书（`tls/` 目录，825 天，到期前自动续期） |

管理网页只提供 HTTPS；同一端口上的明文 HTTP 请求会被重定向到 `https://`，不会处理任何接口。

### 命令行

```
xrayclient-reversepanel -c config.json             运行
xrayclient-reversepanel -c config.json -web ADDR   临时覆盖网页监听地址（不写入配置）
xrayclient-reversepanel info            [-c 配置]  查看网页地址、账号密码、证书
xrayclient-reversepanel init            [-c 配置] [-listen 地址]   生成初始配置
xrayclient-reversepanel reset-password  [-c 配置] [-user 名] [-password 密码|-]
xrayclient-reversepanel set-listen      [-c 配置] 地址
xrayclient-reversepanel gen-cert        [-c 配置]
xrayclient-reversepanel check           [-c 配置]
xrayclient-reversepanel version
```

修改配置的子命令只写文件，需要重启服务生效（`xrpanel` 会自动重启）。没有 `-c` 时使用环境变量 `XRP_CONFIG`，再没有则为当前目录的 `config.json`。

## 设计说明

- **每个服务端一个 Xray 实例**：实例里只有两条出站——连服务端的 VLESS（带 reverse）和一个只放行分配给它的目标的 freedom（作为默认出站）。不需要 Xray 的 router（它会连带引入完整的 DNS 模块，约 5 MB），服务端之间天然隔离
- **配置直接拼成 protobuf**，不经过 `infra/conf`，因此不会引入全部协议；测试里会用官方 `infra/conf` 生成同样的配置逐字段对比
- **状态统计**：替换 Xray 的系统拨号器来区分“隧道连接”和“本地服务连接”，用 Xray 自带的 `stat.CounterConnection` 计数；隧道是否存活按服务端心跳（约 10 秒一次）判断
- **上游竞态的规避**：Xray 的 VLESS reverse 在实例创建 2 秒后才启动监控任务，若在此之前关闭实例，监控任务会残留；热重载时保证实例至少运行 3 秒，并主动关闭旧隧道连接

## 实测

Windows amd64，本机回环，同一服务端：

| | 官方 Xray 作为客户端 | xrayclient-reversepanel |
|---|---|---|
| 空闲工作集 | 23.1 MB | 16–18 MB |
| 20 并发 × 30 MB 下载后 | 61.9 MB | 29 MB |

ARM64（RK3568，Debian 12）空闲 RSS：1 线程约 13 MB，4 线程约 16–17 MB。

泄露测试：多轮“并发下载 + 热重启”后，协程数、Go 堆、隧道连接数均稳定。

| 平台 | 程序体积 | 压缩包 |
|---|---|---|
| linux/arm64 | 13.8 MB | 5.2 MB |
| linux/amd64 | 14.8 MB | 5.8 MB |
| linux/armv7 | 14.3 MB | 5.5 MB |
| linux/mipsle | 15.9 MB | 5.3 MB |
| 对比：官方 Xray（linux/amd64） | 32 MB | |

## 安全说明

- 安装时生成随机初始密码，不再使用固定的默认密码；旧配置里没有密码时仍为 admin / admin，页面顶部会一直提示修改
- 管理网页强制 HTTPS；Cookie 为 HttpOnly + Secure + SameSite=Strict；写请求要求 JSON 并校验 Origin；CSP 禁止内联脚本和外部资源；同一 IP 连续输错 5 次锁定 1 分钟
- 自签名证书只能防窃听，第一次访问时请核对证书指纹（`xrpanel info`）；需要跨公网访问管理网页时，仍建议放在 VPN / Tailscale 之后，或换成自己的证书（`web.cert` / `web.key`）
- systemd 服务带沙箱加固（只读系统、只允许写程序目录、禁止提权）
- 安装脚本校验下载文件的 SHA256；代理地址只允许 URL 字符
- 放行列表尽量写具体的 IP 和端口

## 已知限制

- 只支持 RAW（TCP）传输；不支持 XHTTP、gRPC、WebSocket，也不支持普通 TLS
- `testpre`（预连接）未提供：Xray 在启用 reverse 时会忽略它
- 服务端需使用相同或相近版本的 Xray（本项目当前基于 Xray-core v26.10.31）

## 开发

```
main.go, cli.go     入口和命令行子命令
internal/xray/      与上游 Xray-core 对接的适配层 —— 项目中唯一 import xray-core 的目录
internal/config/    本程序的配置格式与校验
internal/app/       内核生命周期、热重载、回滚
internal/health/    本地服务健康检测
internal/panel/     管理网页（HTTPS、登录、接口）和前端资源 web/
install.sh          一键安装 / 管理脚本
scripts/build.sh    交叉编译全部发布包
```

```bash
go vet ./... && go test ./...
sh scripts/build.sh v1.0.0 arm64 mipsle       # 编译指定架构到 dist/
go build -tags pprof -o xrp-pprof .            # 带 pprof：XRAYCLIENT_REVERSEPANEL_PPROF=127.0.0.1:6060 ./xrp-pprof
```

跟进上游 Xray-core 的方法见 [UPSTREAM.md](UPSTREAM.md)。推送 `v*` 标签后，GitHub Actions 会自动测试、编译全部架构、发布 Release 和 Docker 镜像。

前端无构建步骤；`internal/panel/web/vendor/` 中是 Vue 3.5.13 和 Bootstrap 5.3.3 的发布文件（MIT，许可证见 [LICENSES.txt](internal/panel/web/vendor/LICENSES.txt)）。

## 许可证

[MPL-2.0](LICENSE)，与 Xray-core 一致。

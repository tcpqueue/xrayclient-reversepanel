package main

// 只注册反向代理客户端真正用到的模块。
// 官方完整版在 main/distro/all/all.go 里导入了全部协议/传输/JSON 加载器，
// 这里刻意不导入 infra/conf、main/json、router、dns、geodata 等（router 会连带引入完整 DNS 模块），
// 既减小二进制，也避免它们 init() 时的常驻内存。
import (
	// 核心必需
	_ "github.com/xtls/xray-core/app/dispatcher"
	_ "github.com/xtls/xray-core/app/policy"
	_ "github.com/xtls/xray-core/app/proxyman/outbound"

	// 出站：VLESS（连服务端，内含新版 reverse 与 encryption）+ freedom（连本地服务）
	_ "github.com/xtls/xray-core/proxy/freedom"
	_ "github.com/xtls/xray-core/proxy/vless/outbound"

	// 传输：RAW(TCP) + REALITY
	_ "github.com/xtls/xray-core/transport/internet/reality"
	_ "github.com/xtls/xray-core/transport/internet/tcp"
)

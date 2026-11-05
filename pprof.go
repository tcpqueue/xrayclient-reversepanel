//go:build pprof

// 仅在 `go build -tags pprof` 时编译进来，用于排查内存：
//
//	XRAY_LITE_PPROF=127.0.0.1:6060 ./xray-lite -c config.json
//	go tool pprof http://127.0.0.1:6060/debug/pprof/heap
package main

import (
	"net/http"
	_ "net/http/pprof"
	"os"
)

func init() {
	if addr := os.Getenv("XRAY_LITE_PPROF"); addr != "" {
		go http.ListenAndServe(addr, nil)
	}
}

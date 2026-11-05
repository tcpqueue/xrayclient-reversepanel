package main

// 命令行子命令（安装脚本 install.sh 也通过它们读写配置）。
// 修改配置的子命令只写文件；正在运行的服务需要重启后才会使用新设置。

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/tcpqueue/xrayclient-reversepanel/internal/config"
	"github.com/tcpqueue/xrayclient-reversepanel/internal/panel"
	"github.com/tcpqueue/xrayclient-reversepanel/internal/xray"
)

func usage() {
	fmt.Fprintf(os.Stderr, `xrayclient-reversepanel %s — 只做 VLESS reverse 的精简 Xray 客户端

运行:
  xrayclient-reversepanel [-c config.json] [-web 地址] [-init-listen 地址]

子命令:
  info            [-c 配置]                       查看网页地址、账号密码、证书、服务端数量
  init            [-c 配置] [-listen 地址]        生成初始配置（已存在则不动）
  reset-password  [-c 配置] [-user 名] [-password 密码|-]  重置网页账号密码（不填密码则随机生成）
  set-listen      [-c 配置] 地址                  设置网页监听地址，如 0.0.0.0:7400
  gen-cert        [-c 配置]                       重新生成自签名证书（换了 IP/主机名后使用）
  check           [-c 配置]                       检查配置
  version                                         显示版本

参数:
`, version)
	flag.PrintDefaults()
}

func runCLI(cmd string, args []string) int {
	fset := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cfgPath := fset.String("c", defaultConfigPath(), "配置文件路径")
	var listen, user, password *string
	switch cmd {
	case "init":
		listen = fset.String("listen", "0.0.0.0:7400", "管理网页监听地址")
	case "reset-password":
		user = fset.String("user", "", "新用户名（默认保持不变）")
		password = fset.String("password", "", "新密码（至少 6 位；不填则随机生成；- 表示从标准输入读取）")
	case "version", "-v":
		printVersion()
		return 0
	case "help":
		usage()
		return 0
	case "info", "set-listen", "gen-cert", "check":
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n", cmd)
		usage()
		return 2
	}
	if err := fset.Parse(args); err != nil {
		return 2
	}
	var err error
	switch cmd {
	case "init":
		err = cmdInit(*cfgPath, *listen)
	case "info":
		err = cmdInfo(*cfgPath)
	case "reset-password":
		err = cmdResetPassword(*cfgPath, *user, *password)
	case "set-listen":
		if fset.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "用法: set-listen [-c 配置] 地址，如 0.0.0.0:7400")
			return 2
		}
		err = cmdSetListen(*cfgPath, fset.Arg(0))
	case "gen-cert":
		err = cmdGenCert(*cfgPath)
	case "check":
		var c *config.Config
		if c, err = config.Load(*cfgPath); err == nil {
			if err = xray.Check(c); err == nil {
				fmt.Println("配置正确")
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	return 0
}

// initConfig 生成并保存首次运行的配置和自签名证书，返回明文初始密码。
func initConfig(path, listen string) (*config.Config, string, error) {
	addr, err := panel.NormalizeListen(listen)
	if err != nil {
		return nil, "", err
	}
	c, pw := config.NewDefault(addr)
	if err := c.Save(path); err != nil {
		return nil, "", fmt.Errorf("写入配置失败: %w", err)
	}
	cf, kf, auto := panel.CertPaths(c.Web, path)
	if _, err := panel.LoadOrCreateCert(cf, kf, auto); err != nil {
		return nil, "", err
	}
	return c, pw, nil
}

func cmdInit(path, listen string) error {
	if _, err := os.Stat(path); err == nil {
		fmt.Println("配置文件已存在，未修改:", path)
		return nil
	}
	if _, _, err := initConfig(path, listen); err != nil {
		return err
	}
	fmt.Println("已生成配置文件:", path)
	return cmdInfo(path)
}

func loadWeb(path string) (*config.Config, error) {
	c, err := config.Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("配置文件 %s 不存在（先运行 init）", path)
	}
	if err != nil {
		return nil, err
	}
	if c.Web == nil {
		c.Web = &config.WebConfig{}
	}
	return c, nil
}

func cmdInfo(path string) error {
	c, err := loadWeb(path)
	if err != nil {
		return err
	}
	w := c.Web
	user := w.User
	if user == "" {
		user = "admin"
	}
	var pw string
	switch {
	case w.PasswordHash != "":
		pw = "（已加密保存，无法查看；忘记了用 xrpanel reset-password 重置）"
	case w.Password != "":
		pw = w.Password + "  （初始随机密码，在网页修改后将不再以明文保存）"
	default:
		pw = "admin  （默认密码，请尽快修改）"
	}
	enabled := 0
	for _, s := range c.Servers {
		if !s.Disabled {
			enabled++
		}
	}
	fmt.Println("配置文件:  ", path)
	if w.Listen == "" {
		fmt.Println("管理网页:   未启用（web.listen 为空）")
	} else {
		fmt.Println("监听地址:  ", w.Listen)
		for i, u := range panel.LoginURLs(w.Listen) {
			if i == 0 {
				fmt.Println("登录地址:  ", u)
			} else {
				fmt.Println("           ", u)
			}
		}
	}
	fmt.Println("账号:      ", user)
	fmt.Println("密码:      ", pw)
	cf, kf, auto := panel.CertPaths(w, path)
	if cert, err := panel.ReadCert(cf, kf); err == nil {
		ci := panel.CertInfoOf(cf, cert)
		kind := "自签名"
		if !ci.SelfSigned {
			kind = "自定义"
		}
		fmt.Printf("证书:        %s（%s，有效期至 %s）\n", cf, kind, time.UnixMilli(ci.NotAfter).Format("2006-01-02"))
		fmt.Println("证书指纹:   SHA256", ci.SHA256)
	} else if auto {
		fmt.Println("证书:        尚未生成（首次启动时自动生成）")
	} else {
		fmt.Println("证书:        读取失败:", err)
	}
	fmt.Printf("服务端:      %d 个（启用 %d 个）\n", len(c.Servers), enabled)
	fmt.Printf("本地服务:    %d 个\n", len(c.Allow))
	if err := xray.Check(c); err != nil {
		fmt.Println("配置检查:  ", err)
	}
	return nil
}

func cmdResetPassword(path, user, pw string) error {
	c, err := loadWeb(path)
	if err != nil {
		return err
	}
	if user = strings.TrimSpace(user); user != "" {
		c.Web.User = user
	}
	if c.Web.User == "" {
		c.Web.User = "admin"
	}
	if pw == "-" { // 从标准输入读取，避免密码出现在进程列表里
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		pw = strings.TrimRight(line, "\r\n")
	}
	shown := pw
	if pw == "" {
		shown = config.RandomPassword(12)
		c.Web.Password, c.Web.PasswordHash = shown, "" // 随机密码明文保存，便于 info 查看
	} else {
		if len(pw) < 6 {
			return fmt.Errorf("密码至少 6 位")
		}
		if pw == "admin" {
			return fmt.Errorf("不能使用默认密码 admin")
		}
		c.Web.Password, c.Web.PasswordHash = "", panel.HashPassword(pw) // 自己设的密码只保存哈希
	}
	if err := c.Save(path); err != nil {
		return err
	}
	fmt.Println("账号:", c.Web.User)
	fmt.Println("密码:", shown)
	fmt.Println("已写入配置；重启服务后生效（已登录的会话会失效）")
	return nil
}

func cmdSetListen(path, addr string) error {
	c, err := loadWeb(path)
	if err != nil {
		return err
	}
	if addr, err = panel.NormalizeListen(addr); err != nil {
		return err
	}
	c.Web.Listen = addr
	if err := c.Save(path); err != nil {
		return err
	}
	fmt.Println("管理网页监听地址已设为", addr, "，重启服务后生效")
	for _, u := range panel.LoginURLs(addr) {
		fmt.Println("  " + u)
	}
	return nil
}

func cmdGenCert(path string) error {
	c, err := loadWeb(path)
	if err != nil {
		return err
	}
	cf, kf, auto := panel.CertPaths(c.Web, path)
	if !auto {
		return fmt.Errorf("配置里指定了自己的证书（web.cert / web.key），不会覆盖")
	}
	cert, err := panel.GenerateCert(cf, kf)
	if err != nil {
		return err
	}
	ci := panel.CertInfoOf(cf, cert.Leaf)
	fmt.Println("已重新生成自签名证书:", cf)
	fmt.Println("包含地址:", strings.Join(ci.Names, ", "))
	fmt.Println("证书指纹: SHA256", ci.SHA256)
	fmt.Println("重启服务后生效")
	return nil
}

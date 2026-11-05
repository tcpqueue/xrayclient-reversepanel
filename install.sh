#!/bin/sh
# xrayclient-reversepanel 一键安装 / 管理脚本（XRP_INSTALL_SCRIPT）
#
#   安装:   sh install.sh install [--proxy https://ghfast.top/] [--listen 0.0.0.0:7400]
#   管理:   xrpanel            （安装后可用，打开菜单）
#           xrpanel help       （查看全部命令）
#
# 支持 systemd（Debian/Ubuntu/CentOS/Arch/PiKVM…）、OpenWrt procd、Alpine OpenRC；
# 支持 amd64 / 386 / arm64 / armv7 / armv6 / armv5 / mips / mipsle / mips64 / mips64le / riscv64 / loong64。
# 所有文件都在同一个目录（默认 /opt/xrayclient-reversepanel），备份只需打包这个目录。
#
# 只使用 POSIX sh 语法，兼容 busybox ash。

REPO="tcpqueue/xrayclient-reversepanel"
NAME="xrayclient-reversepanel"
DIR="${XRP_DIR:-/opt/$NAME}"
BIN="$DIR/$NAME"
CONF="$DIR/config.json"
CTL="$DIR/xrpanel"
SETTINGS="$DIR/install.conf"
DEFAULT_LISTEN="0.0.0.0:7400"
OLD_DIRS="/usr/local/xray-lite /usr/local/xrayclient-reversepanel"

GH_PROXY="${GH_PROXY:-}"
PROXY_SET=""   # 命令行/环境变量指定了代理
[ -n "$GH_PROXY" ] && PROXY_SET=1
VER="${XRP_VERSION:-}"
LISTEN="$DEFAULT_LISTEN"
ARCH=""
YES=""
PURGE=""
FORCE=""
TMP=""
PIKVM_RW=""

# ---------------- 输出 ----------------
if [ -t 1 ]; then
	C_R=$(printf '\033[31m'); C_G=$(printf '\033[32m'); C_Y=$(printf '\033[33m'); C_B=$(printf '\033[36m'); C_0=$(printf '\033[0m')
else
	C_R=""; C_G=""; C_Y=""; C_B=""; C_0=""
fi
info() { printf '%s\n' "${C_G}$*${C_0}"; }
warn() { printf '%s\n' "${C_Y}注意: $*${C_0}" >&2; }
err()  { printf '%s\n' "${C_R}错误: $*${C_0}" >&2; }
die()  { err "$*"; exit 1; }

cleanup() {
	[ -n "$TMP" ] && [ -d "$TMP" ] && rm -rf "$TMP"
	[ -n "$PIKVM_RW" ] && command -v ro >/dev/null 2>&1 && ro >/dev/null 2>&1
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# ask 提示 默认值  → 结果在 REPLY；没有终端时直接用默认值
ask() {
	REPLY=""
	if [ -n "$YES" ]; then REPLY="${2:-}"; return; fi
	if [ -n "${2:-}" ]; then printf '%s [%s]: ' "$1" "$2"; else printf '%s: ' "$1"; fi
	if ! { read -r REPLY </dev/tty; } 2>/dev/null; then REPLY=""; echo; fi
	[ -z "$REPLY" ] && REPLY="${2:-}"
	return 0
}
ask_secret() {
	REPLY=""
	printf '%s: ' "$1"
	stty -echo </dev/tty 2>/dev/null
	{ read -r REPLY </dev/tty; } 2>/dev/null
	stty echo </dev/tty 2>/dev/null
	echo
}
# confirm 提示 默认(y/n)
confirm() {
	_def="${2:-n}"
	if [ -n "$YES" ]; then return 0; fi
	if [ "$_def" = y ]; then ask "$1 [Y/n]" ""; else ask "$1 [y/N]" ""; fi
	case "$REPLY" in
		[yY]|[yY][eE][sS]) return 0 ;;
		"") [ "$_def" = y ] ;;
		*) return 1 ;;
	esac
}

need_root() { [ "$(id -u)" = 0 ] || die "请用 root 运行（sudo sh $0 ...）"; }
has() { command -v "$1" >/dev/null 2>&1; }

# ---------------- 环境检测 ----------------
INIT=""
LINK_DIR=""
detect_init() {
	if [ -f /etc/openwrt_release ] && [ -f /etc/rc.common ]; then
		INIT=procd; LINK_DIR=/usr/bin
	elif [ -d /run/systemd/system ]; then
		INIT=systemd; LINK_DIR=/usr/local/bin
	elif has openrc-run || [ -x /sbin/openrc-run ]; then
		INIT=openrc; LINK_DIR=/usr/local/bin
	else
		INIT=none; LINK_DIR=/usr/local/bin
	fi
}

# ELF 头第 N 个字节（十进制）
elf_byte() { dd if=/bin/sh bs=1 skip="$1" count=1 2>/dev/null | od -An -tu1 2>/dev/null | tr -d ' \n'; }

detect_arch() {
	[ -n "$ARCH" ] && return
	if [ -f /etc/openwrt_release ]; then
		_da=$(sed -n "s/^DISTRIB_ARCH=['\"]\{0,1\}\([^'\"]*\).*/\1/p" /etc/openwrt_release)
		case "$_da" in
			x86_64*) ARCH=amd64 ;;
			i386*|i486*|i686*) ARCH=386 ;;
			aarch64*) ARCH=arm64 ;;
			arm_cortex-a*) ARCH=armv7 ;;
			arm_arm1176*|arm_mpcore*) ARCH=armv6 ;;
			arm_*) ARCH=armv5 ;;
			mipsel*) ARCH=mipsle ;;
			mips64el*) ARCH=mips64le ;;
			mips64*) ARCH=mips64 ;;
			mips*) ARCH=mips ;;
			riscv64*) ARCH=riscv64 ;;
			loongarch64*) ARCH=loong64 ;;
		esac
		[ -n "$ARCH" ] && return
	fi
	_m=$(uname -m)
	case "$_m" in
		x86_64|amd64) ARCH=amd64 ;;
		i386|i486|i586|i686|x86) ARCH=386 ;;
		aarch64|arm64) ARCH=arm64 ;;
		armv8l|armv8*|armv7*) ARCH=armv7 ;;
		armv6*) ARCH=armv6 ;;
		armv5*|armv4*|arm) ARCH=armv5 ;;
		mips*)
			# uname 区分不了大小端，看 /bin/sh 的 ELF 头：EI_CLASS(4) 1=32位 2=64位；EI_DATA(5) 1=小端 2=大端
			_cls=$(elf_byte 4); _end=$(elf_byte 5)
			case "$_cls$_end" in
				11) ARCH=mipsle ;; 12) ARCH=mips ;;
				21) ARCH=mips64le ;; 22) ARCH=mips64 ;;
				*) die "无法判断 MIPS 大小端，请用 --arch mips 或 --arch mipsle 指定" ;;
			esac ;;
		riscv64) ARCH=riscv64 ;;
		loongarch64) ARCH=loong64 ;;
		*) die "不支持的 CPU 架构: $_m（可用 --arch 指定：amd64 386 arm64 armv7 armv6 armv5 mips mipsle mips64 mips64le riscv64 loong64）" ;;
	esac
}

# PiKVM 根分区默认只读：安装期间临时 rw，结束后恢复 ro
pikvm_rw() {
	if [ -d /etc/kvmd ] && has rw && [ -z "$PIKVM_RW" ]; then
		rw >/dev/null 2>&1 && PIKVM_RW=1
		trap cleanup EXIT
	fi
}

# ---------------- 设置 ----------------
load_settings() {
	if [ -z "$PROXY_SET" ] && [ -f "$SETTINGS" ]; then
		GH_PROXY=$(sed -n 's/^GH_PROXY=//p' "$SETTINGS" | head -n 1)
	fi
	GH_PROXY=$(normalize_proxy "$GH_PROXY") || die "GitHub 代理地址无效: $GH_PROXY"
}
save_settings() {
	mkdir -p "$DIR" || return 1
	{
		echo "# xrpanel 安装设置（升级时使用）"
		echo "GH_PROXY=$GH_PROXY"
	} >"$SETTINGS"
	chmod 600 "$SETTINGS"
}
# 规范化代理前缀：补全 https://，末尾补 /；只允许 URL 安全字符
normalize_proxy() {
	_p=$(printf '%s' "$1" | tr -d ' \t\r\n')
	[ -z "$_p" ] && { echo ""; return 0; }
	case "$_p" in
		http://*|https://*) ;;
		*) _p="https://$_p" ;;
	esac
	printf '%s' "$_p" | grep -Eq '^https?://[A-Za-z0-9._~:/?#@!&()*+,;=%-]+$' || return 1
	case "$_p" in
		*/) ;;
		*) _p="$_p/" ;;
	esac
	echo "$_p"
}

# ---------------- 下载 ----------------
fetch() {
	if has curl; then
		curl -fL --connect-timeout 15 --retry 2 -sS -o "$2" "$1"
	elif has wget; then
		wget -q -T 30 -O "$2" "$1"
	elif has uclient-fetch; then
		uclient-fetch -q -T 30 -O "$2" "$1"
	else
		die "需要 curl 或 wget（OpenWrt: opkg update && opkg install curl ca-bundle）"
	fi
}
rel_url() {
	if [ -n "$VER" ]; then _r="releases/download/$VER"; else _r="releases/latest/download"; fi
	printf '%s%s' "$GH_PROXY" "https://github.com/$REPO/$_r/$1"
}
mk_tmp() {
	[ -n "$TMP" ] && return
	TMP=$(mktemp -d 2>/dev/null) || { TMP="/tmp/xrp.$$"; mkdir -p "$TMP" || die "无法创建临时目录"; }
	trap cleanup EXIT # 菜单里的命令在子 shell 中运行，子 shell 不继承 trap，这里重新设置
}

NEWVER=""
download_release() {
	detect_arch
	mk_tmp
	ASSET="$NAME-linux-$ARCH.tar.gz"
	info "下载 ${VER:-最新版} ($ARCH)${GH_PROXY:+，代理 $GH_PROXY}"
	if ! fetch "$(rel_url VERSION)" "$TMP/VERSION"; then
		err "下载失败：$(rel_url VERSION)"
		[ -z "$GH_PROXY" ] && warn "国内网络可以加 GitHub 代理重试：sh $0 install --proxy https://ghfast.top/"
		exit 1
	fi
	NEWVER=$(tr -d ' \r\n' <"$TMP/VERSION")
	case "$NEWVER" in v[0-9]*) ;; *) die "获取到的版本号不对（代理可能返回了错误页面）: $NEWVER" ;; esac
	fetch "$(rel_url "$ASSET")" "$TMP/$ASSET" || die "下载 $ASSET 失败"
	if fetch "$(rel_url sha256sums.txt)" "$TMP/sha256sums.txt" && has sha256sum; then
		_want=$(awk -v a="$ASSET" '$2==a || $2=="*"a {print $1}' "$TMP/sha256sums.txt")
		_got=$(sha256sum "$TMP/$ASSET" | awk '{print $1}')
		[ -n "$_want" ] && [ "$_want" = "$_got" ] || die "校验失败：$ASSET 的 SHA256 不匹配（下载不完整或被篡改）"
		info "SHA256 校验通过"
	else
		warn "无法校验 SHA256（缺少 sha256sum 或校验文件）"
	fi
	mkdir -p "$TMP/x"
	tar -xzf "$TMP/$ASSET" -C "$TMP/x" || die "解压失败（tmp 空间不足？可设置 TMPDIR 到有空间的目录）"
	chmod 755 "$TMP/x/$NAME"
	"$TMP/x/$NAME" version >/dev/null 2>&1 || die "下载的程序无法在本机运行（架构 $ARCH 不对？可用 --arch 指定）"
}

check_space() {
	_parent=$(dirname "$DIR")
	[ -d "$_parent" ] || mkdir -p "$_parent"
	_avail=$(df -k "$_parent" 2>/dev/null | awk 'NR==2 {print $4}')
	if [ -n "$_avail" ] && [ "$_avail" -lt 20000 ] 2>/dev/null; then
		warn "$_parent 剩余空间只有 $((_avail / 1024))MB，程序需要约 16MB"
		confirm "仍然继续？" || exit 1
	fi
}

# ---------------- 服务 ----------------
svc() {
	case "$INIT" in
		systemd) systemctl "$1" "$NAME" >/dev/null 2>&1 ;;
		procd) [ -x "/etc/init.d/$NAME" ] && "/etc/init.d/$NAME" "$1" >/dev/null 2>&1 ;;
		openrc) rc-service "$NAME" "$1" >/dev/null 2>&1 ;;
		*) return 1 ;;
	esac
}
get_pid() {
	_pid=""
	if [ "$INIT" = systemd ]; then
		_pid=$(systemctl show -p MainPID --value "$NAME" 2>/dev/null)
		[ "$_pid" = 0 ] && _pid=""
	fi
	[ -z "$_pid" ] && has pidof && _pid=$(pidof "$NAME" 2>/dev/null | awk '{print $1}')
	echo "$_pid"
}
is_running() { [ -n "$(get_pid)" ]; }

write_service() {
	case "$INIT" in
	systemd)
		cat >"/etc/systemd/system/$NAME.service" <<EOF
[Unit]
Description=xrayclient-reversepanel (VLESS reverse proxy client)
Documentation=https://github.com/$REPO
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$DIR
ExecStart=$BIN -c $CONF
Restart=always
RestartSec=5s
LimitNOFILE=65535
UMask=0077
# 加固：只允许写程序目录（网页保存配置、证书）
NoNewPrivileges=true
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
ProtectSystem=strict
ReadWritePaths=$DIR
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
RestrictSUIDSGID=true
RestrictRealtime=true
RestrictNamespaces=true
LockPersonality=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK

[Install]
WantedBy=multi-user.target
EOF
		systemctl daemon-reload
		systemctl enable "$NAME" >/dev/null 2>&1
		;;
	procd)
		cat >"/etc/init.d/$NAME" <<EOF
#!/bin/sh /etc/rc.common
# xrayclient-reversepanel（由 xrpanel 生成）
START=99
STOP=10
USE_PROCD=1

start_service() {
	procd_open_instance
	procd_set_param command $BIN -c $CONF
	procd_set_param respawn 3600 5 0
	procd_set_param limits nofile="65535 65535"
	procd_set_param stdout 1
	procd_set_param stderr 1
	procd_close_instance
}
EOF
		chmod 755 "/etc/init.d/$NAME"
		"/etc/init.d/$NAME" enable
		# 刷固件（sysupgrade）时保留配置
		if [ -f /etc/sysupgrade.conf ]; then
			for _f in "$CONF" "$DIR/tls/" "$SETTINGS"; do
				grep -qxF "$_f" /etc/sysupgrade.conf || echo "$_f" >>/etc/sysupgrade.conf
			done
		fi
		;;
	openrc)
		cat >"/etc/init.d/$NAME" <<EOF
#!/sbin/openrc-run
# xrayclient-reversepanel（由 xrpanel 生成）
name="$NAME"
description="VLESS reverse proxy client"
command="$BIN"
command_args="-c $CONF"
directory="$DIR"
supervisor=supervise-daemon
respawn_delay=5
output_log="/var/log/$NAME.log"
error_log="/var/log/$NAME.log"
rc_ulimit="-n 65535"

depend() {
	need net
	after firewall
}
EOF
		chmod 755 "/etc/init.d/$NAME"
		rc-update add "$NAME" default >/dev/null 2>&1
		;;
	*)
		warn "没有识别到 systemd / procd / OpenRC，需要自己设置开机启动：$BIN -c $CONF"
		;;
	esac
}

remove_service() {
	case "$INIT" in
		systemd)
			systemctl disable --now "$NAME" >/dev/null 2>&1
			rm -f "/etc/systemd/system/$NAME.service"
			systemctl daemon-reload ;;
		procd)
			[ -x "/etc/init.d/$NAME" ] && { "/etc/init.d/$NAME" stop; "/etc/init.d/$NAME" disable; } >/dev/null 2>&1
			rm -f "/etc/init.d/$NAME"
			if [ -f /etc/sysupgrade.conf ]; then
				grep -vxF -e "$CONF" -e "$DIR/tls/" -e "$SETTINGS" /etc/sysupgrade.conf >/etc/sysupgrade.conf.xrp && mv /etc/sysupgrade.conf.xrp /etc/sysupgrade.conf
			fi ;;
		openrc)
			rc-service "$NAME" stop >/dev/null 2>&1
			rc-update del "$NAME" default >/dev/null 2>&1
			rm -f "/etc/init.d/$NAME" ;;
	esac
}

# 启动后确认进程稳定（几秒内 PID 不变，没有在崩溃重启循环里）
start_and_check() {
	svc restart || svc start
	sleep 3
	_p1=$(get_pid)
	[ -n "$_p1" ] || return 1
	sleep 2
	[ "$(get_pid)" = "$_p1" ]
}

# ---------------- 安装 / 升级 ----------------
install_files() {
	mkdir -p "$DIR" && chmod 700 "$DIR"
	cp "$TMP/x/$NAME" "$BIN.new" && chmod 755 "$BIN.new" && mv -f "$BIN.new" "$BIN" || die "写入 $BIN 失败"
	if [ -f "$TMP/x/install.sh" ]; then
		cp "$TMP/x/install.sh" "$CTL.new" && chmod 755 "$CTL.new" && mv -f "$CTL.new" "$CTL"
	elif [ -f "$0" ] && grep -q XRP_INSTALL_SCRIPT "$0" 2>/dev/null && [ "$0" != "$CTL" ]; then
		cp "$0" "$CTL" && chmod 755 "$CTL"
	fi
	for _f in LICENSE README.md; do [ -f "$TMP/x/$_f" ] && cp "$TMP/x/$_f" "$DIR/$_f"; done
	mkdir -p "$LINK_DIR" && ln -sf "$CTL" "$LINK_DIR/xrpanel"
}

# 从旧版目录迁移配置（xray-lite / 早期的 /usr/local/xrayclient-reversepanel）
migrate_old() {
	for _d in $OLD_DIRS; do
		[ "$_d" = "$DIR" ] && continue
		[ -f "$_d/config.json" ] || continue
		if [ -f "$CONF" ]; then
			warn "发现旧版目录 $_d（新配置已存在，未迁移）"
			continue
		fi
		info "发现旧版安装 $_d，迁移配置到 $CONF"
		mkdir -p "$DIR" && chmod 700 "$DIR"
		cp "$_d/config.json" "$CONF" && chmod 600 "$CONF"
		_old=$(basename "$_d")
		if [ "$INIT" = systemd ] && [ -f "/etc/systemd/system/$_old.service" ]; then
			systemctl disable --now "$_old" >/dev/null 2>&1
			[ "$_old" != "$NAME" ] && rm -f "/etc/systemd/system/$_old.service"
			systemctl daemon-reload
		elif [ -x "/etc/init.d/$_old" ]; then
			"/etc/init.d/$_old" stop >/dev/null 2>&1
			[ "$_old" != "$NAME" ] && { "/etc/init.d/$_old" disable >/dev/null 2>&1; rm -f "/etc/init.d/$_old"; }
		fi
		if confirm "删除旧目录 $_d？" y; then
			rm -rf "$_d"
		else
			info "旧目录保留在 $_d"
		fi
	done
}

ask_proxy() {
	[ -n "$PROXY_SET" ] && return
	[ -n "$YES" ] && return
	echo "如果访问 GitHub 很慢或失败，可以填一个 GitHub 加速代理前缀，例如 https://ghfast.top/"
	ask "GitHub 代理（留空直连）" "$GH_PROXY"
	GH_PROXY=$(normalize_proxy "$REPLY") || die "代理地址无效"
}

cmd_install() {
	need_root; detect_init; load_settings
	if [ -x "$BIN" ]; then
		info "已经安装在 $DIR，改为执行升级"
		cmd_upgrade
		return
	fi
	ask_proxy
	pikvm_rw
	check_space
	download_release
	migrate_old
	install_files
	save_settings
	if [ ! -f "$CONF" ]; then
		"$BIN" init -c "$CONF" -listen "$LISTEN" >/dev/null || die "生成配置失败"
	fi
	write_service
	if [ "$INIT" != none ]; then
		start_and_check || warn "服务没有启动成功，用 xrpanel log 查看日志"
	fi
	echo
	info "安装完成：$NEWVER"
	echo "以后用 ${C_B}xrpanel${C_0} 管理（查看设置、重置密码、升级、卸载等）"
	echo
	cmd_info
	[ -n "$PIKVM_RW" ] && warn "PiKVM 根分区平时是只读的：在网页里保存配置前请先执行 rw，保存后执行 ro"
	return 0
}

cmd_upgrade() {
	need_root; detect_init; load_settings
	[ -x "$BIN" ] || die "还没有安装（先运行 install）"
	_cur=$("$BIN" version 2>/dev/null | awk '{print $2}')
	pikvm_rw
	download_release
	if [ "$_cur" = "$NEWVER" ] && [ -z "$FORCE" ]; then
		info "已经是最新版本 $_cur（--force 可强制重装）"
		install_ctl_only
		return 0
	fi
	cp "$BIN" "$TMP/old-bin"
	svc stop
	install_files
	write_service
	if [ "$INIT" != none ] && ! start_and_check; then
		err "新版本启动失败，回滚到 $_cur"
		cp "$TMP/old-bin" "$BIN.new" && mv -f "$BIN.new" "$BIN"
		start_and_check || true
		exit 1
	fi
	info "升级完成：$_cur → $NEWVER"
	return 0
}
install_ctl_only() {
	if [ -f "$TMP/x/install.sh" ]; then
		cp "$TMP/x/install.sh" "$CTL.new" && chmod 755 "$CTL.new" && mv -f "$CTL.new" "$CTL"
	fi
}

cmd_uninstall() {
	need_root; detect_init
	[ -e "$BIN" ] || [ -e "/etc/init.d/$NAME" ] || [ -e "/etc/systemd/system/$NAME.service" ] || die "没有安装"
	confirm "确定卸载 xrayclient-reversepanel？" || exit 0
	pikvm_rw
	remove_service
	rm -f "$LINK_DIR/xrpanel"
	case "$DIR" in
		*/"$NAME") ;;
		*) die "安装目录 $DIR 看起来不对，为安全起见不删除，请手动处理" ;;
	esac
	if [ -n "$PURGE" ] || confirm "同时删除配置和证书（整个 $DIR 目录）？" n; then
		rm -rf "$DIR"
		info "已卸载，并删除了 $DIR"
	else
		rm -f "$BIN" "$CTL" "$DIR/LICENSE" "$DIR/README.md"
		info "已卸载，配置保留在 $DIR（重新安装会继续使用）"
	fi
}

# ---------------- 管理 ----------------
need_installed() { [ -x "$BIN" ] || die "还没有安装（先运行 install）"; }
cur_listen() { sed -n 's/.*"listen": *"\([^"]*\)".*/\1/p' "$CONF" 2>/dev/null | head -n 1; }

cmd_info() {
	need_root; need_installed; detect_init
	"$BIN" info -c "$CONF"
	echo "安装目录:    $DIR（备份打包这个目录即可）"
	echo "运行状态:    $(state_text)"
	if [ "$(cur_listen | cut -d: -f1)" = "0.0.0.0" ] || [ "$(cur_listen | cut -c1-3)" = "[::" ]; then
		echo "提示:        浏览器会提示证书不安全（自签名），核对上面的指纹后继续即可"
	fi
}

state_text() {
	_pid=$(get_pid)
	if [ -n "$_pid" ]; then
		printf '%s' "${C_G}运行中${C_0} (PID $_pid)"
	elif [ -x "$BIN" ]; then
		printf '%s' "${C_R}未运行${C_0}"
	else
		printf '%s' "未安装"
	fi
}

cmd_status() {
	need_installed; detect_init
	echo "版本:     $("$BIN" version 2>/dev/null)"
	echo "服务:     $INIT"
	echo "状态:     $(state_text)"
	_pid=$(get_pid)
	if [ -n "$_pid" ] && [ -r "/proc/$_pid/status" ]; then
		_rss=$(awk '/^VmRSS/ {print $2}' "/proc/$_pid/status")
		_thr=$(awk '/^Threads/ {print $2}' "/proc/$_pid/status")
		echo "内存:     $((_rss / 1024)) MB (RSS)   线程: $_thr"
		if [ -r "/proc/$_pid/stat" ] && [ -r /proc/uptime ]; then
			_start=$(awk '{print $22}' "/proc/$_pid/stat")
			_hz=$(getconf CLK_TCK 2>/dev/null || echo 100)
			_up=$(awk -v s="$_start" -v hz="$_hz" '{printf "%d", $1 - s / hz}' /proc/uptime)
			echo "已运行:   $((_up / 86400))天 $((_up % 86400 / 3600))时 $((_up % 3600 / 60))分"
		fi
	fi
	echo "网页监听: $(cur_listen)（xrpanel info 查看登录地址）"
	echo "---- 最近日志 ----"
	show_log 15
}

show_log() {
	_n="${1:-100}"
	case "$INIT" in
		systemd) journalctl -u "$NAME" -n "$_n" --no-pager 2>/dev/null ;;
		procd) logread -e "$NAME" 2>/dev/null | tail -n "$_n" ;;
		openrc) tail -n "$_n" "/var/log/$NAME.log" 2>/dev/null ;;
		*) echo "（没有服务管理器的日志）" ;;
	esac
}

restart_service() {
	if [ "$INIT" = none ]; then warn "没有服务管理器，请手动重启程序"; return; fi
	if start_and_check; then info "已重启"; else err "启动失败，用 xrpanel log 查看日志"; fi
}

cmd_restart() { need_root; need_installed; detect_init; restart_service; }
cmd_stop() { need_root; detect_init; svc stop && info "已停止"; }
cmd_start() { need_root; need_installed; detect_init; start_and_check && info "已启动" || err "启动失败，用 xrpanel log 查看日志"; }

cmd_reset_password() {
	need_root; need_installed; detect_init; pikvm_rw
	_cu=$(sed -n 's/.*"user": *"\([^"]*\)".*/\1/p' "$CONF" | head -n 1)
	ask "用户名" "${_cu:-admin}"
	_u=$REPLY
	if [ -n "$YES" ]; then REPLY=""; else ask_secret "新密码（至少 6 位，直接回车随机生成）"; fi
	_p=$REPLY
	if [ -n "$_p" ]; then
		ask_secret "再输入一次"
		[ "$REPLY" = "$_p" ] || die "两次输入不一致"
	fi
	printf '%s\n' "$_p" | "$BIN" reset-password -c "$CONF" -user "$_u" -password - || exit 1
	restart_service
}

cmd_set_listen() {
	need_root; need_installed; detect_init; pikvm_rw
	_cur=$(cur_listen)
	_addr="${1:-}"
	if [ -z "$_addr" ]; then
		_h=${_cur%:*}; _p=${_cur##*:}
		echo "0.0.0.0 = 所有 IPv4；:: = 所有 IPv4+IPv6；127.0.0.1 = 仅本机"
		ask "监听 IP" "${_h:-0.0.0.0}"; _h=$REPLY
		ask "端口" "${_p:-7400}"; _p=$REPLY
		case "$_h" in *:*) case "$_h" in \[*) ;; *) _h="[$_h]" ;; esac ;; esac
		_addr="$_h:$_p"
	fi
	"$BIN" set-listen -c "$CONF" "$_addr" || exit 1
	restart_service
}

cmd_gen_cert() {
	need_root; need_installed; detect_init; pikvm_rw
	"$BIN" gen-cert -c "$CONF" || exit 1
	restart_service
}

cmd_set_proxy() {
	need_root
	load_settings
	if [ -n "${1:-}" ]; then _new=$1; else
		echo "GitHub 代理前缀，用于安装和升级时下载，如 https://ghfast.top/ ；输入 none 清除"
		ask "代理" "$GH_PROXY"; _new=$REPLY
	fi
	[ "$_new" = none ] && _new=""
	GH_PROXY=$(normalize_proxy "$_new") || die "代理地址无效"
	if [ -n "$GH_PROXY" ]; then
		mk_tmp
		if fetch "$(rel_url VERSION)" "$TMP/VERSION" && grep -q '^v[0-9]' "$TMP/VERSION"; then
			info "代理可用（最新版本 $(cat "$TMP/VERSION")）"
		else
			warn "通过这个代理下载失败，仍然保存"
		fi
	fi
	save_settings && info "已保存：${GH_PROXY:-直连}"
}

usage() {
	cat <<EOF
xrayclient-reversepanel 安装 / 管理脚本

用法: xrpanel [命令] [选项]      （不带命令时打开菜单）

命令:
  install            安装（已安装则升级）
  upgrade            升级到最新版本（--version 指定版本，--force 强制重装）
  uninstall          卸载（--purge 同时删除配置）
  info               查看当前设置：登录地址、账号密码、证书
  status             运行状态、内存、最近日志
  restart|start|stop 重启 / 启动 / 停止客户端
  reset-password     重置网页账号密码
  set-listen [地址]  设置网页监听地址，如 0.0.0.0:7400
  gen-cert           重新生成 HTTPS 自签名证书
  set-proxy [前缀]   设置 GitHub 代理（none 清除）
  log [行数]         查看日志

选项:
  --proxy URL        GitHub 代理前缀，如 https://ghfast.top/（也可用环境变量 GH_PROXY）
  --listen 地址      安装时网页监听地址（默认 $DEFAULT_LISTEN）
  --version vX.Y.Z   安装/升级到指定版本
  --arch 架构        手动指定 CPU 架构
  -y, --yes          不询问，全部使用默认值
  --purge            卸载时删除配置
  --force            升级时即使版本相同也重装

安装目录: $DIR（可用环境变量 XRP_DIR 修改）
EOF
}

menu() {
	detect_init
	while :; do
		load_settings 2>/dev/null
		_v="未安装"; [ -x "$BIN" ] && _v=$("$BIN" version 2>/dev/null | awk '{print $2}')
		echo
		echo "${C_B}======== xrayclient-reversepanel 管理 ========${C_0}"
		echo " 版本: $_v   状态: $(state_text)   代理: ${GH_PROXY:-直连}"
		echo "---------------------------------------------"
		echo "  1. 安装              2. 升级"
		echo "  3. 卸载"
		echo "  4. 查看当前设置（登录地址 / 账号密码）"
		echo "  5. 运行状态"
		echo "  6. 重启客户端        7. 停止      8. 启动"
		echo "  9. 重置账号密码"
		echo " 10. 设置网页监听地址"
		echo " 11. 重新生成 HTTPS 证书"
		echo " 12. 设置 GitHub 代理"
		echo " 13. 查看日志"
		echo "  0. 退出"
		ask "请选择" ""
		case "$REPLY" in
			1) (cmd_install) ;;
			2) (cmd_upgrade) ;;
			3) (cmd_uninstall) ;;
			4) (cmd_info) ;;
			5) (cmd_status) ;;
			6) (cmd_restart) ;;
			7) (cmd_stop) ;;
			8) (cmd_start) ;;
			9) (cmd_reset_password) ;;
			10) (cmd_set_listen) ;;
			11) (cmd_gen_cert) ;;
			12) (cmd_set_proxy) ;;
			13) (detect_init; show_log 100) ;;
			0|q|"") exit 0 ;;
			*) warn "无效选项" ;;
		esac
	done
}

# ---------------- 参数 ----------------
CMD=""
ARG1=""
while [ $# -gt 0 ]; do
	case "$1" in
		--proxy|-p) [ $# -ge 2 ] || die "$1 需要参数"; GH_PROXY=$2; PROXY_SET=1; shift ;;
		--proxy=*) GH_PROXY=${1#*=}; PROXY_SET=1 ;;
		--listen) [ $# -ge 2 ] || die "$1 需要参数"; LISTEN=$2; shift ;;
		--listen=*) LISTEN=${1#*=} ;;
		--version) [ $# -ge 2 ] || die "$1 需要参数"; VER=$2; shift ;;
		--version=*) VER=${1#*=} ;;
		--arch) [ $# -ge 2 ] || die "$1 需要参数"; ARCH=$2; shift ;;
		--arch=*) ARCH=${1#*=} ;;
		-y|--yes) YES=1 ;;
		--purge) PURGE=1 ;;
		--force) FORCE=1 ;;
		-h|--help|help) CMD=help ;;
		-*) die "未知选项 $1（xrpanel help 查看用法）" ;;
		*) if [ -z "$CMD" ]; then CMD=$1; else ARG1=$1; fi ;;
	esac
	shift
done
case "$VER" in ""|v[0-9]*) ;; *) VER="v$VER" ;; esac
case "$ARCH" in ""|amd64|386|arm64|armv7|armv6|armv5|mips|mipsle|mips64|mips64le|riscv64|loong64) ;; *) die "不支持的架构 $ARCH" ;; esac
if [ -n "$PROXY_SET" ]; then GH_PROXY=$(normalize_proxy "$GH_PROXY") || die "代理地址无效: $GH_PROXY"; fi

case "$CMD" in
	"") menu ;;
	install) cmd_install ;;
	upgrade|update) cmd_upgrade ;;
	uninstall|remove) cmd_uninstall ;;
	info|show) cmd_info ;;
	status) detect_init; cmd_status ;;
	restart) cmd_restart ;;
	start) cmd_start ;;
	stop) cmd_stop ;;
	reset-password|passwd) cmd_reset_password ;;
	set-listen|listen) cmd_set_listen "$ARG1" ;;
	gen-cert) cmd_gen_cert ;;
	set-proxy|proxy) cmd_set_proxy "$ARG1" ;;
	log|logs) detect_init; show_log "${ARG1:-100}" ;;
	help) usage ;;
	*) err "未知命令 $CMD"; usage; exit 2 ;;
esac

#!/bin/sh
# 交叉编译全部发布包到 dist/：
#   sh scripts/build.sh v1.0.0            # 全部架构
#   sh scripts/build.sh v1.0.0 arm64 mipsle  # 只编译指定架构
# 产物：dist/xrayclient-reversepanel-linux-<arch>.tar.gz（含程序、install.sh、LICENSE、README.md）、
#       windows zip、sha256sums.txt、VERSION、install.sh
set -eu
cd "$(dirname "$0")/.."

VERSION="${1:-dev}"
[ $# -gt 0 ] && shift
NAME=xrayclient-reversepanel
OUT=dist
LDFLAGS="-s -w -buildid= -X main.version=$VERSION"

# 名称 GOOS GOARCH 额外环境变量
TARGETS="
amd64    linux   amd64
386      linux   386     GO386=softfloat
arm64    linux   arm64
armv7    linux   arm     GOARM=7
armv6    linux   arm     GOARM=6
armv5    linux   arm     GOARM=5
mips     linux   mips    GOMIPS=softfloat
mipsle   linux   mipsle  GOMIPS=softfloat
mips64   linux   mips64  GOMIPS64=softfloat
mips64le linux   mips64le GOMIPS64=softfloat
riscv64  linux   riscv64
loong64  linux   loong64
windows-amd64 windows amd64
windows-arm64 windows arm64
"

mkdir -p "$OUT"
rm -f "$OUT"/*.tar.gz "$OUT"/*.zip "$OUT/sha256sums.txt"
echo "$TARGETS" | while read -r arch goos goarch extra; do
	[ -z "$arch" ] && continue
	if [ $# -gt 0 ]; then
		ok=0
		for w in "$@"; do [ "$w" = "$arch" ] && ok=1; done
		[ $ok = 1 ] || continue
	fi
	echo ">> $arch"
	work="$OUT/build-$arch"
	rm -rf "$work" && mkdir -p "$work"
	exe="$NAME"
	[ "$goos" = windows ] && exe="$NAME.exe"
	env CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" $extra \
		go build -trimpath -ldflags "$LDFLAGS" -o "$work/$exe" .
	cp LICENSE README.md "$work/"
	if [ "$goos" = windows ]; then
		(cd "$work" && zip -q -9 "../$NAME-$arch.zip" "$exe" LICENSE README.md)
	else
		cp install.sh "$work/install.sh"
		chmod 755 "$work/$exe" "$work/install.sh"
		tar -C "$work" -czf "$OUT/$NAME-linux-$arch.tar.gz" --owner=0 --group=0 "$exe" install.sh LICENSE README.md 2>/dev/null ||
			tar -C "$work" -czf "$OUT/$NAME-linux-$arch.tar.gz" "$exe" install.sh LICENSE README.md
	fi
	rm -rf "$work"
done

cp install.sh "$OUT/install.sh"
printf '%s\n' "$VERSION" >"$OUT/VERSION"
(cd "$OUT" && for f in *.tar.gz *.zip install.sh; do [ -f "$f" ] && sha256sum "$f"; done |
	awk '{f=$2; sub(/^\*/, "", f); print $1 "  " f}' >sha256sums.txt)
ls -l "$OUT"

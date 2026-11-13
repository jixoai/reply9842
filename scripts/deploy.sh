#!/usr/bin/env bash
# 部署 reply9842-server 到远程主机
#
# ── 为什么需要这个脚本 ────────────────────────────────────────────────────
# 2026-11-13 事故：手工 `scp` 二进制到香港机，跨境链路慢，用 `timeout 50`
# 包住了 scp，于是传了 2.5MB / 9.9MB 就被杀掉。随后直接把它部署成 systemd
# 服务，得到的是：
#
#   Main process exited, code=killed, status=11/SEGV
#
# 而 `file` 仍报告 "ELF 64-bit LSB executable, x86-64, statically linked"——
# 因为 ELF header 在前 64 字节，完好无损；坏的是后面的代码段。这个组合
# 极具误导性：本地编译 + 本地跑完全正常，让人以为是交叉编译或 strip 的锅。
#
# 教训固化成本脚本的三条铁律：
#  1. 传输不设短 timeout（用后台跑 + 完成为止）
#  2. 传输后**必须**比对 sha256，不一致立即中止，绝不部署
#  3. 部署后验证服务真的起来了（systemctl is-active + 日志无 SEGV）
#
# 用法：
#   ./scripts/deploy.sh              # 默认目标 gaubee-cloud
#   ./scripts/deploy.sh <ssh-host>   # 指定 ssh 别名
#   ./scripts/deploy.sh --build-only # 只编译 + 打印校验和
set -euo pipefail

HOST="${1:-gaubee-cloud}"
if [ "$HOST" = "--build-only" ]; then
	HOST=""
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# 可选：绑定出站源地址。代理软件的 TUN 全局模式会把所有 IPv4 导进 utun，
# 连 ssh/scp 也会被劫持（实测表现为 Connection closed / kex 阶段断开）。
# 绑到物理网卡 IP 可绕开：
#   SSH_BIND=192.168.1.106 ./scripts/deploy.sh gaubee-cloud
SSH_OPTS=(-o BatchMode=yes)
# scp 与 ssh 的参数不同：ssh 用 -b，scp 只认 -o BindAddress=。
# 早先把 -b 直接塞进共享数组，scp 报 "illegal option -- b"（2026-11-13 实测）。
SCP_OPTS=(-o BatchMode=yes)
if [ -n "${SSH_BIND:-}" ]; then
	SSH_OPTS+=(-b "$SSH_BIND")
	SCP_OPTS+=(-o "BindAddress=${SSH_BIND}")
	echo "（绑定源地址 ${SSH_BIND}）"
fi

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
BIN_LOCAL="/tmp/reply9842-server-deploy"
GZ_LOCAL="/tmp/reply9842-server-deploy.gz"

echo "== 1/6 交叉编译 linux/amd64 (version=$VERSION) =="
# CGO_ENABLED=0 是必须的：macOS 上没有 linux 的 C 工具链，纯静态也更省心
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
	-o "$BIN_LOCAL" ./cmd/server
LOCAL_SIZE=$(stat -f%z "$BIN_LOCAL" 2>/dev/null || stat -c%s "$BIN_LOCAL")
LOCAL_SHA=$(shasum -a 256 "$BIN_LOCAL" 2>/dev/null | cut -d' ' -f1 || sha256sum "$BIN_LOCAL" | cut -d' ' -f1)
echo "   大小: $LOCAL_SIZE"
echo "   sha256: $LOCAL_SHA"

if [ -z "$HOST" ]; then
	echo "== --build-only，结束 =="
	exit 0
fi

echo "== 2/6 本地自检（能在 macOS 上跑吗——不行就跳过）=="
if "$BIN_LOCAL" --help >/dev/null 2>&1; then
	echo "   OK"
else
	# 跨架构产物在本机跑不了是正常的，用 file 兜底确认格式
	file "$BIN_LOCAL" | sed 's/^/   /'
fi

echo "== 3/6 压缩（gzip -9，跨境链路 40% 体积）=="
gzip -9 -c "$BIN_LOCAL" > "$GZ_LOCAL"
GZ_SHA=$(shasum -a 256 "$GZ_LOCAL" 2>/dev/null | cut -d' ' -f1 || sha256sum "$GZ_LOCAL" | cut -d' ' -f1)
GZ_SIZE=$(stat -f%z "$GZ_LOCAL" 2>/dev/null || stat -c%s "$GZ_LOCAL")
echo "   $GZ_SIZE 字节, sha256: $GZ_SHA"

echo "== 4/6 传输（不设短超时，慢就等）=="
# 注意：不用 timeout 包。跨境链路 40MB/s→40KB/s 都可能，掐断就是半截文件。
scp "${SCP_OPTS[@]}" "$GZ_LOCAL" "$HOST:/tmp/r9842-deploy.gz"

echo "== 5/6 远端解压 + 校验 sha256（不一致立即中止）=="
REMOTE_SHA=$(ssh "${SSH_OPTS[@]}" "$HOST" "sha256sum /tmp/r9842-deploy.gz | cut -d' ' -f1")
echo "   远端: $REMOTE_SHA"
if [ "$REMOTE_SHA" != "$GZ_SHA" ]; then
	echo "❌ 传输损坏：远端 sha256 与本地不一致，拒绝部署" >&2
	echo "   本地 $GZ_SHA" >&2
	echo "   远端 $REMOTE_SHA" >&2
	exit 1
fi
echo "   ✓ 校验通过"

ssh "${SSH_OPTS[@]}" "$HOST" '
	set -eu
	gunzip -f /tmp/r9842-deploy.gz
	chmod 0755 /tmp/r9842-deploy
	sudo systemctl stop reply9842-server 2>/dev/null || true
	sudo install -m 0755 -o root -g root /tmp/r9842-deploy /opt/reply9842/reply9842-server
	sudo systemctl reset-failed reply9842-server 2>/dev/null || true
	sudo systemctl start reply9842-server
'

echo "== 6/6 验证服务真的起来了 =="
sleep 3
ssh "${SSH_OPTS[@]}" "$HOST" '
	set -eu
	state=$(systemctl is-active reply9842-server 2>&1 || true)
	echo "   systemctl: $state"
	if [ "$state" != "active" ]; then
		echo "❌ 服务未 active，最近日志：" >&2
		sudo journalctl -u reply9842-server -n 20 --no-pager >&2 || true
		exit 1
	fi
	# 二次确认没在重启循环里（SEGV 时 is-active 可能是 activating）
	sleep 2
	n=$(sudo journalctl -u reply9842-server --since "1 min ago" --no-pager 2>/dev/null \
		| grep -c "SEGV" || true)
	if [ "$n" -gt 0 ]; then
		echo "❌ 检测到 $n 次 SEGV" >&2
		sudo journalctl -u reply9842-server -n 20 --no-pager >&2 || true
		exit 1
	fi
	echo "   ✓ 无 SEGV"
'

echo
echo "✅ 部署完成  version=$VERSION  sha256=$LOCAL_SHA"

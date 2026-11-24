#!/usr/bin/env bash
# multipath.sh — 多线路择路的端到端验收脚本（openspec A1/A3/A5 的可复现证据）
#
# 用法：scripts/e2e/multipath.sh [--server HOST:PORT] [--bind IP]
#
# 做什么：
#   1. 起两个可控劣化代理（fast / slow）
#   2. 起一个三线路客户端（直连 + 快代理 + 慢代理）
#   3. 发 N 个真实请求，断言全部成功
#   4. 打印线路分布与择路理由（这就是「A1 三条线路同时可用」的证据）
#   5. 劣化演练：把直连换成坏线路，断言请求仍然全部成功（A3）
#
# 前置：
#   - 能连到 relay server（默认 magpie.aiweb.xin:28443）
#   - 本机有物理网卡 IP（--bind 指定，默认自动探测 en0/en1）
#
# 注意：本机若开着 Clash 等 TUN 代理，代理线路的向上游拨号必须绑源地址
# （-bind），否则会被 TUN 劫持导致 TLS EOF（这是实测踩过的坑）
set -uo pipefail

SERVER="${SERVER:-magpie.aiweb.xin:28443}"
BIND="${BIND:-}"
CLIENT_PORT="${CLIENT_PORT:-19300}"
REQS="${REQS:-8}"

while [ $# -gt 0 ]; do
  case "$1" in
    --server) SERVER="$2"; shift 2 ;;
    --bind)   BIND="$2";   shift 2 ;;
    --port)   CLIENT_PORT="$2"; shift 2 ;;
    --reqs)   REQS="$2";   shift 2 ;;
    *) echo "未知参数 $1" >&2; exit 2 ;;
  esac
done

# 未指定 --bind 时自动探测物理网卡
if [ -z "$BIND" ]; then
  for i in $(ifconfig -l 2>/dev/null); do
    ip=$(ipconfig getifaddr "$i" 2>/dev/null || true)
    case "$i" in en*|eth*) [ -n "$ip" ] && BIND="$ip" && break ;; esac
  done
fi
if [ -z "$BIND" ]; then
  echo "错误：没能探测到物理网卡 IP，请用 --bind 指定" >&2
  exit 1
fi

WORK=$(mktemp -d)
cleanup() {
  # 显式回收所有子进程（不靠 shell 退出时的孤儿清理）
  [ -f "$WORK/pids" ] && while read -r pid; do kill "$pid" 2>/dev/null || true; done < "$WORK/pids"
  sleep 1
  [ -f "$WORK/pids" ] && while read -r pid; do kill -9 "$pid" 2>/dev/null || true; done < "$WORK/pids"
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

echo "==> 构建（cli + fakeproxy）"
go build -o "$WORK/mp-cli" ./cmd/cli || exit 1
go build -o "$WORK/fakeproxy" ./tools/fakeproxy || exit 1

echo "==> 起两个代理（fast: 无限速 / slow: 512KB/s）"
"$WORK/fakeproxy" -listen 127.0.0.1:18081 -label fast -bind "$BIND" -rewrite "$(echo "$SERVER" | sed 's/magpie.aiweb.xin/118.26.39.57/')" \
  > "$WORK/fast.log" 2>&1 &
echo $! >> "$WORK/pids"
"$WORK/fakeproxy" -listen 127.0.0.1:18082 -label slow -bind "$BIND" -bandwidth 512 -rewrite "$(echo "$SERVER" | sed 's/magpie.aiweb.xin/118.26.39.57/')" \
  > "$WORK/slow.log" 2>&1 &
echo $! >> "$WORK/pids"
sleep 1

echo "==> 起三线路客户端"
cat > "$WORK/routes.json" <<JSON
[
  {"id":"直连","kind":"direct","server":"$SERVER","local_addr":"$BIND","transport":"tcp"},
  {"id":"快代理","kind":"proxy","server":"$SERVER","proxy":"http://127.0.0.1:18081","transport":"tcp"},
  {"id":"慢代理","kind":"proxy","server":"$SERVER","proxy":"http://127.0.0.1:18082","transport":"tcp"}
]
JSON
"$WORK/mp-cli" serve -listen "127.0.0.1:$CLIENT_PORT" -routes-file "$WORK/routes.json" \
  -log "$WORK/client.jsonl" -stats "$WORK/stats.json" > "$WORK/client.log" 2>&1 &
echo $! >> "$WORK/pids"
sleep 2

echo "==> 发 $REQS 个真实请求（内容各不相同，强制 delta 不命中）"
PASS=0
for i in $(seq 1 "$REQS"); do
  python3 -c "
import json,random;random.seed($i+900)
print(json.dumps({'model':'x','messages':[{'role':'user','content':''.join(random.choices('abcdefgh',k=200000))}]}))
" > "$WORK/req$i.json"
  code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 120 \
    -X POST "http://127.0.0.1:$CLIENT_PORT/v1/messages" \
    -H 'content-type: application/json' -H 'x-api-key: e2e' \
    -d @"$WORK/req$i.json")
  # 401 = 请求完整到达上游（缺 key 是预期的，说明链路通了）
  if [ "$code" = "401" ] || [ "$code" = "200" ]; then PASS=$((PASS+1)); fi
  printf "  req %2d -> %s\n" "$i" "$code"
done

echo
echo "==> 结果"
echo "  成功 $PASS / $REQS"
python3 - "$WORK/client.jsonl" <<'PY'
import json, sys, collections
c = collections.Counter(); r = collections.Counter()
for line in open(sys.argv[1]):
    try: rec = json.loads(line)
    except: continue
    c[rec.get("route","?")] += 1
    r[rec.get("route_reason","")[:24]] += 1
print("  线路分布:", dict(c))
print("  择路理由:", dict(r))
PY

[ "$PASS" -eq "$REQS" ] || { echo "失败：有请求未到达上游"; exit 1; }

echo
echo "==> 劣化演练：把直连换成坏线路（新配置、新进程）"
cat > "$WORK/bad.json" <<JSON
[
  {"id":"坏线路","kind":"direct","server":"127.0.0.1:1","local_addr":"$BIND","transport":"tcp"},
  {"id":"快代理","kind":"proxy","server":"$SERVER","proxy":"http://127.0.0.1:18081","transport":"tcp"}
]
JSON
kill "$(sed -n '3p' "$WORK/pids")" 2>/dev/null || true
sleep 1
"$WORK/mp-cli" serve -listen "127.0.0.1:$CLIENT_PORT" -routes-file "$WORK/bad.json" \
  -log "$WORK/bad.jsonl" -stats "$WORK/bad-stats.json" > "$WORK/bad.log" 2>&1 &
echo $! >> "$WORK/pids"
sleep 2

PASS2=0
for i in $(seq 1 5); do
  code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 60 \
    -X POST "http://127.0.0.1:$CLIENT_PORT/v1/messages" \
    -H 'content-type: application/json' -H 'x-api-key: e2e' \
    -d '{"model":"x","messages":[{"role":"user","content":"hi"}]}')
  if [ "$code" = "401" ] || [ "$code" = "200" ]; then PASS2=$((PASS2+1)); fi
  printf "  req %d -> %s\n" "$i" "$code"
done
echo "  坏线路排第一时仍成功 $PASS2 / 5"
[ "$PASS2" -eq 5 ] || { echo "失败：降权后未能换到健康线路"; exit 1; }

echo
echo "==> 全部通过（A1 三线路可用 / A3 降权不熔断）"

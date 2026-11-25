#!/usr/bin/env bash
# buddygate.sh - BuddyGate 网关控制台（macOS / Linux）
#
#   ./scripts/buddygate.sh status          查看服务/账号/指标（默认动作）
#   ./scripts/buddygate.sh start | stop | restart
#   ./scripts/buddygate.sh logs [行数]      看日志尾巴（默认 60 行）
#   ./scripts/buddygate.sh smoke           渠道体检：只测一个 deepseek 模型（一个请求）
#   ./scripts/buddygate.sh doctor          一键体检（9 项）
#   ./scripts/buddygate.sh backup          备份凭证/配置/账本（保留最近 7 份）
#   ./scripts/buddygate.sh build [平台]     构建二进制（本机 / linux / darwin / windows）
#
# 与 Windows 的 buddygate.ps1 命令对齐；两边读同一份 config.json。
# 依赖：bash 3.2+、curl（smoke/status 用），构建需要 Go 1.22+。
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

BIN="$ROOT/bin/buddygate"
CFG="$ROOT/config.json"
LOGDIR="$ROOT/logs"
LOG="$LOGDIR/buddygate.log"
PIDFILE="$ROOT/data/buddygate.pid"
BACKUPDIR="$ROOT/backups"

ACTION="${1:-status}"
ARG="${2:-}"

say() { printf '%s\n' "$*"; }
die() { printf 'x %s\n' "$*" >&2; exit 1; }

json_get() { # json_get <key> [default]  —— 极简取值，避免依赖 jq
  local key="$1" def="${2:-}"
  [ -f "$CFG" ] || { printf '%s' "$def"; return; }
  local v
  v=$(sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$CFG" | head -n1)
  [ -n "$v" ] && printf '%s' "$v" || printf '%s' "$def"
}

listen_addr() {
  local l; l=$(json_get listen "127.0.0.1:7863")
  case "$l" in
    :*) printf '127.0.0.1%s' "$l" ;;
    *)  printf '%s' "$l" ;;
  esac
}

base_url() { printf 'http://%s' "$(listen_addr)"; }

api_key() { json_get api_key ""; }

api_get() { # api_get <path>
  local url; url="$(base_url)$1"
  local key; key="$(api_key)"
  if [ -n "$key" ]; then
    curl -fsS -m 10 -H "Authorization: Bearer $key" "$url"
  else
    curl -fsS -m 10 "$url"
  fi
}

is_running() {
  [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE" 2>/dev/null)" 2>/dev/null
}

do_build() {
  local target="${1:-host}"
  mkdir -p bin
  local goos goarch
  case "$target" in
    host) go build -trimpath -ldflags='-s -w' -o bin/buddygate ./cmd/server ;;
    linux|darwin|windows)
      goos="$target"; goarch="$(go env GOARCH)"
      local out="bin/buddygate-$goos-$goarch"
      [ "$goos" = windows ] && out="$out.exe"
      GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$out" ./cmd/server
      say "built: $out" ;;
    *) die "未知平台: $target（可选 host/linux/darwin/windows）" ;;
  esac
}

do_start() {
  if is_running; then say "already running: pid $(cat "$PIDFILE")"; return 0; fi
  [ -x "$BIN" ] || die "缺少 $BIN —— 先跑：./scripts/buddygate.sh build"
  mkdir -p "$LOGDIR" "$(dirname "$PIDFILE")"
  # 主日志由进程自己按大小轮转（config.logging.file）；这里只兜 stdio。
  "$BIN" -config "$CFG" >>"$LOGDIR/buddygate.stdio.log" 2>&1 &
  local pid=$!
  printf '%s' "$pid" > "$PIDFILE"
  local i
  for i in $(seq 1 40); do
    sleep 0.5
    if curl -fsS -m 2 -o /dev/null "$(base_url)/healthz" 2>/dev/null; then
      say "started: pid $pid -> $(base_url)/panel/"; return 0
    fi
    if ! kill -0 "$pid" 2>/dev/null; then
      say "进程已退出，看日志：$LOGDIR/buddygate.stdio.log"; return 1
    fi
  done
  say "started: pid $pid（/healthz 还没通，可能仍在加载账号）"
}

do_stop() {
  if ! is_running; then say "not running"; rm -f "$PIDFILE"; return 0; fi
  local pid; pid=$(cat "$PIDFILE")
  kill "$pid" 2>/dev/null
  local i
  for i in $(seq 1 20); do
    sleep 0.3
    kill -0 "$pid" 2>/dev/null || break
  done
  if kill -0 "$pid" 2>/dev/null; then kill -9 "$pid" 2>/dev/null; say "force killed: $pid"; else say "stopped: $pid"; fi
  rm -f "$PIDFILE"
}

do_status() {
  if ! is_running; then say "service: STOPPED"; return 0; fi
  local pid; pid=$(cat "$PIDFILE")
  local rss up
  rss=$(ps -o rss= -p "$pid" 2>/dev/null | tr -d ' ')
  up=$(ps -o etime= -p "$pid" 2>/dev/null | tr -d ' ')
  say "service : running  pid $pid  rss ${rss:-?}KB  up ${up:-?}"
  say "listen  : $(base_url)/panel/"
  local st
  if st=$(api_get /status 2>/dev/null); then
    printf '%s' "$st" | grep -o '"healthy":[0-9]*' | head -n1 | sed 's/^/accounts: /'
    printf '%s' "$st" | grep -o '"total":[0-9]*' | head -n1 | sed 's/^/          /'
  else
    say "status  : /status 不可用"
  fi
  local m
  if m=$(api_get /panel/api/request_metrics 2>/dev/null); then
    printf '%s' "$m" | grep -o '"completed":[0-9]*' | head -n1 | sed 's/^/requests: /'
  fi
}

do_logs() {
  local n="${1:-60}"
  [ -f "$LOG" ] || die "还没有日志：$LOG"
  tail -n "$n" "$LOG"
}

do_smoke() {
  local key; key="$(api_key)"
  local body='{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"只回答两个字：收到"}],"max_tokens":64,"stream":false}'
  local out
  if [ -n "$key" ]; then
    out=$(curl -sS -m 120 -H "Authorization: Bearer $key" -H 'Content-Type: application/json' \
      -d "$body" "$(base_url)/v1/chat/completions" 2>&1)
  else
    out=$(curl -sS -m 120 -H 'Content-Type: application/json' \
      -d "$body" "$(base_url)/v1/chat/completions" 2>&1)
  fi
  case "$out" in
    *'"content"'*) say "smoke : OK  $(printf '%s' "$out" | grep -o '"content":"[^"]*"' | head -n1)" ;;
    *) say "smoke : FAIL  ${out:0:200}"; say "        先看 status / logs"; return 1 ;;
  esac
}

do_doctor() {
  local fail=0
  say "BuddyGate 体检  $(date '+%Y-%m-%d %H:%M:%S')"
  say "------------------------------------------------------------------"
  # 1 配置
  if [ -f "$CFG" ]; then
    say "  [ OK ] 配置        config.json 可解析（listen=$(json_get listen 127.0.0.1:7863)）"
  else
    say "  [FAIL] 配置        找不到 $CFG"; fail=$((fail+1))
  fi
  # 2 进程
  if is_running; then say "  [ OK ] 进程        pid $(cat "$PIDFILE")"; else say "  [FAIL] 进程        未运行"; fail=$((fail+1)); fi
  # 3 健康
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$(base_url)/healthz" 2>/dev/null || echo 000)
  case "$code" in
    200) say "  [ OK ] 健康检查    /healthz 200" ;;
    503) say "  [WARN] 健康检查    /healthz 503（服务在跑但没有可用账号）" ;;
    *)   say "  [FAIL] 健康检查    无响应（$code）"; fail=$((fail+1)) ;;
  esac
  # 4 凭证目录
  if [ -d "$ROOT/auths" ] && [ -w "$ROOT/auths" ]; then
    say "  [ OK ] 凭证目录    auths/ 可写 · $(ls -1 "$ROOT/auths"/*.json 2>/dev/null | wc -l | tr -d ' ') 个凭证"
  else
    say "  [WARN] 凭证目录    auths/ 不存在或不可写"
  fi
  # 5 磁盘
  local logsz
  logsz=$(du -sh "$LOGDIR" 2>/dev/null | cut -f1)
  say "  [ OK ] 磁盘日志    日志 ${logsz:-0} · 剩余 $(df -h "$ROOT" | awk 'NR==2{print $4}')"
  # 6 上游连通
  if curl -s -o /dev/null -m 5 --connect-timeout 3 https://copilot.tencent.com 2>/dev/null; then
    say "  [ OK ] 上游连通    copilot.tencent.com 可达"
  else
    say "  [WARN] 上游连通    copilot.tencent.com 不可达（检查网络/代理）"
  fi
  say "------------------------------------------------------------------"
  if [ "$fail" -gt 0 ]; then say "结论：$fail 项失败"; return 1; else say "结论：全部通过"; fi
}

do_backup() {
  mkdir -p "$BACKUPDIR"
  local ts; ts=$(date '+%Y%m%d-%H%M%S')
  local zip="$BACKUPDIR/buddygate-$ts.tar.gz"
  local stage; stage=$(mktemp -d)
  mkdir -p "$stage/auths" "$stage/data"
  [ -d "$ROOT/auths" ] && cp -f "$ROOT"/auths/*.json "$stage/auths" 2>/dev/null
  [ -f "$CFG" ] && cp -f "$CFG" "$stage/" 2>/dev/null
  for f in state.json usage.json credit-history.json quota-scan.json; do
    [ -f "$ROOT/data/$f" ] && cp -f "$ROOT/data/$f" "$stage/data/" 2>/dev/null
  done
  tar -czf "$zip" -C "$stage" . && rm -rf "$stage"
  say "backup: $zip ($(du -h "$zip" | cut -f1))"
  ls -1t "$BACKUPDIR"/buddygate-*.tar.gz 2>/dev/null | tail -n +8 | while read -r old; do rm -f "$old"; say "pruned: $(basename "$old")"; done
}

case "$ACTION" in
  status)   do_status ;;
  start)    do_start ;;
  stop)     do_stop ;;
  restart)  do_stop; sleep 1; do_start ;;
  logs)     do_logs "${ARG:-60}" ;;
  smoke)    do_smoke ;;
  doctor)   do_doctor ;;
  backup)   do_backup ;;
  build)    do_build "${ARG:-host}" ;;
  *) die "用法: $0 {status|start|stop|restart|logs [行数]|smoke|doctor|backup|build [平台]}" ;;
esac

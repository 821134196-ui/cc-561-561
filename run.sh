#!/usr/bin/env bash
# 一条命令启动外卖商户申诉与证据归档系统（含 SQLite + 本地附件 + 模拟平台/消息）。
set -euo pipefail
cd "$(dirname "$0")"

ADDR="${ADDR:-:8080}"
DATA_DIR="${DATA_DIR:-./data}"
mkdir -p "$DATA_DIR/uploads"

# 优先使用仓库自带的预编译二进制；否则尝试本机 Go（常见安装位置也会探测）。
GO_BIN="$(command -v go 2>/dev/null || true)"
if [ -z "$GO_BIN" ] && [ -x "$HOME/go-sdk/go/bin/go" ]; then
  GO_BIN="$HOME/go-sdk/go/bin/go"
fi
if [ -z "$GO_BIN" ] && [ -x "/usr/local/go/bin/go" ]; then
  GO_BIN="/usr/local/go/bin/go"
fi

if [ -x "./bin/appeal" ]; then
  echo "▶ 使用预编译二进制启动 http://localhost${ADDR}"
  exec ./bin/appeal -addr "$ADDR" -data "$DATA_DIR"
elif [ -n "$GO_BIN" ]; then
  echo "▶ 使用 go run 启动 http://localhost${ADDR}"
  exec "$GO_BIN" run . -addr "$ADDR" -data "$DATA_DIR"
else
  echo "错误：未找到 ./bin/appeal 预编译文件，且本机没有 Go 工具链。" >&2
  echo "请安装 Go 1.21+ 后执行 go build -o bin/appeal ." >&2
  exit 1
fi

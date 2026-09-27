#!/bin/bash
set -e
cd "$(dirname "$0")/watcher"

echo "==> 检查 Go"
if ! command -v go >/dev/null 2>&1; then
  echo "未检测到 Go，请先安装："
  echo "  apt update && apt install -y golang-go"
  exit 1
fi

echo "==> 初始化 Go 模块（已存在则跳过）"
if [ ! -f go.mod ]; then
  go mod init watcher
fi

echo "==> 编译 watcher"
CGO_ENABLED=0 go build -o watcher .

echo "==> 完成：$(pwd)/watcher"
ls -lh watcher

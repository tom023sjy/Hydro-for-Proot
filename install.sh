#!/bin/bash
set -e
cd "$(dirname "$0")"

# 1. 装依赖
apt update
apt install -y golang-go nodejs npm g++ python3

echo "==> 1/2 编译 watcher"
bash build.sh

echo "==> 2/2 安装插件依赖"
cd plugin
npm install

echo ""
echo "======================================================"
echo " 部署完成（TMOE Debian 环境）"
echo ""
echo " 下一步："
echo "   1. 在 Hydro 启动脚本里加上："
echo "      export WATCHER_PATH=$(pwd)/../watcher/watcher"
echo "   2. 把 plugin 目录挂到 Hydro 的插件目录，"
echo "      或者在 Hydro 配置里指定加载路径。"
echo "======================================================"

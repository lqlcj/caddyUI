#!/usr/bin/env bash
#
# CaddyUI 一键卸载脚本
#
#   curl -fsSL https://raw.githubusercontent.com/lqlcj/CaddyUI/main/uninstall.sh | sudo bash
#
# 默认把 CaddyUI、Caddy、以及它们的数据全部删掉，包括已经申请下来的 HTTPS 证书。
# 想留着数据和证书（比如只是想重装）：
#
#   curl -fsSL .../uninstall.sh | sudo KEEP_DATA=1 bash

set -euo pipefail

KEEP_DATA="${KEEP_DATA:-0}"

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m警告:\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m错误:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "请用 root 运行：curl -fsSL .../uninstall.sh | sudo bash"

echo
if [ "$KEEP_DATA" = "1" ]; then
  warn "即将停止并卸载 CaddyUI / Caddy（保留数据库、Docker 应用配置和证书）"
else
  warn "即将删除下面这些东西，包含数据库和 HTTPS 证书，删了要重新申请证书："
  echo "    /var/lib/caddyui    面板数据库和 Docker 应用配置"
  echo "    /var/lib/relay      老版本 Relay 的数据库（如果还在）"
  echo "    /var/lib/caddy      Caddy 的证书和 ACME 账户密钥"
  echo "    /etc/caddy          引导配置"
  echo "    caddy 系统用户"
  echo
  echo "  想保留数据请按 Ctrl+C，改用：curl -fsSL .../uninstall.sh | sudo KEEP_DATA=1 bash"
fi
echo
echo "  5 秒后开始，按 Ctrl+C 取消……"
sleep 5
echo

# ---------- 服务 ----------

# relay 是老版本的服务名，一并清掉，免得留个抢端口的僵尸。
for unit in caddyui-docker caddyui relay caddy; do
  if systemctl list-unit-files "${unit}.service" >/dev/null 2>&1 \
     && systemctl cat "${unit}.service" >/dev/null 2>&1; then
    info "停止并禁用 ${unit}"
    systemctl disable --now "${unit}" >/dev/null 2>&1 || true
  fi
  rm -f "/etc/systemd/system/${unit}.service"
done

systemctl daemon-reload
systemctl reset-failed caddyui-docker caddyui relay caddy >/dev/null 2>&1 || true

# ---------- 二进制 ----------

info "删除面板二进制"
rm -f /usr/local/bin/caddyui /usr/local/bin/relay

# 升级助手和它的 sudo 授权。这两个一定要删干净：留着一条指向不存在脚本的
# sudoers 规则没有安全风险，但留着脚本本身就是留了一条没人管的提权入口。
info "删除升级助手与 sudo 授权"
rm -f /etc/sudoers.d/caddyui
rm -rf /usr/local/lib/caddyui

if [ -e /usr/bin/caddy ]; then
  OWNER=""
  command -v dpkg >/dev/null 2>&1 && OWNER="$(dpkg -S /usr/bin/caddy 2>/dev/null || true)"
  command -v rpm  >/dev/null 2>&1 && OWNER="${OWNER}$(rpm -qf /usr/bin/caddy 2>/dev/null || true)"
  if [ -n "$OWNER" ]; then
    warn "/usr/bin/caddy 是包管理器装的，没有删。要删请用 apt remove caddy / yum remove caddy"
  else
    info "删除 caddy 二进制"
    rm -f /usr/bin/caddy
  fi
fi

# ---------- 数据 ----------

if [ "$KEEP_DATA" = "1" ]; then
  info "保留 /var/lib/caddyui、/var/lib/relay、/var/lib/caddy、/etc/caddy 和 caddy 用户"
else
  info "删除配置与数据"
  rm -rf /etc/caddy /var/lib/caddyui /var/lib/relay /var/lib/caddy /run/caddy
  if id caddy >/dev/null 2>&1; then
    info "删除 caddy 用户"
    userdel caddy >/dev/null 2>&1 || warn "caddy 用户没删掉，可能还有进程在用，可稍后手动 userdel caddy"
  fi
fi

echo
info "卸载完成。"

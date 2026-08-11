#!/usr/bin/env bash
#
# CaddyUI —— Caddy 内核升级助手
#
# 这个脚本以 root 身份运行，由面板通过 sudo 调用。
#
# ─────────────────────────────────────────────────────────────────────────
# 为什么要单独搞一个脚本，而不是让面板自己去下载安装？
#
# 面板是以非特权的 caddy 用户跑的，它写不了 /usr/bin/caddy，也重启不了服务。
# 要做到「点一下就升级」，就必须给它一点特权。给多少、怎么给，是这里的关键：
#
#   本脚本不接受任何参数。
#
# 下载哪个仓库、什么版本、校验和对不对，全部由脚本自己决定，面板插不上手。
# 所以即使面板被完全攻破，攻击者能做的也只有「把 Caddy 升级到官方最新版」
# 这一件事 —— 这不是提权。
#
# 反过来，如果脚本设计成「装我给你的这个文件」，那面板就能喂给它任意二进制，
# 等于直接送 root。这条边界不能松。
#
# 配套的 sudoers 规则（/etc/sudoers.d/caddyui）也只放行这一个脚本：
#
#   caddy ALL=(root) NOPASSWD: /usr/local/lib/caddyui/upgrade-caddy.sh
#
# 另外，本脚本必须是 root:root 0755，所在目录也必须 root 所有 ——
# 只要 caddy 用户能改这个文件，上面那条 sudoers 规则就等于白送 root。
# install.sh 每次都会重新设置这些权限。
# ─────────────────────────────────────────────────────────────────────────

set -euo pipefail

# 固定 PATH，不用继承来的。
#
# 脚本要调 curl / tar / systemctl / sha512sum 等一堆外部命令，如果 PATH 能被
# 调用方左右，那随便放一个假的 curl 进去就是 root 代码执行。sudo 的 secure_path
# 默认会重置 PATH，但这是别人的配置，不该拿自己的安全性去赌。
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH

# 只认官方仓库，硬编码，不接受任何外部输入。
REPO="caddyserver/caddy"
API="https://api.github.com/repos/${REPO}/releases/latest"
DL_BASE="https://github.com/${REPO}/releases/download"

SERVICE=caddy

log()  { printf '%s\n' "$*"; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "必须以 root 运行（面板会通过 sudo 调用本脚本）"

command -v curl >/dev/null 2>&1 || die "需要 curl"
command -v tar  >/dev/null 2>&1 || die "需要 tar"

# ---------- 找 caddy 在哪 ----------
#
# 刻意不做成环境变量或参数：这个值最后会变成「往哪里写文件」，
# 调用方能控制它就等于能以 root 覆盖任意文件（把 /usr/bin/sudo 换掉之类）。
# 所以由脚本自己找，找不到就不干活。
CADDY_BIN=""
for c in /usr/bin/caddy /usr/local/bin/caddy; do
  if [ -x "$c" ]; then CADDY_BIN="$c"; break; fi
done
if [ -z "$CADDY_BIN" ]; then
  CADDY_BIN="$(command -v caddy 2>/dev/null || true)"
fi
[ -n "$CADDY_BIN" ] || die "找不到 caddy 可执行文件，无法升级"

# 顺着符号链接走到真身，免得把链接本身覆盖掉。
if [ -L "$CADDY_BIN" ]; then
  CADDY_BIN="$(readlink -f "$CADDY_BIN")" || die "解析 $CADDY_BIN 的符号链接失败"
fi
log "Caddy 位置：$CADDY_BIN"

SHA512=""
if command -v sha512sum >/dev/null 2>&1; then
  SHA512="sha512sum"
elif command -v shasum >/dev/null 2>&1; then
  SHA512="shasum -a 512"
else
  die "找不到 sha512sum，无法校验下载文件，拒绝继续"
fi

# ---------- 架构 ----------

case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  armv7l)        ARCH=armv7 ;;
  armv6l)        ARCH=armv6 ;;
  *) die "不支持的 CPU 架构: $(uname -m)" ;;
esac

# ---------- 别和包管理器打架 ----------
#
# 如果 caddy 是 apt/yum 装的，直接覆盖二进制会让包管理器的记录和实际文件对不上，
# 下次 apt upgrade 可能把它换回去、也可能报冲突。这种情况让用户自己走包管理器。

if [ -e "$CADDY_BIN" ]; then
  OWNER=""
  command -v dpkg >/dev/null 2>&1 && OWNER="$(dpkg -S "$CADDY_BIN" 2>/dev/null || true)"
  command -v rpm  >/dev/null 2>&1 && OWNER="${OWNER}$(rpm -qf "$CADDY_BIN" 2>/dev/null || true)"
  if [ -n "$OWNER" ]; then
    die "$CADDY_BIN 是包管理器安装的，请用 apt upgrade caddy / yum update caddy 升级，本脚本不动它"
  fi
fi

TMP="$(mktemp -d /var/tmp/caddyui-upgrade.XXXXXX)"
trap 'rm -rf "$TMP"' EXIT

# ---------- 查最新版本 ----------

log "正在查询 Caddy 官方最新版本……"
curl -fsSL --retry 2 --max-time 30 "$API" -o "$TMP/rel.json" \
  || die "连不上 GitHub API，检查服务器网络"

TAG="$(grep -oE '"tag_name" *: *"[^"]*"' "$TMP/rel.json" | head -1 | sed 's/.*"tag_name" *: *"//;s/"//')"
[ -n "$TAG" ] || die "没能从 GitHub 返回里解析出版本号"

# 版本号会被拼进下载 URL，必须严格校验形状。
# 这是防注入的关键一步：万一 API 返回了奇怪的东西，到这里就被拦住。
echo "$TAG" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+$' \
  || die "版本号格式不对（$TAG），拒绝继续"

VER="${TAG#v}"
log "最新版本：$TAG"

CURRENT=""
if [ -x "$CADDY_BIN" ]; then
  CURRENT="$("$CADDY_BIN" version 2>/dev/null | head -1 | awk '{print $1}' || true)"
  log "当前版本：${CURRENT:-未知}"
fi

if [ -n "$CURRENT" ] && [ "$CURRENT" = "$TAG" ]; then
  log "已经是最新版本，无需升级。"
  exit 0
fi

# ---------- 下载 ----------

TARBALL="caddy_${VER}_linux_${ARCH}.tar.gz"
SUMS="caddy_${VER}_checksums.txt"

log "下载 $TARBALL ……"
curl -fsSL --retry 2 --max-time 300 "${DL_BASE}/${TAG}/${TARBALL}" -o "$TMP/$TARBALL" \
  || die "下载失败：${DL_BASE}/${TAG}/${TARBALL}"

log "下载校验和文件……"
curl -fsSL --retry 2 --max-time 60 "${DL_BASE}/${TAG}/${SUMS}" -o "$TMP/$SUMS" \
  || die "下载校验和失败，拒绝安装未经校验的文件"

# ---------- 校验 ----------
#
# Caddy 官方发的是 SHA-512。校验和文件和 tarball 都来自 GitHub 的同一个
# release，走的都是 https，所以这一步主要防的是传输损坏和镜像投毒，
# 不是防 GitHub 本身。

log "校验 SHA-512 ……"
EXPECT="$(grep -E "  ${TARBALL}\$" "$TMP/$SUMS" | awk '{print $1}' | head -1)"
[ -n "$EXPECT" ] || die "校验和文件里没有 $TARBALL 这一项"

ACTUAL="$(cd "$TMP" && $SHA512 "$TARBALL" | awk '{print $1}')"
if [ "$EXPECT" != "$ACTUAL" ]; then
  die "校验和不匹配！文件可能损坏或被篡改，已放弃。期望 $EXPECT，实际 $ACTUAL"
fi
log "校验通过"

# ---------- 解包并试运行 ----------

tar -xzf "$TMP/$TARBALL" -C "$TMP" caddy || die "解包失败"
chmod +x "$TMP/caddy"

NEWVER="$("$TMP/caddy" version 2>/dev/null | head -1 | awk '{print $1}' || true)"
[ -n "$NEWVER" ] || die "新下载的二进制跑不起来（架构不对？），已放弃，没有动现有的 Caddy"
log "新二进制自报版本：$NEWVER"

# ---------- 备份 + 安装 ----------

BACKUP=""
if [ -e "$CADDY_BIN" ]; then
  BACKUP="${CADDY_BIN}.bak"
  cp -p "$CADDY_BIN" "$BACKUP" || die "备份现有二进制失败"
  log "已备份到 $BACKUP"
fi

# 先写同目录的临时文件再 mv：mv 在同一文件系统上是原子的，
# 不会出现「文件写了一半正好被执行」的情况。
install -m 0755 "$TMP/caddy" "${CADDY_BIN}.new" || die "写入新二进制失败"
mv -f "${CADDY_BIN}.new" "$CADDY_BIN" || die "替换二进制失败"
log "已安装到 $CADDY_BIN"

# ---------- 重启并确认 ----------

log "重启 $SERVICE ……"
if ! systemctl restart "$SERVICE"; then
  log "重启失败，正在回滚……"
  [ -n "$BACKUP" ] && cp -p "$BACKUP" "$CADDY_BIN" && systemctl restart "$SERVICE" || true
  die "新版本起不来，已回滚到升级前的版本"
fi

# 给它一点时间把端口和配置都拉起来。
sleep 3

if ! systemctl is-active --quiet "$SERVICE"; then
  log "服务没能保持运行，正在回滚……"
  if [ -n "$BACKUP" ]; then
    cp -p "$BACKUP" "$CADDY_BIN"
    systemctl restart "$SERVICE" || true
    sleep 2
    if systemctl is-active --quiet "$SERVICE"; then
      die "新版本起不来，已回滚，网站已恢复"
    fi
    die "新版本起不来，回滚后仍未恢复，请立即 SSH 上服务器执行：journalctl -u caddy -n 50"
  fi
  die "新版本起不来，且没有可回滚的备份"
fi

log "升级完成：${CURRENT:-未知} → $NEWVER"
log "Caddy 已用 --resume 恢复升级前的配置，站点不受影响。"
exit 0

#!/usr/bin/env bash
#
# CaddyUI 一键安装/修复 Docker 的 root 助手。
#
# 安全边界：脚本不接受任何参数，只安装 Docker 官方仓库中的 Engine、CLI、
# containerd、Buildx 和 Compose 插件，然后启动固定的两个服务。面板不能指定
# 下载地址、软件包、命令或脚本内容。

set -euo pipefail

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
info() { printf '==> %s\n' "$*"; }

[ "$#" -eq 0 ] || die "这个助手不接受参数"
[ "$(id -u)" -eq 0 ] || die "必须以 root 运行"
[ "$(uname -s)" = Linux ] || die "只支持 Linux"

export PATH=/usr/sbin:/usr/bin:/sbin:/bin
export LC_ALL=C
unset BASH_ENV ENV CDPATH GIT_CONFIG_GLOBAL GIT_CONFIG_SYSTEM PYTHONPATH PERL5LIB RUBYOPT \
  APT_CONFIG DNF_CONF YUM0 RPM_CONFIGDIR DOCKER_CONFIG CONTAINERD_NAMESPACE CONTAINERD_ADDRESS

if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  info "Docker 和 Compose 已安装，正在确保服务运行"
  systemctl enable --now docker
  docker version --format 'Docker {{.Server.Version}}'
  docker compose version
  exit 0
fi

command -v systemctl >/dev/null 2>&1 || die "系统不是 systemd，无法自动安装"
command -v curl >/dev/null 2>&1 || die "缺少 curl"

# shellcheck source=/dev/null
. /etc/os-release 2>/dev/null || die "无法识别 Linux 发行版"

case "${ID:-}" in
  ubuntu|debian)
    command -v apt-get >/dev/null 2>&1 || die "缺少 apt-get"
    info "配置 Docker 官方 APT 仓库"
    apt-get -o Dpkg::Use-Pty=0 update
    apt-get -o Dpkg::Use-Pty=0 install -y ca-certificates curl gnupg
    install -m 0755 -d /etc/apt/keyrings

    distro="$ID"
    codename="${VERSION_CODENAME:-}"
    [ -n "$codename" ] || die "无法识别发行版代号"
    curl -fsSL "https://download.docker.com/linux/${distro}/gpg" -o /etc/apt/keyrings/docker.asc
    chmod 0644 /etc/apt/keyrings/docker.asc
    arch="$(dpkg --print-architecture)"
    printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/%s %s stable\n' \
      "$arch" "$distro" "$codename" > /etc/apt/sources.list.d/docker.list
    apt-get -o Dpkg::Use-Pty=0 update
    apt-get -o Dpkg::Use-Pty=0 install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
    ;;
  centos|rhel|rocky|almalinux|fedora)
    if command -v dnf >/dev/null 2>&1; then pm=dnf; else pm=yum; fi
    info "配置 Docker 官方 RPM 仓库"
    "$pm" -y install dnf-plugins-core ca-certificates curl
    case "$ID" in
      fedora) repo=https://download.docker.com/linux/fedora/docker-ce.repo ;;
      *) repo=https://download.docker.com/linux/centos/docker-ce.repo ;;
    esac
    "$pm" config-manager --add-repo "$repo"
    "$pm" -y install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
    ;;
  raspbian)
    command -v apt-get >/dev/null 2>&1 || die "缺少 apt-get"
    info "配置 Docker 官方 Debian 仓库（Raspberry Pi OS）"
    apt-get -o Dpkg::Use-Pty=0 update
    apt-get -o Dpkg::Use-Pty=0 install -y ca-certificates curl gnupg
    install -m 0755 -d /etc/apt/keyrings
    curl -fsSL "https://download.docker.com/linux/debian/gpg" -o /etc/apt/keyrings/docker.asc
    chmod 0644 /etc/apt/keyrings/docker.asc
    codename="${VERSION_CODENAME:-}"
    [ -n "$codename" ] || die "无法识别发行版代号"
    arch="$(dpkg --print-architecture)"
    printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian %s stable\n' \
      "$arch" "$codename" > /etc/apt/sources.list.d/docker.list
    apt-get -o Dpkg::Use-Pty=0 update
    apt-get -o Dpkg::Use-Pty=0 install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
    ;;
  *)
    die "暂不支持自动安装到 ${PRETTY_NAME:-$ID}；请先用系统方式安装 Docker Engine 和 Compose V2"
    ;;
esac

info "启动 Docker"
systemctl enable --now docker
docker version --format 'Docker {{.Server.Version}}'
docker compose version
info "安装完成"

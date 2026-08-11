# Relay

超轻量反向代理面板。用起来像 Nginx Proxy Manager，但只有两个静态二进制文件。

填个域名，填个端口，点保存 —— HTTPS 证书自动申请、自动续期，不用碰任何配置文件。

```
┌──────────────┐   unix socket    ┌──────────────┐
│    relay     │ ───────────────► │    caddy     │ ◄── 80 / 443 真实流量
│   控制面      │   POST /load     │    数据面     │
│  (这个项目)   │                  │  (官方原版)   │
└──────┬───────┘                  └──────┬───────┘
       │                                 │
   relay.db                       /var/lib/caddy
  站点·账号·配置历史                  证书·ACME 账户
```

**面板挂了，反代照跑。** 这是刻意的设计：控制面和数据面是两个独立进程。面板崩溃、
升级、被 OOM Killer 杀掉，甚至数据库删了，正在跑的流量都不受任何影响 ——
Caddy 会用自己保存的配置继续工作。

---

## 一键安装

Linux + systemd，root 执行：

```bash
curl -fsSL https://raw.githubusercontent.com/lqlcj/caddyUI/main/install.sh | sudo bash
```

装完屏幕上会打出面板地址，形如 `http://服务器IP:81`。第一次打开会让你创建管理员账号，
然后「添加站点」填域名和转发目标，保存即可 —— 证书会自动签发。

- 面板端口是 **81**，云服务器记得在安全组里放行 **81 / 80 / 443**（后两个是证书和真实流量用的）
- 想换端口：把命令改成 `curl -fsSL https://raw.githubusercontent.com/lqlcj/caddyUI/main/install.sh | sudo PANEL_PORT=8081 bash`
- 重复执行这条命令 = 升级到最新版，数据不会丢
- 81 端口是**救援通道**：万一配置改崩、域名进不来面板了，还能靠它进来点回滚。
  建议在面板里加一条站点把面板自己反代成 HTTPS 域名，再用防火墙把 81 只放给自己的 IP

## 一键卸载

```bash
curl -fsSL https://raw.githubusercontent.com/lqlcj/caddyUI/main/uninstall.sh | sudo bash
```

停掉并删除 Relay、Caddy、systemd 单元、数据库和证书。执行前有 5 秒倒计时，
按 Ctrl+C 可以取消。想保留数据和证书（比如只是重装），在 `sudo` 后面加 `KEEP_DATA=1`。

---

## 为什么不直接用 Nginx Proxy Manager

NPM 是 Node.js + Express + React + nginx + certbot(Python) + s6-overlay 打成一个镜像，
光 Node 进程常驻就 150–250 MB。Relay 把这些全砍掉了：

|              | Nginx Proxy Manager  | Relay              |
| ------------ | -------------------- | ------------------ |
| 运行时       | Node.js + Python     | 两个静态二进制     |
| 证书管理     | certbot（独立进程）  | Caddy 内置，零配置 |
| 改配置       | 生成 nginx.conf → reload | Admin API 原子下发 |
| 配置写错了   | 可能 reload 失败留下不一致状态 | **整份拒绝，旧配置继续跑** |
| 依赖         | 一堆                 | 无                 |

## 实测资源占用

跑一个站点，空闲状态：

| 进程    | 工作集   | 说明                          |
| ------- | -------- | ----------------------------- |
| `caddy` | 31.2 MB  | `GOMEMLIMIT=64MiB GOGC=50`    |
| `relay` | 13.9 MB  | `GOMEMLIMIT=32MiB GOGC=50`    |
| **合计**| **~45 MB** | 128 MB 内存的小机器绰绰有余  |

> **`GOMEMLIMIT` 不是可选项。** 不设的话 `relay` 常驻 55 MB（Go 的 GC 不着急把
> 内存还给系统），设了之后 14 MB。仓库里的 systemd 单元已经写好了。

二进制体积：`relay` 13 MB（`-ldflags="-s -w"` 后），`caddy` 官方版约 45 MB。

---

## 工作原理

整个项目的核心就三步，加起来不到 200 行：

**1. 把数据库里的站点渲染成 Caddyfile**（`internal/caddy/render.go`）

```caddyfile
{
	admin unix//run/caddy/admin.sock
	email you@example.com
}

# 我的博客
blog.example.com, www.blog.example.com {
	reverse_proxy http://127.0.0.1:3000 {
		header_up X-Real-IP {remote_host}
	}
}
```

**2. POST 给 Caddy**（`internal/caddy/client.go`）

这一步是**原子的**。Caddy 会完整校验并 provision 新配置，任何一步出错就整份回滚，
旧配置继续跑。所以面板永远不可能把线上改成半残状态 —— 这是选 Caddy 而不是 nginx
最大的理由，省掉了 NPM 里一大坨「生成配置 → 语法检查 → reload → 失败了怎么办」的胶水代码。

**3. 每次下发都存一份快照**（`internal/store/versions.go`）

配置页有完整历史，点一下就回滚到任意一个成功过的版本。改崩了不用 SSH 上去翻文件。

### 安全上做了什么

- 域名和主机名入库前过**白名单正则**，空格、大括号、换行一律拒绝 —— 杜绝 Caddyfile 注入
- 「高级配置」是原样透传的（那是用户主动进专家模式），但会做大括号配平检查，
  真正的语法校验交给 Caddy，写错了也只是被拒绝，炸不了线上
- 登录限速：同 IP 5 分钟 10 次
- 所有 POST 校验会话内的 CSRF token + 同源检查
- CSP `default-src 'self'`，面板没有任何外部资源、没有内联脚本
- Admin API 走 unix socket，本机其它进程连端口都扫不到

---

## 目录结构

```
install.sh                   一键安装
uninstall.sh                 一键卸载
main.go                      启动、优雅退出、内嵌前端
internal/
  store/                     SQLite：用户、站点、配置版本、会话
  caddy/
    client.go                Admin API 客户端（unix socket / TCP 都支持）
    render.go                站点 → Caddyfile
  app/service.go             渲染 + 下发 + 记录版本 + 回滚
  web/                       路由、会话、CSRF、各页面 handler
web/
  templates/                 Go 模板，服务端渲染
  static/                    手写 CSS 4KB + JS 1.4KB，无框架无构建步骤
deploy/                      systemd 单元、引导配置
```

前端没有 node_modules，没有打包步骤，`go build` 一条命令出成品。

## 启动参数

| 参数      | 环境变量             | 默认值             | 说明                    |
| --------- | -------------------- | ------------------ | ----------------------- |
| `-listen` | `RELAY_LISTEN`       | `:81`              | 面板监听地址            |
| `-data`   | `RELAY_DATA_DIR`     | `./data`           | 数据库存放目录          |
| `-caddy`  | `RELAY_CADDY_ADMIN`  | `127.0.0.1:2019`   | Caddy Admin API 地址    |

`-caddy` 支持两种写法：`127.0.0.1:2019`（TCP）或 `unix//run/caddy/admin.sock`。

---

## 备份

要备份**两样东西**，缺一不可：`/var/lib/relay/relay.db`（站点、账号、配置历史）
和 `/var/lib/caddy`（Caddy 的证书和 ACME 账户密钥）。

第二项**特别容易被忘掉**。丢了会导致重装后所有证书要重新签发，而 Let's Encrypt
对同一组域名有**每周 5 张**的速率限制，撞上了就得干等几天。

## 常见问题

**证书申请不下来**
Caddy 用 HTTP-01 验证，需要 80 端口能被外网访问。检查域名解析有没有指到这台机器、
80 端口有没有被防火墙或云厂商安全组挡住。面板「配置」页能看到下发结果。

**通配符域名 `*.example.com` 证书签不出来**
通配符必须用 DNS-01 验证，需要对应 DNS 厂商的插件，得用 `xcaddy` 重新编译 Caddy，
然后在站点的「高级配置」里写上 `tls { dns cloudflare {env.CF_API_TOKEN} }`。
把这一步做成图形化配置在路线图里。

**面板显示「Caddy 未连接」**
多半是 Caddy 服务没起来，或者 admin 地址对不上 —— 面板的 `-caddy` 参数必须和
Caddy 实际监听的一致。一键安装脚本装出来的是匹配的。

**改配置把网站搞挂了**
不会。Caddy 拒绝坏配置时会原封不动保持旧配置。真出问题就去「配置」页点回滚。

**WebSocket 要不要单独配置**
不用。Caddy 的 `reverse_proxy` 默认就转发 WebSocket 和 HTTP/2，
不像 nginx 那样要手写 `Upgrade` / `Connection` 头。

**面板打不开**
先确认云厂商安全组放行了 81 端口 —— 这是最常见的原因。

---

## 路线图

已完成（v0.1）：

- [x] 站点增删改查、启用停用
- [x] 自动 HTTPS（申请 + 续期全自动）
- [x] 配置版本历史 + 一键回滚
- [x] HTTP Basic Auth 访问密码
- [x] 自定义 Caddyfile 片段（高级模式）
- [x] 启动自动同步、救援通道、登录限速、CSRF

接下来：

- [ ] 上传自有证书
- [ ] DNS-01 图形化配置（Cloudflare / DNSPod / 阿里云）
- [ ] 跳转站点、静态文件站点
- [ ] IP 黑白名单
- [ ] 访问日志与简单统计
- [ ] 证书到期提醒（邮件 / Telegram / Bark）
- [ ] 一键备份恢复
- [ ] 多上游 + 健康检查
- [ ] 两步验证、审计日志
- [ ] TCP/UDP 四层转发（需 caddy-l4 插件）

## 依赖

只有两个：

- `modernc.org/sqlite` —— 纯 Go 的 SQLite，不需要 CGO，交叉编译一条命令
- `golang.org/x/crypto` —— bcrypt（面板密码和站点访问密码共用）

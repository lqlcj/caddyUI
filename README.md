# CaddyUI

超轻量反向代理面板。用起来像 Nginx Proxy Manager，但只有两个静态二进制文件。

填个域名，填个端口，点保存 —— HTTPS 证书自动申请、自动续期，不用碰任何配置文件。

```
┌──────────────┐   unix socket    ┌──────────────┐
│   caddyui    │ ───────────────► │    caddy     │ ◄── 80 / 443 真实流量
│   控制面      │   POST /load     │    数据面     │
│  (这个项目)   │                  │  (官方原版)   │
└──────┬───────┘                  └──────┬───────┘
       │                                 │
  caddyui.db                      /var/lib/caddy
  站点·账号·配置历史                 证书·ACME 账户
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

装完屏幕上会打出面板地址，形如 `http://服务器IP:81`。第一次打开会让你**用邮箱**
创建管理员账号 —— 这个邮箱同时会被自动设成 HTTPS 证书的联系邮箱，不用再单独填。
然后「添加站点」填域名和转发目标，保存即可，证书会自动签发。

- 面板端口是 **81**，云服务器记得在安全组里放行 **81 / 80 / 443**（后两个是证书和真实流量用的）
- 想换端口：把命令改成 `curl -fsSL .../install.sh | sudo PANEL_PORT=8081 bash`
- 重复执行这条命令 = 升级到最新版，数据不会丢
- 81 端口是**救援通道**：万一配置改崩、域名进不来面板了，还能靠它进来点回滚。
  建议在面板里加一条站点把面板自己反代成 HTTPS 域名，再用防火墙把 81 只放给自己的 IP
- 安装脚本会写一条 sudo 授权 `/etc/sudoers.d/caddyui`，用途只有一个：让面板能
  点一下升级 Caddy 内核。它只放行一个 root 拥有、不接受任何参数的助手脚本，
  细节见下面「升级 Caddy 内核」。不想要这个功能就把那个文件删掉，
  面板会显示「一键升级不可用」，其它功能照常

### 从旧版 Relay 升级

直接跑上面那条安装命令就行。脚本会自动停掉 `relay` 服务、把
`/var/lib/relay/relay.db` 复制成 `/var/lib/caddyui/caddyui.db`、装上新的
`caddyui.service`。站点、账号、配置历史都在，旧目录原样保留着以防万一。

只有一点：会话 cookie 换了名字，**需要重新登录一次**。

## 一键卸载

```bash
curl -fsSL https://raw.githubusercontent.com/lqlcj/caddyUI/main/uninstall.sh | sudo bash
```

停掉并删除 CaddyUI、Caddy、systemd 单元、数据库和证书。执行前有 5 秒倒计时，
按 Ctrl+C 可以取消。想保留数据和证书（比如只是重装），在 `sudo` 后面加 `KEEP_DATA=1`。

---

## 功能

### 站点（七层反向代理）

域名 → 上游服务。自动 HTTPS、强制跳转、访问密码、自定义 Caddyfile 片段。

编辑站点时能直接看到**这个域名的证书和私钥在服务器上的绝对路径**，还有有效期和
颁发者。要把证书拿去给别的服务用（MySQL、邮件服务器、Nginx）时，点一下复制就行。
路径是扫描磁盘得到的真实文件，不是按规则拼出来的。

> 证书续期后文件**原地更新、路径不变**，所以引用它的程序记得配成定期重载。
> 私钥权限是 0600、属主 caddy，读它需要 root。

### 升级 Caddy 内核

设置页有个「升级 Caddy 内核」，点一下就把 Caddy 升到官方最新版。

只从 **Caddy 官方 GitHub 仓库** `caddyserver/caddy` 下载，装之前用官方发布的
**SHA-512 校验和**逐字节核对，对不上直接放弃。安装前会备份现有二进制并试运行
新版本，重启后服务没起来会自动回滚。升级只换 Caddy 那个可执行文件，
证书、站点配置、面板数据都不动 —— Caddy 用 `--resume` 启动，重启后自己恢复
升级前的配置。整个过程网站会中断几秒。

**这里有个权限问题值得说清楚。** 面板以非特权的 `caddy` 用户运行，它写不了
`/usr/bin/caddy`，也重启不了服务。要做到点一下就升级，就得给它一点特权 ——
给多少、怎么给是关键：

安装脚本会放一个 root 拥有的助手脚本 `/usr/local/lib/caddyui/upgrade-caddy.sh`，
并只给 caddy 用户放行这一个脚本的 sudo 权限。**这个脚本不接受任何参数** ——
下载哪个仓库、什么版本、校验和对不对，全部由脚本自己决定，面板插不上手。

所以这条授权给出去的能力只有一个：「把 Caddy 升级到官方最新版」。
即使面板被完全攻破，攻击者也只能触发这一件事，拿不到 root。

反过来，如果助手设计成「装我给你的这个文件」，面板就能喂给它任意二进制，
等于直接送 root。这条边界不能松，改那个脚本之前请先读它开头的注释。

装不上 sudo 授权的机器（没有 sudo、visudo 校验没过）会在设置页显示
「一键升级不可用」并给出手动命令，而不是给个点了会报错的按钮。
包管理器装的 Caddy 也会被助手拒绝接管，让你走 `apt upgrade caddy`。

### 界面

配色跟 Claude 一套：暖米白底 + 赤陶橙点缀。右上角可以切深色模式，
默认跟随系统。主题存在 cookie 里由服务端渲染，所以刷新页面不会先闪一下白底。

---

## 为什么不直接用 Nginx Proxy Manager

NPM 是 Node.js + Express + React + nginx + certbot(Python) + s6-overlay 打成一个镜像，
光 Node 进程常驻就 150–250 MB。CaddyUI 把这些全砍掉了：

|              | Nginx Proxy Manager  | CaddyUI            |
| ------------ | -------------------- | ------------------ |
| 运行时       | Node.js + Python     | 两个静态二进制     |
| 证书管理     | certbot（独立进程）  | Caddy 内置，零配置 |
| 改配置       | 生成 nginx.conf → reload | Admin API 原子下发 |
| 配置写错了   | 可能 reload 失败留下不一致状态 | **整份拒绝，旧配置继续跑** |
| 依赖         | 一堆                 | 无                 |

## 实测资源占用

跑一个站点，空闲状态：

| 进程      | 工作集   | 说明                          |
| --------- | -------- | ----------------------------- |
| `caddy`   | 31.2 MB  | `GOMEMLIMIT=64MiB GOGC=50`    |
| `caddyui` | 13.9 MB  | `GOMEMLIMIT=64MiB GOGC=50`    |
| **合计**  | **~45 MB** | 128 MB 内存的小机器绰绰有余  |

> **`GOMEMLIMIT` 不是可选项。** 不设的话面板常驻 55 MB（Go 的 GC 不着急把
> 内存还给系统），设了之后 14 MB。仓库里的 systemd 单元已经写好了。

二进制体积：`caddyui` 13 MB（`-ldflags="-s -w"` 后），`caddy` 官方版约 45 MB。

---

## 工作原理

站点部分的核心就三步，加起来不到 200 行：

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
- 邮箱同样过白名单正则才写进 Caddyfile 的 `email` 指令
- 「高级配置」是原样透传的（那是用户主动进专家模式），但会做大括号配平检查，
  真正的语法校验交给 Caddy，写错了也只是被拒绝，炸不了线上
- 登录限速：同 IP 5 分钟 10 次；账号不存在时也跑一次 bcrypt，防时序探测
- 所有 POST 校验会话内的 CSRF token + 同源检查
- CSP `default-src 'self'`，面板没有任何外部资源、没有内联脚本
  （深色模式因此走 cookie + 服务端渲染，而不是内联 script）
- Admin API 走 unix socket，本机其它进程连端口都扫不到
- 证书那块只读路径和有效期，**证书内容和私钥永远不会出现在页面上**
- 升级 Caddy 只走官方仓库 + SHA-512 校验；面板拿到的 sudo 权限被限死在
  「运行那一个不带参数的助手脚本」上，换不成任意命令

---

## 目录结构

```
install.sh                   一键安装（含从 Relay 迁移）
uninstall.sh                 一键卸载
main.go                      启动、优雅退出、内嵌前端
internal/
  store/                     SQLite：用户、站点、配置版本、会话
  caddy/
    client.go                Admin API 客户端（unix socket / TCP 都支持）
    render.go                站点 → Caddyfile
  certs/                     在磁盘上定位 Caddy 签发的证书，读有效期
  caddybin/                  读 Caddy 版本、查官方最新版、触发升级
  app/service.go             渲染 + 下发 + 记录版本 + 回滚
  web/                       路由、会话、CSRF、主题、各页面 handler
web/
  templates/                 Go 模板，服务端渲染
  static/                    手写 CSS + JS，无框架无构建步骤
deploy/
  caddy.service              Caddy 的 systemd 单元
  caddyui.service            面板的 systemd 单元
  upgrade-caddy.sh           root 拥有的升级助手（面板通过 sudo 调它）
  caddyui.sudoers            只放行上面那一个脚本的 sudo 授权
```

前端没有 node_modules，没有打包步骤，`go build` 一条命令出成品。

## 启动参数

| 参数           | 环境变量               | 默认值           | 说明                          |
| -------------- | ---------------------- | ---------------- | ----------------------------- |
| `-listen`      | `CADDYUI_LISTEN`       | `:81`            | 面板监听地址                  |
| `-data`        | `CADDYUI_DATA_DIR`     | `./data`         | 数据库存放目录                |
| `-caddy`       | `CADDYUI_CADDY_ADMIN`  | `127.0.0.1:2019` | Caddy Admin API 地址          |
| `-caddy-data`  | `CADDYUI_CADDY_DATA`   | 自动探测         | Caddy 数据目录（证书在这里）  |
| `-caddy-bin`   | `CADDYUI_CADDY_BIN`    | 自动探测         | Caddy 可执行文件路径          |

`-caddy` 支持两种写法：`127.0.0.1:2019`（TCP）或 `unix//run/caddy/admin.sock`。

旧的 `RELAY_*` 环境变量仍然认，方便从老版本平滑升级。

---

## 备份

要备份**两样东西**，缺一不可：`/var/lib/caddyui/caddyui.db`（站点、账号、配置历史）
和 `/var/lib/caddy`（Caddy 的证书和 ACME 账户密钥）。

第二项**特别容易被忘掉**。丢了会导致重装后所有证书要重新签发，而 Let's Encrypt
对同一组域名有**每周 5 张**的速率限制，撞上了就得干等几天。

## 常见问题

**证书申请不下来**
Caddy 用 HTTP-01 验证，需要 80 端口能被外网访问。检查域名解析有没有指到这台机器、
80 端口有没有被防火墙或云厂商安全组挡住。面板「配置」页能看到下发结果。

**站点编辑页看不到证书路径**
说明面板读不到 Caddy 的数据目录。「设置」页会显示它在找哪个目录。
一键安装装出来的两个服务同属 `caddy` 用户，正常情况下读得到；
手动部署的话用 `-caddy-data /实际/路径` 指定。域名还没被访问过时证书本来就不存在，
这种情况会显示「尚未签发」。

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

**设置页显示「一键升级不可用」**
说明 sudo 授权没装上：可能系统里没有 sudo，也可能 visudo 校验没通过。
重新跑一次安装脚本通常能修好。实在不行就 SSH 上去手动升级，
面板上会显示当前和最新版本，心里有数。

**升级 Caddy 失败了怎么办**
助手脚本在替换二进制之前会先备份、先试运行；重启后服务没起来会自动回滚并
重启回旧版本。设置页会把失败原因和日志尾巴显示出来。真出问题就
`journalctl -u caddy -n 50` 看详细日志。

---

## 路线图

已完成：

- [x] 站点增删改查、启用停用
- [x] 自动 HTTPS（申请 + 续期全自动）
- [x] 配置版本历史 + 一键回滚
- [x] HTTP Basic Auth 访问密码
- [x] 自定义 Caddyfile 片段（高级模式）
- [x] 启动自动同步、救援通道、登录限速、CSRF
- [x] 邮箱注册，自动作为证书联系邮箱（v0.2）
- [x] 证书 / 私钥路径与有效期展示（v0.2）
- [x] Claude 配色 + 深色模式（v0.2）
- [x] 一键升级 Caddy 内核，官方源 + SHA-512 校验（v0.2）

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

## 依赖

只有两个：

- `modernc.org/sqlite` —— 纯 Go 的 SQLite，不需要 CGO，交叉编译一条命令
- `golang.org/x/crypto` —— bcrypt（面板密码和站点访问密码共用）

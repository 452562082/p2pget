# p2pget

基于 BitTorrent 协议的 P2P 下载器，自带聚合搜索（公共种子站 + DHT 爬虫索引）。

## 功能

- **聚合搜索**：并发查询多个数据源，去重排序，结果流式渐进显示
  - `piratebay` — apibay.org JSON API
  - `nyaa` — nyaa.si RSS（动漫为主）
  - `torrents-csv` — torrents-csv.com 开放 JSON API，综合内容
  - `jackett` — 接入本地 Jackett，一次覆盖其中配置的所有公开/私有 tracker（需配置 API key）
  - `dht-index` — 本地 SQLite，存 DHT 爬虫收集到的种子
- **BitTorrent 引擎**：基于 [anacrolix/torrent](https://github.com/anacrolix/torrent)，支持 magnet 链接、`.torrent` 文件/URL、纯 info-hash
  - 文件级选择：可单独勾选/取消种子内的文件，不必整盘下载（选择随任务持久化）
  - 任务持久化：TUI 模式下载列表写入 `~/.p2pget/tasks.json`，重启自动恢复（已下数据续传）
  - 实时速率：每个任务显示下载/上传速度与 ETA，状态栏汇总全局速度
  - 暂停/恢复：可暂停单个任务，恢复时保留文件选择
  - 限速：可配置下载/上传上限与每种子连接数
  - 元数据超时保护：默认 10 分钟拿不到元数据的任务会被丢弃并标记失败，不再无限挂起
- **DHT 爬虫**：被动监听 DHT 网络的 `get_peers` / `announce_peer` 请求，提取 info-hash，再用 BEP9 拉取元数据存入 SQLite
- **终端 TUI**：基于 bubbletea，搜索 → 结果 → 下载进度 → 文件选择

## 安装与运行

```bash
git clone https://github.com/452562082/p2pget.git
cd p2pget
go build -o p2pget ./cmd/p2pget
./p2pget
```

需要 Go 1.21+。首次编译会下载依赖，国内代理可能不稳定，建议：

```bash
GOPROXY=https://proxy.golang.org,direct go mod tidy
```

运行单元测试：`go test ./...`

## 用法

### TUI（默认）

```bash
./p2pget
```

不带子命令直接运行即进入终端交互界面（搜索 / 下载 / 帮助三个标签页）。
TUI 同样接受下文「通用选项」里的所有参数与环境变量，例如带上
Jackett：`./p2pget -jackett-key <你的key>`。

快捷键：
- `Tab` / `Shift+Tab` 切换标签页
- `/` 聚焦搜索框，`Esc` 离开输入
- 搜索框中 `Enter` 执行搜索
- 结果列表中 `Enter` 加入并立即下载
- 结果列表中 `p` 暂停加入（先进文件选择挑文件，再开始下载，避免下到不想要的文件）
- 下载列表中 `Enter` 进入文件选择视图
- 下载列表中 `p` 暂停 / 恢复任务
- 下载列表中 `d` 移除任务（保留磁盘文件）
- 下载列表中 `D` 移除任务并删除已下载文件（需 `y` 确认）
- `↑` `↓` 移动光标
- `q` 或 `Ctrl+C` 退出

文件选择视图（在下载列表对某个任务按 `Enter` 进入）：
- `↑` `↓` 移动光标
- `空格` 勾选/取消当前文件
- `a` 全选，`n` 全不选
- `Esc` / `Enter` 返回下载列表

### CLI 子命令

```bash
# 聚合搜索（结果按做种数排序，分页显示）
./p2pget search "ubuntu 24.04"
./p2pget search -n 50 "ubuntu 24.04"           # 每页 50 条
./p2pget search -n 20 -page 2 "ubuntu 24.04"   # 第 2 页

# 通过 magnet / hash / .torrent 文件 / .torrent URL 下载
./p2pget get "magnet:?xt=urn:btih:..."
./p2pget get 6a9759bffd5c0af65319979fb7832189f4f3c35d
./p2pget get ./xxx.torrent
./p2pget get https://example.org/xxx.torrent

# DHT 爬虫（前台运行，Ctrl+C 退出）
./p2pget dht-crawl
```

通用选项（CLI 子命令与 TUI 都适用）：

| 参数 | 默认值 | 说明 |
| - | - | - |
| `-data` | `~/.p2pget/data` | 下载文件保存目录 |
| `-index` | `~/.p2pget/index.db` | DHT 索引 SQLite 文件 |
| `-port` | 0（随机） | BT 监听端口 |
| `-no-dht` | false | 禁用 DHT（仅依靠 tracker / PEX） |
| `-no-upload` | false | 不做种 |
| `-rate-down` | 0（不限） | 下载限速，单位 KB/s |
| `-rate-up` | 0（不限） | 上传限速，单位 KB/s |
| `-max-conns` | 0（库默认） | 每个种子的最大连接数 |
| `-jackett-url` | `http://localhost:9117` | Jackett 地址 |
| `-jackett-key` | 空 | Jackett API key，设置后启用 `jackett` 源 |

`-jackett-url` / `-jackett-key` 也可用环境变量
`P2PGET_JACKETT_URL` / `P2PGET_JACKETT_KEY` 设置默认值。

把环境变量写进 shell 配置（zsh 用 `~/.zshrc`，bash 用 `~/.bashrc`）即可永久生效，
之后直接 `./p2pget` 就自动带上，不必每次输入：

```bash
echo 'export P2PGET_JACKETT_KEY=你的key' >> ~/.zshrc
source ~/.zshrc
```

## 搜索源

搜索会并发查询下面的源，去重后按做种数排序、流式渐进显示。**默认实际生效的只有
`piratebay` 和 `nyaa`**，其余源需要额外配置。

### piratebay（默认开，无需配置）

apibay.org 的公开 JSON API，综合资源（影视、剧集、软件等）。开箱即用。

### nyaa（默认开，无需配置）

nyaa.si 的 RSS，**专注动漫与亚洲内容**。搜非动漫（如欧美电影）时它通常返回空，
属正常现象。

### torrents-csv（默认开，无需配置）

torrents-csv.com 的开放社区种子索引，公开 JSON API，综合内容。开箱即用。

### jackett（需配置，推荐——一次接入数百站点）

接入本地 [Jackett](https://github.com/Jackett/Jackett)，一次查询扇出到其中配置的
所有公开/私有 tracker。这是「扩展更多搜索源」的推荐方式，无需改代码。

1. **运行 Jackett**（独立程序，与 p2pget 分开）：
   - Docker：`docker run -d --name jackett -p 9117:9117 lscr.io/linuxserver/jackett`
   - 或从 Jackett 的 Releases 页下载对应平台的包，运行其中的 `jackett`
2. **取 API Key**：浏览器打开 `http://localhost:9117`，面板**右上角的 `API Key`**
   字段就是要用的 key（旁边有复制按钮）。
3. **加 indexer**：在 Jackett 界面点 `Add indexer` 添加想要的站点（私有站需填账号）。
   **不加 indexer 的话，即使 key 正确，搜索结果也会是空的。**
4. **配置给 p2pget**：
   ```bash
   export P2PGET_JACKETT_KEY=你的key
   # Jackett 不在本机默认地址时：export P2PGET_JACKETT_URL=http://host:9117
   ./p2pget
   ```
   也可用 `-jackett-key` / `-jackett-url` 命令行参数。

注意：本工具靠 magnet / DHT 下载，只能用 Jackett 结果中带 magnet 或 info-hash 的条目。

想要 1337x、Rutracker 这类被 Cloudflare 拦截或需登录的站点——在 Jackett 里把它们
加为 indexer 即可。被 Cloudflare 保护的站点，在 Jackett 设置页填上 FlareSolverr
地址（[FlareSolverr](https://github.com/FlareSolverr/FlareSolverr) 用无头浏览器解
Cloudflare 挑战），由 Jackett 统一处理，p2pget 这边无需关心。

### dht-index（需先跑爬虫）

本地 SQLite 全文索引，数据来自 DHT 爬虫。**默认是空的**——需先运行
`./p2pget dht-crawl` 收集一段时间（见下方「DHT 爬虫使用建议」）。攒到数据后，搜索会
基于本地索引返回结果，**完全离线、无中心、不依赖任何站点**。

## DHT 爬虫使用建议

1. 先单独跑一段时间收集种子：
   ```bash
   ./p2pget dht-crawl
   ```
   它会被动监听公网 DHT 流量并尝试通过 BEP9 拉元数据。前 10–30 分钟比较慢，路由表建好后会变快。

2. 之后用 TUI 或 `search` 子命令时，`dht-index` 数据源会基于本地 FTS5 全文索引返回结果，**完全离线、无中心、不依赖任何站点**。

3. 长期跑：可以放到 `nohup` / `tmux` 里。

## 目录结构

```
p2pget/
├── cmd/p2pget/main.go          # 入口 + 子命令
├── internal/
│   ├── engine/                  # anacrolix/torrent 封装
│   ├── search/                  # 搜索聚合（piratebay/nyaa/torrents-csv/jackett）
│   ├── dht/                     # DHT 爬虫 + StoreIndexer
│   ├── store/                   # SQLite (modernc.org/sqlite, 纯 Go)
│   └── tui/                     # bubbletea TUI
└── go.mod
```

## 已知限制

- **DHT 爬虫**：首次启动需要等路由表建好，本机内网可能效果差，建议公网或 NAT-friendly 网络。
- **网络环境（部分地区）**：在对 BT 流量做主动干预的网络下，DHT 可能完全用不了。常见两种叠加干预：
  - **DNS 劫持**：明文 DNS（53 端口）被在链路上拦截，把 DHT 引导域名（`router.bittorrent.com` 等）解析成假 IP。p2pget 已用 **DoH 加密解析 + 硬编码引导 IP 兜底**绕过这一层，本机/路由器 DNS 怎么设都不受影响。
  - **DPI 丢包**：运营商对 DHT 的 KRPC/UDP 流量做深度包检测并丢弃。这层代码改不了 —— DHT 引导包发出去 0 响应。要让 DHT 真正可用必须挂全局代理 / VPN，并且**代理协议必须支持 UDP 转发**（WireGuard / OpenVPN / Hysteria / TUIC / Shadowsocks-UDP / VLESS-reality 等支持，Trojan / VMess-WS 等仅 TCP 协议下 DHT 仍走不通）。简单确认方法：`dig @8.8.8.8 router.bittorrent.com` 应得到 `67.215.246.10` 这类真实 IP，而非 `28.0.0.x` 之类伪造值。
  - DHT 不可用时仍可下载 tracker 有覆盖的种子（搜索结果挑做种数 `S` 较高、来源 `piratebay` 的成功率最高）。
- **法律声明**：本工具是协议实现，类似 qBittorrent / Transmission。下载内容的合法性由使用者自行负责。

## 许可证

[MIT](LICENSE)

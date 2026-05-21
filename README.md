# p2pget

基于 BitTorrent 协议的 P2P 下载器，自带聚合搜索（公共种子站 + DHT 爬虫索引）。

## 功能

- **聚合搜索**：并发查询多个数据源，去重排序，结果流式渐进显示
  - `piratebay` — apibay.org JSON API
  - `nyaa` — nyaa.si RSS
  - `1337x` — HTML 抓取（默认常被 Cloudflare 拦；配 FlareSolverr 后可用）
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
cd p2pget
go build -o p2pget ./cmd/p2pget
./p2pget
```

需要 Go 1.21+。首次编译会下载依赖，国内代理可能不稳定，建议：

```bash
GOPROXY=https://proxy.golang.org,direct go mod tidy
```

## 用法

### TUI（默认）

```bash
./p2pget
```

快捷键：
- `Tab` / `Shift+Tab` 切换标签页
- `/` 聚焦搜索框，`Esc` 离开输入
- 搜索框中 `Enter` 执行搜索
- 结果列表中 `Enter` 加入下载
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
# 聚合搜索
./p2pget search "ubuntu 24.04"

# 通过 magnet / hash / .torrent 文件 / .torrent URL 下载
./p2pget get "magnet:?xt=urn:btih:..."
./p2pget get 6a9759bffd5c0af65319979fb7832189f4f3c35d
./p2pget get ./xxx.torrent
./p2pget get https://example.org/xxx.torrent

# DHT 爬虫（前台运行，Ctrl+C 退出）
./p2pget dht-crawl
```

通用选项：

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
| `-flaresolverr` | 空 | FlareSolverr 地址，设置后用于绕过 Cloudflare |

`-jackett-url` / `-jackett-key` / `-flaresolverr` 也可分别用环境变量
`P2PGET_JACKETT_URL` / `P2PGET_JACKETT_KEY` / `P2PGET_FLARESOLVERR` 设置默认值。

## Jackett / FlareSolverr

两者都是**可选**的外部服务，不配置不影响其它功能。

**Jackett** —— 种子站聚合器，自己管理几百个公开/私有 tracker。装好 Jackett 后，在它界面里加好想用的 tracker，拿到 `API Key`，然后：

```bash
export P2PGET_JACKETT_KEY=你的key
# 若 Jackett 不在本机默认端口：export P2PGET_JACKETT_URL=http://host:9117
./p2pget
```

启用后多一个 `jackett` 搜索源，一次查询会扇出到 Jackett 里配置的全部 tracker（含私有站，登录态由 Jackett 处理）。注意：本工具靠 magnet / DHT 下载，只能用提供 magnet 或 info-hash 的结果。

**FlareSolverr** —— 用无头浏览器解 Cloudflare 挑战的代理服务。`1337x` 默认会被 Cloudflare 拦（返回 403），配置后该源走 FlareSolverr 即可正常抓取：

```bash
export P2PGET_FLARESOLVERR=http://localhost:8191
./p2pget
```

代价：每次请求要启浏览器，较慢；启用后搜索整体超时会放宽到 90s（快的源仍会先出结果）。

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
│   ├── search/                  # 搜索聚合（piratebay/nyaa/1337x）
│   ├── dht/                     # DHT 爬虫 + StoreIndexer
│   ├── store/                   # SQLite (modernc.org/sqlite, 纯 Go)
│   └── tui/                     # bubbletea TUI
└── go.mod
```

## 已知限制

- **1337x.to** 现在几乎一定被 Cloudflare 拦，需要 FlareSolverr 或绕过方案。当前实现把它作为 best-effort 数据源，单源失败不影响整体。
- **DHT 爬虫**：首次启动需要等路由表建好，本机内网可能效果差，建议公网或 NAT-friendly 网络。
- **法律声明**：本工具是协议实现，类似 qBittorrent / Transmission。下载内容的合法性由使用者自行负责。

## 后续可做

- 加入即暂停（先选文件再下载，避免元数据就绪到取消勾选之间的少量带宽浪费）
- Web UI（gin + WebSocket）
- 更多 indexer：Rutracker、TorrentGalaxy、Jackett 代理
- 绕过 Cloudflare：集成 FlareSolverr
- 下载完成 hook、RSS 自动订阅

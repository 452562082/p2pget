# 设计：DHT 引导绕过 DNS 劫持 + 扩充 tracker 覆盖

日期：2026-05-22
分支：`fix/dht-bootstrap-dns-bypass`

## 背景与问题

用户报告「怎么都下载不下来」，任务永久卡在「获取元数据中…」。系统化排查确认：

1. **DNS 劫持（环境，根因）**：当前网络在链路层劫持所有明文 DNS（53 端口）。把 DHT 引导域名解析成伪造 IP `28.0.0.x`。实测向 8.8.8.8 / 1.1.1.1 / 223.5.5.5 / 114.114.114.114 查询 `router.bittorrent.com` 全部返回伪造 IP；仅 DoH（加密 DNS）返回真实 IP。后果：anacrolix/torrent 的 DHT 解析引导域名得到黑洞地址 → DHT 路由表建不起来 → 找不到任何 peer。
2. **代码弱点**：`internal/search/search.go` 的 `MagnetFromInfoHash` 把所有搜索源结果统一重拼 magnet，只挂 5 个固定通用 tracker，丢弃来源站自己的 tracker（如 nyaa 的 `nyaa.tracker.wf:7777`）。DHT 一旦失效，动漫等种子就再无可用 peer 来源。

引擎本身无 bug：ubuntu 官方 ISO 用带通用 tracker 的 magnet 可正常下载 60+MB。

## 目标

- 让 DHT 引导不依赖会被劫持的明文系统 DNS。
- 扩充并保留 tracker，使 DHT 之外仍有可靠的 peer 发现渠道。
- 默认生效，用户无需任何配置；在未被劫持的网络上同样无害。

## 非目标（明确排除）

- 不处理 DPI 对 DHT UDP 流量的丢包/限速。若运营商同时部署了 DPI，DHT 可「起来」但仍可能慢；这无法在代码层解决，需改完实测确认。
- 不复活真正无人做种的死种。
- 不在搜索阶段抓取 nyaa 真实 `.torrent` 文件（每条结果一次网络请求，过重）。

## Part 1：DHT 引导绕过 DNS 劫持

### 新增文件 `internal/engine/bootstrap.go`

职责单一：产出 DHT 引导节点 `[]dht.Addr`，供 `engine.New` 设置 `torrent.ClientConfig.DhtStartingNodes`。

**引导域名（来自 `dht.DefaultGlobalBootstrapHostPorts`）**

```
router.utorrent.com:6881
router.bittorrent.com:6881
dht.transmissionbt.com:6881
dht.aelitis.com:6881
dht.libtorrent.org:25401
```

**解析逻辑**

1. **DoH 优先**：用加密 DNS（HTTPS，链路无法劫持）查上述域名的 A 记录。
   - DoH 端点：主 `https://1.1.1.1/dns-query`，备 `https://dns.google/resolve`，均为 JSON 协议（请求头 `accept: application/dns-json`）。
   - 总超时约 5 秒；主端点失败则试备用端点。
2. **硬编码兜底**：DoH 全部失败（被封锁/超时）时，使用下列硬编码 IP（实现时已由 DoH 实测获得，仅 IPv4）：
   ```
   67.215.246.10:6881      (router.bittorrent.com)
   82.221.103.244:6881     (router.utorrent.com)
   87.98.162.88:6881       (dht.transmissionbt.com)
   212.129.33.59:6881      (dht.transmissionbt.com)
   34.203.221.232:6881     (dht.aelitis.com)
   185.157.221.247:25401   (dht.libtorrent.org)
   ```
3. 每个 IP 经 `net.ResolveUDPAddr` → `dht.NewAddr` 转成 `dht.Addr`。
4. DoH 成功但解析出的地址为空时，同样回退到硬编码列表；保证返回非空。

**缓存与时机**

- `engine.New` 设置：
  ```go
  tcfg.DhtStartingNodes = func(network string) dht.StartingNodesGetter {
      return func() ([]dht.Addr, error) { return bootstrapNodes(), nil }
  }
  ```
- `bootstrapNodes()` 用 `sync.Once` 缓存结果。DoH 在 anacrolix 真正启动 DHT、首次调用 getter 时才惰性执行，不阻塞 `engine.New` / TUI 启动。

**其他**

- 默认开启，不加命令行开关（YAGNI）。
- 不向 stdout/stderr 打日志，避免污染 bubbletea TUI 画面。

## Part 2：扩充 tracker 覆盖

### `internal/search/search.go`

- `MagnetFromInfoHash` 签名改为变参：
  ```go
  func MagnetFromInfoHash(infoHash, name string, extraTrackers ...string) string
  ```
  现有调用点（piratebay / torrents-csv / jackett / dht crawler）不传 `extraTrackers`，源码一行不改。
- 内置通用 tracker 列表由当前 5 个替换为一组更新、更全的公共 tracker（约 12 个，含 UDP 与 HTTP/HTTPS），取自社区常用的「best trackers」清单。
- 拼接 `tr` 参数前对 trackers（内置 + extra）做去重。

### `internal/search/nyaa.go`

- 构造 magnet 时传入 nyaa 官方 tracker：
  ```go
  Magnet: MagnetFromInfoHash(hash, it.Title, "http://nyaa.tracker.wf:7777/announce"),
  ```
  动漫种子真正有 peer 的是 nyaa 自家 tracker。

## Part 3：测试

新增/修改单元测试，且全部现有测试保持通过：

- `internal/engine/bootstrap_test.go`：
  - 硬编码兜底 IP 全部能正确解析为 `dht.Addr`。
  - 注入「永远失败的 DoH」时，`bootstrapNodes()` 回退到硬编码列表且非空。
  - 解析结果数量与预期一致。
- `internal/search/search_test.go`：
  - `MagnetFromInfoHash` 带 `extraTrackers` 时，magnet 的 `tr` 含该 tracker。
  - 内置 tracker 与 extra tracker 重复时去重。
  - nyaa 源产出的 `Result.Magnet` 含 `nyaa.tracker.wf`。

## 验收

- `go build ./...` 与 `go test ./...` 通过。
- 在当前被劫持的网络下，用 `get` 下载一个有公网做种的种子（如 ubuntu 官方 ISO 的纯 magnet）能取得元数据并开始下载（验证 DHT 引导生效）。若仍失败则指向 DPI 限速（非目标范围）。

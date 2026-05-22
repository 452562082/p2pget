# DHT 引导绕过 DNS 劫持 + 扩充 tracker 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 p2pget 在劫持明文 DNS 的网络下也能引导 DHT 并发现 peer，下载不再卡在「元数据中…」。

**Architecture:** 新增 `internal/engine/bootstrap.go`，用 DoH（加密 DNS）解析 DHT 引导节点、失败回退硬编码 IP，经 `torrent.ClientConfig.DhtStartingNodes` 注入；`MagnetFromInfoHash` 改为变参以保留来源站 tracker 并扩充内置公共 tracker。

**Tech Stack:** Go、github.com/anacrolix/torrent v1.61.0、github.com/anacrolix/dht/v2 v2.23.0、DoH JSON API。

设计文档：`docs/superpowers/specs/2026-05-22-dht-bootstrap-dns-bypass-design.md`

---

### Task 1: DHT 引导解析器（bootstrap.go）

**Files:**
- Create: `internal/engine/bootstrap.go`
- Test: `internal/engine/bootstrap_test.go`

- [ ] **Step 1: 写失败测试**

创建 `internal/engine/bootstrap_test.go`：

```go
package engine

import (
	"errors"
	"net/http"
	"testing"
)

// failTransport makes every HTTP request fail, simulating DoH being blocked.
type failTransport struct{}

func (failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("doh unreachable")
}

func TestFallbackNodesValid(t *testing.T) {
	addrs := fallbackNodes()
	if len(addrs) != len(bootstrapFallbackIPs) {
		t.Fatalf("fallbackNodes returned %d addrs, want %d", len(addrs), len(bootstrapFallbackIPs))
	}
	for i, a := range addrs {
		if a.IP() == nil {
			t.Errorf("addr %d (%s) has nil IP", i, a.String())
		}
		if a.Port() == 0 {
			t.Errorf("addr %d (%s) has zero port", i, a.String())
		}
	}
}

func TestResolveBootstrapNodesFallsBackWhenDoHFails(t *testing.T) {
	client := &http.Client{Transport: failTransport{}}
	addrs := resolveBootstrapNodes(client)
	if len(addrs) != len(bootstrapFallbackIPs) {
		t.Fatalf("resolveBootstrapNodes with failing DoH returned %d addrs, want %d (the fallback set)",
			len(addrs), len(bootstrapFallbackIPs))
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/engine/ -run 'Bootstrap|FallbackNodes' -v`
Expected: 编译失败 —— `undefined: fallbackNodes` / `undefined: bootstrapFallbackIPs` / `undefined: resolveBootstrapNodes`。

- [ ] **Step 3: 写实现**

创建 `internal/engine/bootstrap.go`：

```go
// DHT bootstrap that resolves node addresses without trusting the system DNS
// resolver, which some networks hijack for BitTorrent-related domains.
package engine

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/dht/v2"
)

// bootstrapHostPorts are the standard Mainline DHT bootstrap nodes. The DHT
// cannot find peers until it reaches one of these to seed its routing table.
var bootstrapHostPorts = []string{
	"router.utorrent.com:6881",
	"router.bittorrent.com:6881",
	"dht.transmissionbt.com:6881",
	"dht.aelitis.com:6881",
	"dht.libtorrent.org:25401",
}

// bootstrapFallbackIPs are known-good IP:port pairs for the hosts above,
// resolved over DoH on 2026-05-22. They let the DHT bootstrap even when both
// the system DNS and DoH are unavailable.
var bootstrapFallbackIPs = []string{
	"67.215.246.10:6881",    // router.bittorrent.com
	"82.221.103.244:6881",   // router.utorrent.com
	"87.98.162.88:6881",     // dht.transmissionbt.com
	"212.129.33.59:6881",    // dht.transmissionbt.com
	"34.203.221.232:6881",   // dht.aelitis.com
	"185.157.221.247:25401", // dht.libtorrent.org
}

// dohEndpoints are DNS-over-HTTPS resolvers. DoH runs over TLS, so an in-path
// resolver cannot forge answers the way it can for plaintext DNS.
var dohEndpoints = []string{
	"https://1.1.1.1/dns-query",
	"https://dns.google/resolve",
}

var (
	bootstrapOnce  sync.Once
	bootstrapAddrs []dht.Addr
)

// bootstrapNodes returns DHT bootstrap addresses, resolved over DoH with a
// hardcoded IP fallback. The result is computed once and cached.
func bootstrapNodes() []dht.Addr {
	bootstrapOnce.Do(func() {
		bootstrapAddrs = resolveBootstrapNodes(http.DefaultClient)
	})
	return bootstrapAddrs
}

// resolveBootstrapNodes resolves the bootstrap hosts over DoH, falling back to
// the hardcoded IPs when DoH yields nothing. It always returns a non-empty
// slice. The HTTP client is a parameter so tests can inject one.
func resolveBootstrapNodes(client *http.Client) []dht.Addr {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	var addrs []dht.Addr
	for _, hp := range bootstrapHostPorts {
		host, port, err := net.SplitHostPort(hp)
		if err != nil {
			continue
		}
		for _, ip := range dohLookup(ctx, client, host) {
			if a := udpAddr(ip, port); a != nil {
				addrs = append(addrs, dht.NewAddr(a))
			}
		}
	}
	if len(addrs) > 0 {
		return addrs
	}
	return fallbackNodes()
}

// fallbackNodes builds dht.Addrs from the hardcoded bootstrap IPs.
func fallbackNodes() []dht.Addr {
	var addrs []dht.Addr
	for _, ipPort := range bootstrapFallbackIPs {
		host, port, err := net.SplitHostPort(ipPort)
		if err != nil {
			continue
		}
		if a := udpAddr(host, port); a != nil {
			addrs = append(addrs, dht.NewAddr(a))
		}
	}
	return addrs
}

// udpAddr builds a *net.UDPAddr from an IP string and numeric port string, or
// returns nil if either is malformed.
func udpAddr(ip, port string) *net.UDPAddr {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return nil
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return nil
	}
	return &net.UDPAddr{IP: parsed, Port: p}
}

// dohLookup resolves host's A records via DoH, trying each endpoint until one
// answers. It returns nil when every endpoint fails.
func dohLookup(ctx context.Context, client *http.Client, host string) []string {
	for _, ep := range dohEndpoints {
		if ips := dohQuery(ctx, client, ep, host); len(ips) > 0 {
			return ips
		}
	}
	return nil
}

// dohQuery performs one DoH JSON query for host's A records at endpoint ep.
func dohQuery(ctx context.Context, client *http.Client, ep, host string) []string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep, nil)
	if err != nil {
		return nil
	}
	q := req.URL.Query()
	q.Set("name", host)
	q.Set("type", "A")
	req.URL.RawQuery = q.Encode()
	req.Header.Set("accept", "application/dns-json")

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}
	var ips []string
	for _, a := range body.Answer {
		data := strings.TrimSpace(a.Data)
		if a.Type == 1 && net.ParseIP(data) != nil { // type 1 = A record
			ips = append(ips, data)
		}
	}
	return ips
}
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/engine/ -run 'Bootstrap|FallbackNodes' -v`
Expected: PASS（`TestFallbackNodesValid`、`TestResolveBootstrapNodesFallsBackWhenDoHFails`）。

- [ ] **Step 5: 提交**

```bash
git add internal/engine/bootstrap.go internal/engine/bootstrap_test.go
git commit -m "feat(engine): DHT 引导用 DoH 解析，失败回退硬编码 IP"
```

---

### Task 2: 把 DhtStartingNodes 接入 engine.New

**Files:**
- Modify: `internal/engine/engine.go`（import 块；`New` 中 `tcfg` 配置处，`tcfg.NoDHT = cfg.NoDHT` 之后）

- [ ] **Step 1: 加 dht 包 import**

把 `internal/engine/engine.go` 的 import 块改为（新增 `github.com/anacrolix/dht/v2` 一行）：

```go
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"
)
```

- [ ] **Step 2: 注入 DhtStartingNodes**

在 `internal/engine/engine.go` 的 `New` 中，找到这一行：

```go
	tcfg.NoDHT = cfg.NoDHT
```

在它的正下方插入：

```go
	// Bootstrap the DHT via DoH-resolved / hardcoded node addresses instead of
	// the system DNS, which some networks hijack for BitTorrent domains.
	tcfg.DhtStartingNodes = func(string) dht.StartingNodesGetter {
		return func() ([]dht.Addr, error) { return bootstrapNodes(), nil }
	}
```

- [ ] **Step 3: 构建并跑 engine 包测试**

Run: `go build ./... && go test ./internal/engine/`
Expected: 构建成功；engine 包测试全部 PASS（`TestWaitInfoReturnsOnMetadataTimeout` 用 `NoDHT: true`，不会触发 DoH 网络请求）。

- [ ] **Step 4: 提交**

```bash
git add internal/engine/engine.go
git commit -m "feat(engine): 接入 DhtStartingNodes，DHT 引导绕过系统 DNS"
```

---

### Task 3: MagnetFromInfoHash 改变参 + 扩充并去重 tracker

**Files:**
- Modify: `internal/search/search.go`（`MagnetFromInfoHash` 函数，约 197-215 行）
- Test: `internal/search/search_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/search/search_test.go` 的 import 块加入 `"net/url"`，使其为：

```go
import (
	"net/url"
	"strings"
	"testing"
)
```

在文件末尾追加：

```go
func TestMagnetFromInfoHashExtraTrackers(t *testing.T) {
	const hash = "6A9759BFFD5C0AF65319979FB7832189F4F3C35D"
	const extra = "http://nyaa.tracker.wf:7777/announce"
	m := MagnetFromInfoHash(hash, "Name", extra)
	if !strings.Contains(m, url.QueryEscape(extra)) {
		t.Errorf("magnet missing extra tracker %q: %q", extra, m)
	}
}

func TestMagnetFromInfoHashDedups(t *testing.T) {
	const hash = "6A9759BFFD5C0AF65319979FB7832189F4F3C35D"
	dup := defaultTrackers[0]
	m := MagnetFromInfoHash(hash, "Name", dup)
	if n := strings.Count(m, url.QueryEscape(dup)); n != 1 {
		t.Errorf("tracker %q appears %d times, want 1: %q", dup, n, m)
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/search/ -run MagnetFromInfoHash -v`
Expected: 编译失败 —— `too many arguments in call to MagnetFromInfoHash` 与 `undefined: defaultTrackers`（当前签名只有两个参数、且没有 `defaultTrackers` 变量）。

- [ ] **Step 3: 写实现**

把 `internal/search/search.go` 中现有的 `MagnetFromInfoHash` 函数（从 `// MagnetFromInfoHash builds a magnet URI with sane public trackers.` 到其结尾的 `}`）整体替换为：

```go
// defaultTrackers are public BitTorrent trackers attached to every magnet so
// peer discovery still works when DHT is unavailable. Kept roughly in sync
// with the widely-used community "best trackers" list.
var defaultTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.demonii.com:1337/announce",
	"udp://open.stealth.si:80/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://exodus.desync.com:6969/announce",
	"udp://tracker.openbittorrent.com:6969/announce",
	"udp://explodie.org:6969/announce",
	"udp://opentracker.io:6969/announce",
	"udp://tracker.dler.com:6969/announce",
	"udp://tracker-udp.gbitt.info:80/announce",
	"http://tracker.openbittorrent.com:80/announce",
	"https://tracker.tamersunion.org:443/announce",
}

// MagnetFromInfoHash builds a magnet URI with public trackers. extraTrackers,
// when given, are appended to the default set — used to keep a source's own
// tracker (e.g. nyaa's) where the torrent's real peers actually announce.
// Trackers are de-duplicated so an extra tracker already in the default set is
// not added twice.
func MagnetFromInfoHash(infoHash, name string, extraTrackers ...string) string {
	v := url.Values{}
	v.Set("xt", "urn:btih:"+strings.ToLower(infoHash))
	if name != "" {
		v.Set("dn", name)
	}
	seen := map[string]bool{}
	addTracker := func(tr string) {
		if tr == "" || seen[tr] {
			return
		}
		seen[tr] = true
		v.Add("tr", tr)
	}
	for _, tr := range defaultTrackers {
		addTracker(tr)
	}
	for _, tr := range extraTrackers {
		addTracker(tr)
	}
	return "magnet:?" + v.Encode()
}
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/search/ -run MagnetFromInfoHash -v`
Expected: PASS（`TestMagnetFromInfoHash`、`TestMagnetFromInfoHashExtraTrackers`、`TestMagnetFromInfoHashDedups` 三个都过）。

- [ ] **Step 5: 提交**

```bash
git add internal/search/search.go internal/search/search_test.go
git commit -m "feat(search): MagnetFromInfoHash 支持额外 tracker 并扩充内置列表"
```

---

### Task 4: nyaa 源保留 nyaa 官方 tracker

**Files:**
- Modify: `internal/search/nyaa.go:96`
- Test: `internal/search/search_test.go`

- [ ] **Step 1: 写失败测试**

把 `internal/search/search_test.go` 的 import 块改为：

```go
import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)
```

在文件末尾追加：

```go
// stubTransport serves a fixed body for any request, for offline source tests.
type stubTransport struct{ body string }

func (s stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     make(http.Header),
	}, nil
}

func TestNyaaResultIncludesNyaaTracker(t *testing.T) {
	const rss = `<?xml version="1.0"?><rss><channel><item>` +
		`<title>Test Anime 01</title>` +
		`<infoHash>0123456789abcdef0123456789abcdef01234567</infoHash>` +
		`<seeders>5</seeders><leechers>1</leechers><size>500.0 MiB</size>` +
		`</item></channel></rss>`
	n := NewNyaa()
	n.Client = &http.Client{Transport: stubTransport{body: rss}}

	results, err := n.Search(context.Background(), "test")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if !strings.Contains(results[0].Magnet, "nyaa.tracker.wf") {
		t.Errorf("nyaa magnet missing nyaa tracker: %q", results[0].Magnet)
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/search/ -run TestNyaaResultIncludesNyaaTracker -v`
Expected: FAIL —— `nyaa magnet missing nyaa tracker`（nyaa.go 当前没有传 nyaa tracker）。

- [ ] **Step 3: 写实现**

在 `internal/search/nyaa.go` 中，把第 96 行：

```go
			Magnet:   MagnetFromInfoHash(hash, it.Title),
```

改为：

```go
			Magnet:   MagnetFromInfoHash(hash, it.Title, "http://nyaa.tracker.wf:7777/announce"),
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/search/ -run TestNyaaResultIncludesNyaaTracker -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/search/nyaa.go internal/search/search_test.go
git commit -m "feat(search): nyaa 结果保留 nyaa 官方 tracker"
```

---

### Task 5: 全量验证

**Files:** 无（仅验证）

- [ ] **Step 1: 整理依赖并构建**

Run: `go mod tidy && go build ./...`
Expected: 无错误。`go mod tidy` 若改动 go.mod/go.sum 属正常（dht/v2 已是 v2.23.0，通常无变化）。

- [ ] **Step 2: 跑全部单元测试**

Run: `go test ./...`
Expected: 所有包 PASS（含既有 cmd/dht/engine/search/store/tui 测试）。

- [ ] **Step 3: 提交 go.mod/go.sum（若有改动）**

```bash
git add go.mod go.sum
git commit -m "chore: go mod tidy" || echo "无改动，跳过"
```

- [ ] **Step 4: 手动验证 DHT 引导生效**

```bash
go build -o p2pget ./cmd/p2pget
rm -rf /tmp/p2pget_verify && mkdir /tmp/p2pget_verify
./p2pget get -data /tmp/p2pget_verify \
  'magnet:?xt=urn:btih:4a3f5e08bcef825718eda30637230585e3330599&dn=ubuntu-no-tracker'
```

这是一个**不带任何 tracker、纯靠 DHT** 的 magnet。观察约 3-10 分钟：
- 期望：打印「开始下载: ubuntu-24.04.1-desktop-amd64.iso」并出现 `peers=N`（N>0），即 DHT 引导成功、能找到 peer。
- 若仍卡「元数据中…」直至 10 分钟超时：DHT 引导虽已绕过 DNS 劫持，但 UDP 流量疑似被 DPI 限速/丢包（设计文档「非目标」已说明），需走 VPN 等网络层方案。

观察完用 Ctrl+C 停止。

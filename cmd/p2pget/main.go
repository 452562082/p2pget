// p2pget — a P2P (BitTorrent) downloader with aggregated search.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"p2pget/internal/dht"
	"p2pget/internal/engine"
	"p2pget/internal/search"
	"p2pget/internal/store"
	"p2pget/internal/tui"
)

func main() {
	if len(os.Args) < 2 {
		runTUI(os.Args[1:])
		return
	}
	switch os.Args[1] {
	case "tui", "":
		runTUI(os.Args[2:])
	case "search":
		runSearch(os.Args[2:])
	case "get":
		runGet(os.Args[2:])
	case "dht-crawl":
		runDHTCrawl(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		// treat as TUI launch if first arg looks like a flag
		if strings.HasPrefix(os.Args[1], "-") {
			runTUI(os.Args[1:])
			return
		}
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `p2pget — BitTorrent 搜索下载器

用法:
  p2pget                    启动 TUI (默认)
  p2pget tui                同上
  p2pget search <query>     命令行搜索 (-n 每页条数, -page 页码)
  p2pget get <magnet|hash|.torrent|URL>   命令行下载
  p2pget dht-crawl          只跑 DHT 爬虫（后台收集 info-hash 并解析元数据）

通用选项:
  -data DIR       下载目录 (默认 ~/.p2pget/data)
  -index PATH     SQLite 索引路径 (默认 ~/.p2pget/index.db)
  -port N         BT 监听端口 (默认随机)
  -no-dht         关闭 DHT
  -no-upload      不做种 (下载完成后不上传)
  -rate-down N    下载限速 KB/s (默认不限)
  -rate-up N      上传限速 KB/s (默认不限)
  -max-conns N    每个种子最大连接数 (默认库默认值)
  -jackett-url U  Jackett 地址 (默认 http://localhost:9117)
  -jackett-key K  Jackett API key，设置后启用 jackett 搜索源

环境变量 P2PGET_JACKETT_URL / P2PGET_JACKETT_KEY
可作为上述两个选项的默认值，省去每次输入。
`)
}

// ---------------- common ----------------

type commonFlags struct {
	dataDir      string
	indexPath    string
	listenPort   int
	noDHT        bool
	noUpload     bool
	rateDown     int
	rateUp       int
	maxConns     int
	jackettURL string
	jackettKey string
}

// envOr returns the environment variable value for key, or def if unset.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func registerCommon(fs *flag.FlagSet) *commonFlags {
	home, _ := os.UserHomeDir()
	c := &commonFlags{}
	fs.StringVar(&c.dataDir, "data", filepath.Join(home, ".p2pget", "data"), "下载目录")
	fs.StringVar(&c.indexPath, "index", filepath.Join(home, ".p2pget", "index.db"), "DHT 索引数据库路径")
	fs.IntVar(&c.listenPort, "port", 0, "BT 监听端口 (0 表示随机)")
	fs.BoolVar(&c.noDHT, "no-dht", false, "禁用 DHT")
	fs.BoolVar(&c.noUpload, "no-upload", false, "不做种")
	fs.IntVar(&c.rateDown, "rate-down", 0, "下载限速 KB/s (0 表示不限)")
	fs.IntVar(&c.rateUp, "rate-up", 0, "上传限速 KB/s (0 表示不限)")
	fs.IntVar(&c.maxConns, "max-conns", 0, "每个种子最大连接数 (0 表示库默认)")
	fs.StringVar(&c.jackettURL, "jackett-url", envOr("P2PGET_JACKETT_URL", "http://localhost:9117"), "Jackett 地址")
	fs.StringVar(&c.jackettKey, "jackett-key", os.Getenv("P2PGET_JACKETT_KEY"), "Jackett API key (设置后启用 jackett 源)")
	return c
}

// parseArgs parses flags and positional arguments in any order, returning the
// positionals. Go's flag package otherwise stops at the first non-flag token,
// so `search "query" -n 5` would silently ignore the trailing flags. All flags
// must already be registered on fs before calling this.
func parseArgs(fs *flag.FlagSet, args []string) []string {
	var positionals []string
	for {
		_ = fs.Parse(args)
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
	return positionals
}

// openEngine builds an engine. statePath enables download-list persistence;
// pass "" for one-shot commands that should not resurrect prior downloads.
func openEngine(c *commonFlags, statePath string) (*engine.Engine, error) {
	return engine.New(engine.Config{
		DataDir:      c.dataDir,
		ListenPort:   c.listenPort,
		NoUpload:     c.noUpload,
		NoDHT:        c.noDHT,
		StatePath:    statePath,
		DownloadRate: c.rateDown * 1024,
		UploadRate:   c.rateUp * 1024,
		MaxConns:     c.maxConns,
	})
}

func openStore(c *commonFlags) (*store.Store, error) {
	return store.Open(c.indexPath)
}

func newAggregator(st *store.Store, c *commonFlags) *search.Aggregator {
	idx := []search.Indexer{
		search.NewPirateBay(),
		search.NewNyaa(),
		search.NewTorrentsCSV(),
	}
	if c.jackettKey != "" {
		idx = append(idx, search.NewJackett(c.jackettURL, c.jackettKey))
	}
	if st != nil {
		idx = append(idx, &dht.StoreIndexer{Store: st})
	}
	agg := search.New(idx...)
	// Jackett fans out to many trackers and is far slower than the JSON
	// APIs; widen the budget so it isn't cut off. Streaming search keeps
	// the fast sources appearing immediately regardless.
	if c.jackettKey != "" {
		agg.Timeout = 30 * time.Second
	}
	return agg
}

// ---------------- TUI ----------------

func runTUI(args []string) {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	c := registerCommon(fs)
	_ = fs.Parse(args)

	st, err := openStore(c)
	if err != nil {
		log.Fatalf("open index: %v", err)
	}
	defer st.Close()

	eng, err := openEngine(c, filepath.Join(filepath.Dir(c.dataDir), "tasks.json"))
	if err != nil {
		log.Fatalf("open engine: %v", err)
	}
	defer eng.Close()

	agg := newAggregator(st, c)

	model := tui.New(eng, agg)
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		log.Fatalf("tui: %v", err)
	}
}

// ---------------- search subcommand ----------------

func runSearch(args []string) {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	c := registerCommon(fs)
	limit := fs.Int("n", 20, "每页条数")
	page := fs.Int("page", 1, "页码 (从 1 开始)")
	pos := parseArgs(fs, args)

	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, "用法: p2pget search <关键字>")
		os.Exit(2)
	}
	query := strings.Join(pos, " ")

	st, _ := openStore(c) // index optional; ignore error
	if st != nil {
		defer st.Close()
	}
	agg := newAggregator(st, c)

	// The aggregator applies its own timeout (longer when Jackett is
	// enabled), so no extra deadline is needed here.
	results, errs := agg.Search(context.Background(), query)
	for name, err := range errs {
		fmt.Fprintf(os.Stderr, "[%s] %v\n", name, err)
	}

	per := *limit
	if per < 1 {
		per = 20
	}
	totalPages := (len(results) + per - 1) / per
	if totalPages < 1 {
		totalPages = 1
	}
	pg := *page
	if pg < 1 {
		pg = 1
	}
	start := (pg - 1) * per
	end := min(start+per, len(results))

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "S\tL\t SIZE\t SOURCE\t INFOHASH\t NAME")
	for i := start; i < end; i++ {
		r := results[i]
		fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t%s\t%s\n",
			r.Seeders, r.Leechers, search.HumanSize(r.Size),
			r.Source, r.InfoHash[:min(10, len(r.InfoHash))], r.Title)
	}
	tw.Flush()

	if start >= len(results) && len(results) > 0 {
		fmt.Fprintf(os.Stderr, "\n第 %d 页超出范围（共 %d 页）\n", pg, totalPages)
		return
	}
	fmt.Fprintf(os.Stderr, "\n共 %d 条结果 · 第 %d/%d 页", len(results), pg, totalPages)
	if pg < totalPages {
		fmt.Fprintf(os.Stderr, "（看下一页：-page %d）", pg+1)
	}
	fmt.Fprintln(os.Stderr)
}

// ---------------- get subcommand ----------------

func runGet(args []string) {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	c := registerCommon(fs)
	pos := parseArgs(fs, args)

	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, "用法: p2pget get <magnet:?... | info-hash | path/to/.torrent | http(s)://.../x.torrent>")
		os.Exit(2)
	}
	target := pos[0]

	eng, err := openEngine(c, "")
	if err != nil {
		log.Fatalf("open engine: %v", err)
	}
	defer eng.Close()

	var d *engine.Download
	switch {
	case strings.HasPrefix(target, "magnet:"):
		d, err = eng.AddMagnet(target)
	case strings.HasPrefix(target, "http://"), strings.HasPrefix(target, "https://"):
		d, err = eng.AddTorrentURL(target)
	case strings.HasSuffix(target, ".torrent"):
		d, err = eng.AddTorrentFile(target)
	case len(target) == 40:
		d, err = eng.AddInfoHash(target)
	default:
		err = fmt.Errorf("无法识别的目标: %s", target)
	}
	if err != nil {
		log.Fatalf("add: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Printf("元数据中…  %s\n", d.InfoHash())
	if err := eng.WaitInfo(ctx, d); err != nil {
		log.Fatalf("等待元数据: %v", err)
	}
	s := d.Status()
	fmt.Printf("开始下载: %s  (%s)\n", s.Name, search.HumanSize(s.TotalBytes))

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("中断")
			return
		case <-ticker.C:
			d.SampleRate()
			s := d.Status()
			line := fmt.Sprintf("%s  %s/%s  ↓%s  peers=%d seeders=%d  %.1f%%",
				s.Name, search.HumanSize(s.BytesDone), search.HumanSize(s.TotalBytes),
				search.HumanRate(s.DownRate), s.ActivePeers, s.Seeders, s.Progress()*100,
			)
			if eta := s.ETA(); eta > 0 {
				line += "  ETA " + eta.Round(time.Second).String()
			}
			fmt.Printf("\r%s   ", line)
			if s.HaveInfo && s.BytesDone >= s.TotalBytes && s.TotalBytes > 0 {
				fmt.Println("\n完成。")
				return
			}
		}
	}
}

// ---------------- dht-crawl subcommand ----------------

func runDHTCrawl(args []string) {
	fs := flag.NewFlagSet("dht-crawl", flag.ExitOnError)
	c := registerCommon(fs)
	verbose := fs.Bool("v", true, "打印爬虫日志")
	_ = fs.Parse(args)

	st, err := openStore(c)
	if err != nil {
		log.Fatalf("open index: %v", err)
	}
	defer st.Close()

	eng, err := openEngine(c, "")
	if err != nil {
		log.Fatalf("open engine: %v", err)
	}
	defer eng.Close()

	cr := dht.New(st, eng)
	if *verbose {
		cr.Logger = log.New(os.Stderr, "dht ", log.Ltime)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Periodic stats.
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				total, resolved, err := st.Stats(ctx)
				if err == nil {
					fmt.Fprintf(os.Stderr, "[stats] total=%d resolved=%d\n", total, resolved)
				}
			}
		}
	}()

	if err := cr.Start(ctx); err != nil {
		log.Fatalf("crawler: %v", err)
	}
}

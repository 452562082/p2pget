// Package tui implements the bubbletea TUI: search → results → downloads.
package tui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"p2pget/internal/engine"
	"p2pget/internal/search"
)

type tab int

const (
	tabSearch tab = iota
	tabDownloads
	tabHelp
)

type Model struct {
	engine     *engine.Engine
	aggregator *search.Aggregator

	width, height int
	active        tab

	input   textinput.Model
	results list.Model
	dls     list.Model

	status   string
	progress progress.Model

	// fileView, when non-nil, is a modal file-selection screen for one
	// download. It takes over input and rendering until dismissed.
	fileView *fileViewState

	// Streaming-search state. searchGen increments per search so that late
	// chunks from a superseded search can be discarded.
	searchGen  int
	searchAcc  []search.Result
	searchErrs map[string]error

	// dlList is the download slice backing the list; the list is only
	// rebuilt when this set of objects changes (the delegate reads live
	// progress per render). Holding the slice also keeps the pointers
	// alive, so identity comparison stays sound.
	dlList []*engine.Download

	// totalDown/totalUp are aggregate transfer rates across all downloads,
	// recomputed once per refresh tick for the status bar.
	totalDown float64
	totalUp   float64

	// confirmDelete holds the info-hash awaiting a "delete files" y/n
	// confirmation, or "" when no prompt is pending.
	confirmDelete string
}

type fileViewState struct {
	d      *engine.Download
	name   string
	files  []engine.FileInfo
	cursor int
}

func New(eng *engine.Engine, agg *search.Aggregator) *Model {
	ti := textinput.New()
	ti.Placeholder = "输入关键字，回车搜索…"
	ti.Prompt = "🔍 "
	ti.CharLimit = 200
	ti.Width = 60
	ti.Focus()

	results := list.New(nil, resultDelegate{}, 0, 0)
	results.Title = "搜索结果"
	results.SetShowStatusBar(true)
	results.SetFilteringEnabled(false)
	results.DisableQuitKeybindings()

	dls := list.New(nil, downloadDelegate{}, 0, 0)
	dls.Title = "下载任务"
	dls.SetFilteringEnabled(false)
	dls.DisableQuitKeybindings()

	pb := progress.New(progress.WithDefaultGradient())

	return &Model{
		engine:     eng,
		aggregator: agg,
		input:      ti,
		results:    results,
		dls:        dls,
		progress:   pb,
		active:     tabSearch,
	}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, tickRefresh())
}

type refreshMsg time.Time

func tickRefresh() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return refreshMsg(t) })
}

// searchChunkMsg carries one indexer's streamed result. ch is the source
// channel, replayed into the next waitChunk command so chunks keep flowing.
type searchChunkMsg struct {
	ch    <-chan search.IndexerResult
	r     search.IndexerResult
	ok    bool
	query string
	gen   int
}

func (m *Model) startSearch(query string) tea.Cmd {
	m.searchGen++
	m.searchAcc = nil
	m.searchErrs = map[string]error{}
	m.results.SetItems(nil)
	ch := m.aggregator.SearchStream(context.Background(), query)
	return waitChunk(ch, query, m.searchGen)
}

func waitChunk(ch <-chan search.IndexerResult, query string, gen int) tea.Cmd {
	return func() tea.Msg {
		r, ok := <-ch
		return searchChunkMsg{ch: ch, r: r, ok: ok, query: query, gen: gen}
	}
}

func (m *Model) searchErrSummary() string {
	if len(m.searchErrs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m.searchErrs))
	for name, err := range m.searchErrs {
		parts = append(parts, fmt.Sprintf("%s:%v", name, err))
	}
	return " | 错误: " + strings.Join(parts, "; ")
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		listH := m.height - 7 // chrome: tabs, blank, status, totals, help
		if listH < 5 {
			listH = 5
		}
		m.results.SetSize(m.width-4, listH-3)
		m.dls.SetSize(m.width-4, listH)
		m.progress.Width = m.width - 10

	case tea.KeyMsg:
		if m.fileView != nil {
			return m.updateFileView(msg)
		}
		if m.confirmDelete != "" {
			switch msg.String() {
			case "y", "Y":
				hash := m.confirmDelete
				m.confirmDelete = ""
				if err := m.engine.Remove(hash, true); err != nil {
					m.status = "删除失败: " + err.Error()
				} else {
					m.status = "已删除任务及文件"
					m.refreshDownloads()
				}
			case "ctrl+c", "ctrl+q":
				return m, tea.Quit
			default:
				m.confirmDelete = ""
				m.status = "已取消删除"
			}
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c", "ctrl+q":
			return m, tea.Quit
		case "tab":
			m.active = (m.active + 1) % 3
			m.syncInputFocus()
			return m, nil
		case "shift+tab":
			m.active = (m.active + 2) % 3
			m.syncInputFocus()
			return m, nil
		case "q":
			if !m.input.Focused() {
				return m, tea.Quit
			}
		case "/":
			if m.active == tabSearch && !m.input.Focused() {
				m.input.Focus()
				return m, textinput.Blink
			}
		case "d":
			if m.active == tabDownloads && !m.input.Focused() {
				if it, ok := m.dls.SelectedItem().(downloadItem); ok {
					if err := m.engine.Remove(it.d.InfoHash(), false); err != nil {
						m.status = "移除失败: " + err.Error()
					} else {
						m.status = "已移除任务（磁盘文件保留）"
						m.refreshDownloads()
					}
					return m, nil
				}
			}
		case "p":
			if m.active == tabDownloads && !m.input.Focused() {
				if it, ok := m.dls.SelectedItem().(downloadItem); ok {
					paused := !it.d.Paused()
					it.d.SetPaused(paused)
					if paused {
						m.status = "已暂停"
					} else {
						m.status = "已恢复"
					}
					return m, nil
				}
			}
			// On a search result, add it paused so files can be picked
			// before any download starts.
			if m.active == tabSearch && !m.input.Focused() {
				if it, ok := m.results.SelectedItem().(resultItem); ok {
					return m, m.addDownload(it.r, true)
				}
			}
		case "D":
			if m.active == tabDownloads && !m.input.Focused() {
				if it, ok := m.dls.SelectedItem().(downloadItem); ok {
					m.confirmDelete = it.d.InfoHash()
					m.status = "删除「" + it.d.Name() + "」及已下载文件？ y 确认 / 其他键取消"
					return m, nil
				}
			}
		case "esc":
			if m.input.Focused() {
				m.input.Blur()
				return m, nil
			}
		case "enter":
			if m.active == tabSearch && m.input.Focused() {
				q := strings.TrimSpace(m.input.Value())
				if q == "" {
					return m, nil
				}
				m.status = "搜索中: " + q
				m.input.Blur()
				return m, m.startSearch(q)
			}
			if m.active == tabSearch && !m.input.Focused() {
				if it, ok := m.results.SelectedItem().(resultItem); ok {
					return m, m.addDownload(it.r, false)
				}
			}
			if m.active == tabDownloads {
				if it, ok := m.dls.SelectedItem().(downloadItem); ok {
					files := it.d.Files()
					if files == nil {
						m.status = "元数据未就绪，暂时无法选择文件"
					} else {
						m.fileView = &fileViewState{
							d:     it.d,
							name:  it.d.Status().Name,
							files: files,
						}
					}
				}
			}
		}

	case searchChunkMsg:
		if msg.gen != m.searchGen {
			return m, nil // a newer search superseded this one
		}
		if !msg.ok {
			ranked := search.DedupAndRank(m.searchAcc)
			m.status = fmt.Sprintf("找到 %d 条 (q=%q)%s", len(ranked), msg.query, m.searchErrSummary())
			return m, nil
		}
		if msg.r.Err != nil {
			m.searchErrs[msg.r.Name] = msg.r.Err
		} else {
			m.searchAcc = append(m.searchAcc, msg.r.Results...)
		}
		ranked := search.DedupAndRank(m.searchAcc)
		items := make([]list.Item, 0, len(ranked))
		for _, r := range ranked {
			items = append(items, resultItem{r: r})
		}
		m.results.SetItems(items)
		m.status = fmt.Sprintf("搜索中… 已收到 %d 条 (q=%q)", len(ranked), msg.query)
		return m, waitChunk(msg.ch, msg.query, msg.gen)

	case refreshMsg:
		m.engine.SampleRates()
		m.pruneCompleted()
		m.restartStalled()
		m.refreshDownloads()
		m.recomputeTotals()
		if m.fileView != nil {
			m.fileView.files = m.fileView.d.Files()
			if n := len(m.fileView.files); m.fileView.cursor >= n {
				m.fileView.cursor = max(0, n-1)
			}
		}
		return m, tickRefresh()

	case downloadAddedMsg:
		switch {
		case msg.err != nil:
			m.status = "添加失败: " + msg.err.Error()
		case msg.paused:
			m.status = "已暂停加入: " + msg.name + "（下载列表里 Enter 选文件，选好按 p 开始）"
			m.refreshDownloads()
		default:
			m.status = "已加入下载: " + msg.name
			m.refreshDownloads()
		}
		return m, nil
	}

	var cmd tea.Cmd
	if m.active == tabSearch {
		if m.input.Focused() {
			m.input, cmd = m.input.Update(msg)
		} else {
			m.results, cmd = m.results.Update(msg)
		}
	} else if m.active == tabDownloads {
		m.dls, cmd = m.dls.Update(msg)
	}
	return m, cmd
}

type downloadAddedMsg struct {
	name   string
	paused bool
	err    error
}

// addDownload adds r to the engine. When paused is true the download is held
// before any piece is fetched, so the user can pick files first.
func (m *Model) addDownload(r search.Result, paused bool) tea.Cmd {
	return func() tea.Msg {
		var (
			d   *engine.Download
			err error
		)
		switch {
		case r.Magnet != "":
			d, err = m.engine.AddMagnet(r.Magnet)
		case r.InfoHash != "":
			d, err = m.engine.AddInfoHash(r.InfoHash)
		default:
			err = fmt.Errorf("结果没有 magnet 也没有 infohash")
		}
		if err != nil {
			return downloadAddedMsg{err: err}
		}
		if paused {
			// Set before metadata can arrive (network fetch takes much
			// longer than this call), so no unwanted piece is requested.
			d.SetPaused(true)
		}
		name := r.Title
		if name == "" {
			name = d.InfoHash()
		}
		return downloadAddedMsg{name: name, paused: paused}
	}
}

// syncInputFocus drops focus from the search box when the user is no longer
// on the search tab. Tab-switching previously left the input focused even on
// the downloads tab, which silently swallowed shortcut keys like 'd' / 'p'
// because their guards bail out whenever the input claims focus.
func (m *Model) syncInputFocus() {
	if m.active != tabSearch && m.input.Focused() {
		m.input.Blur()
	}
}

func (m *Model) refreshDownloads() {
	dls := m.engine.List()
	// Rebuild only when the set of download objects changes. Identity
	// comparison (not info-hash) matters because a failed download can be
	// replaced by a fresh object under the same hash.
	if sameDownloads(dls, m.dlList) {
		return // unchanged; the delegate renders fresh Status() each frame
	}
	m.dlList = dls
	items := make([]list.Item, 0, len(dls))
	for _, d := range dls {
		items = append(items, downloadItem{d: d})
	}
	m.dls.SetItems(items)
}

// isCompleted reports whether a download has fetched every wanted byte. Status
// counts only wanted files in TotalBytes/BytesDone, so this also returns true
// when the user deselected files mid-download and the remaining ones finished.
func isCompleted(s engine.Status) bool {
	return s.HaveInfo && s.TotalBytes > 0 && s.BytesDone >= s.TotalBytes
}

// pruneCompleted drops finished downloads from the engine (keeping their files
// on disk) so the list reflects "active work" rather than accumulating done
// tasks. Runs on every refresh tick; the file-select modal pins a Download
// pointer, so we leave whichever one it's showing in place to avoid handing
// the modal a dropped torrent.
func (m *Model) pruneCompleted() {
	var done []string
	for _, d := range m.engine.List() {
		if m.fileView != nil && m.fileView.d == d {
			continue
		}
		s := d.Status()
		if !isCompleted(s) {
			continue
		}
		if err := m.engine.Remove(s.InfoHash, false); err == nil {
			done = append(done, s.Name)
		}
	}
	switch len(done) {
	case 0:
		return
	case 1:
		m.status = fmt.Sprintf("✓ %s 下载完成，已移出列表（文件保留在 %s）", done[0], m.engine.DataDir())
	default:
		m.status = fmt.Sprintf("✓ %d 个任务下载完成，已移出列表（文件保留在 %s）", len(done), m.engine.DataDir())
	}
}

// stallRestartThreshold is how long a download must look completely
// disconnected (0 B/s and 0 active peers) before the watchdog drops and
// re-adds it. Tuned for the wake-from-sleep case: long enough that a slow
// network or a brief peer-pool churn won't trigger a needless restart, short
// enough that the user doesn't have to wait forever after the laptop wakes.
const stallRestartThreshold = 3 * time.Minute

// restartStalled looks for downloads that have been completely disconnected
// for longer than stallRestartThreshold and has the engine re-create them.
// Targets the common wake-from-sleep scenario where every TCP connection went
// silently stale and anacrolix's own retry cadence takes minutes to notice.
func (m *Model) restartStalled() {
	for _, d := range m.engine.List() {
		if m.fileView != nil && m.fileView.d == d {
			continue
		}
		s := d.Status()
		// Skip pre-metadata (different failure mode, owned by awaitMetadata's
		// timeout), paused (user's choice), and completed (pruneCompleted
		// will handle it). Active peers or any traffic means alive enough.
		if !s.HaveInfo || s.Paused || isCompleted(s) {
			continue
		}
		if s.ActivePeers > 0 || s.DownRate >= 1 {
			continue
		}
		if d.StallDuration() < stallRestartThreshold {
			continue
		}
		name := s.Name
		if err := m.engine.Restart(s.InfoHash); err == nil {
			m.status = fmt.Sprintf("⟳ %s 连接已僵死 %v，重启任务以重连 peers",
				name, stallRestartThreshold)
		}
	}
}

func (m *Model) recomputeTotals() {
	var down, up float64
	for _, d := range m.dlList {
		s := d.Status()
		down += s.DownRate
		up += s.UpRate
	}
	m.totalDown, m.totalUp = down, up
}

func sameDownloads(a, b []*engine.Download) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (m *Model) updateFileView(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	fv := m.fileView
	switch msg.String() {
	case "ctrl+c", "ctrl+q":
		return m, tea.Quit
	case "esc", "q", "enter":
		m.fileView = nil
		m.refreshDownloads()
	case "up", "k":
		if fv.cursor > 0 {
			fv.cursor--
		}
	case "down", "j":
		if fv.cursor < len(fv.files)-1 {
			fv.cursor++
		}
	case " ":
		if len(fv.files) > 0 {
			f := fv.files[fv.cursor]
			if err := fv.d.SetFileWanted(f.Index, !f.Wanted); err != nil {
				m.status = "切换失败: " + err.Error()
			} else {
				fv.files = fv.d.Files()
			}
		}
	case "a":
		fv.d.SetAllWanted(true)
		fv.files = fv.d.Files()
	case "n":
		fv.d.SetAllWanted(false)
		fv.files = fv.d.Files()
	}
	return m, nil
}

var (
	tabActive   = lipgloss.NewStyle().Background(lipgloss.Color("63")).Foreground(lipgloss.Color("230")).Padding(0, 2).Bold(true)
	tabInactive = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Padding(0, 2)
	statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Italic(true)
	helpStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
)

func (m *Model) View() string {
	if m.fileView != nil {
		return m.fileViewRender()
	}

	var b strings.Builder
	tabs := []string{"搜索", "下载", "帮助"}
	for i, name := range tabs {
		style := tabInactive
		if int(m.active) == i {
			style = tabActive
		}
		b.WriteString(style.Render(fmt.Sprintf("[%d] %s", i+1, name)))
	}
	b.WriteString("\n\n")

	switch m.active {
	case tabSearch:
		b.WriteString(m.input.View())
		b.WriteString("\n\n")
		b.WriteString(m.results.View())
	case tabDownloads:
		b.WriteString(m.dls.View())
	case tabHelp:
		b.WriteString(helpText())
	}

	b.WriteString("\n")
	if m.confirmDelete != "" {
		b.WriteString(leechersStyle.Render(m.status))
	} else {
		b.WriteString(statusStyle.Render(m.status))
	}
	b.WriteString("\n")
	b.WriteString(helpStyle.Render(fmt.Sprintf("总计 ↓%s ↑%s · %d 个任务",
		search.HumanRate(m.totalDown), search.HumanRate(m.totalUp), len(m.dlList))))
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("Tab 切换  /搜索  Enter 下载/选文件  p 暂停  d 移除  q 退出"))
	return b.String()
}

func helpText() string {
	return strings.Join([]string{
		"p2pget — 基于 BitTorrent 的 P2P 搜索下载器",
		"",
		"快捷键:",
		"  Tab / Shift+Tab   切换标签页",
		"  /                 聚焦搜索框",
		"  Esc               取消输入焦点",
		"  Enter (搜索框)    执行搜索",
		"  Enter (结果列表)  加入并立即下载",
		"  p     (结果列表)  暂停加入（先选文件，避免下到不想要的文件）",
		"  Enter (下载列表)  选择要下载的文件",
		"  p     (下载列表)  暂停 / 恢复任务",
		"  d     (下载列表)  移除任务（保留磁盘文件）",
		"  D     (下载列表)  移除任务并删除文件（需确认）",
		"  ↑ ↓               移动选择",
		"  q / Ctrl+C        退出",
		"",
		"数据源:",
		"  - piratebay     apibay.org JSON API（综合）",
		"  - nyaa          nyaa.si RSS（动漫为主）",
		"  - torrents-csv  torrents-csv.com 开放 JSON API（综合）",
		"  - jackett       本地 Jackett（需配置，覆盖数百站点）",
		"  - dht-index     本地 SQLite，存爬虫收集结果",
		"",
		"下载文件保存目录: ~/.p2pget/data/",
		"",
		"任务完成后会自动从列表移除（磁盘文件保留），客户端也会停止做种。",
		"持续 3 分钟无 peer 也无流量（如笔记本睡眠唤醒后连接全部僵死）会自动重启该任务以重新连接 peers。",
	}, "\n")
}

func (m *Model) fileViewRender() string {
	fv := m.fileView
	var b strings.Builder
	b.WriteString(tabActive.Render("文件选择"))
	b.WriteString(" ")
	b.WriteString(titleStyle.Render(truncate(fv.name, m.width-16)))
	b.WriteString("\n\n")

	selected, wantBytes := 0, int64(0)
	for _, f := range fv.files {
		if f.Wanted {
			selected++
			wantBytes += f.Length
		}
	}

	if len(fv.files) == 0 {
		b.WriteString(dimStyle.Render("(无文件)\n"))
	}

	height := m.height - 6
	if height < 3 {
		height = 3
	}
	start := 0
	if fv.cursor >= height {
		start = fv.cursor - height + 1
	}
	end := start + height
	if end > len(fv.files) {
		end = len(fv.files)
	}

	for i := start; i < end; i++ {
		f := fv.files[i]
		cursor := "  "
		if i == fv.cursor {
			cursor = "▸ "
		}
		box := "[ ]"
		if f.Wanted {
			box = "[x]"
		}
		pct := 0.0
		if f.Length > 0 {
			pct = float64(f.Completed) / float64(f.Length) * 100
		}
		line := fmt.Sprintf("%s%s %s  %s  %3.0f%%",
			cursor, box, truncate(f.Path, m.width-28),
			search.HumanSize(f.Length), pct)
		switch {
		case i == fv.cursor:
			b.WriteString(selectStyle.Render(line))
		case f.Wanted:
			b.WriteString(line)
		default:
			b.WriteString(dimStyle.Render(line))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(statusStyle.Render(fmt.Sprintf("已选 %d/%d 文件 · %s",
		selected, len(fv.files), search.HumanSize(wantBytes))))
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("↑↓ 移动 · 空格 勾选 · a 全选 · n 全不选 · Esc/Enter 返回"))
	return b.String()
}

// ----- list items -----

var (
	titleStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("254")).Bold(true)
	sourceStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("105"))
	seedersStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	leechersStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	selectStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true)
)

type resultItem struct{ r search.Result }

func (i resultItem) FilterValue() string { return i.r.Title }

type resultDelegate struct{}

func (d resultDelegate) Height() int                         { return 2 }
func (d resultDelegate) Spacing() int                        { return 0 }
func (d resultDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }
func (d resultDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	it, ok := item.(resultItem)
	if !ok {
		return
	}
	r := it.r
	prefix := "  "
	style := titleStyle
	if index == m.Index() {
		prefix = "▸ "
		style = selectStyle
	}
	line1 := prefix + style.Render(truncate(r.Title, m.Width()-4))
	line2 := fmt.Sprintf("    %s  %s  S:%s L:%s  %s",
		sourceStyle.Render("["+r.Source+"]"),
		dimStyle.Render(search.HumanSize(r.Size)),
		seedersStyle.Render(fmt.Sprintf("%d", r.Seeders)),
		leechersStyle.Render(fmt.Sprintf("%d", r.Leechers)),
		dimStyle.Render(shortHash(r.InfoHash)),
	)
	fmt.Fprintln(w, line1)
	fmt.Fprint(w, line2)
}

type downloadItem struct{ d *engine.Download }

func (i downloadItem) FilterValue() string { return i.d.Name() }

type downloadDelegate struct{}

func (d downloadDelegate) Height() int                         { return 3 }
func (d downloadDelegate) Spacing() int                        { return 1 }
func (d downloadDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }
func (d downloadDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	it, ok := item.(downloadItem)
	if !ok {
		return
	}
	s := it.d.Status()
	name := s.Name
	if name == "" {
		name = "(等待元数据) " + shortHash(s.InfoHash)
	}
	prefix := "  "
	style := titleStyle
	if index == m.Index() {
		prefix = "▸ "
		style = selectStyle
	}
	fmt.Fprintln(w, prefix+style.Render(truncate(name, m.Width()-4)))
	switch {
	case s.Err != "":
		fmt.Fprintln(w, leechersStyle.Render("    [失败: "+s.Err+"]"))
	case s.HaveInfo:
		bar := renderBar(s.Progress(), m.Width()-12)
		line := fmt.Sprintf("    %s %5.1f%%", bar, s.Progress()*100)
		if s.Paused {
			line += "  " + leechersStyle.Render("⏸ 已暂停")
		}
		fmt.Fprintln(w, line)
	default:
		fmt.Fprintln(w, dimStyle.Render("    [获取元数据中…]"))
	}
	line3 := fmt.Sprintf("    %s / %s  ↓%s ↑%s  peers:%d seeders:%d",
		search.HumanSize(s.BytesDone), search.HumanSize(s.TotalBytes),
		search.HumanRate(s.DownRate), search.HumanRate(s.UpRate),
		s.ActivePeers, s.Seeders,
	)
	if eta := s.ETA(); eta > 0 {
		line3 += "  ETA " + formatETA(eta)
	}
	fmt.Fprint(w, dimStyle.Render(truncate(line3, m.Width()-2)))
}

func formatETA(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", d/time.Hour, (d%time.Hour)/time.Minute)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", d/time.Minute, (d%time.Minute)/time.Second)
	default:
		return fmt.Sprintf("%ds", d/time.Second)
	}
}

// truncate shortens s to at most w terminal columns, appending an ellipsis if
// cut. Width is measured in display columns, so wide CJK characters (common in
// anime/movie names) count as two and lines don't overflow their budget.
func truncate(s string, w int) string {
	if w <= 0 {
		return s
	}
	return runewidth.Truncate(s, w, "…")
}

func shortHash(h string) string {
	if len(h) <= 10 {
		return h
	}
	return h[:10]
}

func renderBar(p float64, width int) string {
	if width < 4 {
		width = 4
	}
	filled := int(p * float64(width))
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

// ui.go implements `mem ui`: a Bubble Tea terminal interface over the same
// index the CLI reads.
//
// The design rule is that the TUI owns no search logic of its own. Every
// screen is a thin shell over the Store methods the CLI already uses, so a
// result shown here is the same result `mem search` would print, and the
// safety model (scan reads only, organize copies only) is untouched. The
// interface adds presentation and navigation, not behaviour.
//
// Look and feel follow one rule, borrowed from the tools this borrows its
// shape from: flat monochrome text, one warm accent, a rounded input box, and
// structure carried by an indented gutter (⏺ / ⎿) rather than boxes drawn
// around everything.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

// uiView identifies which screen is on top.
type uiView int

const (
	viewSearch uiView = iota
	viewDetail
	viewRelated
	viewStats
	viewScan
	viewOrganize
	viewHelp
)

// ---------------------------------------------------------------------------
// Messages
//
// Results of I/O come back as messages rather than blocking the update loop,
// which is what keeps the UI responsive while a scan runs or a query is slow.
// ---------------------------------------------------------------------------

type searchMsg struct {
	results []result
	conf    []float64
	pq      parsedQuery
	total   int
	err     error
}

type detailMsg struct {
	content string
	err     error
}

type relatedMsg struct {
	data RelatedData
	err  error
}

type statsMsg struct {
	data StatsData
	err  error
}

// debounceMsg carries the search sequence number it was scheduled for, so a
// stale timer from an earlier keystroke can be ignored.
type debounceMsg struct{ seq int }

type scanEvent struct{ msg tea.Msg }
type scanProgressMsg ScanProgress
type scanDoneMsg struct {
	rep ScanReport
	err error
}

type organizePlanMsg struct {
	plan organizePreview
	err  error
}

type organizeAppliedMsg struct {
	copied  int
	skipped int
	err     error
}

// toastMsg shows a short confirmation; toastExpireMsg clears it again. The
// sequence number makes sure a toast that has already been replaced does not
// clear the newer one when its timer fires.
type toastMsg string
type toastExpireMsg struct{ seq int }

// uiModel is the whole application state.
type uiModel struct {
	store  *Store
	dbPath string

	width, height int
	view          uiView
	prevView      uiView

	// theme
	plain  bool // NO_COLOR / dumb terminal: no colour, ASCII glyphs
	isDark bool

	// search
	input   textinput.Model
	seq     int
	results []result
	conf    []float64
	pq      parsedQuery
	total   int
	cursor  int
	limit   int
	ext     string

	// detail
	detail   result
	content  string
	viewport viewport.Model

	// related
	related RelatedData

	// stats
	stats StatsData

	// scan
	scanInput textinput.Model
	scanning  bool
	scanProg  ScanProgress
	scanRep   *ScanReport
	scanErr   error
	scanCh    chan scanEvent

	// organize
	orgInput   textinput.Model
	orgPlan    *organizePreview
	orgConfirm bool
	orgResult  string
	orgErr     error

	// work-in-progress state, so the spinner knows when to run
	searching  bool
	planning   bool
	spin       spinner.Model
	spinActive bool

	toast    string
	toastSeq int

	styles uiStyles
}

// newUIModel builds the initial state. It opens no files and runs no query:
// the first search happens when the user types, and stats only when asked.
func newUIModel(store *Store, dbPath string, limit int, ext string) *uiModel {
	if limit <= 0 {
		limit = 10
	}

	plain := plainOutput()

	input := textinput.New()
	input.Prompt = "> "
	input.Placeholder = `describe the file you remember, e.g. "the pdf about linux scheduling"`
	input.Focus()

	scanInput := textinput.New()
	scanInput.Prompt = "> "
	scanInput.Placeholder = "folder to index, e.g. ~/Downloads"
	scanInput.SetValue("~/")

	orgInput := textinput.New()
	orgInput.Prompt = "> "
	orgInput.Placeholder = "folder to tidy, e.g. ~/Downloads"
	orgInput.SetValue("~/")

	m := &uiModel{
		store:     store,
		dbPath:    dbPath,
		view:      viewSearch,
		plain:     plain,
		isDark:    true, // assume dark until the terminal tells us otherwise
		input:     input,
		scanInput: scanInput,
		orgInput:  orgInput,
		limit:     limit,
		ext:       ext,
		viewport:  viewport.New(),
	}
	m.styles = newUIStyles(plain, m.isDark)
	m.spin = spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(m.styles.accentS))
	m.applyInputStyles()

	if n, err := store.Count(); err == nil {
		m.total = n
	}
	return m
}

// plainOutput reports whether the terminal wants no colour at all. NO_COLOR is
// the cross-tool convention; TERM=dumb is the fallback for terminals that do
// not do ANSI.
func plainOutput() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return true
	}
	return os.Getenv("TERM") == "dumb"
}

// Init implements tea.Model. We ask the terminal for its background colour so
// the palette can adapt; the first frame is drawn with the dark default.
func (m *uiModel) Init() tea.Cmd { return tea.RequestBackgroundColor }

// Update implements tea.Model.
func (m *uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case tea.BackgroundColorMsg:
		if !m.plain {
			m.isDark = msg.IsDark()
			m.styles = newUIStyles(m.plain, m.isDark)
			m.spin.Style = m.styles.accentS
			m.applyInputStyles()
		}
		return m, nil

	case tea.KeyPressMsg:
		// Quitting is the one key that works the same on every screen.
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m, m.handleKey(msg)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		if !m.isBusy() {
			m.spinActive = false
			return m, nil
		}
		return m, cmd

	case debounceMsg:
		if msg.seq == m.seq {
			m.cursor = 0
			m.searching = true
			return m, tea.Batch(m.runSearch(), m.kickSpin())
		}
		return m, nil

	case searchMsg:
		m.searching = false
		if msg.err != nil {
			m.toast = "search error: " + msg.err.Error()
			return m, nil
		}
		m.results = msg.results
		m.conf = msg.conf
		m.pq = msg.pq
		if msg.total > 0 {
			m.total = msg.total
		}
		if m.cursor >= len(m.results) {
			m.cursor = max(0, len(m.results)-1)
		}
		return m, nil

	case detailMsg:
		if msg.err != nil {
			m.content = "(could not read stored text: " + msg.err.Error() + ")"
		} else {
			m.content = msg.content
		}
		m.renderDetail()
		return m, nil

	case relatedMsg:
		if msg.err != nil {
			m.toast = msg.err.Error()
			return m, nil
		}
		m.related = msg.data
		m.cursor = 0
		return m, nil

	case statsMsg:
		if msg.err != nil {
			m.toast = "stats error: " + msg.err.Error()
			return m, nil
		}
		m.stats = msg.data
		return m, nil

	case scanProgressMsg:
		m.scanProg = ScanProgress(msg)
		return m, waitForScan(m.scanCh)

	case scanDoneMsg:
		m.scanning = false
		if msg.err != nil {
			m.scanErr = msg.err
		} else {
			rep := msg.rep
			m.scanRep = &rep
			m.total = rep.Total
		}
		return m, nil

	case organizePlanMsg:
		m.planning = false
		if msg.err != nil {
			m.orgErr = msg.err
			return m, nil
		}
		plan := msg.plan
		m.orgPlan = &plan
		return m, nil

	case organizeAppliedMsg:
		if msg.err != nil {
			m.orgErr = msg.err
			return m, nil
		}
		m.orgResult = fmt.Sprintf("Copied %d files, skipped %d. Originals untouched.", msg.copied, msg.skipped)
		return m, m.pushToast(fmt.Sprintf("copied %d files", msg.copied))

	case toastMsg:
		return m, m.setToast(string(msg))

	case toastExpireMsg:
		if msg.seq == m.toastSeq {
			m.toast = ""
		}
		return m, nil
	}
	return m, nil
}

// View implements tea.Model. Alt screen is switched on here because Bubble Tea
// v2 moved that from a program option onto the view.
func (m *uiModel) View() tea.View {
	var body string
	switch m.view {
	case viewDetail:
		body = m.viewDetail()
	case viewRelated:
		body = m.viewRelated()
	case viewStats:
		body = m.viewStats()
	case viewScan:
		body = m.viewScan()
	case viewOrganize:
		body = m.viewOrganize()
	case viewHelp:
		body = m.viewHelp()
	default:
		body = m.viewSearch()
	}
	v := tea.NewView(body)
	v.AltScreen = true
	v.WindowTitle = m.windowTitle()
	return v
}

func (m *uiModel) windowTitle() string {
	if m.isBusy() {
		return "mem — " + strings.ToLower(m.busyVerb()) + "…"
	}
	return "mem"
}

// isBusy reports whether any async work is in flight, which is what drives the
// spinner and the busy status line.
func (m *uiModel) isBusy() bool { return m.searching || m.scanning || m.planning }

func (m *uiModel) busyVerb() string {
	switch {
	case m.scanning:
		return "Indexing"
	case m.planning:
		return "Planning"
	case m.searching:
		return "Searching"
	}
	return "Working"
}

// kickSpin starts the spinner's tick loop, but only if it is not already
// running — otherwise every keystroke would add another tick loop.
func (m *uiModel) kickSpin() tea.Cmd {
	if m.spinActive {
		return nil
	}
	m.spinActive = true
	return m.spin.Tick
}

// setToast shows a short message and schedules its own expiry.
func (m *uiModel) setToast(s string) tea.Cmd {
	m.toast = s
	m.toastSeq++
	seq := m.toastSeq
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg {
		return toastExpireMsg{seq: seq}
	})
}

// pushToast is setToast for use from inside already-returned command batches.
func (m *uiModel) pushToast(s string) tea.Cmd { return m.setToast(s) }

// layout resizes the pieces that depend on the terminal size.
func (m *uiModel) layout() {
	w := m.w()
	inner := w - 4
	if inner < 8 {
		inner = 8
	}
	m.input.SetWidth(inner)
	m.scanInput.SetWidth(inner)
	m.orgInput.SetWidth(inner)

	// Detail is the only screen that uses the viewport; its chrome is a header,
	// a four-line file card, and a status line.
	h := m.h() - 6
	if h < 1 {
		h = 1
	}
	m.viewport.SetWidth(w)
	m.viewport.SetHeight(h)
}

// handleKey dispatches a key press to the active screen.
func (m *uiModel) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	switch m.view {
	case viewDetail:
		return m.keyDetail(msg)
	case viewRelated:
		return m.keyRelated(msg)
	case viewStats:
		return m.keyStats(msg)
	case viewScan:
		return m.keyScan(msg)
	case viewOrganize:
		return m.keyOrganize(msg)
	case viewHelp:
		return m.keyHelp(msg)
	default:
		return m.keySearch(msg)
	}
}

// ---------------------------------------------------------------------------
// Search screen
// ---------------------------------------------------------------------------

func (m *uiModel) keySearch(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()

	if m.input.Focused() {
		switch key {
		case "enter":
			m.cursor = 0
			m.searching = true
			return tea.Batch(m.runSearch(), m.kickSpin())
		case "esc":
			// Blur so the command keys (i, o, s, r, q) stop being text.
			m.input.Blur()
			return nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		m.seq++
		return tea.Batch(cmd, m.debounce())
	}

	switch key {
	case "q":
		return tea.Quit
	case "/", "esc":
		return m.input.Focus()
	case "enter":
		if m.cursor < len(m.results) {
			return m.loadDetail(m.results[m.cursor])
		}
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.results)-1 {
			m.cursor++
		}
	case "g":
		m.cursor = 0
	case "G":
		if len(m.results) > 0 {
			m.cursor = len(m.results) - 1
		}
	case "r":
		if m.cursor < len(m.results) {
			return m.loadRelated(m.results[m.cursor])
		}
	case "i":
		m.view = viewScan
		m.scanErr = nil
		return m.scanInput.Focus()
	case "o":
		m.view = viewOrganize
		m.orgErr = nil
		return m.orgInput.Focus()
	case "s":
		return m.loadStats()
	case "?":
		m.view = viewHelp
	}
	return nil
}

// runSearch executes the current input against the index in a command.
func (m *uiModel) runSearch() tea.Cmd {
	q := strings.TrimSpace(m.input.Value())
	store := m.store
	limit := m.limit
	ext := m.ext
	return func() tea.Msg {
		if q == "" {
			return searchMsg{pq: parsedQuery{raw: ""}}
		}
		res, pq, err := store.Search(q, limit, ext)
		if err != nil {
			return searchMsg{err: err}
		}
		if len(res) > limit {
			res = res[:limit]
		}
		total, _ := store.Count()
		return searchMsg{results: res, conf: confidence(res), pq: pq, total: total}
	}
}

// debounce schedules a search after a short pause, so typing does not hammer
// SQLite once per keystroke.
func (m *uiModel) debounce() tea.Cmd {
	seq := m.seq
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg {
		return debounceMsg{seq: seq}
	})
}

// ---------------------------------------------------------------------------
// Detail screen
// ---------------------------------------------------------------------------

func (m *uiModel) loadDetail(r result) tea.Cmd {
	m.detail = r
	m.content = ""
	m.view = viewDetail
	m.viewport.SetContent(m.styles.faint.Render("    loading…"))
	m.viewport.GotoTop()
	store := m.store
	id := r.ID
	return func() tea.Msg {
		c, err := store.Content(id)
		return detailMsg{content: c, err: err}
	}
}

// renderDetail fills the viewport with the file's stored text. The file card
// above it is drawn by viewDetail; this only owns the body.
func (m *uiModel) renderDetail() {
	if strings.TrimSpace(m.content) == "" {
		m.viewport.SetContent(m.styles.faint.Render(
			"    (no readable text — this file can only be found by name)"))
		m.viewport.GotoTop()
		return
	}
	m.viewport.SetContent(m.indentContent(m.content))
	m.viewport.GotoTop()
}

func (m *uiModel) keyDetail(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "q":
		return m.back()
	case "o":
		if err := openPath(m.detail.Path); err != nil {
			m.toast = "open failed: " + err.Error()
		} else {
			return m.pushToast("opened " + m.detail.Filename)
		}
		return nil
	case "c":
		path := m.detail.Path
		return tea.Batch(tea.SetClipboard(path), func() tea.Msg {
			return toastMsg("copied path")
		})
	case "r":
		return m.loadRelated(m.detail)
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return cmd
}

// back returns to the search screen, focusing the input again.
func (m *uiModel) back() tea.Cmd {
	m.view = viewSearch
	m.cursor = clampInt(m.cursor, 0, max(0, len(m.results)-1))
	return m.input.Focus()
}

// ---------------------------------------------------------------------------
// Related screen
// ---------------------------------------------------------------------------

func (m *uiModel) loadRelated(r result) tea.Cmd {
	store := m.store
	path := r.Path
	m.view = viewRelated
	m.related = RelatedData{Target: r}
	m.cursor = 0
	return func() tea.Msg {
		d, err := store.Related(path)
		return relatedMsg{data: d, err: err}
	}
}

func (m *uiModel) keyRelated(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "q":
		return m.back()
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.related.Hits)-1 {
			m.cursor++
		}
	case "enter":
		if m.cursor < len(m.related.Hits) {
			return m.loadDetail(m.related.Hits[m.cursor])
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Stats screen
// ---------------------------------------------------------------------------

func (m *uiModel) loadStats() tea.Cmd {
	store := m.store
	m.view = viewStats
	return func() tea.Msg {
		st, err := store.Stats()
		return statsMsg{data: st, err: err}
	}
}

func (m *uiModel) keyStats(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "q", "enter":
		return m.back()
	case "i":
		m.view = viewScan
		m.scanErr = nil
		return m.scanInput.Focus()
	case "o":
		m.view = viewOrganize
		m.orgErr = nil
		return m.orgInput.Focus()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scan screen
// ---------------------------------------------------------------------------

// startScan runs the real scan pipeline in the background and streams progress
// back as messages. It opens its own Store so a long index write cannot block
// the reads the rest of the UI is doing (WAL allows one writer plus readers).
func (m *uiModel) startScan(force bool) tea.Cmd {
	raw := strings.TrimSpace(m.scanInput.Value())
	if raw == "" {
		m.scanErr = fmt.Errorf("enter a directory to scan")
		return nil
	}
	dir := expandHome(raw)

	m.scanning = true
	m.scanErr = nil
	m.scanRep = nil
	m.scanProg = ScanProgress{}
	m.scanCh = make(chan scanEvent, 64)
	m.scanInput.Blur()

	dbPath := m.dbPath
	ch := m.scanCh
	go func() {
		defer close(ch)
		store, err := OpenStore(dbPath)
		if err != nil {
			ch <- scanEvent{msg: scanDoneMsg{err: err}}
			return
		}
		defer store.Close()
		rep, err := store.ScanAndIndex(dir, force, func(p ScanProgress) {
			ch <- scanEvent{msg: scanProgressMsg(p)}
		})
		ch <- scanEvent{msg: scanDoneMsg{rep: rep, err: err}}
	}()
	// The caller pairs this with the spinner tick; keeping the I/O command on
	// its own makes it directly drivable in tests.
	return waitForScan(ch)
}

// waitForScan reads exactly one event off the channel. The Update loop calls it
// again after each event, which is how a long-running scan streams into the
// model without blocking.
func waitForScan(ch chan scanEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return ev.msg
	}
}

func (m *uiModel) keyScan(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()

	// A running scan cannot be usefully interrupted mid-file, so the only key
	// that does anything is letting it finish. Progress keeps arriving either
	// way, even if the user leaves the screen.
	if m.scanning {
		return nil
	}

	if m.scanInput.Focused() {
		switch key {
		case "enter":
			return tea.Batch(m.startScan(false), m.kickSpin())
		case "esc":
			return m.back()
		}
		var cmd tea.Cmd
		m.scanInput, cmd = m.scanInput.Update(msg)
		return cmd
	}
	switch key {
	case "esc", "q":
		return m.back()
	case "enter", "/":
		return m.scanInput.Focus()
	case "f":
		return tea.Batch(m.startScan(true), m.kickSpin())
	}
	return nil
}

// ---------------------------------------------------------------------------
// Organize screen
// ---------------------------------------------------------------------------

func (m *uiModel) loadOrganizePlan() tea.Cmd {
	raw := strings.TrimSpace(m.orgInput.Value())
	if raw == "" {
		m.orgErr = fmt.Errorf("enter a directory")
		return nil
	}
	dir := expandHome(raw)
	m.orgErr = nil
	m.orgResult = ""
	m.orgConfirm = false
	m.orgPlan = nil
	m.planning = true
	// The caller pairs this with the spinner tick.
	return func() tea.Msg {
		p, err := OrganizePlan(dir)
		return organizePlanMsg{plan: p, err: err}
	}
}

func (m *uiModel) applyOrganize() tea.Cmd {
	if m.orgPlan == nil {
		return nil
	}
	plan := *m.orgPlan
	dest := plan.Dir
	return func() tea.Msg {
		copied, skipped, err := plan.Apply(dest)
		return organizeAppliedMsg{copied: copied, skipped: skipped, err: err}
	}
}

func (m *uiModel) keyOrganize(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()

	if m.orgConfirm {
		switch key {
		case "y":
			m.orgConfirm = false
			return m.applyOrganize()
		case "n", "esc":
			m.orgConfirm = false
		}
		return nil
	}

	if m.orgInput.Focused() {
		switch key {
		case "enter":
			return tea.Batch(m.loadOrganizePlan(), m.kickSpin())
		case "esc":
			return m.back()
		}
		var cmd tea.Cmd
		m.orgInput, cmd = m.orgInput.Update(msg)
		return cmd
	}
	switch key {
	case "esc", "q":
		return m.back()
	case "enter", "/":
		return m.orgInput.Focus()
	case "a":
		if m.orgPlan != nil && m.orgPlan.Count > 0 {
			m.orgConfirm = true
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Help screen
// ---------------------------------------------------------------------------

func (m *uiModel) keyHelp(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "q", "?", "enter":
		return m.back()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// openPath opens a file with the desktop's default handler. It is the one
// place the TUI shells out, and it never writes to the file.
func openPath(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// cmdUI is the `mem ui` entry point.
func cmdUI(args []string, dbPath string) error {
	limit := 10
	if v, ok := flagValue(args, "--limit"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	} else if v, ok := flagValue(args, "-n"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	var ext string
	if v, ok := flagValue(args, "--type"); ok {
		ext = v
	} else if v, ok := flagValue(args, "-t"); ok {
		ext = v
	}

	store, err := OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	m := newUIModel(store, dbPath, limit, ext)
	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		return err
	}
	return nil
}

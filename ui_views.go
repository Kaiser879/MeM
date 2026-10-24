// ui_views.go renders each TUI screen with Lip Gloss. It is deliberately
// separate from ui.go: ui.go decides what happens, this file decides only how
// it looks. No query or filesystem call lives here.
//
// The visual language is flat and text-first: bold text for primary content,
// dim text for secondary, one warm accent, a rounded input box, and an
// indented bullet/gutter (⏺ / ⎿) instead of boxes around everything. It adapts
// to light and dark terminals, and falls back to plain ASCII when colour is
// switched off.
package main

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
)

// uiStyles holds every style once, so the palette can be changed in one place.
type uiStyles struct {
	plain  bool
	accent color.Color // kept as a colour too, for the text-input cursor

	// text hierarchy
	text   lipgloss.Style
	dim    lipgloss.Style
	faint  lipgloss.Style
	bold   lipgloss.Style
	brand  lipgloss.Style
	prompt lipgloss.Style

	// accents
	accentS     lipgloss.Style
	marker      lipgloss.Style
	bullet      lipgloss.Style
	bulletDim   lipgloss.Style
	highlight   lipgloss.Style
	label       lipgloss.Style
	nameSel     lipgloss.Style
	snippetText lipgloss.Style

	// confidence
	pctHigh lipgloss.Style
	pctMid  lipgloss.Style
	pctLow  lipgloss.Style

	// feedback
	ok   lipgloss.Style
	warn lipgloss.Style
	err  lipgloss.Style

	// chrome
	box        lipgloss.Style
	boxFocus   lipgloss.Style
	rule       lipgloss.Style
	bar        lipgloss.Style
	barEmpty   lipgloss.Style
	key        lipgloss.Style
	hint       lipgloss.Style
	modal      lipgloss.Style
	modalFocus lipgloss.Style

	// glyphs, so a plain terminal gets ASCII
	gBullet   string
	gGutter   string
	gMarker   string
	gOK       string
	gSearch   string
	gBar      string
	gBarEmpty string
	gRule     string
}

func newUIStyles(plain, isDark bool) uiStyles {
	ld := lipgloss.LightDark(isDark)

	// The one accent. On dark terminals it is the warm coral; on light ones it
	// darkens so it stays readable against white.
	var accent color.Color
	if !plain {
		accent = ld(lipgloss.Color("#b5532f"), lipgloss.Color("#d97757"))
	}

	fg := func(c color.Color) lipgloss.Style {
		s := lipgloss.NewStyle()
		if !plain && c != nil {
			s = s.Foreground(c)
		}
		return s
	}

	var cText, cDim, cFaint, cOK, cWarn, cErr color.Color
	if !plain {
		cText = ld(lipgloss.Color("#1f1f1f"), lipgloss.Color("#e6e6e6"))
		cDim = ld(lipgloss.Color("#565656"), lipgloss.Color("#9a9a9a"))
		cFaint = ld(lipgloss.Color("#8a8a8a"), lipgloss.Color("#666666"))
		cOK = ld(lipgloss.Color("#3f7d4f"), lipgloss.Color("#7fd88f"))
		cWarn = ld(lipgloss.Color("#8a6d1f"), lipgloss.Color("#e5c07b"))
		cErr = ld(lipgloss.Color("#a33d38"), lipgloss.Color("#e06c75"))
	}

	glyph := func(rich, ascii string) string {
		if plain {
			return ascii
		}
		return rich
	}

	st := uiStyles{
		plain:       plain,
		accent:      accent,
		text:        fg(cText),
		dim:         fg(cDim),
		faint:       fg(cFaint),
		bold:        fg(cText).Bold(true),
		brand:       fg(accent).Bold(true),
		prompt:      fg(accent),
		accentS:     fg(accent),
		marker:      fg(accent).Bold(true),
		bullet:      fg(accent),
		bulletDim:   fg(cFaint),
		highlight:   fg(accent).Bold(true),
		label:       fg(cDim),
		nameSel:     fg(cText).Bold(true),
		snippetText: fg(cDim),
		pctHigh:     fg(cOK),
		pctMid:      fg(accent),
		pctLow:      fg(cFaint),
		ok:          fg(cOK),
		warn:        fg(cWarn),
		err:         fg(cErr),
		rule:        fg(cFaint),
		bar:         fg(accent),
		barEmpty:    fg(cFaint),
		key:         fg(accent).Bold(true),
		hint:        fg(cDim),
		gBullet:     glyph("⏺", "*"),
		gGutter:     glyph("⎿", "|_"),
		gMarker:     glyph("❯", ">"),
		gOK:         glyph("✔", "+"),
		gSearch:     glyph("⌕", "?"),
		gBar:        glyph("█", "#"),
		gBarEmpty:   glyph("░", "-"),
		gRule:       glyph("─", "-"),
	}

	if plain {
		b := lipgloss.Border{TopLeft: "+", Top: "-", TopRight: "+", Left: "|", Right: "|", BottomLeft: "+", Bottom: "-", BottomRight: "+"}
		st.box = lipgloss.NewStyle().Border(b)
		st.boxFocus = st.box
		st.modal = lipgloss.NewStyle().Border(b).Padding(1, 2)
		st.modalFocus = st.modal
	} else {
		st.box = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cFaint)
		st.boxFocus = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent)
		st.modal = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cFaint).Padding(1, 2)
		st.modalFocus = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(1, 2)
	}
	return st
}

// applyInputStyles pushes the palette into the text inputs, including the
// cursor colour. Called once at start-up and again if the terminal background
// turns out to be light.
func (m *uiModel) applyInputStyles() {
	s := m.input.Styles()
	s.Focused.Text = m.styles.text
	s.Focused.Placeholder = m.styles.faint
	s.Focused.Prompt = m.styles.prompt
	s.Focused.Suggestion = m.styles.faint
	s.Blurred.Text = m.styles.dim
	s.Blurred.Placeholder = m.styles.faint
	s.Blurred.Prompt = m.styles.dim
	s.Blurred.Suggestion = m.styles.faint
	if m.styles.accent != nil {
		s.Cursor.Color = m.styles.accent
	}
	for _, ti := range []*textinput.Model{&m.input, &m.scanInput, &m.orgInput} {
		ti.SetStyles(s)
	}
}

func (m *uiModel) w() int {
	if m.width > 0 {
		return m.width
	}
	return 80
}

func (m *uiModel) h() int {
	if m.height > 0 {
		return m.height
	}
	return 24
}

func (m *uiModel) viewName() string {
	switch m.view {
	case viewDetail:
		return "detail"
	case viewRelated:
		return "related"
	case viewStats:
		return "stats"
	case viewScan:
		return "scan"
	case viewOrganize:
		return "organize"
	case viewHelp:
		return "help"
	}
	return "search"
}

// headerLine is the slim top bar: a wordmark and breadcrumb on the left, the
// index and file count on the right.
func (m *uiModel) headerLine() string {
	w := m.w()
	left := m.styles.brand.Render("mem") + m.styles.faint.Render(" › ") + m.styles.dim.Render(m.viewName())
	right := m.styles.faint.Render(fmt.Sprintf("%s · %d files", prettyPath(m.dbPath), m.total))
	lw, rw := lipgloss.Width(left), lipgloss.Width(right)
	if lw+rw+2 > w {
		return left
	}
	return left + strings.Repeat(" ", w-lw-rw) + right
}

// statusLine is the bottom bar: key hints on the left, and on the right either
// a transient confirmation or the spinner while work is in flight.
func (m *uiModel) statusLine(pairs [][2]string, short string) string {
	w := m.w()
	right := ""
	switch {
	case m.toast != "":
		right = m.styles.ok.Render(m.styles.gOK + " " + m.toast)
	case m.isBusy():
		right = m.styles.accentS.Render(m.spin.View() + " " + m.busyVerb() + "…")
	}
	rw := lipgloss.Width(right)

	hints := m.hintChips(pairs)
	if lipgloss.Width(hints)+rw+1 > w {
		hints = m.styles.faint.Render(truncatePlain(short, maxInt(0, w-rw-1)))
	}
	if right == "" {
		return hints
	}
	gap := w - lipgloss.Width(hints) - rw
	if gap < 1 {
		gap = 1
	}
	return hints + strings.Repeat(" ", gap) + right
}

func (m *uiModel) hintChips(pairs [][2]string) string {
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, m.styles.key.Render(p[0])+" "+m.styles.hint.Render(p[1]))
	}
	return strings.Join(parts, m.styles.faint.Render("  ·  "))
}

// inputBox wraps a text input in the rounded prompt box. Focused boxes take the
// accent border; blurred ones stay quiet.
func (m *uiModel) inputBox(ti textinput.Model, focused bool) string {
	w := m.w()
	style := m.styles.box
	if focused {
		style = m.styles.boxFocus
	}
	// Lip Gloss v2 counts the border inside Width, so passing w makes the whole
	// box exactly w columns wide.
	cw := w
	if cw < 10 {
		cw = 10
	}
	return style.Width(cw).Render(ti.View())
}

// queryLine shows how the words were parsed. It is the tool's signature
// transparency, kept but made quiet.
func (m *uiModel) queryLine() string {
	if strings.TrimSpace(m.pq.raw) == "" {
		return ""
	}
	var parts []string
	if len(m.pq.terms) > 0 {
		parts = append(parts, m.styles.accentS.Render(m.styles.gSearch+" ")+
			m.styles.dim.Render(strings.Join(dedupe(m.pq.terms), " ")))
	}
	if len(m.pq.extensions) > 0 {
		parts = append(parts, m.styles.dim.Render(strings.Join(m.pq.extensions, " or ")))
	}
	if m.pq.window.label != "" {
		parts = append(parts, m.styles.dim.Render(m.pq.window.label))
	}
	if len(parts) == 0 {
		return ""
	}
	return "  " + strings.Join(parts, m.styles.faint.Render("  ·  "))
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

func (m *uiModel) viewSearch() string {
	bodyH := m.h() - 6
	if bodyH < 1 {
		bodyH = 1
	}

	var body string
	switch {
	case strings.TrimSpace(m.pq.raw) == "":
		body = m.welcome()
	case len(m.results) == 0:
		body = m.missText()
	default:
		body = m.resultsView(bodyH)
	}

	var hints [][2]string
	if m.input.Focused() {
		hints = [][2]string{{"enter", "search"}, {"esc", "commands"}, {"ctrl+c", "quit"}}
	} else {
		hints = [][2]string{
			{"enter", "open"}, {"↑↓", "move"}, {"r", "related"},
			{"i", "scan"}, {"o", "organize"}, {"s", "stats"},
			{"?", "help"}, {"q", "quit"},
		}
	}

	return strings.Join([]string{
		m.headerLine(),
		clampLines(body, bodyH),
		m.queryLine(),
		m.inputBox(m.input, m.input.Focused()),
		m.statusLine(hints, "? help · ctrl+c quit"),
	}, "\n")
}

func (m *uiModel) welcome() string {
	return strings.Join([]string{
		"",
		"  " + m.styles.bold.Render("mem — find files by what they say, not what they are called"),
		"",
		"  " + m.styles.dim.Render("Type a description below. Results appear as you type."),
		"  " + m.styles.faint.Render(`Try:  the pdf about linux scheduling    ·    last week's notes on optics`),
		"",
	}, "\n")
}

// resultsView windows the result blocks so the selected one is always visible.
func (m *uiModel) resultsView(avail int) string {
	perPage := avail / 4
	if perPage < 1 {
		perPage = 1
	}
	start := 0
	if m.cursor >= perPage {
		start = m.cursor - perPage + 1
	}
	if maxStart := len(m.results) - perPage; start > maxStart {
		start = maxStart
	}
	if start < 0 {
		start = 0
	}
	end := start + perPage
	if end > len(m.results) {
		end = len(m.results)
	}

	lines := make([]string, 0, (end-start)*4)
	for i := start; i < end; i++ {
		lines = append(lines, m.resultBlock(i, m.results[i])...)
	}
	return strings.Join(lines, "\n")
}

func (m *uiModel) confVal(i int) float64 {
	if i < len(m.conf) {
		return m.conf[i]
	}
	return 0
}

func (m *uiModel) pctStyle(pct float64) lipgloss.Style {
	switch {
	case pct >= 70:
		return m.styles.pctHigh
	case pct >= 40:
		return m.styles.pctMid
	default:
		return m.styles.pctLow
	}
}

// resultBlock renders one result as four aligned lines in the bullet/gutter
// idiom: a header with the score, the path, the matched text, and the meta.
func (m *uiModel) resultBlock(i int, r result) []string {
	w := m.w()
	sel := i == m.cursor

	pre := "  "
	bullet := m.styles.bulletDim.Render(m.styles.gBullet)
	if sel {
		pre = m.styles.marker.Render(m.styles.gMarker) + " "
		bullet = m.styles.bullet.Render(m.styles.gBullet)
	}
	pct := m.confVal(i)
	pctS := m.pctStyle(pct).Render(fmt.Sprintf("%d%%", int(pct+0.5)))

	prefix := pre + bullet + " "
	prefixW := lipgloss.Width(prefix)
	pctW := lipgloss.Width(pctS)
	nameMax := w - prefixW - pctW - 1
	if nameMax < 4 {
		nameMax = 4
	}
	name := truncatePlain(r.Filename, nameMax)
	nameS := m.styles.text.Render(name)
	if sel {
		nameS = m.styles.nameSel.Render(name)
	}
	gap := w - prefixW - lipgloss.Width(nameS) - pctW
	if gap < 1 {
		gap = 1
	}
	line1 := prefix + nameS + strings.Repeat(" ", gap) + pctS

	gutter := "  " + m.styles.faint.Render(m.styles.gGutter) + " "
	line2 := gutter + m.styles.dim.Render(truncatePlain(prettyPath(r.Path), w-4))

	line3 := ""
	switch {
	case r.Snippet != "":
		line3 = "    " + m.renderSnippet(r.Snippet, w-4)
	case r.Source == "recent":
		line3 = "    " + m.styles.faint.Render("no content match — newest of this type")
	case len(r.Matched) > 0:
		line3 = "    " + m.styles.label.Render("matches: ") +
			m.styles.faint.Render(truncatePlain(strings.Join(r.Matched, ", "), w-13))
	}

	meta := fmt.Sprintf("%s · %s · %s", humanBytes(uint64(r.Size)), timeAgo(r.Modified), r.Category)
	line4 := "    " + m.styles.faint.Render(truncatePlain(meta, w-4))

	return []string{line1, line2, line3, line4}
}

// renderSnippet turns the FTS5 highlight markers into styled spans, truncating
// at the visible-width budget so a long fragment cannot wrap the layout.
func (m *uiModel) renderSnippet(s string, max int) string {
	s = strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
	if max <= 0 {
		return ""
	}
	budget := max - 1 // room for the ellipsis
	var b strings.Builder
	hl := false
	vis := 0
	truncated := false

	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], ">>"):
			hl = true
			i += 2
			continue
		case strings.HasPrefix(s[i:], "<<"):
			hl = false
			i += 2
			continue
		}
		j := i
		for j < len(s) && !strings.HasPrefix(s[j:], ">>") && !strings.HasPrefix(s[j:], "<<") {
			j++
		}
		chunk := []rune(s[i:j])
		room := budget - vis
		if room <= 0 {
			truncated = true
			break
		}
		if len(chunk) > room {
			chunk = chunk[:room]
			truncated = true
		}
		if hl {
			b.WriteString(m.styles.highlight.Render(string(chunk)))
		} else {
			b.WriteString(m.styles.snippetText.Render(string(chunk)))
		}
		vis += len(chunk)
		i = j
		if truncated {
			break
		}
	}
	if truncated {
		b.WriteString(m.styles.faint.Render("…"))
	}
	return b.String()
}

// missText explains an empty result the same honest way the CLI does.
func (m *uiModel) missText() string {
	gutter := "  " + m.styles.faint.Render(m.styles.gGutter) + " "
	var b strings.Builder
	b.WriteString(m.styles.warn.Render(fmt.Sprintf("No match for %q.", m.pq.raw)))

	if len(m.pq.terms) > 0 {
		b.WriteString("\n" + gutter + m.styles.dim.Render(
			"searched for: "+strings.Join(dedupe(m.pq.terms), ", ")))
	}
	if m.pq.blockedByDate > 0 {
		b.WriteString("\n" + gutter + m.styles.warn.Render(fmt.Sprintf(
			"your words match %d file(s), outside the date range you asked for", m.pq.blockedByDate)))
	}
	if m.pq.unsearchable > 0 {
		b.WriteString("\n" + gutter + m.styles.dim.Render(fmt.Sprintf(
			"%d indexed files have no readable text, so they are findable only by filename", m.pq.unsearchable)))
	}

	switch {
	case len(m.pq.extensions) > 0 && m.pq.window.after > 0:
		b.WriteString("\n" + gutter + m.styles.dim.Render(fmt.Sprintf(
			"both a file type and a date range were required · %d files are indexed in total", m.total)))
	case len(m.pq.extensions) > 0:
		b.WriteString("\n" + gutter + m.styles.dim.Render(fmt.Sprintf(
			"only %s files were considered · %d indexed in total", strings.Join(m.pq.extensions, " or "), m.total)))
	case m.pq.window.after > 0 || m.pq.window.before > 0:
		b.WriteString("\n" + gutter + m.styles.dim.Render(fmt.Sprintf(
			"only files in that date range were considered · %d indexed in total", m.total)))
	default:
		b.WriteString("\n" + gutter + m.styles.dim.Render(fmt.Sprintf(
			"%d files are indexed · try fewer or more general words", m.total)))
		b.WriteString("\n" + gutter + m.styles.dim.Render("index somewhere else with i"))
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Detail
// ---------------------------------------------------------------------------

func (m *uiModel) viewDetail() string {
	w := m.w()
	r := m.detail

	head := []string{
		m.styles.bullet.Render(m.styles.gBullet) + " " +
			m.styles.bold.Render(truncatePlain(r.Filename, w-4)),
		"  " + m.styles.faint.Render(m.styles.gGutter) + " " +
			m.styles.dim.Render(truncatePlain(prettyPath(r.Path), w-4)),
		"    " + m.styles.faint.Render(fmt.Sprintf("%s · %s · %s · %s",
			r.Category, r.Extension, humanBytes(uint64(r.Size)), timeAgo(r.Modified))),
		m.styles.rule.Render(strings.Repeat(m.styles.gRule, w)),
	}

	return strings.Join([]string{
		m.headerLine(),
		strings.Join(head, "\n") + "\n" + m.viewport.View(),
		m.statusLine([][2]string{
			{"↑↓", "scroll"}, {"o", "open"}, {"c", "copy"}, {"r", "related"}, {"esc", "back"},
		}, "esc back"),
	}, "\n")
}

// indentContent pads stored text into the gutter idiom used everywhere else.
func (m *uiModel) indentContent(s string) string {
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(s)
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = "    " + m.styles.text.Render(ln)
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// Related
// ---------------------------------------------------------------------------

func (m *uiModel) viewRelated() string {
	w := m.w()
	bodyH := m.h() - 2
	if bodyH < 1 {
		bodyH = 1
	}

	var b strings.Builder
	b.WriteString(m.styles.bullet.Render(m.styles.gBullet) + " " +
		m.styles.bold.Render(truncatePlain("Related to "+m.related.Target.Filename, w-4)))
	b.WriteString("\n  " + m.styles.faint.Render(m.styles.gGutter) + " " +
		m.styles.dim.Render(truncatePlain(prettyPath(m.related.Target.Path), w-4)))

	switch {
	case len(m.related.Terms) == 0:
		b.WriteString("\n\n  " + m.styles.faint.Render("(this file has no extracted text to compare on)"))
	case len(m.related.Hits) == 0:
		b.WriteString("\n\n  " + m.styles.faint.Render("(nothing else in the index shares vocabulary with this file)"))
	default:
		for i, r := range m.related.Hits {
			b.WriteString("\n")
			if i == 0 {
				b.WriteString("\n")
			}
			b.WriteString(m.hitLine(i, r))
		}
	}

	return strings.Join([]string{
		m.headerLine(),
		clampLines(b.String(), bodyH),
		m.statusLine([][2]string{{"↑↓", "move"}, {"enter", "open"}, {"esc", "back"}}, "esc back"),
	}, "\n")
}

func (m *uiModel) hitLine(i int, r result) string {
	w := m.w()
	sel := i == m.cursor
	pre := "  "
	bullet := m.styles.bulletDim.Render(m.styles.gBullet)
	if sel {
		pre = m.styles.marker.Render(m.styles.gMarker) + " "
		bullet = m.styles.bullet.Render(m.styles.gBullet)
	}
	meta := fmt.Sprintf("%s · %s", humanBytes(uint64(r.Size)), timeAgo(r.Modified))
	metaS := m.styles.faint.Render(meta)

	prefix := pre + bullet + " "
	prefixW := lipgloss.Width(prefix)
	metaW := lipgloss.Width(metaS)
	nameMax := w - prefixW - metaW - 2
	if nameMax < 4 {
		nameMax = 4
	}
	name := truncatePlain(r.Filename, nameMax)
	nameS := m.styles.text.Render(name)
	if sel {
		nameS = m.styles.bold.Render(name)
	}
	gap := w - prefixW - lipgloss.Width(nameS) - metaW
	if gap < 1 {
		gap = 1
	}
	return prefix + nameS + strings.Repeat(" ", gap) + metaS
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

func (m *uiModel) viewStats() string {
	bodyH := m.h() - 2
	if bodyH < 1 {
		bodyH = 1
	}
	st := m.stats

	var b strings.Builder
	if st.Total == 0 {
		b.WriteString("  " + m.styles.dim.Render("The index is empty. Press i to scan a folder."))
	} else {
		b.WriteString(m.statRow("Memories indexed", fmt.Sprintf("%d", st.Total)))
		b.WriteString("\n" + m.statRow("Total size", humanBytes(uint64(st.TotalBytes))))

		b.WriteString("\n\n  " + m.styles.dim.Render("By category"))
		maxCount := 1
		for _, c := range st.Categories {
			if c.Count > maxCount {
				maxCount = c.Count
			}
		}
		barMax := m.w() - 36
		if barMax < 6 {
			barMax = 6
		}
		if barMax > 28 {
			barMax = 28
		}
		for _, c := range st.Categories {
			filled := int(float64(c.Count)/float64(maxCount)*float64(barMax) + 0.5)
			if filled < 1 {
				filled = 1
			}
			b.WriteString(fmt.Sprintf("\n    %-12s %s %4d",
				truncatePlain(c.Name, 12), m.barFrac(filled, barMax), c.Count))
		}

		b.WriteString("\n\n  " + m.styles.dim.Render("By file type"))
		for _, e := range st.Extensions {
			b.WriteString(fmt.Sprintf("\n    %-12s %4d", truncatePlain(e.Ext, 12), e.Count))
		}
	}

	return strings.Join([]string{
		m.headerLine(),
		clampLines(b.String(), bodyH),
		m.statusLine([][2]string{{"i", "scan"}, {"o", "organize"}, {"esc", "back"}}, "esc back"),
	}, "\n")
}

func (m *uiModel) statRow(label, value string) string {
	return "  " + m.styles.dim.Render(fmt.Sprintf("%-17s", label)) + " " + m.styles.bold.Render(value)
}

func (m *uiModel) barFrac(filled, total int) string {
	if filled > total {
		filled = total
	}
	if filled < 0 {
		filled = 0
	}
	return m.styles.bar.Render(strings.Repeat(m.styles.gBar, filled)) +
		m.styles.barEmpty.Render(strings.Repeat(m.styles.gBarEmpty, total-filled))
}

// ---------------------------------------------------------------------------
// Scan
// ---------------------------------------------------------------------------

func (m *uiModel) viewScan() string {
	w := m.w()
	bodyH := m.h() - 5
	if bodyH < 1 {
		bodyH = 1
	}
	gutter := "  " + m.styles.faint.Render(m.styles.gGutter) + " "

	var b strings.Builder
	switch {
	case m.scanErr != nil:
		b.WriteString(m.styles.bullet.Render(m.styles.gBullet) + " " + m.styles.err.Render("Scan failed"))
		b.WriteString("\n" + gutter + m.styles.err.Render(m.scanErr.Error()))
	case m.scanning:
		b.WriteString(m.styles.accentS.Render(m.spin.View()) + " " + m.styles.bold.Render(m.busyVerb()+"…"))
		b.WriteString("\n" + gutter + m.styles.dim.Render(
			fmt.Sprintf("%d/%d", m.scanProg.Done, m.scanProg.Total)))
		if m.scanProg.Path != "" {
			b.WriteString("\n" + gutter + m.styles.faint.Render(
				truncatePlain(prettyPath(m.scanProg.Path), w-6)))
		}
		b.WriteString("\n\n  " + m.scanBar())
	case m.scanRep != nil:
		rep := m.scanRep
		b.WriteString(m.styles.ok.Render(m.styles.gOK) + " " + m.styles.bold.Render("Indexing complete"))
		b.WriteString("\n" + gutter + m.styles.dim.Render(fmt.Sprintf(
			"Indexed %d files · extracted %d · skipped %d unchanged",
			rep.Indexed, rep.Extracted, rep.Unchanged)))
		if rep.OCR.used > 0 {
			b.WriteString("\n" + gutter + m.styles.dim.Render(fmt.Sprintf(
				"OCR'd %d images: %d readable, %d with no text, %d unreadable noise",
				rep.OCR.used, rep.OCR.readable, rep.OCR.nothing, rep.OCR.noise)))
		}
		if len(rep.Pruned) > 0 {
			b.WriteString("\n" + gutter + m.styles.dim.Render(fmt.Sprintf(
				"Removed %d file(s) no longer on disk", len(rep.Pruned))))
		}
		b.WriteString("\n" + gutter + m.styles.dim.Render(fmt.Sprintf(
			"%d files known in total", rep.Total)))
	default:
		b.WriteString("  " + m.styles.dim.Render("Enter a folder to index. Files are only ever read, never changed."))
		b.WriteString("\n  " + m.styles.faint.Render("press f to force a full re-read of unchanged files"))
	}

	return strings.Join([]string{
		m.headerLine(),
		clampLines(b.String(), bodyH),
		m.inputBox(m.scanInput, m.scanInput.Focused()),
		m.statusLine([][2]string{{"enter", "scan"}, {"f", "force"}, {"esc", "back"}}, "esc back"),
	}, "\n")
}

func (m *uiModel) scanBar() string {
	w := m.w() - 8
	if w < 10 {
		w = 10
	}
	if w > 40 {
		w = 40
	}
	pct := 0.0
	if m.scanProg.Total > 0 {
		pct = float64(m.scanProg.Done) / float64(m.scanProg.Total)
	}
	filled := int(pct*float64(w) + 0.5)
	return m.barFrac(filled, w)
}

// ---------------------------------------------------------------------------
// Organize
// ---------------------------------------------------------------------------

func (m *uiModel) viewOrganize() string {
	bodyH := m.h() - 5
	if bodyH < 1 {
		bodyH = 1
	}
	gutter := "  " + m.styles.faint.Render(m.styles.gGutter) + " "

	var b strings.Builder
	switch {
	case m.orgErr != nil:
		b.WriteString(m.styles.bullet.Render(m.styles.gBullet) + " " + m.styles.err.Render("Organize failed"))
		b.WriteString("\n" + gutter + m.styles.err.Render(m.orgErr.Error()))
	case m.orgPlan == nil:
		b.WriteString("  " + m.styles.dim.Render("Enter a folder to preview a tidy-up plan. Nothing changes until you press a."))
	case m.orgPlan.Count == 0:
		b.WriteString("  " + m.styles.dim.Render("Nothing to organize in ") +
			m.styles.faint.Render(prettyPath(m.orgPlan.Dir)))
	default:
		p := m.orgPlan
		b.WriteString(m.styles.bullet.Render(m.styles.gBullet) + " " +
			m.styles.bold.Render(fmt.Sprintf("Plan for %s", prettyPath(p.Dir))) +
			"  " + m.styles.faint.Render(fmt.Sprintf("%d files", p.Count)))
		if m.orgResult != "" {
			b.WriteString("\n" + gutter + m.styles.ok.Render(m.orgResult))
		}
		for _, g := range p.Groups {
			b.WriteString("\n\n  " + m.styles.bold.Render(fmt.Sprintf("%-20s", g.Category+"/")) +
				" " + m.styles.faint.Render(fmt.Sprintf("%d", len(g.Items))))
			for ii, it := range g.Items {
				conn := "├─"
				if ii == len(g.Items)-1 {
					conn = "└─"
				}
				if m.styles.plain {
					if ii == len(g.Items)-1 {
						conn = "\\-"
					} else {
						conn = "+-"
					}
				}
				b.WriteString("\n      " + m.styles.faint.Render(conn) + " " + m.styles.dim.Render(it.name))
			}
		}
	}

	body := b.String()
	if m.orgConfirm && m.orgPlan != nil {
		body = lipgloss.Place(m.w(), bodyH, lipgloss.Center, lipgloss.Center,
			m.confirmModal(m.orgPlan.Count))
	}

	return strings.Join([]string{
		m.headerLine(),
		clampLines(body, bodyH),
		m.inputBox(m.orgInput, m.orgInput.Focused()),
		m.statusLine([][2]string{{"enter", "preview"}, {"a", "apply"}, {"esc", "back"}}, "esc back"),
	}, "\n")
}

func (m *uiModel) confirmModal(count int) string {
	lines := []string{
		m.styles.bold.Render(fmt.Sprintf("Copy %d files into the folders below?", count)),
		m.styles.dim.Render("Files are copied — never moved, deleted or overwritten."),
		"",
		m.styles.key.Render("[y]") + m.styles.hint.Render(" yes") + "     " +
			m.styles.key.Render("[n]") + m.styles.hint.Render(" no"),
	}
	return m.styles.modalFocus.Render(strings.Join(lines, "\n"))
}

// ---------------------------------------------------------------------------
// Help
// ---------------------------------------------------------------------------

func (m *uiModel) viewHelp() string {
	bodyH := m.h() - 2
	if bodyH < 1 {
		bodyH = 1
	}

	rows := [][2]string{
		{"type", "search as you type"},
		{"enter", "search now, or open the selected result"},
		{"esc", "leave the input, or go back"},
		{"↑ ↓", "move the selection"},
		{"r", "related files"},
		{"i", "scan a folder"},
		{"o", "organize preview"},
		{"s", "stats"},
		{"?", "this help"},
		{"ctrl+c", "quit"},
	}

	var b strings.Builder
	b.WriteString(m.styles.bold.Render("mem — find files by what they say, not what they are called"))
	b.WriteString("\n")
	for _, r := range rows {
		b.WriteString("\n  " + m.styles.key.Width(7).Render(r[0]) + " " + m.styles.hint.Render(r[1]))
	}
	b.WriteString("\n\n  " + m.styles.faint.Render("A scan and a search only ever read. Organize copies;"))
	b.WriteString("\n  " + m.styles.faint.Render("it never moves, deletes or overwrites."))

	return strings.Join([]string{
		m.headerLine(),
		clampLines(b.String(), bodyH),
		m.statusLine([][2]string{{"esc", "back"}}, "esc back"),
	}, "\n")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// clampLines trims or pads a block to exactly h lines, so the footer stays put.
func clampLines(s string, h int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// truncatePlain cuts a plain (unstyled) string to w display cells, adding an
// ellipsis. Used before styling, so it never has to reason about escape codes.
func truncatePlain(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return string(r[:w])
	}
	return string(r[:w-1]) + "…"
}

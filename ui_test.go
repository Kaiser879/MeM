package main

import (
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// uiFixture builds a small indexed corpus and a model over it.
func uiFixture(t *testing.T) (*uiModel, string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("notes.txt", "Linux process scheduling and the kernel scheduler. The kernel decides.")
	write("optics.md", "Wave optics interference and diffraction patterns in light.")

	dbPath := filepath.Join(t.TempDir(), "index.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	if _, err := store.ScanAndIndex(dir, false, nil); err != nil {
		t.Fatal(err)
	}

	m := newUIModel(store, dbPath, 10, "")
	m.width, m.height = 100, 30
	m.layout()
	return m, dir
}

// view drives one View() call and returns the rendered content.
func view(m *uiModel) string { return m.View().Content }

func TestUISearchFlowRendersResult(t *testing.T) {
	m, _ := uiFixture(t)

	m.input.SetValue("linux process scheduling")
	msg := m.runSearch()()
	if _, ok := msg.(searchMsg); !ok {
		t.Fatalf("runSearch returned %T", msg)
	}
	m.Update(msg)

	out := view(m)
	if !strings.Contains(out, "notes.txt") {
		t.Fatalf("search result not rendered; got:\n%s", out)
	}
	if len(m.pq.terms) == 0 {
		t.Fatalf("expected the query to be parsed into terms; pq=%+v", m.pq)
	}
	// The parsed words are echoed back on the query line above the input.
	if !strings.Contains(out, m.pq.terms[0]) {
		t.Fatalf("expected the parsed-query line to show %q; got:\n%s", m.pq.terms[0], out)
	}
}

func TestUISearchMissExplainsItself(t *testing.T) {
	m, _ := uiFixture(t)

	m.input.SetValue("zzzqqqxyzzy wobbleflurb")
	m.Update(m.runSearch()())

	out := view(m)
	if !strings.Contains(out, "No match for") {
		t.Fatalf("expected an honest miss; got:\n%s", out)
	}
}

func TestUIDetailAndRelatedAndStatsRender(t *testing.T) {
	m, _ := uiFixture(t)

	// Search to populate results, then open the first one.
	m.input.SetValue("linux kernel")
	m.Update(m.runSearch()())
	if len(m.results) == 0 {
		t.Fatal("fixture produced no results")
	}
	r := m.results[0]

	m.Update(m.loadDetail(r)())
	if m.view != viewDetail {
		t.Fatalf("expected detail view, got %v", m.view)
	}
	if out := view(m); !strings.Contains(out, r.Filename) {
		t.Fatalf("detail view missing filename; got:\n%s", out)
	}

	m.Update(m.loadRelated(r)())
	if m.view != viewRelated {
		t.Fatalf("expected related view, got %v", m.view)
	}
	_ = view(m) // must not panic

	m.Update(m.loadStats()())
	if m.view != viewStats {
		t.Fatalf("expected stats view, got %v", m.view)
	}
	if out := view(m); !strings.Contains(out, "Memories indexed") {
		t.Fatalf("stats view missing totals; got:\n%s", out)
	}
}

func TestUIScanScreenShowsProgressAndReport(t *testing.T) {
	m, _ := uiFixture(t)

	m.view = viewScan

	// Mid-scan frame with an animated progress bar must render.
	m.scanning = true
	m.scanProg = ScanProgress{Done: 3, Total: 10, Path: "/tmp/x.txt"}
	if out := view(m); !strings.Contains(out, "3/10") {
		t.Fatalf("scan progress not rendered; got:\n%s", out)
	}

	// A completed scan renders its report.
	m.scanning = false
	m.scanRep = &ScanReport{Indexed: 10, Extracted: 8, Unchanged: 2, Total: 12}
	out := view(m)
	if !strings.Contains(out, "Indexing complete") || !strings.Contains(out, "12 files known in total") {
		t.Fatalf("scan report not rendered; got:\n%s", out)
	}
}

func TestUIOrganizePreviewRendersAndNeverWrites(t *testing.T) {
	m, dir := uiFixture(t)

	m.view = viewOrganize
	m.orgInput.SetValue(dir)
	m.Update(m.loadOrganizePlan()())

	if m.orgPlan == nil || m.orgPlan.Count == 0 {
		t.Fatal("expected a non-empty organize plan")
	}
	out := view(m)
	if !strings.Contains(out, "Plan for") {
		t.Fatalf("organize preview not rendered; got:\n%s", out)
	}

	// A preview must not have created anything on disk.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("preview created a directory %q; it must be read-only", e.Name())
		}
	}

	// Applying copies; it must never remove the originals.
	m.Update(m.applyOrganize()())
	if m.orgErr != nil {
		t.Fatalf("apply failed: %v", m.orgErr)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatalf("apply removed an original file: %v", err)
	}
	if m.orgResult == "" {
		t.Fatal("apply produced no result summary")
	}
	gotDir := false
	entries, _ = os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			gotDir = true
		}
	}
	if !gotDir {
		t.Fatal("apply copied nothing into new folders")
	}
}

func TestUIScanRunsAndStreamsToCompletion(t *testing.T) {
	m, dir := uiFixture(t)
	m.view = viewScan
	m.scanInput.SetValue(dir)

	cmd := m.startScan(false)
	if !m.scanning {
		t.Fatal("startScan should mark the model as scanning")
	}

	deadline := time.After(15 * time.Second)
	for m.scanning {
		if cmd == nil {
			t.Fatal("nil command while a scan is still running")
		}
		msgCh := make(chan tea.Msg, 1)
		go func(c tea.Cmd) { msgCh <- c() }(cmd)
		var msg tea.Msg
		select {
		case msg = <-msgCh:
		case <-deadline:
			t.Fatal("scan did not finish in time")
		}
		if msg == nil {
			break
		}
		_, cmd = m.Update(msg)
	}

	if m.scanErr != nil {
		t.Fatalf("scan errored: %v", m.scanErr)
	}
	if m.scanRep == nil {
		t.Fatal("scan finished without a report")
	}
	if out := view(m); !strings.Contains(out, "Indexing complete") {
		t.Fatalf("scan report not rendered; got:\n%s", out)
	}
}

func TestUIHelpAndBackNavigation(t *testing.T) {
	m, _ := uiFixture(t)

	m.view = viewHelp
	if out := view(m); !strings.Contains(out, "mem — find files") {
		t.Fatalf("help view not rendered; got:\n%s", out)
	}

	// Any of these keys returns to search.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.view != viewSearch {
		t.Fatalf("help did not return to search, got %v", m.view)
	}
}

func TestUIKeyRoutingAndCommands(t *testing.T) {
	m, _ := uiFixture(t)

	// esc while the search input is focused only blurs it.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.input.Focused() {
		t.Fatal("esc should blur the search input")
	}
	// esc again, in command mode, refocuses the input.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !m.input.Focused() {
		t.Fatal("esc in command mode should refocus the search input")
	}

	// Blur, then 'i' opens the scan screen and focuses its input.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m.Update(tea.KeyPressMsg{Code: 'i', Text: "i"})
	if m.view != viewScan || !m.scanInput.Focused() {
		t.Fatalf("i should open scan and focus its input; view=%v focused=%v",
			m.view, m.scanInput.Focused())
	}
	// esc backs out of scan and refocuses search.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.view != viewSearch || !m.input.Focused() {
		t.Fatalf("esc from scan should return to search, got view=%v", m.view)
	}

	// Search, leave the input with esc, then enter opens the top result.
	m.input.SetValue("linux kernel")
	m.Update(m.runSearch()())
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape}) // blur to command mode
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})  // open the selected result
	if m.view != viewDetail {
		t.Fatalf("enter in command mode should open the selected result, got %v", m.view)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape}) // back to search
	if m.view != viewSearch {
		t.Fatalf("esc should leave the detail view, got %v", m.view)
	}

	// Blur, then 's' opens stats and kicks off a load command.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	_, cmd := m.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	if m.view != viewStats {
		t.Fatalf("s should open stats, got %v", m.view)
	}
	if cmd != nil {
		m.Update(cmd())
	}

	// ctrl+c quits from anywhere.
	if _, quit := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); quit == nil {
		t.Fatal("ctrl+c should return a quit command")
	}
}

func TestUIRenderEveryViewDoesNotPanic(t *testing.T) {
	m, _ := uiFixture(t)
	m.input.SetValue("linux")
	m.Update(m.runSearch()())
	if len(m.results) > 0 {
		m.Update(m.loadDetail(m.results[0])())
		m.Update(m.loadRelated(m.results[0])())
	}
	for _, v := range []uiView{viewSearch, viewDetail, viewRelated, viewStats, viewScan, viewOrganize, viewHelp} {
		m.view = v
		_ = view(m)
	}
}

func TestUITruncatePlainIsRuneSafe(t *testing.T) {
	if got := truncatePlain("abcdef", 4); got != "abc…" {
		t.Fatalf("truncatePlain = %q, want %q", got, "abc…")
	}
	if got := truncatePlain("short", 10); got != "short" {
		t.Fatalf("short strings must pass through, got %q", got)
	}
	// Multi-byte runes must be cut at rune, not byte, boundaries.
	if got := truncatePlain("héllo wörld", 5); len([]rune(got)) != 5 {
		t.Fatalf("truncatePlain produced %q (%d runes), want 5 runes", got, len([]rune(got)))
	}
}

func TestUIPlainFallbackUsesASCII(t *testing.T) {
	st := newUIStyles(true, true)
	if st.gBullet != "*" || st.gGutter != "|_" || st.gMarker != ">" || st.gBar != "#" {
		t.Fatalf("plain mode should use ASCII glyphs, got %q %q %q %q",
			st.gBullet, st.gGutter, st.gMarker, st.gBar)
	}
	if st.accent != nil {
		t.Fatal("plain mode should carry no accent colour")
	}
	if st.box.GetBorderStyle().TopLeft != "+" {
		t.Fatalf("plain mode should use an ASCII box border, got %q", st.box.GetBorderStyle().TopLeft)
	}
}

func TestUIPaletteAdaptsToBackground(t *testing.T) {
	dark := newUIStyles(false, true)
	light := newUIStyles(false, false)
	if dark.accent == nil || light.accent == nil {
		t.Fatal("coloured palettes must have an accent")
	}
	if fmt.Sprintf("%v", dark.accent) == fmt.Sprintf("%v", light.accent) {
		t.Fatal("the accent should differ between light and dark terminals")
	}

	m, _ := uiFixture(t)
	m.plain = false
	if !m.isDark {
		t.Fatal("the default before any terminal reply should be dark")
	}
	m.Update(tea.BackgroundColorMsg{Color: color.RGBA{R: 255, G: 255, B: 255, A: 255}})
	if m.isDark {
		t.Fatal("a white background should be treated as light")
	}
	m.Update(tea.BackgroundColorMsg{Color: color.RGBA{R: 0, G: 0, B: 0, A: 255}})
	if !m.isDark {
		t.Fatal("a black background should be treated as dark")
	}
}

func TestUIBusySpinnerAndToastRender(t *testing.T) {
	m, _ := uiFixture(t)

	m.searching = true
	if out := view(m); !strings.Contains(out, "Searching") {
		t.Fatalf("busy status line should name the work; got:\n%s", out)
	}
	m.searching = false

	m.toast = "copied path"
	if out := view(m); !strings.Contains(out, "copied path") {
		t.Fatalf("toast should be visible; got:\n%s", out)
	}

	// The spinner tick loop only runs while something is in flight.
	if m.kickSpin() == nil {
		t.Fatal("kickSpin should start a tick loop when idle")
	}
	if m.kickSpin() != nil {
		t.Fatal("kickSpin should not stack a second tick loop")
	}
}

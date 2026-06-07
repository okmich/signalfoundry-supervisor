package tui

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// btop-style chrome: rounded panels with the title embedded in the top border, an accent on the
// corners/title and the key-hint trigger letters, and reverse-video active tabs.
var (
	boxBorderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	boxTitleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
	hintKeyStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
	tabStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	tabActiveStyle = lipgloss.NewStyle().Reverse(true).Bold(true)
)

// titledBox renders body inside a rounded border whose top edge carries `left` (already-styled — a
// title, optionally followed by inline tabs/menu) at the start and `right` (already-styled, e.g. a
// state badge) at the end. innerW is the content width; the box is innerW+2 columns wide. body is
// padded/clipped to innerH rows; callers pass lines already within innerW (raw log lines must be
// clipped first — titledBox does not measure ANSI inside body lines, so an over-wide line would wrap).
func titledBox(left, right string, innerW, innerH int, body []string) string {
	if innerW < 1 {
		innerW = 1
	}
	if innerH < 1 {
		innerH = 1
	}
	norm := make([]string, innerH)
	for i := 0; i < innerH; i++ {
		if i < len(body) {
			norm[i] = body[i]
		}
	}
	rendered := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("238")).
		Width(innerW).
		Render(strings.Join(norm, "\n"))
	lines := strings.Split(rendered, "\n")
	if len(lines) > 0 {
		lines[0] = topBorder(left, right, innerW) // replace lipgloss's plain top edge with the titled one
	}
	return strings.Join(lines, "\n")
}

// topBorder builds the titled top edge — ╭─ left ─…─ right ─╮ — exactly innerW+2 columns wide. The
// structural runs are border-dim; left/right are rendered by the caller (their ANSI width is
// measured). If the labels can't fit (e.g. many symbol tabs in a narrow box), `left` is clamped so a
// ≥1 dash fill always remains and the width invariant holds.
func topBorder(left, right string, innerW int) string {
	if right == "" {
		left = clampWidth(left, innerW-4) // "╭─ " + left + " " + (≥1 dash) + "╮"
		fill := max(innerW-3-lipgloss.Width(left), 1)
		return boxBorderStyle.Render("╭─ ") + left + boxBorderStyle.Render(" "+strings.Repeat("─", fill)+"╮")
	}
	rw := lipgloss.Width(right)
	left = clampWidth(left, innerW-7-rw) // "╭─ " + left + " " + (≥1 dash) + " " + right + " ─╮"
	fill := max(innerW-6-lipgloss.Width(left)-rw, 1)
	return boxBorderStyle.Render("╭─ ") + left +
		boxBorderStyle.Render(" "+strings.Repeat("─", fill)+" ") + right + boxBorderStyle.Render(" ─╮")
}

// clampWidth truncates a (possibly styled) string to at most w visible columns, preserving ANSI.
func clampWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

// hint is one key→action pair for the bottom hint bar.
type hint struct{ key, label string }

// hintBar renders the btop-style bottom legend: each trigger key accented, its label dim, separated
// by gaps.
func hintBar(hints ...hint) string {
	parts := make([]string, len(hints))
	for i, h := range hints {
		parts[i] = hintKeyStyle.Render(h.key) + " " + dimStyle.Render(h.label)
	}
	return " " + strings.Join(parts, dimStyle.Render("   "))
}

// plural renders "1 system" / "3 systems".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// cols / rows give the terminal size with a sane default before the first WindowSizeMsg.
func (m model) cols() int {
	if m.width > 0 {
		return m.width
	}
	return 100
}

func (m model) rows() int {
	if m.height > 0 {
		return m.height
	}
	return 30
}

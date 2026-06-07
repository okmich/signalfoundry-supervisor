package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// Every rendered line of a box must be exactly innerW+2 columns — the titled top edge included — or
// side-by-side panes (JoinHorizontal) would misalign.
func TestTitledBoxWidthInvariant(t *testing.T) {
	cases := []struct {
		name        string
		left, right string
		innerW      int
	}{
		{"title only", boxTitleStyle.Render("fleet"), "", 40},
		{"title + right badge", boxTitleStyle.Render("status"), okStyle.Render("Running"), 60},
		{"narrow", boxTitleStyle.Render("x"), "", 8},
		{"over-stuffed clamps fill", boxTitleStyle.Render(strings.Repeat("z", 30)), okStyle.Render("Running"), 20},
	}
	for _, c := range cases {
		out := titledBox(c.left, c.right, c.innerW, 3, []string{"line one", "line two"})
		for i, ln := range strings.Split(out, "\n") {
			if w := lipgloss.Width(ln); w != c.innerW+2 {
				t.Errorf("%s: line %d width = %d, want %d (%q)", c.name, i, w, c.innerW+2, ln)
			}
		}
	}
}

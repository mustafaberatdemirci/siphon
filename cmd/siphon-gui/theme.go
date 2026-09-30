package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// compactTheme is the default theme with desktop-sized text and padding,
// for the table and the category tree: Fyne's defaults are sized for touch
// and gave rows twice the height of a download manager's.
type compactTheme struct{ fyne.Theme }

func newCompactTheme() fyne.Theme { return compactTheme{theme.DefaultTheme()} }

func (t compactTheme) Size(n fyne.ThemeSizeName) float32 {
	switch n {
	case theme.SizeNameText:
		return 13
	case theme.SizeNameInnerPadding:
		return 4
	case theme.SizeNamePadding:
		return 2
	case theme.SizeNameLineSpacing:
		return 2
	case theme.SizeNameInlineIcon:
		return 16
	}
	return t.Theme.Size(n)
}

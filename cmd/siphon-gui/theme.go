package main

import (
	"image/color"

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

// countTheme draws the category tree's counts. They are low-importance
// labels, which Fyne paints in its disabled colour: #39393a on the dark
// theme's #171718 and #e3e3e3 on the light one's white, 1.6:1 and 1.3:1,
// which couldn't be read, least of all on the selected row. The text colour
// at about 80% opacity keeps them quieter than the names and above 4.5:1 on
// both (the placeholder grey reaches only 3.5:1 on white).
type countTheme struct{ fyne.Theme }

func (t countTheme) Color(n fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	if n != theme.ColorNameDisabled {
		return t.Theme.Color(n, v)
	}
	c := color.NRGBAModel.Convert(t.Theme.Color(theme.ColorNameForeground, v)).(color.NRGBA)
	c.A = 0xd1
	return c
}

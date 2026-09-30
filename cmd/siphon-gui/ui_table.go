package main

import (
	"image/color"
	"strconv"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
)

// column is one column of the download table.
type column struct {
	title string
	key   sortKey
	width float32 // fixed width; 0 means it shares what is left
	share float32 // its part of what is left
	align fyne.TextAlign
}

// columns, in a download manager's order. File name and Status take what
// the fixed ones leave; a long name or error is cut with an ellipsis instead
// of widening the table.
var columns = []column{
	{title: msg("File name"), key: sortName, share: 0.6, align: fyne.TextAlignLeading},
	{title: msg("Size"), key: sortSize, width: 74, align: fyne.TextAlignTrailing},
	{title: msg("Status"), key: sortStatus, share: 0.4, align: fyne.TextAlignLeading},
	{title: msg("Time left"), key: sortETA, width: 66, align: fyne.TextAlignTrailing},
	{title: msg("Transfer rate"), key: sortSpeed, width: 90, align: fyne.TextAlignTrailing},
	{title: msg("Conn."), key: sortConns, width: 44, align: fyne.TextAlignCenter},
	{title: msg("Added"), key: sortAdded, width: 104, align: fyne.TextAlignLeading},
}

const (
	colGap       = 6
	minShareArea = 240 // File name and Status together never get less
)

// columnLayout places one object per column side by side. The header and
// every row use it, so their columns line up.
type columnLayout struct{}

// headerFloors are the widths the fixed columns need for their titles in
// the current language, the sort arrow included: German titles are longer
// than the English ones the widths were picked for, and a title wider than
// its column ran into the next one.
var (
	floorsMu    sync.Mutex
	floorsCache = map[language][]float32{}
)

func headerFloors() []float32 {
	floorsMu.Lock()
	defer floorsMu.Unlock()
	if f, ok := floorsCache[current]; ok {
		return f
	}
	f := make([]float32, len(columns))
	for i, c := range columns {
		if c.width > 0 {
			// The header is a button in the compact theme: bold 13 pt text
			// with inner padding on both sides.
			f[i] = fyne.MeasureText(T(c.title)+" ▼", 13, fyne.TextStyle{Bold: true}).Width + 20
		}
	}
	floorsCache[current] = f
	return f
}

// fixedWidth is a fixed column's width: its own, or its title's if wider.
func fixedWidth(i int) float32 {
	if f := headerFloors()[i]; f > columns[i].width {
		return f
	}
	return columns[i].width
}

func columnWidths(total float32) []float32 {
	fixed := float32(colGap * (len(columns) - 1))
	var shares float32
	for i, c := range columns {
		if c.width > 0 {
			fixed += fixedWidth(i)
		}
		shares += c.share
	}
	rest := total - fixed
	if rest < minShareArea {
		rest = minShareArea
	}
	w := make([]float32, len(columns))
	for i, c := range columns {
		if c.width > 0 {
			w[i] = fixedWidth(i)
		} else {
			w[i] = rest * c.share / shares
		}
	}
	return w
}

func (columnLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	x := float32(0)
	for i, w := range columnWidths(size.Width) {
		if i >= len(objs) {
			return
		}
		objs[i].Resize(fyne.NewSize(w, size.Height))
		objs[i].Move(fyne.NewPos(x, 0))
		x += w + colGap
	}
}

func (columnLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	var h float32
	for _, o := range objs {
		if m := o.MinSize().Height; m > h {
			h = m
		}
	}
	var w float32
	for _, cw := range columnWidths(0) {
		w += cw
	}
	return fyne.NewSize(w+colGap*float32(len(columns)-1), h)
}

// ---------- Header ----------

// newHeader builds the column titles; clicking one sorts by it. update
// redraws the arrow of the column sorted by.
func newHeader(onSort func(sortKey)) (obj fyne.CanvasObject, update func(sortOrder)) {
	buttons := make([]*widget.Button, len(columns))
	cells := make([]fyne.CanvasObject, len(columns))
	for i, c := range columns {
		key := c.key
		b := widget.NewButton(T(c.title), func() { onSort(key) })
		b.Importance = widget.LowImportance
		switch c.align {
		case fyne.TextAlignTrailing:
			b.Alignment = widget.ButtonAlignTrailing
		case fyne.TextAlignCenter:
			b.Alignment = widget.ButtonAlignCenter
		default:
			b.Alignment = widget.ButtonAlignLeading
		}
		buttons[i], cells[i] = b, b
	}
	update = func(o sortOrder) {
		for i, c := range columns {
			title := T(c.title)
			if o.key == c.key {
				if o.desc {
					title += " ▼"
				} else {
					title += " ▲"
				}
			}
			buttons[i].SetText(title)
		}
	}
	update(sortOrder{})
	return container.NewVBox(container.New(columnLayout{}, cells...), widget.NewSeparator()), update
}

// ---------- Status cell ----------

// statusCell is the Status column: the text, and under it a thin line
// showing how far the download is.
type statusCell struct {
	widget.BaseWidget
	label *widget.Label
	track *canvas.Rectangle
	fill  *canvas.Rectangle
	value float64 // 0..1; <0 hides the line
}

func newStatusCell() *statusCell {
	c := &statusCell{
		label: widget.NewLabel(""),
		track: canvas.NewRectangle(theme.Color(theme.ColorNameInputBorder)),
		fill:  canvas.NewRectangle(theme.Color(theme.ColorNamePrimary)),
		value: -1,
	}
	c.label.Truncation = fyne.TextTruncateEllipsis
	c.ExtendBaseWidget(c)
	return c
}

func (c *statusCell) set(text string, imp widget.Importance, value float64) {
	c.label.Importance = imp
	c.label.SetText(text)
	c.value = value
	c.Refresh()
}

func (c *statusCell) CreateRenderer() fyne.WidgetRenderer {
	return &statusCellRenderer{c: c, objs: []fyne.CanvasObject{c.label, c.track, c.fill}}
}

type statusCellRenderer struct {
	c    *statusCell
	objs []fyne.CanvasObject
}

func (r *statusCellRenderer) Layout(size fyne.Size) {
	r.c.label.Resize(size)
	r.c.label.Move(fyne.NewPos(0, 0))
	const h, margin = 3, 2
	pad := theme.InnerPadding()
	w := size.Width - 2*pad
	if w < 0 {
		w = 0
	}
	y := size.Height - h - margin
	r.c.track.Resize(fyne.NewSize(w, h))
	r.c.track.Move(fyne.NewPos(pad, y))
	fw := w * float32(r.c.value)
	if fw < 0 {
		fw = 0
	}
	r.c.fill.Resize(fyne.NewSize(fw, h))
	r.c.fill.Move(fyne.NewPos(pad, y))
}

func (r *statusCellRenderer) MinSize() fyne.Size { return r.c.label.MinSize() }

func (r *statusCellRenderer) Refresh() {
	show := r.c.value >= 0
	r.c.track.Hidden, r.c.fill.Hidden = !show, !show
	r.c.track.FillColor = theme.Color(theme.ColorNameInputBorder)
	r.c.fill.FillColor = theme.Color(theme.ColorNamePrimary)
	r.Layout(r.c.Size())
	r.c.label.Refresh()
	r.c.track.Refresh()
	r.c.fill.Refresh()
}

func (r *statusCellRenderer) Objects() []fyne.CanvasObject { return r.objs }
func (r *statusCellRenderer) Destroy()                     {}

// ---------- Row ----------

// jobRow is one row of the table. Fyne recycles rows, so set rebinds it.
// It takes the pointer events itself: the list's own selection is a single
// row, and here Ctrl/Shift-click select several and a right-click opens the
// actions for them.
type jobRow struct {
	widget.BaseWidget
	q      *queueTab
	id     string
	bg     *canvas.Rectangle
	icon   *widget.Icon
	name   *widget.Label
	size   *widget.Label
	status *statusCell
	eta    *widget.Label
	speed  *widget.Label
	conns  *widget.Label
	added  *widget.Label
	root   fyne.CanvasObject
}

func newJobRow(q *queueTab) *jobRow {
	label := func(align fyne.TextAlign) *widget.Label {
		l := widget.NewLabel("")
		l.Alignment = align
		l.Truncation = fyne.TextTruncateEllipsis
		return l
	}
	r := &jobRow{
		q:      q,
		bg:     canvas.NewRectangle(color.Transparent),
		icon:   widget.NewIcon(nil),
		name:   label(fyne.TextAlignLeading),
		size:   label(fyne.TextAlignTrailing),
		status: newStatusCell(),
		eta:    label(fyne.TextAlignTrailing),
		speed:  label(fyne.TextAlignTrailing),
		conns:  label(fyne.TextAlignCenter),
		added:  label(fyne.TextAlignLeading),
	}
	cells := container.New(columnLayout{},
		container.NewBorder(nil, nil, r.icon, nil, r.name),
		r.size, r.status, r.eta, r.speed, r.conns, r.added)
	r.root = container.NewStack(r.bg, cells)
	r.ExtendBaseWidget(r)
	return r
}

func (r *jobRow) CreateRenderer() fyne.WidgetRenderer { return widget.NewSimpleRenderer(r.root) }

func (r *jobRow) set(rw row, selected bool) {
	j := rw.Job
	r.id = j.ID
	if selected {
		r.bg.FillColor = theme.Color(theme.ColorNameSelection)
	} else {
		r.bg.FillColor = color.Transparent
	}
	r.bg.Refresh()
	r.icon.SetResource(iconFor(kindOf(j.Filename)))
	r.name.SetText(j.Filename)
	r.size.SetText(sizeText(j))
	bar := -1.0
	if (j.State == queue.StateRunning || j.State == queue.StatePaused && j.Done > 0) && j.Size > 0 {
		bar = rowProgress(j)
	}
	r.status.set(statusText(rw, time.Now()), statusImportance(j.State), bar)
	r.eta.SetText(etaText(rw))
	r.speed.SetText(speedText(rw))
	r.conns.SetText(connsText(j))
	r.added.SetText(addedText(j.AddedAt, time.Now()))
}

// MouseDown selects: a click one row, Ctrl-click adds or removes one,
// Shift-click a range. A right-click on a row that isn't selected selects
// it first, so the menu acts on what was clicked.
func (r *jobRow) MouseDown(ev *desktop.MouseEvent) {
	if r.id != "" {
		r.q.rowPressed(r.id, ev.Button, ev.Modifier)
	}
}

func (r *jobRow) MouseUp(*desktop.MouseEvent) {}

// Tapped is taken here so the list doesn't select the row on its own.
func (r *jobRow) Tapped(*fyne.PointEvent) {}

func (r *jobRow) TappedSecondary(ev *fyne.PointEvent) {
	if r.id != "" {
		r.q.showRowMenu(ev.AbsolutePosition)
	}
}

func (r *jobRow) DoubleTapped(*fyne.PointEvent) {
	if r.id != "" {
		r.q.rowDoubleTapped(r.id)
	}
}

// kindIcons are the file kinds' icons in the name column: a shape per
// kind, colored for the ones people look for most. Made on first use:
// theme icons made before the app starts log an error each.
var (
	kindIconsOnce sync.Once
	kindIcons     map[fileKind]fyne.Resource
)

func iconFor(k fileKind) fyne.Resource {
	kindIconsOnce.Do(func() {
		kindIcons = map[fileKind]fyne.Resource{
			kindVideo:    theme.NewColoredResource(theme.MediaVideoIcon(), theme.ColorNamePrimary),
			kindImage:    theme.NewColoredResource(theme.MediaPhotoIcon(), theme.ColorNameSuccess),
			kindAudio:    theme.NewColoredResource(theme.MediaMusicIcon(), theme.ColorNameWarning),
			kindArchive:  theme.StorageIcon(),
			kindDocument: theme.DocumentIcon(),
			kindProgram:  theme.ComputerIcon(),
			kindOther:    theme.FileIcon(),
		}
	})
	if r, ok := kindIcons[k]; ok {
		return r
	}
	return theme.FileIcon()
}

// statusImportance colors the Status column: running blue, waiting amber,
// failed red, done green.
func statusImportance(s queue.State) widget.Importance {
	switch s {
	case queue.StateRunning:
		return widget.HighImportance
	case queue.StateFailed:
		return widget.DangerImportance
	case queue.StateWaiting:
		return widget.WarningImportance
	case queue.StateDone, queue.StateSkipped:
		return widget.SuccessImportance
	}
	return widget.MediumImportance
}

// ---------- Category tree ----------

func newTreeItem(bool) fyne.CanvasObject {
	name := widget.NewLabel("")
	name.Truncation = fyne.TextTruncateEllipsis
	count := widget.NewLabel("")
	count.Alignment = fyne.TextAlignTrailing
	count.Importance = widget.LowImportance
	return container.NewBorder(nil, nil, nil, container.NewThemeOverride(count, countTheme{newCompactTheme()}), name)
}

func setTreeItem(o fyne.CanvasObject, label string, n int) {
	c := o.(*fyne.Container)
	name := c.Objects[0].(*widget.Label)
	count := c.Objects[1].(*container.ThemeOverride).Content.(*widget.Label)
	name.SetText(label)
	if n > 0 {
		count.SetText(strconv.Itoa(n))
	} else {
		count.SetText("")
	}
}

// ---------- Toolbar ----------

// toolItem is a toolbar button in a download manager's style: the icon
// above, the label under it, flat until the pointer is over it.
type toolItem struct {
	widget.BaseWidget
	icon     fyne.Resource
	text     string
	tapped   func()
	disabled bool
	hovered  bool

	bg    *canvas.Rectangle
	img   *widget.Icon
	label *canvas.Text
}

func newToolItem(text string, icon fyne.Resource, tapped func()) *toolItem {
	t := &toolItem{icon: icon, text: text, tapped: tapped,
		bg: canvas.NewRectangle(color.Transparent), img: widget.NewIcon(icon), label: canvas.NewText(text, theme.Color(theme.ColorNameForeground))}
	t.bg.CornerRadius = theme.InputRadiusSize()
	t.label.Alignment = fyne.TextAlignCenter
	t.label.TextSize = 12
	t.ExtendBaseWidget(t)
	return t
}

func (t *toolItem) SetText(s string)        { t.text = s; t.Refresh() }
func (t *toolItem) SetIcon(r fyne.Resource) { t.icon = r; t.Refresh() }
func (t *toolItem) Disabled() bool          { return t.disabled }
func (t *toolItem) Enable()                 { t.disabled = false; t.Refresh() }
func (t *toolItem) Disable()                { t.disabled = true; t.hovered = false; t.Refresh() }
func (t *toolItem) MouseIn(*desktop.MouseEvent) {
	if !t.disabled {
		t.hovered = true
		t.Refresh()
	}
}
func (t *toolItem) MouseMoved(*desktop.MouseEvent) {}
func (t *toolItem) MouseOut()                      { t.hovered = false; t.Refresh() }

func (t *toolItem) Tapped(*fyne.PointEvent) {
	if !t.disabled && t.tapped != nil {
		t.tapped()
	}
}

func (t *toolItem) CreateRenderer() fyne.WidgetRenderer {
	return &toolItemRenderer{t: t, objs: []fyne.CanvasObject{t.bg, t.img, t.label}}
}

type toolItemRenderer struct {
	t    *toolItem
	objs []fyne.CanvasObject
}

const toolIconSize = 26

func (r *toolItemRenderer) MinSize() fyne.Size {
	ts := fyne.MeasureText(r.t.text, r.t.label.TextSize, fyne.TextStyle{})
	w := ts.Width + 16
	if w < 58 {
		w = 58
	}
	return fyne.NewSize(w, toolIconSize+ts.Height+12)
}

func (r *toolItemRenderer) Layout(size fyne.Size) {
	r.t.bg.Resize(size)
	r.t.img.Resize(fyne.NewSize(toolIconSize, toolIconSize))
	r.t.img.Move(fyne.NewPos((size.Width-toolIconSize)/2, 4))
	ts := fyne.MeasureText(r.t.text, r.t.label.TextSize, fyne.TextStyle{})
	r.t.label.Resize(fyne.NewSize(size.Width, ts.Height))
	r.t.label.Move(fyne.NewPos(0, 4+toolIconSize+2))
}

func (r *toolItemRenderer) Refresh() {
	t := r.t
	t.label.Text = t.text
	if t.disabled {
		t.img.SetResource(theme.NewDisabledResource(t.icon))
		t.label.Color = theme.Color(theme.ColorNameDisabled)
	} else {
		t.img.SetResource(t.icon)
		t.label.Color = theme.Color(theme.ColorNameForeground)
	}
	if t.hovered {
		t.bg.FillColor = theme.Color(theme.ColorNameHover)
	} else {
		t.bg.FillColor = color.Transparent
	}
	r.Layout(t.Size())
	t.bg.Refresh()
	t.label.Refresh()
}

func (r *toolItemRenderer) Objects() []fyne.CanvasObject { return r.objs }
func (r *toolItemRenderer) Destroy()                     {}

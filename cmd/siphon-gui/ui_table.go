package main

import (
	"image/color"
	"strconv"
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

// columns: Name and Status take what the fixed ones leave; a long name or
// error is cut with an ellipsis instead of widening the table.
var columns = []column{
	{title: "Name", key: sortName, share: 0.52, align: fyne.TextAlignLeading},
	{title: "Size", key: sortSize, width: 80, align: fyne.TextAlignTrailing},
	{title: "Progress", key: sortProgress, width: 120, align: fyne.TextAlignCenter},
	{title: "Speed", key: sortSpeed, width: 86, align: fyne.TextAlignTrailing},
	{title: "Time left", key: sortETA, width: 74, align: fyne.TextAlignTrailing},
	{title: "Status", key: sortStatus, share: 0.48, align: fyne.TextAlignLeading},
}

const (
	colGap       = 6
	minShareArea = 220 // Name and Status together never get less
)

// columnLayout places one object per column side by side. The header and
// every row use it, so their columns line up.
type columnLayout struct{}

func columnWidths(total float32) []float32 {
	fixed := float32(colGap * (len(columns) - 1))
	var shares float32
	for _, c := range columns {
		fixed += c.width
		shares += c.share
	}
	rest := total - fixed
	if rest < minShareArea {
		rest = minShareArea
	}
	w := make([]float32, len(columns))
	for i, c := range columns {
		if c.width > 0 {
			w[i] = c.width
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
		// Each cell keeps its own height, centered: a progress bar stretched
		// to the row's height would look like a block.
		h := objs[i].MinSize().Height
		if h > size.Height {
			h = size.Height
		}
		objs[i].Resize(fyne.NewSize(w, h))
		objs[i].Move(fyne.NewPos(x, (size.Height-h)/2))
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
		b := widget.NewButton(c.title, func() { onSort(key) })
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
			title := c.title
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

// ---------- Row ----------

// jobRow is one row of the table. Fyne recycles rows, so set rebinds it.
// It takes the pointer events itself: the list's own selection is a single
// row, and here Ctrl/Shift-click select several and a right-click opens the
// actions for them.
type jobRow struct {
	widget.BaseWidget
	q       *queueTab
	id      string
	bg      *canvas.Rectangle
	icon    *widget.Icon
	name    *widget.Label
	size    *widget.Label
	bar     *widget.ProgressBar
	barText string
	speed   *widget.Label
	eta     *widget.Label
	status  *widget.Label
	root    fyne.CanvasObject
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
		bar:    widget.NewProgressBar(),
		speed:  label(fyne.TextAlignTrailing),
		eta:    label(fyne.TextAlignTrailing),
		status: label(fyne.TextAlignLeading),
	}
	r.bar.TextFormatter = func() string { return r.barText }
	r.bg.CornerRadius = theme.SelectionRadiusSize()
	cells := container.New(columnLayout{},
		container.NewBorder(nil, nil, r.icon, nil, r.name),
		r.size, r.bar, r.speed, r.eta, r.status)
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
	r.icon.SetResource(stateIcon(j.State))
	r.name.SetText(j.Filename)
	r.size.SetText(sizeText(j))
	r.barText = progressText(j)
	r.bar.SetValue(rowProgress(j))
	r.bar.Refresh() // the text may change while the value doesn't
	r.speed.SetText(speedText(rw))
	r.eta.SetText(etaText(rw))
	r.status.Importance = statusImportance(j.State)
	r.status.SetText(statusText(rw, time.Now()))
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

// stateIcon marks a row's state at a glance.
func stateIcon(s queue.State) fyne.Resource {
	switch s {
	case queue.StateRunning:
		return theme.DownloadIcon()
	case queue.StateQueued:
		return theme.HistoryIcon()
	case queue.StatePaused, queue.StateStopped:
		return theme.MediaPauseIcon()
	case queue.StateWaiting:
		return theme.WarningIcon()
	case queue.StateFailed:
		return theme.ErrorIcon()
	case queue.StateDone, queue.StateSkipped:
		return theme.ConfirmIcon()
	}
	return theme.FileIcon()
}

func statusImportance(s queue.State) widget.Importance {
	switch s {
	case queue.StateFailed:
		return widget.DangerImportance
	case queue.StateWaiting:
		return widget.WarningImportance
	case queue.StateDone, queue.StateSkipped:
		return widget.SuccessImportance
	}
	return widget.MediumImportance
}

// ---------- Sidebar ----------

// sidebarEntry is one line of the sidebar.
type sidebarEntry struct {
	f     filter
	count int
}

// sidebarEntries lists the filters with their counts. "Waiting for quota"
// only shows while something waits (it is mega's alone), unless it is the
// one selected.
func sidebarEntries(counts map[filter]int, current filter) []sidebarEntry {
	out := make([]sidebarEntry, 0, len(filters))
	for _, f := range filters {
		if f == filterWaiting && counts[f] == 0 && current != filterWaiting {
			continue
		}
		out = append(out, sidebarEntry{f: f, count: counts[f]})
	}
	return out
}

func newSidebarItem() fyne.CanvasObject {
	name := widget.NewLabel("")
	name.Truncation = fyne.TextTruncateEllipsis
	count := widget.NewLabel("")
	count.Alignment = fyne.TextAlignTrailing
	count.Importance = widget.LowImportance
	return container.NewBorder(nil, nil, nil, count, name)
}

func setSidebarItem(o fyne.CanvasObject, e sidebarEntry) {
	c := o.(*fyne.Container)
	name, count := c.Objects[0].(*widget.Label), c.Objects[1].(*widget.Label)
	name.SetText(e.f.label())
	if e.count > 0 {
		count.SetText(strconv.Itoa(e.count))
	} else {
		count.SetText("")
	}
}

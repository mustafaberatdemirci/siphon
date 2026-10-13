package main

import (
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// TestREADMEDemo draws docs/demo.gif, the README's moving picture: a mega
// folder link is pasted, its files download over several connections, the
// quota runs out and the jobs wait, then they resume after a VPN switch. It
// is drawn off screen from sample jobs, like docs/screenshot.png. Skipped
// unless asked for:
//
//	SIPHON_DEMO=1 go test ./cmd/siphon-gui -run READMEDemo
func TestREADMEDemo(t *testing.T) {
	if os.Getenv("SIPHON_DEMO") == "" {
		t.Skip("set SIPHON_DEMO=1 to regenerate docs/demo.gif")
	}
	a := test.NewTempApp(t)
	test.ApplyTheme(t, theme.DefaultTheme())
	a.Preferences().SetString(prefOutDir, `D:\Downloads`)

	cfg := site.SiteConfig{Name: "mega", MaxSegments: 8}.WithDefaults()
	eng, err := queue.New(queue.Options{Resolvers: []site.Resolver{shotResolver{}}, Configs: []site.SiteConfig{cfg}})
	if err != nil {
		t.Fatal(err)
	}
	vm := newViewModel()
	w := test.NewTempWindow(t, container.NewStack())
	q, view := newQueueTab(w, a.Preferences(), eng, vm, nil)
	w.SetContent(view)
	w.Resize(fyne.NewSize(1100, 600))

	// The caption is a window of its own, stacked under the app's, so a
	// dialog's shade doesn't cover it.
	caption := canvas.NewText("", color.White)
	caption.TextSize = 20
	caption.TextStyle = fyne.TextStyle{Bold: true}
	captionBox := container.NewCenter(caption)
	cw := test.NewTempWindow(t, container.NewStack(
		canvas.NewRectangle(color.NRGBA{R: 0x1f, G: 0x4f, B: 0xb8, A: 0xff}), captionBox))
	cw.Resize(fyne.NewSize(1100, 52))

	var frames []*image.NRGBA
	var delays []int
	shot := func(text string, d time.Duration) {
		q.render()
		// A real window lays itself out again when the quota banner
		// appears; the test window only does on a resize.
		w.Resize(fyne.NewSize(1101, 600))
		w.Resize(fyne.NewSize(1100, 600))
		caption.Text = text
		captionBox.Refresh() // centres it again for the new width
		frames = append(frames, stackImages(w.Canvas().Capture(), cw.Canvas().Capture()))
		delays = append(delays, int(d/(10*time.Millisecond)))
	}

	// 1. Paste a link.
	const pasteCaption = "Paste a link: a whole mega folder at once"
	shot(pasteCaption, 900*time.Millisecond)
	q.showAddDialog("https://mega.nz/folder/AbCdEfGh#sample-key-for-the-demo")
	shot(pasteCaption, 2200*time.Millisecond)
	for _, o := range w.Canvas().Overlays().List() {
		w.Canvas().Overlays().Remove(o)
	}

	// 2. Its files are queued and download over 8 connections each.
	const mb = 1 << 20
	files := []struct {
		name string
		size int64
		rate float64 // MB/s when running
	}{
		{"Holiday 2025 - Day 1.mp4", 1480 * mb, 21.4},
		{"Holiday 2025 - Day 2.mp4", 1210 * mb, 18.9},
		{"Holiday 2025 - Day 3.mp4", 960 * mb, 23.6},
		{"Holiday 2025 - Day 4.mp4", 1730 * mb, 0},
		{"Holiday 2025 - Day 5.mp4", 1320 * mb, 0},
		{"Holiday 2025 - Drone.mp4", 2410 * mb, 0},
		{"Holiday 2025 - Photos.zip", 640 * mb, 0},
		{"Holiday 2025 - Music.flac", 286 * mb, 0},
		{"Holiday 2025 - Notes.pdf", 4 * mb, 0},
	}
	start := time.Now()
	jobs := make([]queue.Job, len(files))
	rates := make([]float64, len(files))
	for i, f := range files {
		jobs[i] = queue.Job{ID: string(rune('a' + i)), Site: "mega", Filename: f.name, Size: f.size,
			State: queue.StateQueued, AddedAt: start}
		rates[i] = f.rate
	}
	const active = 3
	for i := 0; i < active; i++ {
		jobs[i].State = queue.StateRunning
	}
	// wobble makes the speeds move a little from frame to frame, as they do.
	wobble := []float64{1, 1.06, 0.97, 1.03, 0.94, 1.02, 0.99, 1.05}
	step := 0
	publish := func() {
		vm.mu.Lock()
		defer vm.mu.Unlock()
		for i, j := range jobs {
			vm.apply(j, start)
			if j.State == queue.StateRunning {
				vm.rate[j.ID] = rates[i] * wobble[(step+i)%len(wobble)] * mb
				jobs[i].Conns = 8
			}
		}
		// The test draws each frame itself; the window's refresh loop must
		// not draw at the same time (text shaping isn't safe for that).
		vm.dirty = false
	}
	// advance moves the running jobs on by about frac of their size and
	// starts the next queued job in place of a finished one.
	advance := func(frac float64) {
		step++
		for i := range jobs {
			if jobs[i].State != queue.StateRunning {
				continue
			}
			jobs[i].Conns = 8
			jobs[i].Done += int64(frac * float64(jobs[i].Size) * rates[i] / 21)
			if jobs[i].Done < jobs[i].Size {
				continue
			}
			jobs[i].Done, jobs[i].State, jobs[i].Conns = jobs[i].Size, queue.StateDone, 0
			for k := range jobs {
				if jobs[k].State == queue.StateQueued {
					jobs[k].State = queue.StateRunning
					rates[k] = 17 + float64(k%3)*2.7
					break
				}
			}
		}
	}

	const queuedCaption = "Every file is queued and downloads over 8 connections"
	publish()
	shot(queuedCaption, 700*time.Millisecond)
	for i := 0; i < 10; i++ {
		advance(0.045)
		publish()
		shot(queuedCaption, 300*time.Millisecond)
	}

	// 3. The quota runs out: the jobs wait instead of failing.
	retry := start.Add(3*time.Hour + 40*time.Minute)
	var held []int
	for i := range jobs {
		if jobs[i].State == queue.StateRunning {
			jobs[i].State, jobs[i].RetryAt, jobs[i].Conns = queue.StateWaiting, retry, 0
			held = append(held, i)
		}
	}
	publish()
	shot("Transfer quota used up? The jobs wait instead of failing", 3200*time.Millisecond)

	// 4. A VPN switch: they resume by themselves.
	for _, i := range held {
		jobs[i].State, jobs[i].RetryAt = queue.StateRunning, time.Time{}
	}
	const resumeCaption = "Switch your VPN server and they resume by themselves"
	for i := 0; i < 14; i++ {
		advance(0.07)
		publish()
		shot(resumeCaption, 300*time.Millisecond)
	}
	delays[len(delays)-1] = 250 // hold the last frame before the loop

	out := filepath.Join("..", "..", "docs", "demo.gif")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := gif.EncodeAll(f, encodeGIF(frames, delays)); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s: %d frames", out, len(frames))
}

// stackImages puts b under a, as one image.
func stackImages(a, b image.Image) *image.NRGBA {
	ab, bb := a.Bounds(), b.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, max(ab.Dx(), bb.Dx()), ab.Dy()+bb.Dy()))
	draw.Draw(out, image.Rect(0, 0, ab.Dx(), ab.Dy()), a, ab.Min, draw.Src)
	draw.Draw(out, image.Rect(0, ab.Dy(), bb.Dx(), ab.Dy()+bb.Dy()), b, bb.Min, draw.Src)
	return out
}

// encodeGIF turns the frames into a looping GIF. A flat interface has few
// colours, so one palette of the most frequent ones serves every frame
// without dithering (which would speckle the text). After the first frame
// only the rectangle that changed is stored, and in it the pixels that
// didn't change are transparent, so a frame where one progress bar moved
// costs a few kilobytes.
func encodeGIF(frames []*image.NRGBA, delays []int) *gif.GIF {
	const transparent = 255
	pal := framePalette(frames, transparent)
	nearest := map[color.NRGBA]uint8{}
	index := func(c color.NRGBA) uint8 {
		c.A = 0xff
		if i, ok := nearest[c]; ok {
			return i
		}
		best, bestD := 0, int(^uint(0)>>1)
		for i, p := range pal[:transparent] {
			if d := int(colorDist(c, p.(color.NRGBA))); d < bestD {
				best, bestD = i, d
			}
		}
		nearest[c] = uint8(best)
		return uint8(best)
	}

	g := &gif.GIF{LoopCount: 0}
	var prev *image.NRGBA
	for n, f := range frames {
		r := f.Bounds()
		if prev != nil {
			r = changedRect(prev, f)
			if r.Empty() {
				// Nothing moved: lengthen the previous frame instead.
				g.Delay[len(g.Delay)-1] += delays[n]
				continue
			}
		}
		p := image.NewPaletted(r, pal)
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				c := f.NRGBAAt(x, y)
				if prev != nil && prev.NRGBAAt(x, y) == c {
					p.SetColorIndex(x, y, transparent)
					continue
				}
				p.SetColorIndex(x, y, index(c))
			}
		}
		g.Image = append(g.Image, p)
		g.Delay = append(g.Delay, delays[n])
		g.Disposal = append(g.Disposal, gif.DisposalNone)
		prev = f
	}
	return g
}

// framePalette picks the palette from every frame's colours, with the
// transparent slot at index transparent. Picking the most frequent colours
// alone lost the rare ones: the green of "Complete", on three rows near the
// end, came out grey. So each pick is the colour that is frequent AND far
// from what the palette already has (count × distance), which keeps every
// hue the window uses and still spends most slots on the greys of the text.
func framePalette(frames []*image.NRGBA, transparent int) color.Palette {
	count := map[color.NRGBA]int{}
	for _, f := range frames {
		for i := 0; i < len(f.Pix); i += 4 {
			count[color.NRGBA{R: f.Pix[i], G: f.Pix[i+1], B: f.Pix[i+2], A: 0xff}]++
		}
	}
	type cand struct {
		c    color.NRGBA
		n    float64
		dist float64 // to the nearest colour picked so far
	}
	cands := make([]cand, 0, len(count))
	for c, n := range count {
		cands = append(cands, cand{c: c, n: float64(n), dist: -1})
	}
	// A fixed order, so the same frames always give the same file.
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i].c, cands[j].c
		return a.R < b.R || a.R == b.R && (a.G < b.G || a.G == b.G && a.B < b.B)
	})
	pal := make(color.Palette, transparent+1)
	for k := 0; k < transparent; k++ {
		best, bestScore := -1, -1.0
		for i, c := range cands {
			score := c.n
			if c.dist >= 0 {
				score *= c.dist
			}
			if score > bestScore {
				best, bestScore = i, score
			}
		}
		if best < 0 || bestScore == 0 {
			for ; k < transparent; k++ {
				pal[k] = color.NRGBA{A: 0xff}
			}
			break
		}
		picked := cands[best].c
		pal[k] = picked
		for i := range cands {
			if d := colorDist(cands[i].c, picked); cands[i].dist < 0 || d < cands[i].dist {
				cands[i].dist = d
			}
		}
	}
	pal[transparent] = color.NRGBA{}
	return pal
}

// colorDist is a squared distance weighted roughly by how the eye sees it.
func colorDist(a, b color.NRGBA) float64 {
	dr, dg, db := float64(a.R)-float64(b.R), float64(a.G)-float64(b.G), float64(a.B)-float64(b.B)
	return 3*dr*dr + 4*dg*dg + 2*db*db
}

// changedRect is the smallest rectangle holding every pixel that differs.
func changedRect(a, b *image.NRGBA) image.Rectangle {
	r := image.Rectangle{}
	bounds := b.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if a.NRGBAAt(x, y) != b.NRGBAAt(x, y) {
				r = r.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	return r
}

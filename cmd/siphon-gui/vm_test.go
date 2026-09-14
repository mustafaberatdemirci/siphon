package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
)

// TestMain, bassiz bir Fyne uygulamasi kurar: bazi yardimcilar Fyne'a
// dokunuyor ve calisan bir uygulama olmadan nil pointer panigi atiyor.
func TestMain(m *testing.M) {
	test.NewApp()
	os.Exit(m.Run())
}

func job(id string, st queue.State, done, size int64) queue.Job {
	return queue.Job{ID: id, Filename: id + ".mp4", State: st, Done: done, Size: size}
}

// --- Gorunum modeli ---

func TestViewModelKeepsInsertionOrder(t *testing.T) {
	vm := newViewModel()
	vm.Apply(job("b", queue.StateQueued, 0, 10))
	vm.Apply(job("a", queue.StateQueued, 0, 10))
	vm.Apply(job("b", queue.StateRunning, 5, 10)) // guncelleme sirayi bozmamali
	rows := vm.Rows()
	if len(rows) != 2 || rows[0].Job.ID != "b" || rows[1].Job.ID != "a" {
		t.Fatalf("sira bozuk: %+v", rows)
	}
	if rows[0].Job.State != queue.StateRunning {
		t.Error("guncelleme uygulanmadi")
	}
}

// Hiz yalnizca calisan islerde olculur; duran isin hizi silinir.
func TestViewModelTracksRateOnlyWhileRunning(t *testing.T) {
	vm := newViewModel()
	now := time.Now()
	vm.mu.Lock()
	vm.apply(job("a", queue.StateRunning, 0, 1<<20), now)
	vm.apply(job("a", queue.StateRunning, 512<<10, 1<<20), now.Add(time.Second))
	vm.mu.Unlock()
	r, _ := vm.Row(0)
	if r.Rate <= 0 {
		t.Fatalf("calisan isin hizi olculmedi: %v", r.Rate)
	}
	vm.Apply(job("a", queue.StatePaused, 512<<10, 1<<20))
	r, _ = vm.Row(0)
	if r.Rate != 0 {
		t.Fatalf("duraklayan isin hizi kaldi: %v", r.Rate)
	}
}

// Replace, motordan gelen tam listeyle eslesir: kaldirilan is dusmeli.
func TestViewModelReplaceDropsMissing(t *testing.T) {
	vm := newViewModel()
	vm.Apply(job("a", queue.StateQueued, 0, 10))
	vm.Apply(job("b", queue.StateQueued, 0, 10))
	vm.Replace([]queue.Job{job("b", queue.StateDone, 10, 10)})
	rows := vm.Rows()
	if len(rows) != 1 || rows[0].Job.ID != "b" || rows[0].Job.State != queue.StateDone {
		t.Fatalf("Replace yanlis: %+v", rows)
	}
}

// Degisiklik bayragi: olay basina degil, periyodik cizim icin.
func TestViewModelDirtyFlag(t *testing.T) {
	vm := newViewModel()
	if vm.TakeDirty() {
		t.Fatal("bos modelde dirty")
	}
	vm.Apply(job("a", queue.StateQueued, 0, 10))
	if !vm.TakeDirty() {
		t.Fatal("degisiklik sonrasi dirty degil")
	}
	if vm.TakeDirty() {
		t.Fatal("bayrak sifirlanmadi")
	}
}

func TestSummaryCountsAndTotalRate(t *testing.T) {
	vm := newViewModel()
	if got := vm.Summary(); !strings.Contains(got, "boş") {
		t.Errorf("bos ozet: %q", got)
	}
	now := time.Now()
	vm.mu.Lock()
	vm.apply(job("a", queue.StateRunning, 0, 1<<20), now)
	vm.apply(job("a", queue.StateRunning, 1<<20, 1<<20), now.Add(time.Second))
	vm.apply(job("b", queue.StateQueued, 0, 5), now)
	vm.apply(job("c", queue.StateDone, 5, 5), now)
	vm.apply(job("d", queue.StatePaused, 1, 5), now)
	vm.apply(job("e", queue.StateFailed, 0, 5), now)
	vm.mu.Unlock()
	got := vm.Summary()
	for _, want := range []string{"1 aktif", "1 sırada", "1 duraklatıldı", "1 hata", "1 bitti", "MB/s"} {
		if !strings.Contains(got, want) {
			t.Errorf("ozet %q icinde %q yok", got, want)
		}
	}
}

// Bildirim, yenileme dongusunun ozetiyle ezilmemeli: kuyruk bosken kalici,
// kuyruk doluyken ozetle yan yana ve 30 sn sonra ozete birakir.
func TestStatusLineKeepsNoticeOverSummary(t *testing.T) {
	vm := newViewModel()
	vm.TakeDirty()
	vm.Notify("Eklenemedi: klasör boş")
	if !vm.TakeDirty() {
		t.Fatal("bildirim yeniden cizim istemedi")
	}
	now := time.Now()
	if got := vm.statusLine(now.Add(time.Hour)); got != "Eklenemedi: klasör boş" {
		t.Errorf("bos kuyrukta bildirim kaybolmus: %q", got)
	}
	vm.mu.Lock()
	vm.apply(job("a", queue.StateQueued, 0, 5), now)
	vm.mu.Unlock()
	if got := vm.statusLine(now); !strings.HasPrefix(got, "Eklenemedi: klasör boş  ·  1 sırada") {
		t.Errorf("dolu kuyrukta bildirim + ozet bekleniyordu: %q", got)
	}
	if got := vm.statusLine(now.Add(noticeTTL + time.Second)); got != "1 sırada" {
		t.Errorf("suresi dolan bildirim kalkmali: %q", got)
	}
}

// --- Satir bicimlendirme ---

func TestRowMetaByState(t *testing.T) {
	run := row{Job: job("a", queue.StateRunning, 20<<20, 100<<20), Rate: 10 << 20}
	if got := rowMeta(run); !strings.Contains(got, "MB/s") || !strings.Contains(got, "kalan") {
		t.Errorf("calisan satir: %q", got)
	}
	// Boyut bilinmiyorken kalan sure UYDURULMAMALI.
	unk := row{Job: job("u", queue.StateRunning, 5<<20, -1), Rate: 1 << 20}
	if got := rowMeta(unk); strings.Contains(got, "kalan") {
		t.Errorf("boyutsuz satirda kalan sure: %q", got)
	}
	// Hiz bilinmiyorken "0 B/s" yazilmamali.
	slow := row{Job: job("s", queue.StateRunning, 1, 100), Rate: 0}
	if got := rowMeta(slow); strings.Contains(got, "/s") {
		t.Errorf("hizsiz satirda hiz: %q", got)
	}
	failed := job("f", queue.StateFailed, 0, 10)
	failed.Error = "mega aktarım kotası doldu (HTTP 509): IP başına sınır\nikinci satır"
	if got := rowMeta(row{Job: failed}); !strings.Contains(got, "kota") || strings.Contains(got, "ikinci") {
		t.Errorf("hatali satir ilk satiri gostermeli: %q", got)
	}
	done := job("d", queue.StateDone, 10, 10)
	if got := rowMeta(row{Job: done}); !strings.Contains(got, "bitti") {
		t.Errorf("biten satir: %q", got)
	}
}

func TestRowProgress(t *testing.T) {
	if p := rowProgress(job("a", queue.StateRunning, 50, 200)); p < 0.24 || p > 0.26 {
		t.Errorf("ilerleme = %v", p)
	}
	if p := rowProgress(job("a", queue.StateRunning, 50, -1)); p != 0 {
		t.Errorf("boyutsuz ilerleme uyduruldu: %v", p)
	}
	if p := rowProgress(job("a", queue.StateDone, 0, 0)); p != 1 {
		t.Errorf("biten is dolu gorunmeli: %v", p)
	}
	if p := rowProgress(job("a", queue.StateRunning, 300, 200)); p != 1 {
		t.Errorf("tasma 1'e kirpilmali: %v", p)
	}
}

func TestActionForState(t *testing.T) {
	cases := map[queue.State]rowAction{
		queue.StateQueued: actionPause, queue.StateRunning: actionPause,
		queue.StatePaused: actionResume, queue.StateFailed: actionResume, queue.StateStopped: actionResume,
		queue.StateDone: actionNone, queue.StateSkipped: actionNone,
	}
	for st, want := range cases {
		if _, got := actionFor(st); got != want {
			t.Errorf("%s -> %v, %v bekleniyordu", st, got, want)
		}
	}
}

// --- Hiz siniri girdisi ---

func TestParseSpeedLimit(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"", 0, false}, {"0", 0, false}, {"2", 2 << 20, false},
		{"2.5", int64(2.5 * 1024 * 1024), false}, {"2,5", int64(2.5 * 1024 * 1024), false},
		{"abc", 0, true}, {"-1", 0, true},
	}
	for _, c := range cases {
		got, err := parseSpeedLimit(c.in)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("parseSpeedLimit(%q) = %d, %v; %d, err=%v bekleniyordu", c.in, got, err, c.want, c.err)
		}
	}
}

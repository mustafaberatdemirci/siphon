package queue

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/dl"
	snet "github.com/mustafaberatdemirci/siphon/internal/net"
	"github.com/mustafaberatdemirci/siphon/internal/run"
	"github.com/mustafaberatdemirci/siphon/internal/site"
	"github.com/mustafaberatdemirci/siphon/internal/store"
)

// DefaultSegments, kullanıcı ayarlamadıysa dosya başına istenen bağlantı.
// Site tavanı (max_segments) bunu kırpıyor; pixeldrain ve mega'da 1.
const DefaultSegments = 4

// DefaultMaxActive, aynı anda indirilen iş sayısı. Site başına host sınırı
// ayrıca uygulanıyor; bu genel bir tavan.
const DefaultMaxActive = 3

// saveDebounce, kuyruk dosyasının en sık ne kadar yazılacağı. İlerleme
// bildirimleri 250 ms'de bir geliyor; her birinde diske yazmak anlamsız.
const saveDebounce = 500 * time.Millisecond

// Options, motorun kurulumu.
type Options struct {
	ConfigPath string
	// StatePath, kuyruk dosyası; boşsa kalıcılık yok (testler).
	StatePath string
	MaxActive int
	Client    *http.Client
	// Events: Debugf/Errorf/Infof log için kullanılıyor. Item olayları motor
	// tarafından ele alınıyor; buradaki ItemDone gibi alanlar YOK SAYILIR.
	Events run.Events
	// OnChange, bir işin durumu veya ilerlemesi her değiştiğinde çağrılır.
	// Arka plan goroutine'lerinden gelir; arayüz kendi iş parçacığına taşımalı.
	OnChange func(Job)

	// Testler için: verilirse config yüklenmez, bunlar kullanılır.
	Resolvers []site.Resolver
	Configs   []site.SiteConfig
}

// Engine, kuyruğun kendisi.
type Engine struct {
	opt      Options
	client   *http.Client
	throttle *dl.Throttle

	cfgs      []site.SiteConfig
	resolvers []site.Resolver
	workers   map[string]*run.Worker // site adı -> worker

	mu        sync.Mutex
	jobs      []*Job
	byID      map[string]*Job
	items     map[string]site.Item          // runtime: çözülmüş Item (yeniden başlatmada yok)
	active    map[string]context.CancelFunc // çalışan işler
	pauseWant map[string]bool               // iptal edilen iş kullanıcı isteğiyle mi?
	wipeWant  map[string]bool               // kaldırılan iş bitince yarım dosyası silinsin mi?
	ledgers   map[string]*store.Ledger      // outDir -> kayıt
	pausedAll bool
	// pausedByCaptcha: genel duraklatmayı kullanıcı değil, bir captcha
	// koydu. Fark önemli: kullanıcı tek bir işe "devam" dediğinde captcha
	// duraklatması kalkmalı (niyet açık: VPN değiştirdi, tekrar deniyor),
	// kullanıcının kendi "Tümünü duraklat"ı ise korunmalı.
	pausedByCaptcha bool
	closing         bool
	segments        int // kullanıcının istediği bağlantı/dosya
	// holdSince: site -> kotanın dolduğu an. Bir iş başarıyla bitince
	// sitenin bekleyenlerini serbest bırakmak için "bu iş kotadan SONRA mı
	// başladı?" sorusuna cevap; kotadan önce başlamış ve yalnızca bitmesine
	// izin verilmiş bir aktarım kotanın açıldığını kanıtlamaz.
	holdSince map[string]time.Time

	wake chan struct{}

	saveMu    sync.Mutex
	saveTimer *time.Timer
}

// New, config'i yükler, resolver'ları ve varsa kuyruk dosyasını okur.
func New(opt Options) (*Engine, error) {
	if opt.MaxActive <= 0 {
		opt.MaxActive = DefaultMaxActive
	}
	e := &Engine{
		opt:       opt,
		client:    opt.Client,
		throttle:  &dl.Throttle{},
		workers:   map[string]*run.Worker{},
		byID:      map[string]*Job{},
		items:     map[string]site.Item{},
		active:    map[string]context.CancelFunc{},
		pauseWant: map[string]bool{},
		wipeWant:  map[string]bool{},
		ledgers:   map[string]*store.Ledger{},
		wake:      make(chan struct{}, 1),
	}
	if e.client == nil {
		e.client = snet.NewClient()
	}

	if len(opt.Resolvers) > 0 {
		e.cfgs, e.resolvers = opt.Configs, opt.Resolvers
	} else {
		cfgs, resolvers, err := run.Setup(opt.Events, opt.ConfigPath, nil, nil)
		if err != nil {
			return nil, err
		}
		e.cfgs, e.resolvers = cfgs, resolvers
	}
	for i, r := range e.resolvers {
		cfg := e.cfgs[i]
		ev := opt.Events
		ev.Progress = e.onProgress
		w := run.NewWorker(r, cfg, e.client, ev)
		w.Down.Throttle = e.throttle
		e.workers[cfg.Name] = w
	}

	// Varsayılan istek 4 bağlantı/dosya; site tavanları bunu kırpar.
	e.SetSegments(DefaultSegments)

	if opt.StatePath != "" {
		jobs, err := load(opt.StatePath)
		if err != nil {
			// Bozuk kuyruk dosyası uygulamayı açılmaz yapmamalı; boş başla,
			// ama sebebi söyle.
			opt.Events.Errorf("%v — kuyruk boş başlıyor", err)
		}
		for _, j := range jobs {
			e.jobs = append(e.jobs, j)
			e.byID[j.ID] = j
		}
	}
	return e, nil
}

// ---------- Kuyruğa ekleme ----------

// Add, bir URL'i çözer ve item'larını kuyruğa ekler. Çözümleme ağ gerektirir
// ve saniyeler sürebilir; arayüz bunu ayrı bir goroutine'den çağırmalı.
// Aynı dosya aynı klasöre ikinci kez eklenirse çoğalmaz. Eklenen iş sayısını
// döndürür.
func (e *Engine) Add(ctx context.Context, rawURL, outDir string) (int, error) {
	r, idx := run.Pick(e.resolvers, rawURL)
	if r == nil {
		return 0, fmt.Errorf("eşleşen site yok: %s", rawURL)
	}
	siteName := e.cfgs[idx].Name

	added := 0
	itemErrs, err := r.Resolve(ctx, rawURL, func(it site.Item) error {
		j := &Job{
			ID:         jobID(outDir, it.SourcePage, it.Dir, it.Filename),
			Site:       siteName,
			SourcePage: it.SourcePage,
			OutDir:     outDir,
			Dir:        it.Dir,
			Filename:   it.Filename,
			Size:       it.Size,
			Index:      it.Index,
			State:      StateQueued,
			AddedAt:    time.Now(),
		}
		e.mu.Lock()
		if existing, dup := e.byID[j.ID]; dup {
			// Bitmiş bir iş yeniden eklenirse dokunma; duraklamış/hatalı bir
			// iş yeniden eklenirse kuyruğa geri koy. Kullanıcının niyeti bu.
			if existing.State.Resumable() {
				existing.State = StateQueued
				existing.Error = ""
			}
			e.items[j.ID] = it
			snap := *existing
			e.mu.Unlock()
			e.changed(snap)
			return nil
		}
		e.jobs = append(e.jobs, j)
		e.byID[j.ID] = j
		e.items[j.ID] = it
		snap := *j
		e.mu.Unlock()
		added++
		e.changed(snap)
		return nil
	})
	for _, ie := range itemErrs {
		e.opt.Events.Errorf("item: %v", ie)
	}
	e.scheduleSave()
	e.kick()
	if err != nil {
		return added, err
	}
	return added, nil
}

// ---------- Kontroller ----------

// Pause, bir işi durdurur. Çalışıyorsa iptal edilir (.part yerinde kalır),
// sıradaysa duraklatılmış olarak işaretlenir.
func (e *Engine) Pause(id string) {
	e.mu.Lock()
	j, ok := e.byID[id]
	if !ok {
		e.mu.Unlock()
		return
	}
	var snap *Job
	switch j.State {
	case StateRunning:
		e.pauseWant[id] = true
		if cancel := e.active[id]; cancel != nil {
			cancel()
		}
		// Durum, çalışan goroutine bitince Paused'a çekilecek.
	case StateQueued:
		j.State = StatePaused
		s := *j
		snap = &s
	}
	e.mu.Unlock()
	if snap != nil {
		e.changed(*snap)
		e.scheduleSave()
	}
}

// Resume, duraklamış, hatalı veya durdurulmuş bir işi kuyruğa geri koyar.
func (e *Engine) Resume(id string) {
	e.mu.Lock()
	j, ok := e.byID[id]
	if !ok || !j.State.Resumable() {
		e.mu.Unlock()
		return
	}
	j.State = StateQueued
	j.Error = ""
	j.RetryAt = time.Time{}
	// Kullanıcı açıkça "devam" dedi. Duraklatmayı bir captcha koyduysa
	// kaldır; aksi halde iş "sırada" görünür ama hiç başlamaz ve kullanıcı
	// "engellendi" sanır (ölçüldü: VPN değiştirip ▶'ye basınca tam bu oldu).
	if e.pausedAll && e.pausedByCaptcha {
		e.pausedAll = false
		e.pausedByCaptcha = false
	}
	snap := *j
	e.mu.Unlock()
	e.changed(snap)
	e.scheduleSave()
	e.kick()
}

// PausedByCaptcha, genel duraklatmanın bir captcha'dan geldiğini söyler;
// arayüz düğme etiketinde sebebi gösteriyor.
func (e *Engine) PausedByCaptcha() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pausedAll && e.pausedByCaptcha
}

// Remove, işi kuyruktan çıkarır. deleteFiles true ise yarım dosya ve durumu
// da silinir; tamamlanmış dosyaya DOKUNULMAZ.
func (e *Engine) Remove(id string, deleteFiles bool) {
	e.mu.Lock()
	j, ok := e.byID[id]
	if !ok {
		e.mu.Unlock()
		return
	}
	running := false
	if cancel := e.active[id]; cancel != nil {
		// Çalışıyor: dosyayı ŞİMDİ silmek yarış üretir (goroutine hâlâ
		// yazıyor, state dosyasını yeniden oluşturabilir). Silme, goroutine
		// bitince runJob'da yapılıyor.
		e.pauseWant[id] = true
		e.wipeWant[id] = deleteFiles && !j.State.Finished()
		running = true
		cancel()
	}
	delete(e.byID, id)
	delete(e.items, id)
	for i, x := range e.jobs {
		if x.ID == id {
			e.jobs = append(e.jobs[:i], e.jobs[i+1:]...)
			break
		}
	}
	path := j.Path
	finished := j.State.Finished()
	e.mu.Unlock()

	if !running && deleteFiles && path != "" && !finished {
		wipePartial(path)
	}
	e.scheduleSave()
	e.kick()
}

// wipePartial, yarım dosyayı ve durumunu siler; tamamlanmış dosyaya dokunmaz.
func wipePartial(path string) {
	_ = os.Remove(path + ".part")
	_ = os.Remove(path + ".part.state")
}

// PauseAll, yeni iş başlatmayı keser ve çalışanları duraklatır.
func (e *Engine) PauseAll() {
	e.mu.Lock()
	e.pausedAll = true
	e.pausedByCaptcha = false
	ids := make([]string, 0, len(e.active))
	for id := range e.active {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	for _, id := range ids {
		e.Pause(id)
	}
}

// ResumeAll, duraklamış tüm işleri kuyruğa geri koyar ve başlatmayı açar.
func (e *Engine) ResumeAll() {
	e.mu.Lock()
	e.pausedAll = false
	e.pausedByCaptcha = false
	var snaps []Job
	for _, j := range e.jobs {
		if j.State == StatePaused || j.State == StateWaiting {
			j.State = StateQueued
			j.RetryAt = time.Time{}
			j.Error = ""
			snaps = append(snaps, *j)
		}
	}
	e.mu.Unlock()
	for _, s := range snaps {
		e.changed(s)
	}
	e.scheduleSave()
	e.kick()
}

// ClearFinished, biten ve atlanan işleri listeden kaldırır.
func (e *Engine) ClearFinished() {
	e.mu.Lock()
	kept := e.jobs[:0]
	for _, j := range e.jobs {
		if j.State.Finished() {
			delete(e.byID, j.ID)
			delete(e.items, j.ID)
			continue
		}
		kept = append(kept, j)
	}
	e.jobs = kept
	e.mu.Unlock()
	e.scheduleSave()
}

// SetSegments, dosya başına istenen bağlantı sayısını ayarlar. Etkin değer
// site tavanını (max_segments) aşamaz; 1 parçalı indirmeyi kapatır. Süren
// indirmeler etkilenmez, sonraki başlayanlar yeni değeri kullanır.
func (e *Engine) SetSegments(n int) {
	if n < 1 {
		n = 1
	}
	e.mu.Lock()
	e.segments = n
	e.mu.Unlock()
	for _, w := range e.workers {
		eff := n
		if w.Cfg.MaxSegments > 0 && eff > w.Cfg.MaxSegments {
			eff = w.Cfg.MaxSegments
		}
		w.Down.Segments = eff
	}
}

// Segments, istenen bağlantı sayısı (site tavanları uygulanmadan).
func (e *Engine) Segments() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.segments
}

// SetSpeedLimit, toplam bayt/saniye sınırını ayarlar; 0 kaldırır.
func (e *Engine) SetSpeedLimit(bytesPerSec int64) { e.throttle.SetRate(bytesPerSec) }

// SpeedLimit, geçerli sınırı döndürür.
func (e *Engine) SpeedLimit() int64 { return e.throttle.Rate() }

// Paused, genel duraklatmanın açık olup olmadığı.
func (e *Engine) Paused() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pausedAll
}

// Jobs, kuyruğun anlık kopyasını ekleme sırasıyla döndürür.
func (e *Engine) Jobs() []Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Job, len(e.jobs))
	for i, j := range e.jobs {
		out[i] = *j
	}
	return out
}

// ---------- Zamanlayıcı ----------

// Run, zamanlayıcıyı ctx bitene kadar sürer. Çıkarken çalışan işleri iptal
// eder; onlar kuyruk dosyasına "queued" olarak yazılır ve bir sonraki açılışta
// kendiliğinden devam eder.
func (e *Engine) Run(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		e.releaseDue(time.Now())
		e.dispatch(ctx)
		// Bekleyen iş varsa en yakın RetryAt'te uyan.
		if next, ok := e.nextRetry(); ok {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(time.Until(next) + 50*time.Millisecond)
		}
		select {
		case <-ctx.Done():
			e.shutdown()
			return
		case <-e.wake:
		case <-timer.C:
		}
	}
}

// DefaultQuotaWait, site sıfırlanma süresini söylemezse yeniden deneme aralığı.
const DefaultQuotaWait = 10 * time.Minute

// nextRetry, bekleyen işlerin en erken RetryAt'i.
func (e *Engine) nextRetry() (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var next time.Time
	for _, j := range e.jobs {
		if j.State == StateWaiting && (next.IsZero() || j.RetryAt.Before(next)) {
			next = j.RetryAt
		}
	}
	return next, !next.IsZero()
}

// releaseDue, süresi dolan bekleyen işleri kuyruğa geri koyar.
func (e *Engine) releaseDue(now time.Time) {
	e.mu.Lock()
	var snaps []Job
	for _, j := range e.jobs {
		if j.State == StateWaiting && !j.RetryAt.After(now) {
			j.State = StateQueued
			j.RetryAt = time.Time{}
			j.Error = ""
			delete(e.holdSince, j.Site)
			snaps = append(snaps, *j)
		}
	}
	e.mu.Unlock()
	for _, s := range snaps {
		e.changed(s)
	}
	if len(snaps) > 0 {
		e.scheduleSave()
	}
}

// enterQuotaWait, kilit altında: kota dolduğunda işi ve aynı sitenin sıradaki
// işlerini beklemeye alır. Dönen anlık görüntüler kilit dışında yayımlanır.
//
// Aynı sitenin sıradakileri de bekliyor: kota IP başına ve site geneli;
// her biri sırayla başlayıp aynı 509'u alacaktı. Çalışanlara DOKUNULMUYOR:
// mega başlamış aktarımı kesmiyor, bitmesine izin vermek kazanç.
//
// Çözülmüş Item atılıyor: mega'nın indirme adresi onu isteyen IP'ye bağlı.
// Kullanıcı VPN değiştirip ▶ dediğinde taze bir "g" gerekir; eski adres
// yalnızca bir 403 turu harcatırdı.
func (e *Engine) enterQuotaWait(j *Job, wait time.Duration, msg string) []Job {
	if wait <= 0 {
		wait = DefaultQuotaWait
	}
	now := time.Now()
	until := now.Add(wait)
	if e.holdSince == nil {
		e.holdSince = map[string]time.Time{}
	}
	e.holdSince[j.Site] = now
	var snaps []Job
	for _, o := range e.jobs {
		if o.Site != j.Site {
			continue
		}
		if o != j && o.State != StateQueued {
			continue
		}
		o.State = StateWaiting
		o.RetryAt = until
		o.Error = msg
		delete(e.items, o.ID)
		snaps = append(snaps, *o)
	}
	return snaps
}

// releaseSite, kilit altında: sitenin bekleyen işlerini kuyruğa döndürür.
// Kotadan SONRA başlamış bir iş başarıyla indiyse (süre dolmuş ya da
// kullanıcı IP değiştirip ▶ demiş) diğerlerinin beklemesi için sebep kalmadı.
func (e *Engine) releaseSite(siteName string, started time.Time) []Job {
	if since, held := e.holdSince[siteName]; !held || started.Before(since) {
		return nil
	}
	delete(e.holdSince, siteName)
	var snaps []Job
	for _, o := range e.jobs {
		if o.Site == siteName && o.State == StateWaiting {
			o.State = StateQueued
			o.RetryAt = time.Time{}
			o.Error = ""
			snaps = append(snaps, *o)
		}
	}
	return snaps
}

func (e *Engine) kick() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// dispatch, boş yuva olduğu sürece sıradaki işleri başlatır.
func (e *Engine) dispatch(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pausedAll || e.closing {
		return
	}
	for _, j := range e.jobs {
		if len(e.active) >= e.opt.MaxActive {
			return
		}
		if j.State != StateQueued {
			continue
		}
		w, ok := e.workers[j.Site]
		if !ok {
			j.State = StateFailed
			j.Error = "site tanımı yok: " + j.Site
			continue
		}
		j.State = StateRunning
		j.Error = ""
		jctx, cancel := context.WithCancel(ctx)
		e.active[j.ID] = cancel
		item, haveItem := e.items[j.ID]
		snap := *j
		go e.runJob(jctx, w, snap, item, haveItem)
	}
}

// runJob, tek bir işi baştan sona sürer ve sonucu kuyruğa işler.
func (e *Engine) runJob(ctx context.Context, w *run.Worker, j Job, it site.Item, haveItem bool) {
	started := time.Now()
	e.changed(j)
	e.scheduleSave()

	outcome := e.execute(ctx, w, &j, it, haveItem)

	e.mu.Lock()
	live, still := e.byID[j.ID]
	delete(e.active, j.ID)
	wantedPause := e.pauseWant[j.ID]
	delete(e.pauseWant, j.ID)
	wipe := e.wipeWant[j.ID]
	delete(e.wipeWant, j.ID)
	closing := e.closing
	var extra []Job // aynı sitenin etkilenen diğer işleri
	if still {
		live.Path, live.Filename = j.Path, j.Filename
		if j.Size > 0 {
			live.Size = j.Size
		}
		switch outcome.Kind {
		case run.OutcomeDone:
			live.State = StateDone
			live.Done = outcome.Result.Size
			live.Path = outcome.Result.Path
			live.Filename = filepath.Base(outcome.Result.Path)
			live.FinishedAt = time.Now()
			live.Error = ""
			extra = e.releaseSite(live.Site, started)
		case run.OutcomeSkipped:
			live.State = StateSkipped
			live.Done = outcome.Entry.Size
			live.Size = outcome.Entry.Size
			live.Path = filepath.Join(j.OutDir, outcome.Entry.Path)
			live.FinishedAt = time.Now()
		case run.OutcomeStopped:
			live.State = StateStopped
			live.Error = outcome.Err.Error()
		case run.OutcomeCanceled:
			switch {
			case closing:
				// Uygulama kapanıyor: bir sonraki açılışta kendiliğinden
				// devam etsin.
				live.State = StateQueued
			case wantedPause:
				live.State = StatePaused
			default:
				live.State = StateQueued
			}
		default:
			if q, ok := site.QuotaOf(outcome.Err); ok {
				extra = e.enterQuotaWait(live, q.Wait, outcome.Err.Error())
				break
			}
			live.State = StateFailed
			if outcome.Err != nil {
				live.Error = outcome.Err.Error()
			}
		}
		if outcome.Kind == run.OutcomeStopped {
			// Captcha tüm siteyi etkiler; başka işleri boşuna başlatma.
			// Kullanıcı duraklatması zaten açıksa ona dokunma.
			if !e.pausedAll {
				e.pausedAll = true
				e.pausedByCaptcha = true
			}
		}
	}
	var snap Job
	if still {
		snap = *live
	}
	e.mu.Unlock()

	if !still && wipe && j.Path != "" {
		wipePartial(j.Path)
	}
	if still {
		e.changed(snap)
	}
	for _, x := range extra {
		if x.ID != snap.ID {
			e.changed(x)
		}
	}
	e.scheduleSave()
	e.kick()
}

// execute, işi Worker'a verir; Item yoksa SourcePage'den yeniden çözer.
func (e *Engine) execute(ctx context.Context, w *run.Worker, j *Job, it site.Item, haveItem bool) run.Outcome {
	if !haveItem {
		fresh, err := w.Resolver.ResolveOne(ctx, j.SourcePage)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return run.Outcome{Kind: run.OutcomeCanceled, Err: err}
			}
			return run.Outcome{Kind: run.OutcomeFailed, Err: fmt.Errorf("yeniden çözümleme: %w", err)}
		}
		it = fresh
		it.Dir, it.Index = j.Dir, j.Index
		if j.Filename != "" {
			it.Filename = j.Filename
		}
	}

	// Nihai ad indirmeden ÖNCE belirleniyor ve kuyruğa yazılıyor: uygulama
	// kapanıp açılınca aynı adla devam etmenin garantisi bu.
	if path, err := w.Down.Plan(j.OutDir, it); err == nil {
		j.Path = path
		j.Filename = filepath.Base(path)
		it.Filename = j.Filename
	}
	if it.Size > 0 {
		j.Size = it.Size
	}

	ledger, err := e.ledgerFor(j.OutDir)
	if err != nil {
		return run.Outcome{Kind: run.OutcomeFailed, Err: err}
	}
	return w.DownloadItem(ctx, j.OutDir, ledger, it, e.opt.Events)
}

// ledgerFor, çıktı klasörünün kaydını açar veya önbellekten verir.
func (e *Engine) ledgerFor(outDir string) (*store.Ledger, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if l, ok := e.ledgers[outDir]; ok {
		return l, nil
	}
	l, err := store.Open(outDir)
	if err != nil {
		return nil, err
	}
	e.ledgers[outDir] = l
	return l, nil
}

// onProgress, indiriciden gelen ilerlemeyi ilgili işe yazar.
func (e *Engine) onProgress(it site.Item, done, total int64) {
	e.mu.Lock()
	var snap *Job
	for _, j := range e.jobs {
		if j.State == StateRunning && j.SourcePage == it.SourcePage && j.Dir == it.Dir {
			j.Done = done
			if total > 0 {
				j.Size = total
			}
			s := *j
			snap = &s
			break
		}
	}
	e.mu.Unlock()
	if snap != nil {
		e.changed(*snap)
	}
}

func (e *Engine) changed(j Job) {
	if e.opt.OnChange != nil {
		e.opt.OnChange(j)
	}
}

// ---------- Kalıcılık ve kapanış ----------

func (e *Engine) scheduleSave() {
	if e.opt.StatePath == "" {
		return
	}
	e.saveMu.Lock()
	defer e.saveMu.Unlock()
	if e.saveTimer != nil {
		return
	}
	e.saveTimer = time.AfterFunc(saveDebounce, func() {
		e.saveMu.Lock()
		e.saveTimer = nil
		e.saveMu.Unlock()
		e.flush()
	})
}

// flush, kuyruğu hemen yazar.
func (e *Engine) flush() {
	if e.opt.StatePath == "" {
		return
	}
	e.mu.Lock()
	jobs := make([]*Job, len(e.jobs))
	for i, j := range e.jobs {
		c := *j
		jobs[i] = &c
	}
	e.mu.Unlock()
	sort.SliceStable(jobs, func(a, b int) bool { return jobs[a].AddedAt.Before(jobs[b].AddedAt) })
	if err := save(e.opt.StatePath, jobs); err != nil {
		e.opt.Events.Errorf("kuyruk yazılamadı: %v", err)
	}
}

// shutdown, çalışan işleri iptal eder ve bitmelerini bekler.
func (e *Engine) shutdown() {
	e.mu.Lock()
	e.closing = true
	cancels := make([]context.CancelFunc, 0, len(e.active))
	for _, c := range e.active {
		cancels = append(cancels, c)
	}
	e.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	// İşler bitince runJob'lar active'den düşer; kısa bir süre bekle ki
	// son durum diske yazılsın.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		n := len(e.active)
		e.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.saveMu.Lock()
	if e.saveTimer != nil {
		e.saveTimer.Stop()
		e.saveTimer = nil
	}
	e.saveMu.Unlock()
	e.flush()

	e.mu.Lock()
	for _, l := range e.ledgers {
		_ = l.Close()
	}
	e.ledgers = map[string]*store.Ledger{}
	e.mu.Unlock()
}

package dl

import (
	"context"
	"testing"
	"time"
)

// 72 KB'lik payload 144 KB/s sinirla en az ~0.5 sn surmeli (ilk saniyelik
// patlama kovasi bos basladigi icin tam sure, kisaltma yok).
func TestThrottleSlowsDownload(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	th := &Throttle{}
	th.SetRate(int64(len(payload)) * 2) // 2 saniyede payload -> ~0.5 sn
	d := &Downloader{Client: srv.Client(), Throttle: th}

	start := time.Now()
	if _, err := d.Download(context.Background(), out, testItem(srv.URL+"/veri.bin", "veri.bin")); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 350*time.Millisecond {
		t.Fatalf("sinirli indirme %v surdu; sinir uygulanmamis", el)
	}
}

func TestThrottleZeroIsUnlimited(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client(), Throttle: &Throttle{}}
	start := time.Now()
	if _, err := d.Download(context.Background(), out, testItem(srv.URL+"/veri.bin", "veri.bin")); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("sinirsiz indirme %v surdu", el)
	}
}

// Beklerken iptal edilirse hata context'ten gelmeli ve hemen donmeli.
func TestThrottleWaitHonorsCancel(t *testing.T) {
	th := &Throttle{}
	th.SetRate(1) // 1 bayt/sn: 1 MB icin bir milyon saniye
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := th.Wait(ctx, 1<<20)
	if err == nil {
		t.Fatal("iptal edilen bekleme hata vermedi")
	}
	if time.Since(start) > time.Second {
		t.Fatal("iptal beklemeyi kesmedi")
	}
}

// Sinir varken okuma parcasi kuculmeli ki ilerleme cubugu takilmis gorunmesin.
func TestThrottleChunkShrinks(t *testing.T) {
	th := &Throttle{}
	if got := th.chunkFor(256 << 10); got != 256<<10 {
		t.Errorf("sinirsizken parca kuculdu: %d", got)
	}
	th.SetRate(100 << 10) // 100 KB/s -> ceyrek saniye = 25 KB
	if got := th.chunkFor(256 << 10); got != 25<<10 {
		t.Errorf("100 KB/s'de parca = %d, 25 KB bekleniyordu", got)
	}
	th.SetRate(1 << 10) // 1 KB/s -> taban 16 KB
	if got := th.chunkFor(256 << 10); got != 16<<10 {
		t.Errorf("cok dusuk sinirda taban 16 KB olmali: %d", got)
	}
	var nilTh *Throttle
	if got := nilTh.chunkFor(100); got != 100 {
		t.Errorf("nil Throttle parcayi degistirdi: %d", got)
	}
	if err := nilTh.Wait(context.Background(), 10); err != nil {
		t.Errorf("nil Throttle Wait hata verdi: %v", err)
	}
}

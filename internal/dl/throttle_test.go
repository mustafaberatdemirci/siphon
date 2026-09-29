package dl

import (
	"context"
	"testing"
	"time"
)

// A 72 KB payload at a 144 KB/s limit must take at least ~0.5 s (the burst
// bucket starts empty, so the full duration applies, no shortcut).
func TestThrottleSlowsDownload(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	th := &Throttle{}
	th.SetRate(int64(len(payload)) * 2) // payload in 2 seconds -> ~0.5 s
	d := &Downloader{Client: srv.Client(), Throttle: th}

	start := time.Now()
	if _, err := d.Download(context.Background(), out, testItem(srv.URL+"/data.bin", "data.bin")); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 350*time.Millisecond {
		t.Fatalf("the limited download took %v; the limit was not applied", el)
	}
}

func TestThrottleZeroIsUnlimited(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client(), Throttle: &Throttle{}}
	start := time.Now()
	if _, err := d.Download(context.Background(), out, testItem(srv.URL+"/data.bin", "data.bin")); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("the unlimited download took %v", el)
	}
}

// If canceled while waiting, the error must come from the context and return right away.
func TestThrottleWaitHonorsCancel(t *testing.T) {
	th := &Throttle{}
	th.SetRate(1) // 1 byte/s: a million seconds for 1 MB
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := th.Wait(ctx, 1<<20)
	if err == nil {
		t.Fatal("a canceled wait returned no error")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation did not cut the wait")
	}
}

// When the limit is smaller than the read chunk (8 KB/s, 16 KB chunk) the
// wait MUST END. Because the bucket cap equals the limit, the tokens used to
// never reach 16 KB and the download stalled forever; only cancellation got
// it out.
func TestThrottleBelowChunkDoesNotStall(t *testing.T) {
	th := &Throttle{}
	th.SetRate(8 << 10)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := th.Wait(ctx, 16<<10); err != nil {
		t.Fatalf("a chunk twice the limit failed after %v: %v", time.Since(start), err)
	}
	if el := time.Since(start); el < 1500*time.Millisecond {
		t.Errorf("16 KB at 8 KB/s took %v; ~2 s was expected", el)
	}
}

// With a limit the read chunk must shrink so the progress bar doesn't look stuck.
func TestThrottleChunkShrinks(t *testing.T) {
	th := &Throttle{}
	if got := th.chunkFor(256 << 10); got != 256<<10 {
		t.Errorf("chunk shrank without a limit: %d", got)
	}
	th.SetRate(100 << 10) // 100 KB/s -> a quarter second = 25 KB
	if got := th.chunkFor(256 << 10); got != 25<<10 {
		t.Errorf("chunk at 100 KB/s = %d, want 25 KB", got)
	}
	th.SetRate(1 << 10) // 1 KB/s -> floor of 16 KB
	if got := th.chunkFor(256 << 10); got != 16<<10 {
		t.Errorf("at a very low limit the floor must be 16 KB: %d", got)
	}
	var nilTh *Throttle
	if got := nilTh.chunkFor(100); got != 100 {
		t.Errorf("a nil Throttle changed the chunk: %d", got)
	}
	if err := nilTh.Wait(context.Background(), 10); err != nil {
		t.Errorf("a nil Throttle's Wait returned an error: %v", err)
	}
}

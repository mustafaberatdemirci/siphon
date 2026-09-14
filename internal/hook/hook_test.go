package hook

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// "echo" hem cmd'de hem sh'de var; cikti toplanmali.
func TestRunCapturesOutput(t *testing.T) {
	out, err := Run(context.Background(), "echo merhaba kota", 0)
	if err != nil {
		t.Fatalf("Run: %v (%q)", err, out)
	}
	if !strings.Contains(out, "merhaba kota") {
		t.Errorf("cikti = %q", out)
	}
}

// Bosluklu yol + argumanlar: Windows'ta /S /C tirnaklamasinin, sh'de -c'nin
// satiri bozmadigi. Komut, bosluklu bir dizindeki dosyaya yazıyor.
func TestRunKeepsQuotedPathsIntact(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bosluklu dizin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "iz.txt")
	if _, err := Run(context.Background(), `echo calisti> "`+marker+`"`, 0); err != nil {
		t.Fatalf("Run: %v", err)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("komut dosyayi yazmadi: %v", err)
	}
	if !strings.Contains(string(b), "calisti") {
		t.Errorf("icerik = %q", b)
	}
}

// Sifirdan farkli cikis kodu hata olmali; cikti yine de donmeli.
func TestRunReportsFailure(t *testing.T) {
	out, err := Run(context.Background(), "echo sorun var&& exit 3", 0)
	if err == nil {
		t.Fatal("basarisiz komut hata dondurmedi")
	}
	if !exitError(err) {
		t.Errorf("hata turu %T: %v", err, err)
	}
	if !strings.Contains(out, "sorun var") {
		t.Errorf("basarisiz komutun ciktisi kayboldu: %q", out)
	}
}

// Asili kalan komut zaman asiminda oldurulmeli.
func TestRunKillsOnTimeout(t *testing.T) {
	sleep := "sleep 5"
	if isWindows() {
		sleep = "ping -n 6 127.0.0.1 >nul"
	}
	start := time.Now()
	_, err := Run(context.Background(), sleep, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "bitmedi") {
		t.Fatalf("zaman asimi hatasi bekleniyordu: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("komut oldurulmedi, %s beklendi", time.Since(start))
	}
}

func TestRunRejectsEmpty(t *testing.T) {
	if _, err := Run(context.Background(), "   ", 0); err == nil {
		t.Fatal("bos komut kabul edildi")
	}
}

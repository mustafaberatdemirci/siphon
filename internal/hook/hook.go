// Package hook, kullanıcının tanımladığı bir komut satırını çalıştırır.
//
// İlk kullanım: MEGA kotası dolunca VPN'i değiştiren bir betik (MegaBasterd'in
// "509'da komut çalıştır" özelliği). Komut kullanıcının kendi makinesinde,
// kendi yetkisiyle, kendi yazdığı satır; burada yapılan yalnızca onu işletim
// sisteminin kabuğuna vermek, çıktısını toplamak ve süresini sınırlamak.
package hook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout: VPN değiştiren bir komutun makul üst sınırı. Aşarsa
// öldürülür; asılı kalan bir betik kuyruğu sonsuza dek "komut çalışıyor"da
// bırakmasın.
const DefaultTimeout = 2 * time.Minute

// outputTail, kullanıcıya gösterilen çıktı uzunluğu.
const outputTail = 400

// Run, satırı kabukta çalıştırır (Windows: cmd /S /C, diğerleri: sh -c) ve
// birleşik çıktının son kısmını döndürür. Sıfırdan farklı çıkış kodu hata.
// timeout <= 0 ise DefaultTimeout.
func Run(ctx context.Context, line string, timeout time.Duration) (string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", errors.New("komut boş")
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := shellCommand(ctx, line)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// Zaman aşımında kabuk öldürülür ama kabuğun başlattığı alt süreç (ör.
	// VPN istemcisi) çıktı borusunu açık tutabilir; WaitDelay olmadan Run
	// o boru kapanana dek asılı kalırdı.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	tail := tailOf(out.String())
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return tail, fmt.Errorf("komut %s içinde bitmedi, öldürüldü", timeout)
		}
		return tail, err
	}
	return tail, nil
}

func tailOf(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= outputTail {
		return s
	}
	return "…" + string(r[len(r)-outputTail:])
}

// exitError, testlerin kabuk farkından bağımsız "başarısız çıktı" kontrolü için.
func exitError(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
}

package main

import (
	"syscall"
	"unsafe"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver"
	"golang.org/x/sys/windows/registry"
)

// requestAttention, kullanıcı başka bir penceredeyken "buraya bak" der:
// görev çubuğu düğmesi pencere öne gelene kadar yanıp söner ve kısa bir
// uyarı sesi çalar.
//
// ÖLÇÜLDÜ: bu makinede Windows bildirimleri hesap genelinde kapalı
// (ToastEnabled=0); PowerShell'in kendi kimliğiyle gönderilen deneme bile
// görünmedi. Kota uyarısı o ayara bağımlı kalamaz; FlashWindowEx ve
// MessageBeep bildirim ayarından etkilenmez.
func requestAttention(w fyne.Window) {
	nw, ok := w.(driver.NativeWindow)
	if !ok {
		w.RequestFocus()
		return
	}
	nw.RunNative(func(ctx any) {
		wc, ok := ctx.(driver.WindowsWindowContext)
		if !ok || wc.HWND == 0 {
			return
		}
		flashWindow(wc.HWND)
	})
	messageBeep()
}

var (
	user32            = syscall.NewLazyDLL("user32.dll")
	procFlashWindow   = user32.NewProc("FlashWindowEx")
	procMessageBeep   = user32.NewProc("MessageBeep")
	procGetForeground = user32.NewProc("GetForegroundWindow")
)

const (
	flashwAll       = 0x3 // başlık + görev çubuğu
	flashwTimerNoFG = 0xC // pencere öne gelene kadar
	mbIconWarning   = 0x30
)

type flashInfo struct {
	size    uint32
	hwnd    uintptr
	flags   uint32
	count   uint32
	timeout uint32
}

func flashWindow(hwnd uintptr) {
	// Pencere zaten öndeyse yanıp sönmeye gerek yok; şerit ve ses yeter.
	if fg, _, _ := procGetForeground.Call(); fg == hwnd {
		return
	}
	fi := flashInfo{hwnd: hwnd, flags: flashwAll | flashwTimerNoFG}
	fi.size = uint32(unsafe.Sizeof(fi))
	_, _, _ = procFlashWindow.Call(uintptr(unsafe.Pointer(&fi)))
}

func messageBeep() {
	_, _, _ = procMessageBeep.Call(mbIconWarning)
}

// registerToastIdentity, Windows'un masaüstü uygulamalarından beklediği
// AppUserModelId kaydını HKCU'ya yazar (yönetici gerekmez). Bildirimler
// açıksa toast "Siphon" adıyla görünür; kayıt olmadan Windows toast'ı
// sessizce düşürebiliyor. Thunderbird, Acrobat ve TreeSize aynı anahtarı
// kullanıyor (ölçüldü). Bildirimler kapalıysa etkisi yok, zararı da yok.
func registerToastIdentity(appID string) {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Classes\AppUserModelId\`+appID, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.SetStringValue("DisplayName", "Siphon")
}

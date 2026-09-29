package main

import (
	"syscall"
	"unsafe"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver"
	"golang.org/x/sys/windows/registry"
)

// requestAttention says "look here" while the user is in another window: the
// taskbar button flashes until the window comes to the front and a short
// warning sound plays.
//
// MEASURED: on this machine Windows notifications are turned off
// account-wide (ToastEnabled=0); even a test sent with PowerShell's own
// identity didn't show up. The quota warning can't depend on that setting;
// FlashWindowEx and MessageBeep aren't affected by notification settings.
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
	flashwAll       = 0x3 // caption + taskbar
	flashwTimerNoFG = 0xC // until the window comes to the front
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
	// If the window is already in front there's no need to flash; the banner and the sound are enough.
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

// registerToastIdentity writes the AppUserModelId entry Windows expects from
// desktop apps into HKCU (no admin needed). If notifications are on, the
// toast shows up under the name "Siphon"; without the entry Windows may
// silently drop the toast. Thunderbird, Acrobat and TreeSize use the same key
// (measured). If notifications are off it has no effect, and no harm either.
func registerToastIdentity(appID string) {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Classes\AppUserModelId\`+appID, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.SetStringValue("DisplayName", "Siphon")
}

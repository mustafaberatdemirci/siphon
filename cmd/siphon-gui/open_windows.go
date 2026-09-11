//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// explorerPath, explorer.exe'yi TAM YOLLA döndürür: çıplak ad %PATH% üzerinden
// çözülür ve yazılabilir bir PATH dizinine konan explorer.exe bu düğmeyle
// çalışırdı.
func explorerPath() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		return filepath.Join(root, "explorer.exe")
	}
	return "explorer"
}

// openInExplorer, bir klasörü açar ya da bir dosyayı klasöründe SEÇİLİ açar.
//
// ÖLÇÜLDÜ (2026-09-11): Go'nun exec paketi boşluk içeren argümanı bütünüyle
// tırnaklar ve komut satırı `explorer "/select,E:\Casting curvy\x.mp4"` olur.
// explorer bunu tanımaz ve sessizce Belgeler'i açar. Çalışan biçim yalnızca
// yolun tırnaklanması: `explorer /select,"E:\Casting curvy\x.mp4"`. Go bu
// biçimi üretemediği için komut satırı burada elle kuruluyor.
func openInExplorer(path string, selectFile bool) error {
	exe := explorerPath()
	cmd := exec.Command(exe)
	if selectFile {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			CmdLine: fmt.Sprintf(`"%s" /select,"%s"`, exe, path),
		}
	} else {
		cmd.Args = []string{exe, path}
	}
	// explorer.exe BAŞARIDA BİLE 1 döndürüyor; çıkış kodu kontrol edilmiyor,
	// yalnızca başlatma hatası anlamlı.
	return cmd.Start()
}

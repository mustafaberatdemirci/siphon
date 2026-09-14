package hook

import (
	"context"
	"os/exec"
	"syscall"
)

// shellCommand, satırı cmd.exe'ye verir.
//
// Go'nun argüman kaçışlaması cmd için yanlış: "/C" ve satırı ayrı argüman
// verince satır boşluk içeriyorsa tırnaklanır ve cmd onu tek bir program adı
// sanır. Bu yüzden komut satırı ham yazılıyor. /S ile cmd ilk ve son tırnağı
// atıp arasını olduğu gibi işler; böylece kullanıcının satırı boşluklu yol
// ve argüman içerse de bozulmaz: cmd /S /C "  "C:\vpn\degistir.bat" tr  ".
//
// HideWindow: GUI konsolsuz (windowsgui) derlendiği için her kota dolduğunda
// siyah bir pencere fırlamasın.
func shellCommand(ctx context.Context, line string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:    `cmd.exe /S /C "` + line + `"`,
		HideWindow: true,
	}
	return cmd
}

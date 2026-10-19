package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procSetSuspendState = syscall.NewLazyDLL("powrprof.dll").NewProc("SetSuspendState")

// sleepComputer puts the computer to sleep (not hibernation: the first
// argument says so; rundll32's way of calling it hibernates where
// hibernation is on). Windows asks for the shutdown privilege, which every
// user has but disabled; it is enabled for this process first.
func sleepComputer() error {
	if err := enableShutdownPrivilege(); err != nil {
		return err
	}
	if r, _, err := procSetSuspendState.Call(0, 0, 0); r == 0 {
		return err
	}
	return nil
}

// shutdownComputer shuts Windows down the usual way: programs are asked to
// close (Siphon saves its queue then).
func shutdownComputer() error {
	exe := filepath.Join(os.Getenv("SystemRoot"), "System32", "shutdown.exe")
	cmd := exec.Command(exe, "/s", "/t", "0")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run()
}

func enableShutdownPrivilege() error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()
	var luid windows.LUID
	name, err := windows.UTF16PtrFromString("SeShutdownPrivilege")
	if err != nil {
		return err
	}
	if err := windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
		return err
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	return windows.AdjustTokenPrivileges(token, false, &tp, uint32(unsafe.Sizeof(tp)), nil, nil)
}

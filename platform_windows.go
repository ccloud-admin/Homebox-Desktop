//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// protectSecret encrypts data with DPAPI so only this Windows user on this
// machine can read it back.
func protectSecret(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

func unprotectSecret(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

// The engine runs without a console window.
func hideChildWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}

var killJob windows.Handle

// bindChildToApp puts the engine in a Windows job object that is closed when
// Homebox Desktop exits for any reason (including a crash), so the engine is
// never left running in the background holding the database open.
func bindChildToApp(pid int) {
	if killJob == 0 {
		job, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			return
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
			_ = windows.CloseHandle(job)
			return
		}
		killJob = job
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.AssignProcessToJobObject(killJob, h)
}

func openInFileManager(path string) {
	cmd := exec.Command("explorer.exe", path)
	_ = cmd.Start()
}

// launchInstaller starts a downloaded installer. ShellExecute is used so
// Windows can show its usual "allow this app to make changes" prompt.
func launchInstaller(path string) error {
	verb, _ := windows.UTF16PtrFromString("open")
	file, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL)
}

var (
	shell32              = windows.NewLazySystemDLL("shell32.dll")
	procSHFileOperationW = shell32.NewProc("SHFileOperationW")
)

// shFileOpStruct mirrors SHFILEOPSTRUCTW (64-bit layout).
type shFileOpStruct struct {
	hwnd                  uintptr
	wFunc                 uint32
	pFrom                 *uint16
	pTo                   *uint16
	fFlags                uint16
	fAnyOperationsAborted int32
	hNameMappings         uintptr
	lpszProgressTitle     *uint16
}

// moveToRecycleBin sends files to the Recycle Bin so a mistaken delete can
// be undone. Drives without a Recycle Bin (some USB and network drives)
// delete permanently. Anything left behind is removed directly.
func moveToRecycleBin(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	var from []uint16
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		u, err := windows.UTF16FromString(abs)
		if err != nil {
			return err
		}
		from = append(from, u...) // includes the terminating zero
	}
	from = append(from, 0) // list ends with a second zero
	const (
		foDelete          = 0x3
		fofSilent         = 0x4
		fofNoConfirmation = 0x10
		fofAllowUndo      = 0x40
		fofNoErrorUI      = 0x400
	)
	op := shFileOpStruct{wFunc: foDelete, pFrom: &from[0], fFlags: fofAllowUndo | fofNoConfirmation | fofSilent | fofNoErrorUI}
	runtime.LockOSThread()
	_, _, _ = procSHFileOperationW.Call(uintptr(unsafe.Pointer(&op)))
	runtime.UnlockOSThread()
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			if err := os.Remove(p); err != nil {
				return err
			}
		}
	}
	return nil
}

// Compile-time check that shFileOpStruct is exactly the 56 bytes Windows
// expects on 64-bit systems.
var (
	_ [56 - unsafe.Sizeof(shFileOpStruct{})]byte
	_ [unsafe.Sizeof(shFileOpStruct{}) - 56]byte
)

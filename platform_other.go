//go:build !windows

package main

import (
	"os"
	"os/exec"
	"runtime"
)

// Non-Windows builds exist only so the app logic can be developed and tested
// on other systems. Secrets are stored as-is (the settings file is 0600).
func protectSecret(data []byte) ([]byte, error)   { return append([]byte(nil), data...), nil }
func unprotectSecret(data []byte) ([]byte, error) { return append([]byte(nil), data...), nil }

func hideChildWindow(cmd *exec.Cmd) {}

func bindChildToApp(pid int) {}

func openInFileManager(path string) {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	_ = exec.Command(opener, path).Start()
}

func launchInstaller(path string) error {
	return exec.Command(path).Start()
}

// moveToRecycleBin deletes files. Only Windows has a Recycle Bin to use.
func moveToRecycleBin(paths []string) error {
	for _, p := range paths {
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	return nil
}

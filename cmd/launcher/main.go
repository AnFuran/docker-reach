// Launcher creates docker-reach-up.exe and docker-reach-down.exe.
// Built with -H windowsgui so they show no console window.
// They use ShellExecute "runas" to request elevation (UAC prompt)
// and run docker-reach.exe hidden in the background.
package main

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	shell32      = syscall.NewLazyDLL("shell32.dll")
	shellExecute = shell32.NewProc("ShellExecuteW")
)

func main() {
	action := "up"
	if len(os.Args) > 1 {
		action = os.Args[1]
	}

	// Determine which action based on the exe name.
	exeName := filepath.Base(os.Args[0])
	if exeName == "docker-reach-down.exe" || exeName == "docker-reach-down" {
		action = "down"
	}

	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	target := filepath.Join(dir, "docker-reach.exe")

	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(target)
	args, _ := syscall.UTF16PtrFromString(action)
	cwd, _ := syscall.UTF16PtrFromString(dir)

	shellExecute.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(args)),
		uintptr(unsafe.Pointer(cwd)),
		0, // SW_HIDE
	)
}

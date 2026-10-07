package main

import (
	"runtime"
	"syscall"
	"unsafe"
)

const (
	rebootMagic1            = 0xfee1dead
	rebootMagic2            = 672274793
	rebootBootloaderCommand = 0xa1b2c3d4
)

// requestBootloaderReboot 通过 reboot(2) 将设备交给 bootloader/Fastboot
func requestBootloaderReboot() error {
	argument := []byte("bootloader\x00")
	argumentPointer := uintptr(unsafe.Pointer(&argument[0]))
	syscall.Sync()

	_, _, errno := syscall.RawSyscall6(syscall.SYS_REBOOT,
		rebootMagic1, rebootMagic2, rebootBootloaderCommand, argumentPointer, 0, 0)
	runtime.KeepAlive(argument)
	if errno != 0 {
		return errno
	}
	return nil
}

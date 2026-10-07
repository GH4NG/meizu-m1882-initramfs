package main

import (
	"runtime"
	"syscall"
	"unsafe"
)

const (
	inputEventPattern         = "/dev/input/event*"
	inputEventSize            = 24
	keyVolumeUp               = 115
	keyVolumeDown             = 114
	keyPower                  = 116
	maxInputKeyCode           = 0x2ff
	eventTypeKey              = 0x01
	inputEventBitsRequestBase = 0x20
	inputKeyStateRequest      = 0x18
	keyCodeBitmapSize         = (maxInputKeyCode + 8) / 8
)

// readInputKeyBitmap 调用 EVIOCGBIT 一类 ioctl，取得设备支持的按键位图
func readInputKeyBitmap(fd int, ioctlRequest int) ([keyCodeBitmapSize]byte, error) {
	var keyBitmap [keyCodeBitmapSize]byte
	request := uintptr(2)<<30 |
		uintptr(len(keyBitmap))<<16 |
		uintptr('E')<<8 |
		uintptr(ioctlRequest)
	// 按 Linux _IOC(dir, type, nr, size) 规则编码读取请求
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		request,
		uintptr(unsafe.Pointer(&keyBitmap[0])),
	)
	runtime.KeepAlive(keyBitmap)
	if errno != 0 {
		return keyBitmap, errno
	}
	return keyBitmap, nil
}

// keyIsSet 按 Linux 输入子系统的位图格式检查指定键码是否置位
func keyIsSet(bitmap [keyCodeBitmapSize]byte, keyCode int) bool {
	return bitmap[keyCode/8]&(1<<uint(keyCode%8)) != 0
}

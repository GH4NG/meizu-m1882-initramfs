package main

import (
	"fmt"
	"os"

	"github.com/u-root/u-root/pkg/ulog"
)

// 使用 u-root 写入内核日志，并把同一条信息输出到当前控制台
func logf(format string, args ...any) {
	message := fmt.Sprintf("initramfs: "+format, args...)
	fmt.Fprintln(os.Stderr, message)
	ulog.KernelLog.Printf("%s\n", message)
}

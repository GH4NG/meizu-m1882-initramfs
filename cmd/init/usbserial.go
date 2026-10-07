package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	urootmount "github.com/u-root/u-root/pkg/mount"
	"github.com/u-root/u-root/pkg/termios"
)

// setupUSBSerial 建立 ACM 和可选 NCM，绑定 UDC，并返回 NCM 是否可用
func setupUSBSerial() (bool, error) {
	logf("starting USB CDC ACM setup")
	if err := prepareUSBGadget(); err != nil {
		return false, err
	}
	ncmReady := true
	if err := configureUSBNCMFunction(); err != nil {
		logf("USB NCM setup failed; keeping CDC ACM console: %v", err)
		ncmReady = false
	}
	if err := bindUSBUDC(); err != nil {
		return false, err
	}
	logf("USB CDC ACM ready")
	return ncmReady, nil
}

// prepareUSBGadget 创建 M1882 gadget、写入描述符并加入 ACM 功能
func prepareUSBGadget() error {
	if err := os.MkdirAll(configFSPath, 0o755); err != nil {
		return fmt.Errorf("create configfs mountpoint: %w", err)
	}
	if _, err := urootmount.Mount("configfs", configFSPath, "configfs", "", 0); err != nil && !errors.Is(err, syscall.EBUSY) {
		return fmt.Errorf("mount configfs: %w", err)
	}

	directories := []string{
		usbGadgetPath,
		filepath.Join(usbGadgetPath, "strings/0x409"),
		filepath.Join(usbGadgetPath, "configs/c.1"),
		filepath.Join(usbGadgetPath, "configs/c.1/strings/0x409"),
	}
	for _, directory := range directories {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", directory, err)
		}
	}

	attributes := []struct{ name, value string }{
		{"idVendor", "0x0525"},
		{"idProduct", "0xa4a7"},
		{"bcdUSB", "0x0200"},
		{"bcdDevice", "0x0100"},
		{"bDeviceClass", "0xef"},
		{"bDeviceSubClass", "0x02"},
		{"bDeviceProtocol", "0x01"},
		{"strings/0x409/manufacturer", "M1882"},
		{"strings/0x409/product", "M1882 initramfs console"},
		{"strings/0x409/serialnumber", "m1882-initramfs"},
		{"configs/c.1/MaxPower", "100"},
		{"configs/c.1/strings/0x409/configuration", "CDC ACM console"},
	}
	// 所有功能需在绑定 UDC 前加入配置，避免重配 gadget 时断开 ACM 串口。
	for _, attribute := range attributes {
		if err := writeAttribute(filepath.Join(usbGadgetPath, attribute.name), attribute.value); err != nil {
			return fmt.Errorf("write %s: %w", attribute.name, err)
		}
	}
	return ensureGadgetFunction(acmFunctionName)
}

// ensureGadgetFunction 创建 gadget function 目录并将其链接进 c.1 配置
func ensureGadgetFunction(name string) error {
	function := filepath.Join(usbGadgetPath, "functions", name)
	if err := os.MkdirAll(function, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", function, err)
	}
	link := filepath.Join(usbGadgetPath, "configs/c.1", name)
	if err := os.Symlink(function, link); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("link %s function: %w", name, err)
	}
	return nil
}

// bindUSBUDC 等待控制器注册后绑定 gadget；已绑定到同一控制器时直接返回
func bindUSBUDC() error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		controllers, err := os.ReadDir("/sys/class/udc")
		if err == nil {
			for _, controller := range controllers {
				if strings.HasPrefix(controller.Name(), ".") {
					continue
				}
				udcPath := filepath.Join(usbGadgetPath, "UDC")
				current, _ := os.ReadFile(udcPath)
				if strings.TrimSpace(string(current)) == controller.Name() {
					return nil
				}
				if err := writeAttribute(udcPath, controller.Name()); err != nil {
					return fmt.Errorf("bind UDC %s: %w", controller.Name(), err)
				}
				logf("bound USB gadget to UDC %s", controller.Name())
				return nil
			}
		}
		if time.Now().After(deadline) {
			return errors.New("no UDC registered after 15 seconds")
		}
		time.Sleep(usbPollInterval)
	}
}

// writeAttribute 向 configfs/sysfs 属性写入一行值并检查关闭错误
func writeAttribute(path, value string) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, writeErr := file.WriteString(value + "\n")
	return errors.Join(writeErr, file.Close())
}

// openConsole 按 USB ACM、SoC UART、内核 console 的优先级打开控制台
func openConsole(usbSerialReady bool) (*os.File, string, error) {
	devices := []string{"/dev/ttyGS0", "/dev/ttyMSM0", "/dev/console"}
	if !usbSerialReady {
		devices = devices[1:]
	}
	var lastErr error
	for _, device := range devices {
		file, err := os.OpenFile(device, os.O_RDWR|syscall.O_NOCTTY, 0)
		if err == nil {
			return file, device, nil
		}
		lastErr = err
		logf("open %s: %v", device, err)
	}
	return nil, "", fmt.Errorf("no console device could be opened: %w", lastErr)
}

// configureConsole 将选中的串口设为 115200 8N1，并关闭流控
func configureConsole(file *os.File) error {
	settings, err := termios.GetTermios(file.Fd())
	if err != nil {
		return err
	}
	settings = termios.MakeSerialDefault(settings)
	settings, err = termios.MakeSerialBaud(settings, 115200)
	if err != nil {
		return err
	}
	settings.Cflag &^= uint32(syscall.CSTOPB|syscall.PARENB|syscall.PARODD) | serialFlowControlMask
	return termios.SetTermios(file.Fd(), settings)
}

// redirectConsole 把 PID 1 的标准输入、输出和错误输出重定向到控制台
func redirectConsole(console *os.File) error {
	for fd := 0; fd < 3; fd++ {
		if err := syscall.Dup3(int(console.Fd()), fd, 0); err != nil {
			return err
		}
	}
	return nil
}

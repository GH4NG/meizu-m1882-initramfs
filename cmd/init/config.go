package main

import (
	"time"
)

const (
	// 根文件系统的挂载点和默认 init 路径
	rootMountPath = "/newroot"
	rootInitPath  = "/sbin/init"

	rootDeviceWaitTimeout             = 15 * time.Second
	rootDeviceWaitTimeoutWithRootwait = 15 * time.Second
	bootMenuPollInterval              = 50 * time.Millisecond
	usbSSHStartupNetworkWaitTimeout   = 3 * time.Second
	usbSSHRescueNetworkWaitTimeout    = 15 * time.Second
	usbPollInterval                   = 100 * time.Millisecond

	// BOOT_PARTITION 是项目参数；未设置时兼容 Linux 常用的 root= 参数
	rootKernelArgument          = "root"
	bootPartitionKernelArgument = "BOOT_PARTITION"

	serialFlowControlMask = 0x80000000

	// configfs USB gadget 根目录
	configFSPath  = "/sys/kernel/config"
	usbGadgetPath = configFSPath + "/usb_gadget/m1882"

	acmFunctionName        = "acm.usb0"
	ncmFunctionName        = "ncm.usb0"
	ncmInterfaceName       = "usb0"
	ncmDeviceMAC           = "02:18:82:00:00:02"
	ncmHostMAC             = "02:18:82:00:00:01"
	ncmDeviceIP            = "169.254.188.2"
	ncmNetworkPrefixLength = 16

	// initramfs USB SSH 的监听端口
	sshListenPort = "22"
)

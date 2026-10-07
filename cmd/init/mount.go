package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/u-root/u-root/pkg/cmdline"
	urootmount "github.com/u-root/u-root/pkg/mount"
	"github.com/u-root/u-root/pkg/mount/block"
)

// configuredRootPartition 按优先级读取 BOOT_PARTITION=、root= 和同名环境变量
func configuredRootPartition() (string, error) {
	args := cmdline.NewCmdLine()
	if args.Err != nil {
		return "", fmt.Errorf("read kernel command line: %w", args.Err)
	}
	return rootPartitionFromCmdline(args)
}

// rootPartitionFromCmdline 按项目参数、标准 root= 和环境变量读取根分区
func rootPartitionFromCmdline(args *cmdline.CmdLine) (string, error) {
	partition, _ := args.Flag(bootPartitionKernelArgument)
	if partition == "" {
		partition, _ = args.Flag(rootKernelArgument)
	}
	if partition == "" {
		partition = os.Getenv(bootPartitionKernelArgument)
	}
	if partition == "" {
		return "", fmt.Errorf("no %s= or %s= kernel argument", bootPartitionKernelArgument, rootKernelArgument)
	}
	return partition, nil
}

// 本文件负责根设备解析和切根，块设备与挂载操作复用 u-root
func listBlockDevices() {
	blkid := exec.Command("/bbin/blkid")
	blkid.Stdout = os.Stdout
	blkid.Stderr = os.Stderr
	if err := blkid.Run(); err != nil {
		logf("list block devices: %v", err)
	}
}

// switchRoot 解析根设备、挂载根文件系统并调用 u-root 的切根实现
// partition 为空时使用内核默认根和 rootfstype/rootflags；显式值用于菜单选择
func switchRoot(partition, initPath string, beforeSwitch func()) error {
	args := cmdline.NewCmdLine()
	if args.Err != nil {
		return fmt.Errorf("read kernel command line: %w", args.Err)
	}
	var err error
	filesystemType, rootFlags := "", ""
	if partition == "" {
		partition, err = rootPartitionFromCmdline(args)
		if err != nil {
			return err
		}
		filesystemType, _ = args.Flag("rootfstype")
		rootFlags, _ = args.Flag("rootflags")
	}
	if initPath == "" {
		initPath = rootInitPath
	}
	logf("using root partition %s", partition)

	rootDelay, _ := args.Flag("rootdelay")
	if delay, err := time.ParseDuration(rootDelay + "s"); err == nil && delay > 0 {
		logf("waiting %s before probing root partition", delay)
		time.Sleep(delay)
	}

	device, err := waitForRootDevice(partition, rootDeviceWaitTimeout)
	if err != nil {
		return err
	}
	mountFlags, mountData := rootMountOptions(rootFlags)
	createMountPoint := func() error { return os.MkdirAll(rootMountPath, 0o755) }
	var rootMount *urootmount.MountPoint
	if filesystemType == "" {
		rootMount, err = urootmount.TryMount(device, rootMountPath, mountData, mountFlags, createMountPoint)
	} else {
		rootMount, err = urootmount.Mount(device, rootMountPath, filesystemType, mountData, mountFlags, createMountPoint)
	}
	if err != nil {
		if filesystemType == "" {
			filesystemType = "an automatically detected filesystem"
		}
		return fmt.Errorf("mount %s as %s: %w", device, filesystemType, err)
	}
	defer rootMount.Unmount(0) // 成功切根会直接 exec；仅失败返回时卸载。
	// 根系统的 init 必须存在；否则卸载探测挂载并回到救援流程
	if _, err := os.Stat(filepath.Join(rootMountPath, strings.TrimPrefix(initPath, "/"))); err != nil {
		return fmt.Errorf("root filesystem has no %s: %w", initPath, err)
	}
	logf("switching root to %s mounted from %s; starting %s", rootMountPath, device, initPath)
	if beforeSwitch != nil {
		beforeSwitch()
	}
	if err := urootmount.SwitchRoot(rootMountPath, initPath); err != nil {
		return fmt.Errorf("switch root filesystem: %w", err)
	}
	return nil
}

// rootMountOptions 将 ro/rw 转为 mount 标志，其余文件系统参数保留为 data 字符串
func rootMountOptions(options string) (uintptr, string) {
	flags := uintptr(urootmount.MS_RELATIME)
	var data []string
	for _, option := range strings.Split(options, ",") {
		switch option {
		case "ro":
			flags |= urootmount.MS_RDONLY
		case "rw", "":
		default:
			data = append(data, option)
		}
	}
	return flags, strings.Join(data, ",")
}

// waitForRootDevice 周期性重试设备路径或分区标识，直到设备出现或超时
func waitForRootDevice(spec string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		device, err := findRootDevice(spec)
		if err == nil {
			return device, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("root partition %q not found after %s: %w", spec, timeout, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// findRootDevice 解析 /dev 路径、文件系统 UUID 或 GPT 分区标识
func findRootDevice(spec string) (string, error) {
	if strings.HasPrefix(spec, "/dev/") {
		if _, err := os.Stat(spec); err != nil {
			return "", err
		}
		return spec, nil
	}

	key, value, ok := strings.Cut(spec, "=")
	if !ok || value == "" {
		return "", fmt.Errorf("unsupported root partition specifier %q", spec)
	}
	devices, err := block.GetBlockDevices()
	if err != nil {
		return "", fmt.Errorf("list block devices: %w", err)
	}
	var matches block.BlockDevices
	switch key {
	case "UUID":
		for _, device := range devices {
			if strings.EqualFold(device.FsUUID, value) {
				matches = append(matches, device)
			}
		}
	case "PARTUUID":
		matches = devices.FilterPartID(value)
	case "PARTLABEL":
		matches = devices.FilterPartLabel(value)
	default:
		return "", fmt.Errorf("unsupported root partition specifier %q", key)
	}
	for _, device := range matches {
		devicePath := device.DevicePath()
		if _, err := os.Stat(devicePath); err == nil {
			return devicePath, nil
		}
	}
	return "", fmt.Errorf("no block device matches %q", spec)
}

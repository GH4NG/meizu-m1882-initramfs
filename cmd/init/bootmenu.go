package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/u-root/u-root/pkg/cmdline"
	urootmount "github.com/u-root/u-root/pkg/mount"
	"github.com/u-root/u-root/pkg/mount/block"
)

const (
	bootMenuProbePath = "/run/bootmenu/probe"
	bootMenuTimeout   = 20 * time.Second
)

type bootAction uint8

// 菜单仅提供启动系统、进入救援、重启到 bootloader 和关机。
const (
	bootActionRoot bootAction = iota
	bootActionRescue
	bootActionBootloader
	bootActionPowerOff
)

// bootRoot 保存只读探测到的 Linux 根系统信息。
type bootRoot struct {
	name     string
	device   string
	spec     string
	initPath string
}

type bootMenuEntry struct {
	title    string
	detail   string
	action   bootAction
	rootSpec string
	initPath string
}

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

// discoverLinuxRoots 只读挂载分区，筛选出包含可启动 init 的 Linux 根系统
func discoverLinuxRoots() ([]bootRoot, error) {
	devices, err := block.GetBlockDevices()
	if err != nil {
		return nil, fmt.Errorf("list block devices: %w", err)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })
	if err := os.MkdirAll(bootMenuProbePath, 0o755); err != nil {
		return nil, fmt.Errorf("create rootfs probe mountpoint: %w", err)
	}

	var roots []bootRoot
	var mountFailures int
	for _, device := range devices {
		if _, err := os.Stat(filepath.Join("/sys/class/block", device.Name, "partition")); err != nil {
			continue
		}
		devicePath := device.DevicePath()
		if _, err := os.Stat(devicePath); err != nil {
			continue
		}
		// 每次只探测一个分区，并在读取 init 与 os-release 后立即卸载
		mounted, err := urootmount.TryMount(devicePath, bootMenuProbePath, "", urootmount.ReadOnly)
		if err != nil {
			mountFailures++
			continue
		}

		initPath := findRootInit(bootMenuProbePath)
		if initPath != "" {
			name := readRootPrettyName(bootMenuProbePath)
			if name == "" {
				name = "Linux system"
			}
			spec := devicePath
			if device.FsUUID != "" {
				spec = "UUID=" + device.FsUUID
			}
			roots = append(roots, bootRoot{
				name: name, device: devicePath, spec: spec, initPath: initPath,
			})
		}
		if err := mounted.Unmount(0); err != nil {
			return roots, fmt.Errorf("unmount rootfs probe %s: %w", devicePath, err)
		}
	}
	if mountFailures > 0 {
		logf("rootfs scan skipped %d partitions that could not be mounted read-only", mountFailures)
	}
	return roots, nil
}

// findRootInit 返回根系统中第一个存在的标准 init 路径
func findRootInit(root string) string {
	for _, path := range []string{rootInitPath, "/usr/lib/systemd/systemd", "/bin/init"} {
		if _, err := os.Stat(filepath.Join(root, strings.TrimPrefix(path, "/"))); err == nil {
			return path
		}
	}
	return ""
}

// readRootPrettyName 从 os-release 读取系统名称，供菜单作为显示标题
func readRootPrettyName(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "etc/os-release"))
	if err != nil {
		data, err = os.ReadFile(filepath.Join(root, "usr/lib/os-release"))
	}
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key != "PRETTY_NAME" {
			continue
		}
		value = strings.TrimSpace(value)
		if decoded, err := strconv.Unquote(value); err == nil {
			value = decoded
		}
		return strings.TrimSpace(value)
	}
	return ""
}

// makeBootMenuEntries 合并扫描结果、默认根系统及固定维护入口
func makeBootMenuEntries(roots []bootRoot, defaultRoot string) ([]bootMenuEntry, int) {
	entries := make([]bootMenuEntry, 0, len(roots)+3)
	defaultDevice, _ := findRootDevice(defaultRoot)
	defaultIndex := -1
	for _, root := range roots {
		if defaultIndex < 0 &&
			((defaultDevice != "" && filepath.Clean(root.device) == filepath.Clean(defaultDevice)) ||
				(defaultDevice == "" && sameRootSpec(root.spec, defaultRoot))) {
			defaultIndex = len(entries)
		}
		entries = append(entries, bootMenuEntry{
			title: root.name, detail: root.device, action: bootActionRoot,
			rootSpec: root.spec, initPath: root.initPath,
		})
	}
	if defaultRoot != "" && defaultIndex < 0 {
		device := defaultRoot
		if defaultDevice != "" {
			device = defaultDevice
		}
		defaultIndex = len(entries)
		entries = append(entries, bootMenuEntry{
			title: "Configured system", detail: device, action: bootActionRoot,
			rootSpec: defaultRoot, initPath: rootInitPath,
		})
	}
	entries = append(entries,
		bootMenuEntry{title: "Rescue shell", detail: "Initramfs maintenance shell", action: bootActionRescue},
		bootMenuEntry{title: "Bootloader / Fastboot", detail: "Restart to bootloader Fastboot", action: bootActionBootloader},
		bootMenuEntry{title: "Power off", detail: "Turn off the device", action: bootActionPowerOff},
	)
	return entries, defaultIndex
}

// sameRootSpec 比较两个设备路径或分区标识是否指向同一个默认根
func sameRootSpec(a, b string) bool {
	if strings.HasPrefix(a, "/dev/") && strings.HasPrefix(b, "/dev/") {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return strings.EqualFold(a, b)
}

// runBootMenu 绘制菜单并处理超时、音量键导航和电源键确认
func runBootMenu(display *bootMenuDisplay, entries []bootMenuEntry, defaultIndex int) (bootMenuEntry, error) {
	if len(entries) == 0 {
		return bootMenuEntry{}, errors.New("boot menu has no entries")
	}
	selected := defaultIndex
	rescue := bootMenuEntry{title: "Rescue shell", action: bootActionRescue}
	if selected < 0 {
		selected = 0
	}
	input, err := openBootMenuInput()
	if err != nil {
		logf("boot menu input setup failed: %v", err)
		if defaultIndex >= 0 {
			logf("starting configured root because menu input is unavailable")
			return entries[defaultIndex], nil
		}
		return rescue, nil
	}
	defer input.close()

	deadline := time.Time{}
	if defaultIndex >= 0 {
		deadline = time.Now().Add(bootMenuTimeout)
	}
	firstVisible := 0
	lastSecond := -1
	redraw := true
	for {
		if selected < firstVisible {
			firstVisible = selected
		}
		visible := display.bootMenuVisibleRows()
		if selected >= firstVisible+visible {
			firstVisible = selected - visible + 1
		}
		remaining := -1
		if !deadline.IsZero() {
			remaining = int(time.Until(deadline).Seconds()) + 1
			if remaining < 0 {
				remaining = 0
			}
		}
		if redraw || remaining != lastSecond {
			display.drawBootMenu(entries, selected, firstVisible, remaining)
			lastSecond = remaining
			redraw = false
			if display.writeErr != nil {
				logf("boot menu text output failed; falling back to configured root")
				if defaultIndex >= 0 {
					return entries[defaultIndex], nil
				}
				return rescue, nil
			}
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			logf("boot menu timeout; starting default root %s", entries[selected].rootSpec)
			return entries[selected], nil
		}

		time.Sleep(bootMenuPollInterval)
		delta, activate := input.poll()
		if delta != 0 {
			selected = wrapMenuIndex(selected+delta, len(entries))
			deadline = time.Time{}
			redraw = true
		}
		if activate {
			logf("boot menu activated: %d/%d title=%q", selected+1, len(entries), entries[selected].title)
			return entries[selected], nil
		}
	}
}

// wrapMenuIndex 让菜单索引越过首尾时循环回到另一端
func wrapMenuIndex(index, count int) int {
	if count == 0 {
		return 0
	}
	index %= count
	if index < 0 {
		index += count
	}
	return index
}

// bootMenuInput 聚合多个 evdev 设备的按键状态与菜单动作
type bootMenuInput struct {
	devices     []*bootInputDevice
	keys        map[int]bool
	initialized bool
}

// bootInputDevice 保存单个输入设备及尚未组成完整事件的数据
type bootInputDevice struct {
	file     *os.File
	keyCodes []int
	pending  []byte
}

// openBootMenuInput 只打开支持音量加、音量减或电源键的 evdev 设备
func openBootMenuInput() (*bootMenuInput, error) {
	paths, err := filepath.Glob(inputEventPattern)
	if err != nil {
		return nil, err
	}
	input := &bootMenuInput{keys: make(map[int]bool)}
	for _, path := range paths {
		fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		device := &bootInputDevice{file: os.NewFile(uintptr(fd), path)}
		keyBitmap, keyErr := readInputKeyBitmap(fd, inputEventBitsRequestBase+eventTypeKey)
		if keyErr == nil {
			for _, key := range []int{keyVolumeUp, keyVolumeDown, keyPower} {
				if keyIsSet(keyBitmap, key) {
					device.keyCodes = append(device.keyCodes, key)
					input.keys[key] = false
				}
			}
		}
		if len(device.keyCodes) == 0 {
			_ = device.file.Close()
		} else {
			input.devices = append(input.devices, device)
		}
	}
	if len(input.devices) == 0 {
		return nil, errors.New("no usable evdev devices found")
	}
	return input, nil
}

// poll 合并 ioctl 当前状态与事件队列，返回菜单移动量及是否确认
func (input *bootMenuInput) poll() (delta int, activate bool) {
	current := make(map[int]bool, len(input.keys))
	eventPressed := make(map[int]bool, len(input.keys))
	for _, device := range input.devices {
		fd := int(device.file.Fd())
		bitmap, err := readInputKeyBitmap(fd, inputKeyStateRequest)
		if err == nil {
			for _, key := range device.keyCodes {
				current[key] = current[key] || keyIsSet(bitmap, key)
			}
		}
		device.readInputEvents(eventPressed)
	}
	if !input.initialized {
		for key := range input.keys {
			input.keys[key] = current[key]
		}
		input.initialized = true
	}
	for key := range input.keys {
		pressed := current[key]
		if eventPressed[key] || (pressed && !input.keys[key]) {
			switch key {
			case keyVolumeUp:
				delta--
			case keyVolumeDown:
				delta++
			case keyPower:
				activate = true
			}
		}
		input.keys[key] = pressed
	}
	return delta, activate
}

// readInputEvents 读取非阻塞事件队列，并保留不完整的尾部数据供下次解析
func (device *bootInputDevice) readInputEvents(pressedKeys map[int]bool) {
	var buffer [inputEventSize * 32]byte
	for {
		n, err := syscall.Read(int(device.file.Fd()), buffer[:])
		if err != nil || n == 0 {
			break
		}
		device.pending = append(device.pending, buffer[:n]...)
		if n < len(buffer) {
			break
		}
	}

	for len(device.pending) >= inputEventSize {
		event := device.pending[:inputEventSize]
		device.pending = device.pending[inputEventSize:]
		if binary.LittleEndian.Uint16(event[16:18]) == eventTypeKey &&
			int32(binary.LittleEndian.Uint32(event[20:24])) == 1 {
			pressedKeys[int(binary.LittleEndian.Uint16(event[18:20]))] = true
		}
	}
}

// close 关闭菜单打开的全部 evdev 设备文件
func (input *bootMenuInput) close() {
	for _, device := range input.devices {
		_ = device.file.Close()
	}
}

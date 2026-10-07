package main

import (
	"os"
	"os/exec"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/u-root/u-root/pkg/libinit"
	"github.com/u-root/u-root/pkg/ulog"
)

// main 启动 initramfs PID 1
func main() {
	libinit.SetEnv()
	libinit.Namespace = slices.DeleteFunc(libinit.Namespace, func(creator libinit.Creator) bool {
		mount, ok := creator.(libinit.Mount)
		return ok && mount.Source == "efivarfs"
	})
	libinit.CreateRootfs()

	ulog.Log = ulog.Null
	logf("PID 1 started")

	ncmReady, console, consoleDevice := setupConsole()
	defer console.Close()
	if err := setupPartitionFirmware(); err != nil {
		logf("partition firmware setup incomplete: %v", err)
	}
	runInitramfs(console, consoleDevice, ncmReady)
}

// setupConsole 配置 USB gadget 并打开串口、UART 或内核控制台
func setupConsole() (bool, *os.File, string) {
	ncmReady, serialErr := setupUSBSerial()
	if serialErr != nil {
		logf("USB serial setup failed; keeping UART console: %v", serialErr)
	}
	console, device, err := openConsole(serialErr == nil)
	if err != nil {
		logf("console setup failed: %v", err)
		waitForUSBRescue(ncmReady)
	}
	if err := configureConsole(console); err != nil {
		logf("configure %s: %v", device, err)
	}
	if err := redirectConsole(console); err != nil {
		logf("redirect console: %v", err)
		waitForUSBRescue(ncmReady)
	}
	return ncmReady, console, device
}

// waitForUSBRescue 保持 PID 1 存活，并在可用时开放 SSH 救援入口
func waitForUSBRescue(ncmReady bool) {
	if _, err := startUSBSSHWithTimeout(ncmReady, usbSSHRescueNetworkWaitTimeout); err != nil {
		logf("rescue USB SSH unavailable: %v", err)
	}
	select {}
}

// runInitramfs 启动 SSH、显示启动菜单，并在失败时进入救援 shell
func runInitramfs(console *os.File, consoleDevice string, ncmReady bool) {
	sshServer, err := startUSBSSHWithTimeout(ncmReady, usbSSHStartupNetworkWaitTimeout)
	if err != nil {
		logf("USB SSH startup failed: %v", err)
	}
	listBlockDevices()
	stopSSHServer := func() {
		if sshServer != nil {
			sshServer.close()
			sshServer = nil
		}
	}

	menuShell := handleBootMenu(console, consoleDevice, stopSSHServer)
	if menuShell != nil {
		<-menuShell.done
		return
	}

	if sshServer == nil {
		if _, err := startUSBSSHWithTimeout(ncmReady, usbSSHRescueNetworkWaitTimeout); err != nil {
			logf("rescue USB SSH unavailable: %v", err)
		}
	}
	rescueShell := startRescueShellSession(console, consoleDevice)
	<-rescueShell.done
}

// handleBootMenu 显示系统选择菜单，并处理选中的启动或维护动作
func handleBootMenu(console *os.File, consoleDevice string, stopSSHServer func()) *rescueShellSession {
	display, err := openBootMenuDisplay()
	if err != nil {
		logf("text boot menu unavailable; using configured root: %v", err)
		if err := switchRoot("", "", stopSSHServer); err != nil {
			logf("root mount or switch_root failed; entering rescue: %v", err)
		}
		return nil
	}
	defer display.close()

	// 先显示扫描提示，再只读探测分区并等待菜单选择
	display.drawBootMenuScanning()
	roots, err := discoverLinuxRoots()
	if err != nil {
		logf("Linux rootfs scan failed: %v", err)
	}
	defaultRoot, err := configuredRootPartition()
	if err != nil {
		logf("default root unavailable: %v", err)
	}
	entries, defaultIndex := makeBootMenuEntries(roots, defaultRoot)
	menuShell := startRescueShellSession(console, consoleDevice)
	selection, err := runBootMenu(display, entries, defaultIndex)
	display.restoreConsole()
	if err != nil {
		logf("boot menu failed: %v", err)
		menuShell.stopAndWait()
		return nil
	}

	if selection.action != bootActionRescue {
		menuShell.stopAndWait()
	}
	switch selection.action {
	case bootActionRoot:
		display.drawBootMenuStatus("Starting "+selection.title, "Mounting selected root filesystem")
		if err := switchRoot(selection.rootSpec, selection.initPath, stopSSHServer); err != nil {
			logf("selected root mount or switch_root failed; entering rescue: %v", err)
		}
	case bootActionRescue:
		display.drawBootMenuStatus("Rescue shell", "Connect through USB serial or SSH")
		logf("rescue shell selected from boot menu")
		return menuShell
	case bootActionBootloader:
		display.drawBootMenuStatus("Restarting to bootloader", "")
		if err := requestBootloaderReboot(); err != nil {
			logf("bootloader restart request failed: %v", err)
		}
	case bootActionPowerOff:
		display.drawBootMenuStatus("Powering off", "")
		logf("poweroff selected from boot menu")
		if err := exec.Command("/bbin/poweroff").Run(); err != nil {
			logf("poweroff failed: %v", err)
		}
	}
	return nil
}

// rescueShellSession 在启动菜单期间提供 u-root gosh 串口 shell，并可在切根前停止
type rescueShellSession struct {
	console *os.File
	device  string
	stop    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	command *exec.Cmd
}

// startRescueShellSession 启动可重启的串口救援 shell 会话
func startRescueShellSession(console *os.File, device string) *rescueShellSession {
	session := &rescueShellSession{
		console: console,
		device:  device,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go session.run()
	return session
}

// run 在菜单等待期间保持 u-root gosh 可用，退出后按间隔重新启动
func (session *rescueShellSession) run() {
	defer close(session.done)
	retryDelay := time.Second
	for {
		if session.stopping() {
			return
		}
		logf("starting u-root gosh rescue shell on %s", session.device)
		shell := exec.Command("/bbin/gosh")
		shell.Stdin, shell.Stdout, shell.Stderr = session.console, session.console, session.console
		// USB ACM 终端不重复设置 controlling TTY，避免主机重连时触发 EIO
		shell.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		err := shell.Start()
		if err == nil {
			session.mu.Lock()
			session.command = shell
			session.mu.Unlock()
			if session.stopping() {
				signalRescueShellGroup(shell, syscall.SIGTERM)
			}
			err = shell.Wait()
			session.mu.Lock()
			session.command = nil
			session.mu.Unlock()
		}
		if session.stopping() {
			return
		}
		if err != nil {
			logf("gosh failed: %v", err)
		} else {
			retryDelay = time.Second
		}
		logf("restarting gosh rescue shell in %s", retryDelay)
		if session.waitOrStop(retryDelay) {
			return
		}
		if err != nil && retryDelay < 5*time.Second {
			retryDelay += time.Second
		}
	}
}

// stopAndWait 结束 shell 进程组，确保进入所选系统前不会遗留串口读取者
func (session *rescueShellSession) stopAndWait() {
	close(session.stop)
	session.signal(syscall.SIGTERM)
	select {
	case <-session.done:
	case <-time.After(time.Second):
		session.signal(syscall.SIGKILL)
		<-session.done
	}
}

// signal 在锁内读取当前进程，避免 shell 重启时漏发停止信号。
func (session *rescueShellSession) signal(signal syscall.Signal) {
	session.mu.Lock()
	defer session.mu.Unlock()
	signalRescueShellGroup(session.command, signal)
}

// stopping 检查是否已收到切根或退出请求
func (session *rescueShellSession) stopping() bool {
	select {
	case <-session.stop:
		return true
	default:
		return false
	}
}

// waitOrStop 在重启间隔内允许启动流程立即停止 shell
func (session *rescueShellSession) waitOrStop(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-session.stop:
		return true
	case <-timer.C:
		return false
	}
}

// signalRescueShellGroup 通过独立 session 的进程组结束救援 shell
func signalRescueShellGroup(shell *exec.Cmd, signal syscall.Signal) {
	if shell == nil || shell.Process == nil {
		return
	}
	if err := syscall.Kill(-shell.Process.Pid, signal); err != nil {
		_ = shell.Process.Signal(signal)
	}
}

package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/u-root/u-root/pkg/termios"
)

const (
	menuConsolePath = "/dev/tty0"

	// 终端查询失败时使用常见的 80x25 文本布局
	defaultMenuColumns = 80
	defaultMenuRows    = 25
	menuReservedRows   = 6
	menuEntryLines     = 2
	menuMaximumWidth   = 40
	menuLogHistory     = 128

	consoleFontIoctl = 0x4b72
	consoleFontGet   = 1
	consoleFontSet   = 0
	consoleFontPitch = 32
	consoleMaxWidth  = 64
	consoleMaxGlyphs = 512
)

// consoleFontOperation 对应 Linux KDFONTOP 控制台字体操作结构
type consoleFontOperation struct {
	operation uint32
	flags     uint32
	width     uint32
	height    uint32
	glyphs    uint32
	data      uintptr
}

type consoleFontSnapshot struct {
	operation consoleFontOperation
	data      []byte
}

type bootMenuDisplay struct {
	file         *os.File
	columns      int
	rows         int
	writeErr     error
	originalFont *consoleFontSnapshot
	kmsgRedirect byte
	kmsgConsole  *os.File
	kernelLogFD  int
	kernelLines  []string
}

// menuLine 保存一行菜单文本及其是否为当前选中项
type menuLine struct {
	text     string
	selected bool
	indent   int
}

// openBootMenuDisplay 打开 tty0，并查询文本终端的行列数
func openBootMenuDisplay() (*bootMenuDisplay, error) {
	file, err := os.OpenFile(menuConsolePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("open text console %s: %w", menuConsolePath, err)
	}
	display := &bootMenuDisplay{
		file:        file,
		columns:     defaultMenuColumns,
		rows:        defaultMenuRows,
		kernelLogFD: -1,
	}
	display.kernelLogFD, err = syscall.Open("/dev/kmsg", syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		display.kernelLogFD = -1
		display.kernelLines = []string{"Kernel log unavailable; use the USB console."}
		logf("open kernel log: %v", err)
	}
	if err := display.redirectKernelMessages(); err != nil {
		logf("isolate boot menu from kernel console messages: %v", err)
	}
	originalFont, err := doubleConsoleFont(file.Fd())
	if err != nil {
		logf("console font scaling unavailable; using kernel default: %v", err)
	} else {
		display.originalFont = originalFont
	}
	display.refreshSize()
	return display, nil
}

// consoleControl 使用 Linux TIOCLINUX 子命令；不调整全局 printk 等级，
// 即使启动参数含 ignore_loglevel，也只隔离 VT 输出，保留 UART 和 dmesg。
func (display *bootMenuDisplay) consoleControl(command *[2]byte) (uintptr, error) {
	result, _, errno := syscall.Syscall(syscall.SYS_IOCTL, display.file.Fd(),
		0x541c, uintptr(unsafe.Pointer(&command[0])))
	runtime.KeepAlive(command)
	if errno != 0 {
		return 0, errno
	}
	return result, nil
}

func (display *bootMenuDisplay) redirectKernelMessages() error {
	foreground := [2]byte{12} // TIOCL_GETFGCONSOLE 返回从 0 开始的前台 VT。
	active, err := display.consoleControl(&foreground)
	if err != nil {
		return err
	}
	saved := [2]byte{17} // TIOCL_GETKMSGREDIRECT 将原值写回首字节。
	if _, err := display.consoleControl(&saved); err != nil {
		return err
	}
	target := byte(2)
	if active == 1 {
		target = 3
	}
	console, err := os.OpenFile(fmt.Sprintf("/dev/tty%d", target), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return err
	}
	redirect := [2]byte{11, target} // TIOCL_SETKMSGREDIRECT 使用从 1 开始的 VT。
	if _, err := display.consoleControl(&redirect); err != nil {
		_ = console.Close()
		return err
	}
	display.kmsgRedirect = saved[0]
	display.kmsgConsole = console
	return nil
}

// restoreConsole 在切根、救援或关闭菜单前恢复原来的内核输出目标和字体。
func (display *bootMenuDisplay) restoreConsole() {
	if display == nil || display.file == nil {
		return
	}
	if display.kernelLogFD >= 0 {
		_ = syscall.Close(display.kernelLogFD)
		display.kernelLogFD = -1
	}
	if display.kmsgConsole != nil {
		command := [2]byte{11, display.kmsgRedirect}
		if _, err := display.consoleControl(&command); err != nil {
			logf("restore kernel console redirect: %v", err)
		} else {
			_ = display.kmsgConsole.Close()
			display.kmsgConsole = nil
		}
	}
	display.restoreConsoleFont()
}

// refreshSize 更新当前终端的行列数；查询失败时保留默认或上次值
func (display *bootMenuDisplay) refreshSize() {
	size, err := termios.GetWinSize(display.file.Fd())
	if err != nil {
		return
	}
	if size.Col > 0 {
		display.columns = int(size.Col)
	}
	if size.Row > 0 {
		display.rows = int(size.Row)
	}
}

func (display *bootMenuDisplay) readKernelLogs() bool {
	if display.kernelLogFD < 0 {
		return false
	}
	changed := false
	var buffer [8192]byte
	for range 256 {
		n, err := syscall.Read(display.kernelLogFD, buffer[:])
		if err == syscall.EPIPE || err == syscall.EINTR {
			continue
		}
		if err == syscall.EAGAIN || n == 0 && err == nil {
			break
		}
		if err != nil {
			_ = syscall.Close(display.kernelLogFD)
			display.kernelLogFD = -1
			display.kernelLines = append(display.kernelLines, "Kernel log read failed: "+err.Error())
			return true
		}
		metadata, message, ok := strings.Cut(string(buffer[:n]), ";")
		if !ok {
			continue
		}
		message, _, _ = strings.Cut(message, "\n")
		fields := strings.Split(metadata, ",")
		if len(fields) >= 3 {
			if stamp, err := strconv.ParseUint(fields[2], 10, 64); err == nil {
				message = fmt.Sprintf("[%5d.%06d] %s", stamp/1_000_000, stamp%1_000_000, message)
			}
		}
		display.kernelLines = append(display.kernelLines, menuASCII(message))
		if len(display.kernelLines) > menuLogHistory {
			display.kernelLines = display.kernelLines[len(display.kernelLines)-menuLogHistory:]
		}
		changed = true
	}
	return changed
}

func (display *bootMenuDisplay) logRows() int {
	return min(24, max(1, display.rows/2))
}

func (display *bootMenuDisplay) menuRows() int {
	return max(0, display.rows-display.logRows()-2)
}

func (display *bootMenuDisplay) drawKernelLogs(output *strings.Builder, row int) {
	width := max(1, display.columns)
	fmt.Fprintf(output, "\x1b[%d;1H%s", row, fitMenuText("--- Kernel log ---", width))
	var lines []string
	for _, line := range display.kernelLines {
		for len(line) > width {
			lines = append(lines, line[:width])
			line = line[width:]
		}
		lines = append(lines, line)
	}
	if len(lines) > display.logRows() {
		lines = lines[len(lines)-display.logRows():]
	}
	for index, line := range lines {
		if row+index+1 > display.rows {
			break
		}
		fmt.Fprintf(output, "\x1b[%d;1H%s", row+index+1, line)
	}
}

// doubleConsoleFont 从当前虚拟终端读取内核字形并放大两倍后写回
func doubleConsoleFont(fd uintptr) (*consoleFontSnapshot, error) {
	fontData := make([]byte, (consoleMaxWidth/8)*consoleFontPitch*consoleMaxGlyphs)
	operation := consoleFontOperation{
		operation: consoleFontGet,
		width:     consoleMaxWidth,
		height:    128,
		glyphs:    consoleMaxGlyphs,
		data:      uintptr(unsafe.Pointer(&fontData[0])),
	}
	if err := runConsoleFontIoctl(fd, &operation); err != nil {
		return nil, fmt.Errorf("read current console font: %w", err)
	}
	if operation.width == 0 || operation.height == 0 || operation.glyphs == 0 {
		return nil, fmt.Errorf("kernel returned invalid font dimensions %dx%d with %d glyphs",
			operation.width, operation.height, operation.glyphs)
	}
	if operation.width > consoleMaxWidth/2 || operation.height > 64 {
		return nil, fmt.Errorf("current font %dx%d cannot be doubled", operation.width, operation.height)
	}
	fontSize := int((operation.width+7)/8) * consoleFontPitch * int(operation.glyphs)
	originalFont := &consoleFontSnapshot{
		operation: operation,
		data:      append([]byte(nil), fontData[:fontSize]...),
	}

	scaledWidth := operation.width * 2
	scaledHeight := operation.height * 2
	sourceStride := int((operation.width+7)/8) * consoleFontPitch
	scaledStride := int((scaledWidth+7)/8) * consoleFontPitch
	scaledData := make([]byte, scaledStride*int(operation.glyphs))
	for glyph := uint32(0); glyph < operation.glyphs; glyph++ {
		for row := uint32(0); row < operation.height; row++ {
			for column := uint32(0); column < operation.width; column++ {
				sourceOffset := int(glyph)*sourceStride + int(row)*int((operation.width+7)/8) + int(column/8)
				if fontData[sourceOffset]&(1<<(7-column%8)) == 0 {
					continue
				}
				for dy := uint32(0); dy < 2; dy++ {
					for dx := uint32(0); dx < 2; dx++ {
						pixel := column*2 + dx
						destinationOffset := int(glyph)*scaledStride + int(row*2+dy)*int((scaledWidth+7)/8) + int(pixel/8)
						scaledData[destinationOffset] |= 1 << (7 - pixel%8)
					}
				}
			}
		}
	}

	operation.operation = consoleFontSet
	operation.width = scaledWidth
	operation.height = scaledHeight
	operation.data = uintptr(unsafe.Pointer(&scaledData[0]))
	if err := runConsoleFontIoctl(fd, &operation); err != nil {
		return nil, fmt.Errorf("set doubled console font: %w", err)
	}
	runtime.KeepAlive(fontData)
	runtime.KeepAlive(scaledData)
	return originalFont, nil
}

// runConsoleFontIoctl 调用内核 KDFONTOP 接口读写当前终端字形
func runConsoleFontIoctl(fd uintptr, operation *consoleFontOperation) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, consoleFontIoctl,
		uintptr(unsafe.Pointer(operation)))
	runtime.KeepAlive(operation)
	if errno != 0 {
		return errno
	}
	return nil
}

// close 关闭文本控制台文件
func (display *bootMenuDisplay) close() {
	if display != nil && display.file != nil {
		display.restoreConsole()
		_ = display.file.Close()
		display.file = nil
	}
}

// restoreConsoleFont 在离开启动菜单前恢复打开菜单时保存的原始字形
func (display *bootMenuDisplay) restoreConsoleFont() {
	if display == nil || display.file == nil || display.originalFont == nil {
		return
	}
	font := display.originalFont
	operation := font.operation
	operation.operation = consoleFontSet
	operation.data = uintptr(unsafe.Pointer(&font.data[0]))
	if err := runConsoleFontIoctl(display.file.Fd(), &operation); err != nil {
		logf("restore original console font failed: %v", err)
		return
	}
	display.originalFont = nil
	display.refreshSize()
	runtime.KeepAlive(font.data)
}

// bootMenuVisibleRows 根据终端高度和紧凑列表布局计算可见项数
func (display *bootMenuDisplay) bootMenuVisibleRows() int {
	return max(1, (display.menuRows()-menuReservedRows)/(menuEntryLines+1))
}

// drawBootMenuScanning 在屏幕中央显示只读扫描提示
func (display *bootMenuDisplay) drawBootMenuScanning() {
	display.drawLines([]menuLine{
		{text: "M1882 Boot Manager"},
		{text: ""},
		{text: "Scanning Linux root filesystems..."},
	})
}

// drawBootMenu 显示系统列表、选中状态、按键提示和自动启动倒计时
func (display *bootMenuDisplay) drawBootMenu(entries []bootMenuEntry, selected, firstVisible, secondsLeft int) {
	// 将标题、列表和操作提示组成紧凑文本块，在终端中央显示
	lines := []menuLine{
		{text: "M1882 Boot Manager"},
		{text: strings.Repeat("-", display.menuWidth())},
		{text: "Select a system or maintenance entry:"},
		{text: ""},
	}
	visible := display.bootMenuVisibleRows()
	end := min(firstVisible+visible, len(entries))
	for index := firstVisible; index < end; index++ {
		lines = append(lines,
			menuLine{text: entries[index].title, selected: index == selected},
			menuLine{text: entries[index].detail, indent: 4},
		)
		if index+1 < end {
			lines = append(lines, menuLine{text: ""})
		}
	}
	lines = append(lines, menuLine{text: ""})
	if secondsLeft >= 0 {
		lines = append(lines, menuLine{text: fmt.Sprintf("Automatic boot in %d seconds", secondsLeft)})
	} else {
		lines = append(lines, menuLine{text: "Automatic boot paused"})
	}
	lines = append(lines, menuLine{text: "Volume Up/Down: select    Power: boot"})
	// 每帧清除旧布局，避免分辨率变化或其他控制台输出留下重影。
	display.drawLines(lines)
}

// drawBootMenuStatus 显示切根、救援或维护动作的当前状态
func (display *bootMenuDisplay) drawBootMenuStatus(title, detail string) {
	display.drawLines([]menuLine{
		{text: "M1882 Boot Manager"},
		{text: ""},
		{text: title},
		{text: detail},
	})
}

// drawLines 用终端光标和反显属性更新文本菜单，不访问 framebuffer 像素
func (display *bootMenuDisplay) drawLines(lines []menuLine) {
	if display == nil || display.file == nil || display.writeErr != nil {
		return
	}
	display.readKernelLogs()
	// 菜单与日志组成一个居中的纵向块，不允许菜单侵入下方日志区域。
	if len(lines) > display.menuRows() {
		lines = lines[:display.menuRows()]
	}
	width := display.menuWidth()
	left := (display.columns-width)/2 + 1
	if left < 1 {
		left = 1
	}
	top := (display.rows-len(lines)-display.logRows()-2)/2 + 1
	if top < 1 {
		top = 1
	}

	var output strings.Builder
	output.WriteString("\x1b[0m\x1b[2J\x1b[H")
	for index, line := range lines {
		prefixWidth := line.indent
		if line.selected {
			prefixWidth = 2
		}
		text := fitMenuText(line.text, width-prefixWidth)
		fmt.Fprintf(&output, "\x1b[%d;%dH\x1b[K", top+index, left)
		if line.selected {
			fmt.Fprintf(&output, "\x1b[7m> %-*s\x1b[0m", width-prefixWidth, text)
		} else if line.text != "" {
			fmt.Fprintf(&output, "%s%s", strings.Repeat(" ", prefixWidth), text)
		}
	}
	display.drawKernelLogs(&output, min(display.rows, top+len(lines)+1))
	// 光标移到日志区末尾，菜单和日志始终由同一次写入提交。
	nextRow := max(1, min(top+len(lines)+display.logRows()+1, display.rows))
	fmt.Fprintf(&output, "\x1b[%d;1H", nextRow)
	if _, err := io.WriteString(display.file, output.String()); err != nil {
		display.writeErr = fmt.Errorf("write text boot menu: %w", err)
		logf("text boot menu update failed: %v", display.writeErr)
	}
}

// menuWidth 选择第一版文本菜单的紧凑宽度，并避免超出窄终端
func (display *bootMenuDisplay) menuWidth() int {
	return max(4, min(menuMaximumWidth, display.columns-2))
}

// fitMenuText 仅保留可打印 ASCII，并按终端列宽裁切菜单文本
func fitMenuText(text string, width int) string {
	text = menuASCII(text)
	if width < 1 {
		return ""
	}
	if len(text) <= width {
		return text
	}
	if width <= 3 {
		return text[:width]
	}
	return text[:width-3] + "..."
}

// menuASCII 将控制字符和非 ASCII 字符替换为空格，避免破坏终端菜单
func menuASCII(text string) string {
	text = strings.Map(func(character rune) rune {
		if character < 0x20 || character > 0x7e {
			return ' '
		}
		return character
	}, text)
	return strings.Join(strings.Fields(text), " ")
}

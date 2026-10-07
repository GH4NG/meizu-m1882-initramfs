package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
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
	menuReservedRows   = 8
	menuEntryLines     = 2
	menuMaximumWidth   = 40

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
	flags  uint32
	width  uint32
	height uint32
	glyphs uint32
	data   []byte
}

type bootMenuDisplay struct {
	file         *os.File
	columns      int
	rows         int
	menuStarted  bool
	writeErr     error
	originalFont *consoleFontSnapshot
	fontRestored bool
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
		file:    file,
		columns: defaultMenuColumns,
		rows:    defaultMenuRows,
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
		flags: operation.flags, width: operation.width, height: operation.height,
		glyphs: operation.glyphs, data: append([]byte(nil), fontData[:fontSize]...),
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
		display.restoreConsoleFont()
		_ = display.file.Close()
		display.file = nil
	}
}

// restoreConsoleFont 在离开启动菜单前恢复打开菜单时保存的原始字形
func (display *bootMenuDisplay) restoreConsoleFont() {
	if display == nil || display.file == nil || display.originalFont == nil || display.fontRestored {
		return
	}
	font := display.originalFont
	operation := consoleFontOperation{
		operation: consoleFontSet,
		flags:     font.flags,
		width:     font.width,
		height:    font.height,
		glyphs:    font.glyphs,
		data:      uintptr(unsafe.Pointer(&font.data[0])),
	}
	if err := runConsoleFontIoctl(display.file.Fd(), &operation); err != nil {
		logf("restore original console font failed: %v", err)
		return
	}
	display.fontRestored = true
	display.refreshSize()
	runtime.KeepAlive(font.data)
}

// bootMenuVisibleRows 根据终端高度和紧凑列表布局计算可见项数
func (display *bootMenuDisplay) bootMenuVisibleRows() int {
	visible := (display.rows - menuReservedRows) / (menuEntryLines + 1)
	if visible < 1 {
		visible = 1
	}
	return visible
}

// drawBootMenuScanning 在屏幕中央显示只读扫描提示
func (display *bootMenuDisplay) drawBootMenuScanning() {
	display.drawLines([]menuLine{
		{text: "M1882 Boot Manager"},
		{text: ""},
		{text: "Scanning Linux root filesystems..."},
	}, true)
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
	end := firstVisible + visible
	if end > len(entries) {
		end = len(entries)
	}
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
		lines = append(lines, menuLine{text: "Automatic boot paused after menu navigation"})
	}
	lines = append(lines, menuLine{text: "Volume Up/Down: select    Power: boot"})
	display.drawLines(lines, !display.menuStarted)
	display.menuStarted = true
}

// drawBootMenuStatus 显示切根、救援或维护动作的当前状态
func (display *bootMenuDisplay) drawBootMenuStatus(title, detail string) {
	display.drawLines([]menuLine{
		{text: "M1882 Boot Manager"},
		{text: ""},
		{text: title},
		{text: detail},
	}, true)
}

// drawLines 用终端光标和反显属性更新文本菜单，不访问 framebuffer 像素
func (display *bootMenuDisplay) drawLines(lines []menuLine, clear bool) {
	if display == nil || display.file == nil || display.writeErr != nil {
		return
	}
	width := display.menuWidth()
	left := (display.columns-width)/2 + 1
	if left < 1 {
		left = 1
	}
	top := (display.rows-len(lines))/2 + 1
	if top < 1 {
		top = 1
	}

	var output strings.Builder
	if clear {
		output.WriteString("\x1b[2J")
	}
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
	output.WriteString("\x1b[J")
	// 光标留在菜单最后一行下方，避免后续启动日志粘到页脚文字后面
	nextRow := top + len(lines)
	if nextRow > display.rows {
		nextRow = display.rows
	}
	if nextRow < 1 {
		nextRow = 1
	}
	fmt.Fprintf(&output, "\x1b[%d;1H", nextRow)
	if _, err := io.WriteString(display.file, output.String()); err != nil {
		display.writeErr = fmt.Errorf("write text boot menu: %w", err)
		logf("text boot menu update failed: %v", display.writeErr)
	}
}

// menuWidth 选择第一版文本菜单的紧凑宽度，并避免超出窄终端
func (display *bootMenuDisplay) menuWidth() int {
	width := menuMaximumWidth
	if width > display.columns-2 {
		width = display.columns - 2
	}
	if width < 4 {
		width = 4
	}
	return width
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
	var output strings.Builder
	for _, character := range text {
		if character >= 0x20 && character <= 0x7e {
			output.WriteByte(byte(character))
		} else {
			output.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(output.String()), " ")
}

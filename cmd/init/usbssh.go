package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/u-root/u-root/pkg/pty"
	"github.com/u-root/u-root/pkg/termios"
	"github.com/vishvananda/netlink"
	"golang.org/x/crypto/ssh"
)

// 本文件在 USB NCM 链路上运行仅供救援使用的 SSH shell
// usbSSHServer 管理监听 socket、并发连接及关闭状态
type usbSSHServer struct {
	listener    net.Listener
	mu          sync.Mutex
	connections map[net.Conn]struct{}
	closed      bool
}

// sshTerminalRequest 保存 SSH 客户端申请的伪终端参数
type sshTerminalRequest struct {
	Term    string
	Columns uint32
	Rows    uint32
	XPixel  uint32
	YPixel  uint32
	Modes   string
}

// startUSBSSHWithTimeout 配置 NCM 地址、临时主机密钥并开始监听 SSH
func startUSBSSHWithTimeout(ncmReady bool, networkWait time.Duration) (*usbSSHServer, error) {
	logf("starting rescue USB NCM/SSH setup")
	if !ncmReady {
		return nil, errors.New("NCM function unavailable; CDC ACM console remains active")
	}
	if err := configureUSBNetwork(networkWait); err != nil {
		return nil, fmt.Errorf("setup USB network: %w", err)
	}
	serverConfig, err := newSSHServerConfig()
	if err != nil {
		return nil, fmt.Errorf("create temporary SSH host identity: %w", err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(ncmDeviceIP, sshListenPort))
	if err != nil {
		return nil, fmt.Errorf("listen for USB SSH: %w", err)
	}
	server := &usbSSHServer{
		listener:    listener,
		connections: make(map[net.Conn]struct{}),
	}
	go server.serve(serverConfig)
	logf("rescue SSH ready at %s:%s; client authentication is disabled on USB NCM", ncmDeviceIP, sshListenPort)
	return server, nil
}

// newSSHServerConfig 在内存生成每次启动不同的主机密钥，不读取或保存密钥文件
func newSSHServerConfig() (*ssh.ServerConfig, error) {
	// SSH 协议要求服务端主机密钥；客户端认证关闭，仅限直连 USB NCM 的救援网络
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		return nil, err
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	return config, nil
}

// serve 接受 SSH 连接并为每个客户端建立独立处理协程
func (s *usbSSHServer) serve(config *ssh.ServerConfig) {
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if !closed {
				logf("accept USB SSH connection: %v", err)
			}
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = connection.Close()
			return
		}
		s.connections[connection] = struct{}{}
		s.mu.Unlock()
		go func() {
			defer func() {
				s.mu.Lock()
				delete(s.connections, connection)
				s.mu.Unlock()
				_ = connection.Close()
			}()
			serveSSHConnection(connection, config)
		}()
	}
}

// close 停止监听并关闭已有连接，使切根前可以完整回收救援 SSH
func (s *usbSSHServer) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	_ = s.listener.Close()
	for connection := range s.connections {
		_ = connection.Close()
	}
}

// serveSSHConnection 完成 SSH 握手，只接受 session 通道
func serveSSHConnection(connection net.Conn, config *ssh.ServerConfig) {
	client, channels, requests, err := ssh.NewServerConn(connection, config)
	if err != nil {
		logf("SSH handshake failed: %v", err)
		return
	}
	logf("USB SSH client connected as %s from %s", client.User(), client.RemoteAddr())
	go ssh.DiscardRequests(requests)
	for channelRequest := range channels {
		if channelRequest.ChannelType() != "session" {
			_ = channelRequest.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		channel, channelRequests, err := channelRequest.Accept()
		if err != nil {
			logf("accept SSH session: %v", err)
			continue
		}
		go serveSSHSession(channel, channelRequests)
	}
}

// serveSSHSession 处理终端、shell 和单条 exec 请求
func serveSSHSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	var terminal *sshTerminalRequest
	commandStarted := false
	for request := range requests {
		switch request.Type {
		case "pty-req":
			var requested sshTerminalRequest
			if err := ssh.Unmarshal(request.Payload, &requested); err != nil {
				_ = request.Reply(false, nil)
				continue
			}
			terminal = &requested
			_ = request.Reply(true, nil)
		case "shell":
			if commandStarted {
				_ = request.Reply(false, nil)
				continue
			}
			commandStarted = true
			_ = request.Reply(true, nil)
			go runSSHCommand(channel, terminal)
		case "exec":
			if commandStarted {
				_ = request.Reply(false, nil)
				continue
			}
			var command struct{ Command string }
			if err := ssh.Unmarshal(request.Payload, &command); err != nil {
				_ = request.Reply(false, nil)
				continue
			}
			commandStarted = true
			_ = request.Reply(true, nil)
			go runSSHCommand(channel, terminal, "-c", command.Command)
		case "env", "window-change":
			_ = request.Reply(true, nil)
		default:
			_ = request.Reply(false, nil)
		}
	}
	_ = channel.Close()
}

// runSSHCommand 在无 PTY 时直连通道；有 PTY 时双向转发终端数据
func runSSHCommand(channel ssh.Channel, terminal *sshTerminalRequest, args ...string) {
	defer channel.Close()
	if terminal == nil {
		command := exec.Command("/bbin/gosh", args...)
		command.Stdin, command.Stdout, command.Stderr = channel, channel, channel
		finishSSHCommand(channel, command.Run())
		return
	}

	terminalPTY, err := pty.New()
	if err != nil {
		// PTY 分配错误只返回给当前 SSH 用户
		_, _ = fmt.Fprintf(channel, "initramfs: allocate terminal: %v\r\n", err)
		finishSSHCommand(channel, err)
		return
	}
	defer terminalPTY.Ptm.Close()
	defer terminalPTY.Pts.Close()
	requestedSize := &termios.Winsize{
		Row: uint16(terminal.Rows), Col: uint16(terminal.Columns),
		Xpixel: uint16(terminal.XPixel), Ypixel: uint16(terminal.YPixel),
	}
	if err := termios.SetWinSize(terminalPTY.Pts.Fd(), requestedSize); err != nil {
		logf("set USB SSH terminal size: %v", err)
	}
	terminalPTY.Command("/bbin/gosh", args...)
	if terminal.Term != "" {
		terminalPTY.C.Env = append(os.Environ(), "TERM="+terminal.Term)
	}
	if err := terminalPTY.C.Start(); err != nil {
		// shell 启动错误只返回给当前 SSH 用户
		_, _ = fmt.Fprintf(channel, "initramfs: start shell: %v\r\n", err)
		finishSSHCommand(channel, err)
		return
	}
	go func() {
		_, _ = io.Copy(terminalPTY.Ptm, channel)
	}()
	go func() {
		_, _ = io.Copy(channel, terminalPTY.Ptm)
	}()
	finishSSHCommand(channel, terminalPTY.C.Wait())
}

// finishSSHCommand 将救援命令的退出状态按 SSH exit-status 请求返回给客户端
func finishSSHCommand(channel ssh.Channel, err error) {
	var status uint32
	if err != nil {
		status = 1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			status = uint32(exitErr.ExitCode())
		}
	}
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ ExitStatus uint32 }{status}))
}

// configureUSBNCMFunction 设置设备/主机 MAC，并把 NCM 加入已配置的 gadget
func configureUSBNCMFunction() error {
	function := filepath.Join(usbGadgetPath, "functions", ncmFunctionName)
	link := filepath.Join(usbGadgetPath, "configs/c.1", ncmFunctionName)
	if err := os.MkdirAll(function, 0o755); err != nil {
		return fmt.Errorf("create NCM function: %w", err)
	}
	for _, attribute := range []struct{ name, value string }{
		{"dev_addr", ncmDeviceMAC},
		{"host_addr", ncmHostMAC},
	} {
		if err := writeAttribute(filepath.Join(function, attribute.name), attribute.value); err != nil {
			return fmt.Errorf("set NCM %s: %w", attribute.name, err)
		}
	}
	if err := ensureGadgetFunction(ncmFunctionName); err != nil {
		return err
	}
	if err := writeAttribute(filepath.Join(usbGadgetPath, "configs/c.1/strings/0x409/configuration"), "CDC ACM console + NCM"); err != nil {
		// 描述配置失败时移除链接，保证 CDC ACM 仍可单独启动
		_ = os.Remove(link)
		return fmt.Errorf("describe USB configuration: %w", err)
	}
	return nil
}

// configureUSBNetwork 等待 usb0 出现，分配链路本地地址并启用接口
func configureUSBNetwork(timeout time.Duration) error {
	var link netlink.Link
	deadline := time.Now().Add(timeout)
	for {
		found, err := netlink.LinkByName(ncmInterfaceName)
		if err == nil {
			link = found
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("network interface %s did not appear: %w", ncmInterfaceName, err)
		}
		time.Sleep(usbPollInterval)
	}

	ip := net.ParseIP(ncmDeviceIP).To4()
	if ip == nil {
		return fmt.Errorf("invalid USB network address %q", ncmDeviceIP)
	}
	address := &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: net.CIDRMask(ncmNetworkPrefixLength, 32)}}
	if err := netlink.AddrReplace(link, address); err != nil {
		return fmt.Errorf("assign %s/%d to %s: %w", ncmDeviceIP, ncmNetworkPrefixLength, ncmInterfaceName, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring up %s: %w", ncmInterfaceName, err)
	}
	return nil
}

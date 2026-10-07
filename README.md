# M1882 Go initramfs

[English README](README.EN.md)

Meizu 16th（M1882）的 Go initramfs

## 文件结构

```text
cmd/init/
  main.go     PID 1 启动流程
  config.go   根分区和 USB 配置
  mount.go    块设备枚举、根分区解析、挂载和 switch_root
  bootmenu.go Linux rootfs 探测、系统选择和维护入口
  display.go  tty0/systemd-boot 风格文本菜单
  input.go    evdev 按键状态读取
  logging.go  串口及内核日志
  usbserial.go USB CDC ACM 串口
  usbssh.go   USB NCM 网络及 SSH 服务
  reboot.go   reboot 命令
go.mod
go.sum
Makefile
```

## 内核配置

```text
CONFIG_BLK_DEV_INITRD=y          # 支持从外部 initramfs 启动
CONFIG_RD_LZ4=y                  # 解压 LZ4 压缩的 initramfs
CONFIG_BLOCK=y                   # 块设备层，供根分区扫描使用
CONFIG_DEVTMPFS=y                # 自动创建设备节点（/dev）
CONFIG_PROC_FS=y                 # 提供 /proc
CONFIG_SYSFS=y                   # 提供 /sys 设备信息
CONFIG_TMPFS=y                   # 支持内存临时文件系统（/tmp、/dev/shm）
CONFIG_UNIX98_PTYS=y             # 提供 SSH 交互终端所需的伪终端
CONFIG_INPUT=y                   # Linux 输入子系统
CONFIG_INPUT_EVDEV=y             # 通过 evdev 读取音量键和电源键
CONFIG_TTY=y                     # TTY 和串口终端支持
CONFIG_VT=y                      # 虚拟终端支持
CONFIG_VT_CONSOLE=y              # 将内核控制台连接到虚拟终端（tty0）
CONFIG_FB=y                      # 帧缓冲支持
CONFIG_DRM_FBDEV_EMULATION=y     # 为 DRM 显示设备提供 fbdev 接口
CONFIG_FRAMEBUFFER_CONSOLE=y     # 在帧缓冲上显示文本控制台菜单
CONFIG_USB_GADGET=y              # USB 设备端（gadget）框架
CONFIG_CONFIGFS_FS=y             # 提供配置 USB gadget 所需的 configfs
CONFIG_USB_CONFIGFS=y            # 通过 configfs 配置 USB gadget
CONFIG_USB_CONFIGFS_ACM=y        # USB CDC ACM 串口功能
CONFIG_USB_CONFIGFS_NCM=y        # USB NCM 网络功能
CONFIG_NET=y                     # 网络协议栈
CONFIG_INET=y                    # IPv4，供 USB NCM 和 SSH 使用
```

## 构建

在项目目录执行：

```sh
make
```

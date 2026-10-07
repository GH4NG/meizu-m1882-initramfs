# M1882 Go initramfs

[中文说明](README.md)

Go initramfs project for the Meizu 16th (M1882)

## Files

```text
cmd/init/
  main.go     PID 1 boot flow
  config.go   root partition and USB settings
  mount.go    block enumeration, root resolution, mount, and switch_root
  bootmenu.go Linux root discovery, system selection, and maintenance actions
  display.go  tty0/systemd-boot-style text menu
  input.go    evdev key-state reads
  logging.go  serial and kernel logging
  usbserial.go USB CDC ACM serial console
  usbssh.go   USB NCM networking and SSH service
  reboot.go   reboot command
go.mod
go.sum
Makefile
```

## Kernel configuration

```text
CONFIG_BLK_DEV_INITRD=y          # Boot from an external initramfs
CONFIG_RD_LZ4=y                  # Decompress an LZ4-compressed initramfs
CONFIG_BLOCK=y                   # Block layer for root partition scanning
CONFIG_DEVTMPFS=y                # Automatically create device nodes in /dev
CONFIG_PROC_FS=y                 # Provide /proc
CONFIG_SYSFS=y                   # Provide device information in /sys
CONFIG_TMPFS=y                   # RAM-backed temporary filesystems (/tmp, /dev/shm)
CONFIG_UNIX98_PTYS=y             # Pseudo-terminals for interactive SSH sessions
CONFIG_INPUT=y                   # Linux input subsystem
CONFIG_INPUT_EVDEV=y             # Read volume and power keys through evdev
CONFIG_TTY=y                     # TTY and serial terminal support
CONFIG_VT=y                      # Virtual terminal support
CONFIG_VT_CONSOLE=y              # Connect the kernel console to a virtual terminal (tty0)
CONFIG_FB=y                      # Framebuffer support
CONFIG_DRM_FBDEV_EMULATION=y     # Provide an fbdev interface for DRM displays
CONFIG_FRAMEBUFFER_CONSOLE=y     # Show the text boot menu on the framebuffer console
CONFIG_USB_GADGET=y              # USB device-side (gadget) framework
CONFIG_CONFIGFS_FS=y             # Provide configfs for USB gadget setup
CONFIG_USB_CONFIGFS=y            # Configure USB gadgets through configfs
CONFIG_USB_CONFIGFS_ACM=y        # USB CDC ACM serial function
CONFIG_USB_CONFIGFS_NCM=y        # USB NCM networking function
CONFIG_NET=y                     # Networking stack
CONFIG_INET=y                    # IPv4 for USB NCM and SSH
```

## Build

Run in this project directory:

```sh
make
```

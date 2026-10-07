package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	urootmount "github.com/u-root/u-root/pkg/mount"
)

const (
	partitionFirmwarePath      = "/run/initramfs/firmware"
	firmwareSearchPath         = partitionFirmwarePath + "/files"
	firmwarePathParameter      = "/sys/module/firmware_class/parameters/path"
	modemWLANCalibrationSHA256 = "5511af71abbadfc4254305d23bb9f3339a30a6611da4ca7dc9758ae62c683e7e"
)

// setupPartitionFirmware 只读挂载原厂固件分区。
func setupPartitionFirmware() error {
	if err := os.MkdirAll("/run", 0o755); err != nil {
		return err
	}
	same, err := urootmount.SameFilesystem("/", "/run")
	if err != nil {
		return fmt.Errorf("check runtime mount: %w", err)
	}
	if same {
		if _, err := urootmount.Mount("tmpfs", "/run", "tmpfs", "mode=0755", syscall.MS_NOSUID|syscall.MS_NODEV); err != nil {
			return fmt.Errorf("mount runtime tmpfs: %w", err)
		}
	}
	if err := os.MkdirAll(firmwareSearchPath, 0o755); err != nil {
		return err
	}

	var failures error
	count := 0
	for _, source := range []struct {
		label, filesystem, options, directory string
	}{
		{"modem", "vfat", "", "image"},
		// ext4 即使只读挂载也可能回放日志；noload 禁止对原厂分区进行恢复写入。
		{"vendor", "ext4", "noload", "firmware"},
	} {
		device, err := waitForRootDevice("PARTLABEL="+source.label, rootDeviceWaitTimeout)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		mountPath := filepath.Join(partitionFirmwarePath, source.label)
		flags := uintptr(syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC)
		_, err = urootmount.Mount(device, mountPath, source.filesystem, source.options, flags,
			func() error { return os.MkdirAll(mountPath, 0o755) })
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("mount %s read-only: %w", source.label, err))
			continue
		}
		linked, err := linkPartitionFirmware(filepath.Join(mountPath, source.directory), source.label == "modem")
		count += linked
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("index %s firmware: %w", source.label, err))
		}
		logf("firmware partition %s mounted read-only; indexed %d files", source.label, linked)
	}
	if count == 0 {
		return errors.Join(failures, errors.New("no partition firmware available; keeping rootfs firmware search paths"))
	}
	// 固件加载器优先读取此目录；标准 /lib/firmware 搜索路径仍作为回退。
	if err := os.WriteFile(firmwarePathParameter, []byte(firmwareSearchPath+"\n"), 0); err != nil {
		failures = errors.Join(failures, fmt.Errorf("set kernel firmware search path: %w", err))
	}
	return failures
}

// linkPartitionFirmware 只索引普通文件，不跟随原分区内的符号链接。
// 原厂扁平文件名同时映射为主线请求的 qcom/ 与 qcom/sdm845/ 路径。
func linkPartitionFirmware(sourceRoot string, modem bool) (int, error) {
	count := 0
	var failures error
	err := fs.WalkDir(os.DirFS(sourceRoot), ".", func(relative string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			failures = errors.Join(failures, walkErr)
			return nil
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		path := filepath.Join(sourceRoot, relative)
		if modem {
			relative = strings.ToLower(relative)
		}
		// 不公开其他机型的 bdwlan；M1882 校准数据校验通过后才建立链接。
		if strings.HasPrefix(strings.ToLower(filepath.Base(relative)), "bdwlan") {
			if !modem || relative != "bdwlan.b01" {
				return nil
			}
			if err := verifyModemCalibration(path); err != nil {
				failures = errors.Join(failures, err)
				return nil
			}
		}
		names := []string{relative}
		if !strings.ContainsRune(relative, filepath.Separator) {
			names = append(names, filepath.Join("qcom", relative), filepath.Join("qcom/sdm845", relative))
		}
		for _, name := range names {
			destination := filepath.Join(firmwareSearchPath, name)
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				failures = errors.Join(failures, err)
				continue
			}
			if err := os.Symlink(path, destination); err != nil && !errors.Is(err, os.ErrExist) {
				failures = errors.Join(failures, err)
			}
		}
		count++
		return nil
	})
	return count, errors.Join(failures, err)
}

func verifyModemCalibration(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != modemWLANCalibrationSHA256 {
		return fmt.Errorf("M1882 Wi-Fi calibration checksum mismatch: %s", got)
	}
	return nil
}

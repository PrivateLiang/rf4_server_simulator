package aibridge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// The instance root is provisioned by installation. Only the immediate data
// directory may be created here; publication confirms its entry in that root.
func prepareMasterKeyDirectory(dir string) error {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := checkCredentialDirectory(filepath.Dir(absolute)); err != nil {
		return err
	}
	if err := makeCredentialDirectory(absolute); err != nil {
		return err
	}
	return checkCredentialDirectory(absolute)
}

func makeCredentialDirectory(dir string) error {
	err := os.Mkdir(dir, 0o700)
	if errors.Is(err, os.ErrExist) {
		info, statErr := os.Stat(dir)
		if statErr != nil {
			return statErr
		}
		if info.IsDir() {
			return nil
		}
	}
	return err
}

func prepareCredentialDirectory(dir string) error {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	dataDir := filepath.Dir(absolute)
	instanceDir := filepath.Dir(dataDir)
	if credentialDirSync == nil && instanceDir != dataDir {
		if err := syncCredentialDirectory(instanceDir); err != nil {
			return fmt.Errorf("同步实例数据目录的父级失败: %w", err)
		}
	}
	if err := makeCredentialDirectory(absolute); err != nil {
		return err
	}
	if credentialDirSync != nil {
		return nil
	}
	return checkCredentialDirectory(absolute)
}

func checkCredentialDirectory(dir string) error {
	handle, err := openCredentialDirectory(dir)
	if err != nil {
		return err
	}
	return windows.CloseHandle(handle)
}

func syncCredentialDirectory(dir string) error {
	handle, err := openCredentialDirectory(dir)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := windows.FlushFileBuffers(handle); err != nil {
		return fmt.Errorf("刷新目录: %w", err)
	}
	return nil
}

func openCredentialDirectory(dir string) (handle windows.Handle, err error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return 0, err
	}
	volume := filepath.VolumeName(absolute)
	if len(volume) != 2 || volume[1] != ':' {
		return 0, ErrCredentialVolumeUnsupported
	}
	path, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return 0, err
	}
	opened, err := windows.CreateFile(path, windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, fmt.Errorf("打开可写目录句柄: %w", err)
	}
	defer func() {
		if err != nil {
			_ = windows.CloseHandle(opened)
		}
	}()

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(opened, &info); err != nil {
		return 0, fmt.Errorf("读取目录句柄: %w", err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return 0, ErrCredentialVolumeUnsupported
	}

	finalPath := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(opened, &finalPath[0], uint32(len(finalPath)), 0)
	if err != nil {
		return 0, fmt.Errorf("%w: 解析目录路径: %w", ErrCredentialVolumeUnsupported, err)
	}
	if n == 0 || n >= uint32(len(finalPath)) ||
		!strings.EqualFold(windows.UTF16ToString(finalPath[:n]), `\\?\`+absolute) {
		return 0, ErrCredentialVolumeUnsupported
	}
	var volumeRoot [windows.MAX_PATH]uint16
	if err := windows.GetVolumePathName(path, &volumeRoot[0], uint32(len(volumeRoot))); err != nil {
		return 0, fmt.Errorf("%w: 读取卷挂载点: %w", ErrCredentialVolumeUnsupported, err)
	}
	if !strings.EqualFold(windows.UTF16ToString(volumeRoot[:]), volume+`\`) ||
		windows.GetDriveType(&volumeRoot[0]) != windows.DRIVE_FIXED {
		return 0, ErrCredentialVolumeUnsupported
	}
	var fileSystem [32]uint16
	if err := windows.GetVolumeInformationByHandle(opened, nil, 0, nil, nil, nil,
		&fileSystem[0], uint32(len(fileSystem))); err != nil {
		return 0, fmt.Errorf("%w: 读取目录所在卷: %w", ErrCredentialVolumeUnsupported, err)
	}
	if !strings.EqualFold(windows.UTF16ToString(fileSystem[:]), "NTFS") {
		return 0, ErrCredentialVolumeUnsupported
	}
	return opened, nil
}

// CredentialInstallIssue classifies Win32 failures without exposing paths or
// OS diagnostics to the management page.
func CredentialInstallIssue(err error) error {
	switch {
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION), errors.Is(err, windows.ERROR_LOCK_VIOLATION):
		return ErrCredentialDirectoryBusy
	case errors.Is(err, windows.ERROR_DISK_FULL), errors.Is(err, windows.ERROR_HANDLE_DISK_FULL):
		return ErrCredentialDiskFull
	default:
		return err
	}
}

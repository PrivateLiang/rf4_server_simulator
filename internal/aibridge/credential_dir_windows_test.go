package aibridge

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestDR109WindowsDirectoryFlush(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "credentials")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{parent, child} {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			path, err := windows.UTF16PtrFromString(dir)
			if err != nil {
				t.Fatal(err)
			}
			h, err := windows.CreateFile(path, windows.GENERIC_READ,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
				windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
			if err != nil {
				t.Fatal(err)
			}
			readErr := windows.FlushFileBuffers(h)
			windows.CloseHandle(h)
			if !errors.Is(readErr, windows.ERROR_ACCESS_DENIED) {
				t.Fatalf("只读句柄刷新 = %v", readErr)
			}
			if err := syncCredentialDirectory(dir); err != nil {
				t.Fatalf("原生可写句柄刷新: %v", err)
			}
		})
	}
	t.Run("实际共享冲突", func(t *testing.T) {
		path, err := windows.UTF16PtrFromString(child)
		if err != nil {
			t.Fatal(err)
		}
		h, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ,
			nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer windows.CloseHandle(h)
		if err := CredentialInstallIssue(syncCredentialDirectory(child)); !errors.Is(err, ErrCredentialDirectoryBusy) {
			t.Fatalf("目录共享冲突应拒绝并分类，得到 %v", err)
		}
	})
	if err := syncCredentialDirectory(`\\localhost\not-a-local-volume`); !errors.Is(err, ErrCredentialVolumeUnsupported) {
		t.Fatalf("UNC 应拒绝，得到 %v", err)
	}
	file := filepath.Join(child, "file")
	if err := os.WriteFile(file, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncCredentialDirectory(file); !errors.Is(err, ErrCredentialVolumeUnsupported) {
		t.Fatalf("普通文件不能替代目录，得到 %v", err)
	}
}

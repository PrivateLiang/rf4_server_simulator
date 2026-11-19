//go:build !windows

package aibridge

import "os"

func prepareMasterKeyDirectory(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

func prepareCredentialDirectory(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

func syncCredentialDirectory(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func CredentialInstallIssue(err error) error {
	return err
}

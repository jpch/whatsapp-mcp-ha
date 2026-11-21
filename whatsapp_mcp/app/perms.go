package main

import (
	"errors"
	"io/fs"
	"os"
	"strings"
)

// sensitiveDataFile reports the files in the data folder that hold the WhatsApp session, the message
// store or the generated token (SQLite also writes -wal, -shm and -journal files next to a database).
func sensitiveDataFile(name string) bool {
	for _, base := range []string{"whatsmeow.db", "messages.db", authTokenFile, tokenShownFile, sendTokenFile, sendTokenShownFile} {
		if name == base || strings.HasPrefix(name, base+"-") || strings.HasPrefix(name, base+".") {
			return true
		}
	}
	return false
}

// tightenDataDir makes the data folder 0700 and the session, message and token files in it 0600,
// including files left by older versions. It returns how many entries were changed.
func tightenDataDir(dir string) (int, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	changed := 0
	var errs []error
	if n, err := chmodIn(root, ".", 0o700); err != nil {
		errs = append(errs, err)
	} else {
		changed += n
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return changed, errors.Join(append(errs, err)...)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !sensitiveDataFile(e.Name()) {
			continue // never follow symlinks, leave other files (e.g. options.json) alone
		}
		n, err := chmodIn(root, e.Name(), 0o600)
		changed += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return changed, errors.Join(errs...)
}

// tightenTree makes every folder under dir 0700 and every regular file 0600. Symlinks are skipped and
// os.Root keeps every change inside dir; dir itself must not be a symlink. A missing dir is not an error.
func tightenTree(dir string) (int, error) {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	root, err := OpenRealRoot(dir)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	changed := 0
	var errs []error
	walkErr := fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		var mode fs.FileMode
		switch {
		case d.IsDir():
			mode = 0o700
		case d.Type().IsRegular():
			mode = 0o600
		default:
			return nil
		}
		n, err := chmodIn(root, p, mode)
		changed += n
		if err != nil {
			errs = append(errs, err)
		}
		return nil
	})
	return changed, errors.Join(append(errs, walkErr)...)
}

// chmodIn sets mode on name inside root when its permission bits differ. It refuses symlinks.
func chmodIn(root *os.Root, name string, mode fs.FileMode) (int, error) {
	st, err := root.Lstat(name)
	if err != nil {
		return 0, err
	}
	if st.Mode()&fs.ModeSymlink != 0 || st.Mode().Perm() == mode {
		return 0, nil
	}
	if err := root.Chmod(name, mode); err != nil {
		return 0, err
	}
	return 1, nil
}

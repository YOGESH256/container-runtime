// Package fsync writes files so they survive a power loss.
//
// A plain write followed by rename can come back after a crash as an empty
// file: the rename reached the disk but the data was still in the page
// cache. The cluster tests hit exactly that when a node was powered off.
package fsync

import (
	"os"
	"path/filepath"
)

// WriteFile atomically replaces path with data: write a temporary file,
// fsync it, rename it over path, then fsync the directory so the rename
// itself is durable.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // no-op once renamed
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return Dir(filepath.Dir(path))
}

// Dir fsyncs a directory, making renames and creations inside it durable.
func Dir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

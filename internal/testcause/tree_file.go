package testcause

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// readTreeFile reads name under tree only when it is a regular file of at
// most limit bytes. The work tree is written by the implementing AI, so a
// symlink is not followed (in or out of the tree), and a FIFO or device is
// refused instead of blocking the read. os.Root resolves a symlink that stays
// in the tree even with O_NOFOLLOW, so the name is checked with Lstat first
// and the opened file must be that same file.
func readTreeFile(tree, name string, limit int64) ([]byte, error) {
	root, err := os.OpenRoot(tree)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("通常のファイルではありません")
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		return nil, errors.New("通常のファイルではありません")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("ファイルが大きすぎます")
	}
	return data, nil
}

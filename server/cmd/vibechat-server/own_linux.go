package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ownLikeData: run as root (setup over SSH), a file under the data folder is handed to the folder's owner (the
// vibechat user), with every folder between them, so the service can read it.
func ownLikeData(data, file string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	st, err := os.Stat(data)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Uid == 0 {
		return nil
	}
	rel, err := filepath.Rel(data, file)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil
	}
	cur := data
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		if err := os.Lchown(cur, int(sys.Uid), int(sys.Gid)); err != nil {
			return err
		}
	}
	return nil
}

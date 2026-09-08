package runtimehost

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Snapshot current tracked and untracked source, not HEAD or a different Windows
// checkout. Git metadata, ignored build output and credentials are not shipped.
func snapshotRepository(ctx context.Context, directory string) ([]byte, string, error) {
	real, err := filepath.EvalSymlinks(directory)
	if err != nil || real != directory {
		return nil, "", fmt.Errorf("repository must not be a symlink")
	}
	cmd := exec.CommandContext(ctx, "git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	cmd.Dir = directory
	names, err := cmd.Output()
	if err != nil {
		return nil, "", err
	}
	var buffer bytes.Buffer
	zip := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(zip)
	seen := map[string]bool{}
	var total int64
	for _, name := range strings.Split(string(names), "\x00") {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if filepath.IsAbs(name) || strings.Contains(name, "\\") || strings.Contains(name, ":") || filepath.ToSlash(filepath.Clean(name)) != name || name == ".." || strings.HasPrefix(name, "../") {
			return nil, "", fmt.Errorf("unsafe source path")
		}
		for _, part := range strings.Split(name, "/") {
			if strings.EqualFold(part, ".git") {
				return nil, "", fmt.Errorf("git metadata not allowed")
			}
		}
		file := filepath.Join(directory, filepath.FromSlash(name))
		resolved, e := filepath.EvalSymlinks(file)
		if os.IsNotExist(e) {
			continue
		} // Tracked deletions remain deleted.
		if e != nil || resolved != file {
			return nil, "", fmt.Errorf("source symlink not supported: %s", name)
		}
		info, e := os.Lstat(file)
		if e != nil {
			return nil, "", e
		}
		if !info.Mode().IsRegular() {
			return nil, "", fmt.Errorf("non-file source: %s", name)
		}
		total += info.Size()
		if total > 512<<20 {
			return nil, "", fmt.Errorf("source snapshot exceeds 512 MiB")
		}
		h := &tar.Header{Name: name, Mode: 0644, Size: info.Size()}
		if info.Mode()&0111 != 0 {
			h.Mode = 0755
		}
		if e = archive.WriteHeader(h); e != nil {
			return nil, "", e
		}
		f, e := os.Open(file)
		if e != nil {
			return nil, "", e
		}
		_, e = io.CopyN(archive, f, info.Size())
		f.Close()
		if e != nil {
			return nil, "", e
		}
	}
	if !seen["build.ps1"] || !seen["CMakeLists.txt"] {
		return nil, "", fmt.Errorf("repository build entry missing")
	}
	if err = archive.Close(); err != nil {
		return nil, "", err
	}
	if err = zip.Close(); err != nil {
		return nil, "", err
	}
	if buffer.Len() > 64<<20 {
		return nil, "", fmt.Errorf("compressed source exceeds 64 MiB")
	}
	data := buffer.Bytes()
	return data, fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

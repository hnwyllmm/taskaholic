package runtimehost

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRepositorySnapshotIncludesUncommittedAndExcludesIgnored(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, e := c.CombinedOutput(); e != nil {
			t.Fatal(e, string(out))
		}
	}
	git("init", "-q")
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("build.ps1", "original")
	write("CMakeLists.txt", "project")
	write(".gitignore", "secret\n")
	git("add", ".")
	write("build.ps1", "modified")
	write("new.cpp", "new")
	write("secret", "private")
	archive, hash, err := snapshotRepository(context.Background(), dir)
	if err != nil || len(hash) != 64 {
		t.Fatal(err)
	}
	zip, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer zip.Close()
	reader := tar.NewReader(zip)
	files := map[string]string{}
	for {
		h, e := reader.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		data, e := io.ReadAll(reader)
		if e != nil {
			t.Fatal(e)
		}
		files[h.Name] = string(data)
	}
	if files["build.ps1"] != "modified" || files["new.cpp"] != "new" {
		t.Fatal(files)
	}
	if _, ok := files["secret"]; ok {
		t.Fatal("ignored content leaked")
	}
	if _, ok := files[".git"]; ok {
		t.Fatal("git metadata leaked")
	}
	write("build.ps1", "modified again")
	_, next, err := snapshotRepository(context.Background(), dir)
	if err != nil || next == hash {
		t.Fatal("changed scripts not captured", err)
	}
	if err = os.Symlink(filepath.Join(dir, "secret"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, _, err = snapshotRepository(context.Background(), dir); err == nil {
		t.Fatal("symlink accepted")
	}
}

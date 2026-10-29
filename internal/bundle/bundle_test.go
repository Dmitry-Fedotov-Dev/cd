package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type item struct {
	name, body string
	typ        byte
	link       string
}

func archive(t *testing.T, items ...item) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, it := range items {
		h := &tar.Header{Name: it.name, Typeflag: it.typ, Mode: 0o644, Linkname: it.link}
		if it.typ == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(it.body))
		}
		if h.Typeflag == tar.TypeDir {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(it.body))
		}
	}
	tw.Close()
	gz.Close()
	return &buf
}

func TestExtractStripsPrefix(t *testing.T) {
	dir := t.TempDir()
	err := Extract(archive(t,
		item{name: "app/", typ: tar.TypeDir},
		item{name: "app/package.json", body: "{}"},
		item{name: "app/src/index.ts", body: "x"},
	), dir, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "src", "index.ts")); err != nil || string(b) != "x" {
		t.Fatalf("file: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "app")); !os.IsNotExist(err) {
		t.Fatal("prefix not stripped")
	}
}

func TestExtractRejects(t *testing.T) {
	cases := map[string]*bytes.Buffer{
		"symlink":  archive(t, item{name: "a", typ: tar.TypeSymlink, link: "/etc/passwd"}),
		"hardlink": archive(t, item{name: "a", typ: tar.TypeLink, link: "b"}),
		"dotdot":   archive(t, item{name: "../evil", body: "x"}),
		"absolute": archive(t, item{name: "/etc/evil", body: "x"}),
		"device":   archive(t, item{name: "dev", typ: tar.TypeChar}),
		"empty":    archive(t),
		"dup":      archive(t, item{name: "a", body: "1"}, item{name: "a", body: "2"}),
	}
	for name, buf := range cases {
		if err := Extract(buf, t.TempDir(), DefaultLimits); !errors.Is(err, ErrRejected) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if err := Extract(bytes.NewBufferString("plain text"), t.TempDir(), DefaultLimits); !errors.Is(err, ErrRejected) {
		t.Errorf("not gzip: %v", err)
	}
}

func TestExtractLimits(t *testing.T) {
	big := archive(t, item{name: "a", body: string(make([]byte, 2048))})
	if err := Extract(big, t.TempDir(), Limits{MaxFiles: 10, MaxBytes: 1024}); !errors.Is(err, ErrRejected) {
		t.Errorf("size: %v", err)
	}
	many := archive(t, item{name: "a", body: "1"}, item{name: "b", body: "2"})
	if err := Extract(many, t.TempDir(), Limits{MaxFiles: 1, MaxBytes: 1024}); !errors.Is(err, ErrRejected) {
		t.Errorf("files: %v", err)
	}
}

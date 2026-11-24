package iso9660_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

// TestReadJolietDeepDirectories reads directories more than one level deep
// in a Joliet image without Rock Ridge, which are only found through the
// path table.
func TestReadJolietDeepDirectories(t *testing.T) {
	gz, err := os.ReadFile("./testdata/joliet-deep.iso.gz")
	if err != nil {
		t.Fatal(err)
	}
	r, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(t.TempDir() + "/joliet-deep.iso")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	size, err := io.Copy(f, io.LimitReader(r, 1<<20))
	if err != nil {
		t.Fatal(err)
	}

	fs, err := iso9660.Read(file.New(f, true), size, 0, 2048)
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"one", "one/two", "one/two/three"} {
		if _, err := fs.ReadDir(p); err != nil {
			t.Errorf("reading %s: %v", p, err)
		}
	}

	got, err := fs.ReadFile("one/two/three/deep.txt")
	if err != nil || string(got) != "deep\n" {
		t.Errorf("reading one/two/three/deep.txt: %q, %v", got, err)
	}
}

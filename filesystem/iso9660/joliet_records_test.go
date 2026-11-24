package iso9660_test

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

// jolietTree walks a Joliet tree by its directory records alone, as readers
// which don't use the path table do, and returns every path in it.
func jolietTree(t *testing.T, f *os.File) []string {
	t.Helper()
	const blocksize = 2048

	read := func(lba, size uint32) []byte {
		b := make([]byte, size)
		if _, err := f.ReadAt(b, int64(lba)*blocksize); err != nil {
			t.Fatalf("reading %d bytes at block %d: %v", size, lba, err)
		}
		return b
	}

	var root []byte
	for sector := uint32(16); root == nil; sector++ {
		d := read(sector, blocksize)
		switch {
		case d[0] == 255:
			t.Fatal("no Joliet volume descriptor")
		case d[0] == 2 && string(d[88:90]) == "%/":
			root = d[156:190]
		}
	}

	var paths []string
	var walk func(dir string, lba, size uint32, depth int)
	walk = func(dir string, lba, size uint32, depth int) {
		if depth > 16 {
			t.Fatalf("%s: too deep; a directory record points back up", dir)
		}
		extent := read(lba, size)
		for i := 0; i < len(extent); {
			length := int(extent[i])
			if length == 0 {
				i += blocksize - i%blocksize
				continue
			}
			r := extent[i : i+length]
			i += length

			id := r[33 : 33+int(r[32])]
			if len(id) == 1 && id[0] <= 1 {
				continue // "." and ".."
			}
			units := make([]uint16, len(id)/2)
			for j := range units {
				units[j] = binary.BigEndian.Uint16(id[2*j:])
			}
			p := dir + "/" + strings.TrimSuffix(string(utf16.Decode(units)), ";1")
			paths = append(paths, p)
			if r[25]&0x02 != 0 {
				walk(p, binary.LittleEndian.Uint32(r[2:6]), binary.LittleEndian.Uint32(r[10:14]), depth+1)
			}
		}
	}
	walk("", binary.LittleEndian.Uint32(root[2:6]), binary.LittleEndian.Uint32(root[10:14]), 0)

	sort.Strings(paths)
	return paths
}

// TestJolietSubdirectoryRecords checks the Joliet tree's subdirectory records
// point at Joliet directories, so readers following them see Joliet names.
func TestJolietSubdirectoryRecords(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options iso9660.FinalizeOptions
	}{
		{"joliet", iso9660.FinalizeOptions{Joliet: true}},
		{"rock ridge and joliet", iso9660.FinalizeOptions{RockRidge: true, Joliet: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.MkdirAll(filepath.Join(workspace, "one", "Two Words", "three"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(workspace, "other"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workspace, "one", "Two Words", "three", "Deep File.txt"), []byte("deep\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			f, err := os.Create(filepath.Join(t.TempDir(), "image.iso"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			fs, err := iso9660.Create(file.New(f, false), 0, 0, 2048, workspace)
			if err != nil {
				t.Fatal(err)
			}
			if err := fs.Finalize(tc.options); err != nil {
				t.Fatal(err)
			}

			want := []string{
				"/one", "/one/Two Words", "/one/Two Words/three", "/one/Two Words/three/Deep File.txt", "/other",
			}
			if got := jolietTree(t, f); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("the Joliet records give\n%s\nnot\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

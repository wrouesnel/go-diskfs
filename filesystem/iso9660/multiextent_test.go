package iso9660_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"github.com/diskfs/go-diskfs/testhelper"
)

// pattern is n bytes which differ from one place to the next, so a read
// from the wrong place shows.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/2048)
	}
	return b
}

// splitExtent rewrites the directory record of the file of size bytes, in
// the directory extent at lba, as two records: the first of its first
// sectors sectors, flagged as having more, then the rest. That's how
// ECMA-119 records a multi-extent file, as level-3 writers write files of
// 4 GiB or more. It returns the directory's new size, also set in its "."
// record.
func splitExtent(t *testing.T, f *os.File, lba, size uint32, fileSize, sectors int) uint32 {
	t.Helper()
	const blocksize = 2048

	extent := make([]byte, size)
	if _, err := f.ReadAt(extent, int64(lba)*blocksize); err != nil {
		t.Fatalf("reading directory extent at %d: %v", lba, err)
	}

	var out []byte
	split := false
	for i := 0; i < len(extent); {
		length := int(extent[i])
		if length == 0 {
			i += blocksize - i%blocksize
			continue
		}
		record := extent[i : i+length]
		i += length

		if int(binary.LittleEndian.Uint32(record[10:14])) != fileSize || record[25]&0x02 != 0 {
			out = append(out, record...)
			continue
		}

		first := append([]byte{}, record...)
		binary.LittleEndian.PutUint32(first[10:14], uint32(sectors*blocksize))
		binary.BigEndian.PutUint32(first[14:18], uint32(sectors*blocksize))
		first[25] |= 0x80

		second := append([]byte{}, record...)
		location := binary.LittleEndian.Uint32(record[2:6]) + uint32(sectors)
		binary.LittleEndian.PutUint32(second[2:6], location)
		binary.BigEndian.PutUint32(second[6:10], location)
		binary.LittleEndian.PutUint32(second[10:14], uint32(fileSize-sectors*blocksize))
		binary.BigEndian.PutUint32(second[14:18], uint32(fileSize-sectors*blocksize))

		out = append(out, first...)
		out = append(out, second...)
		split = true
	}

	if !split {
		t.Fatalf("no record of a %d byte file in the directory at %d", fileSize, lba)
	}
	if len(out) > blocksize {
		t.Fatalf("the split directory is %d bytes, more than a sector", len(out))
	}

	newSize := uint32(len(out))
	binary.LittleEndian.PutUint32(out[10:14], newSize)
	binary.BigEndian.PutUint32(out[14:18], newSize)

	out = append(out, make([]byte, blocksize-len(out))...)
	if _, err := f.WriteAt(out, int64(lba)*blocksize); err != nil {
		t.Fatalf("writing directory extent at %d: %v", lba, err)
	}
	return newSize
}

// rootExtents are the root directory's extents, in the primary tree and,
// if there's one, the Joliet tree: where each is, its size, and where its
// volume descriptor is.
func rootExtents(t *testing.T, f *os.File) [][3]uint32 {
	t.Helper()
	const blocksize = 2048

	var out [][3]uint32
	for sector := int64(16); ; sector++ {
		d := make([]byte, blocksize)
		if _, err := f.ReadAt(d, sector*blocksize); err != nil {
			t.Fatalf("reading volume descriptor %d: %v", sector, err)
		}
		if d[0] == 255 {
			return out
		}
		if d[0] == 1 || (d[0] == 2 && string(d[88:90]) == "%/") {
			out = append(out, [3]uint32{binary.LittleEndian.Uint32(d[158:162]), binary.LittleEndian.Uint32(d[166:170]), uint32(sector)})
		}
	}
}

// TestReadMultiExtentFile checks a file recorded in several extents reads
// as one file, with all of its contents, in every kind of image.
func TestReadMultiExtentFile(t *testing.T) {
	const (
		blocksize = 2048
		sectors   = 2
	)
	content := pattern(3*blocksize + 100)

	for _, tc := range []struct {
		name    string
		options iso9660.FinalizeOptions
	}{
		{"plain", iso9660.FinalizeOptions{}},
		{"rock ridge", iso9660.FinalizeOptions{RockRidge: true}},
		{"joliet", iso9660.FinalizeOptions{Joliet: true}},
		{"rock ridge and joliet", iso9660.FinalizeOptions{RockRidge: true, Joliet: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "big.dat"), content, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workspace, "small.txt"), []byte("small\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			f, err := os.Create(filepath.Join(t.TempDir(), "image.iso"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			fs, err := iso9660.Create(file.New(f, false), 0, 0, blocksize, workspace)
			if err != nil {
				t.Fatalf("creating the image: %v", err)
			}
			if err := fs.Finalize(tc.options); err != nil {
				t.Fatalf("finalizing the image: %v", err)
			}

			for _, root := range rootExtents(t, f) {
				size := splitExtent(t, f, root[0], root[1], len(content), sectors)
				// The volume descriptor's root record has the new size too.
				both := make([]byte, 8)
				binary.LittleEndian.PutUint32(both, size)
				binary.BigEndian.PutUint32(both[4:], size)
				if _, err := f.WriteAt(both, int64(root[2])*blocksize+166); err != nil {
					t.Fatal(err)
				}
			}

			info, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			read, err := iso9660.Read(file.New(f, true), info.Size(), 0, blocksize)
			if err != nil {
				t.Fatalf("reading the image: %v", err)
			}

			// One entry, the file's whole size.
			entries, err := read.ReadDir(".")
			if err != nil {
				t.Fatalf("reading the root: %v", err)
			}
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
				if strings.EqualFold(e.Name(), "big.dat") || strings.HasPrefix(strings.ToLower(e.Name()), "big.dat") {
					fi, err := e.Info()
					if err != nil {
						t.Fatal(err)
					}
					if fi.Size() != int64(len(content)) {
						t.Errorf("%s is %d bytes, not %d", e.Name(), fi.Size(), len(content))
					}
				}
			}
			if len(entries) != 2 {
				t.Fatalf("the root has %v, not two files", names)
			}

			name := "/big.dat"
			if tc.options == (iso9660.FinalizeOptions{}) {
				name = "/BIG.DAT"
			}

			got, err := read.ReadFile(strings.TrimPrefix(name, "/"))
			if err != nil {
				t.Fatalf("reading %s: %v", name, err)
			}
			if !bytes.Equal(got, content) {
				t.Errorf("%s read %d bytes, not its %d", name, len(got), len(content))
			}

			// Reads which span the extents, and seeks into the second.
			fl, err := read.OpenFile(name, os.O_RDONLY)
			if err != nil {
				t.Fatal(err)
			}
			at := int64(sectors*blocksize - 10)
			if _, err := fl.Seek(at, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			across := make([]byte, 50)
			if _, err := io.ReadFull(fl, across); err != nil {
				t.Fatalf("reading across the extents: %v", err)
			}
			if !bytes.Equal(across, content[at:at+50]) {
				t.Error("a read across the extents read the wrong bytes")
			}
			end, err := fl.Seek(-5, io.SeekEnd)
			if err != nil || end != int64(len(content))-5 {
				t.Fatalf("seeking from the end: %d, %v", end, err)
			}
			tail, err := io.ReadAll(fl)
			if err != nil || !bytes.Equal(tail, content[len(content)-5:]) {
				t.Errorf("reading the end: %q, %v", tail, err)
			}
		})
	}
}

// TestReadXorrisoLargeFile checks a file of more than 4 GiB, which xorriso
// writes in several extents, reads whole. It writes a 4 GiB image.
func TestReadXorrisoLargeFile(t *testing.T) {
	if intImage == "" {
		t.Skip("skipping xorriso integration test (TEST_IMAGE not set)")
	}

	const size = 4<<30 + 3000

	workspace := t.TempDir()
	if err := os.Chmod(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	big, err := os.Create(filepath.Join(workspace, "big.dat"))
	if err != nil {
		t.Fatal(err)
	}
	// Sparse but for a marker at the end, past the first extent.
	if _, err := big.WriteAt([]byte("the end"), size-7); err != nil {
		t.Fatal(err)
	}
	if err := big.Close(); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	output := new(bytes.Buffer)
	script := "cd /workspace && xorriso -as mkisofs -R -J -iso-level 3 -o /output/large.iso ."
	if err := testhelper.DockerRun(strings.NewReader(script), output, false, true,
		map[string]string{workspace: "/workspace", outDir: "/output"}, intImage, "sh", "-c", "cat | sh"); err != nil {
		t.Fatalf("xorriso failed: %v\n%s", err, output)
	}

	f, err := os.Open(filepath.Join(outDir, "large.iso"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}

	fs, err := iso9660.Read(file.New(f, true), info.Size(), 0, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fl, err := fs.OpenFile("/big.dat", os.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := fl.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != size {
		t.Fatalf("big.dat is %d bytes, not %d", fi.Size(), size)
	}
	if _, err := fl.Seek(-7, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	tail, err := io.ReadAll(fl)
	if err != nil || string(tail) != "the end" {
		t.Errorf("big.dat ends %q (%v), not %q", tail, err, "the end")
	}
}

package iso9660

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/diskfs/go-diskfs/backend/file"
)

func streamContent(i, n int) []byte {
	b := make([]byte, n)
	for j := range b {
		b[j] = byte(i*31 + j*7 + j/2048)
	}
	return b
}

func writeStream(t *testing.T, files []StreamFile, options StreamOptions) (layout *StreamLayout, name string) {
	t.Helper()

	layout, err := LayoutStream(files, options)
	if err != nil {
		t.Fatalf("laying out: %v", err)
	}

	name = filepath.Join(t.TempDir(), "stream.iso")
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	n, err := layout.WriteTo(f, func(i int) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(streamContent(i, int(files[i].Size)))), nil
	})
	if err != nil {
		t.Fatalf("writing: %v", err)
	}
	if n != layout.Size {
		t.Fatalf("wrote %d bytes, not the %d laid out", n, layout.Size)
	}

	return layout, name
}

func TestStreamRoundTrip(t *testing.T) {
	// Small extents, so files are written in several.
	defer func(was int64) { streamMaxExtent = was }(streamMaxExtent)
	streamMaxExtent = 2 * streamSector

	files := []StreamFile{
		{Path: "cache/nix-cache-info", Size: 21},
		{Path: "cache/nar/0000000000000000000000000000000000000000000000000001.nar", Size: 5*streamSector + 7},
		{Path: "cache/00000000000000000000000000000001.narinfo", Size: 400},
		{Path: "empty", Size: 0},
		{Path: "A Name With Spaces.txt", Size: streamSector},
	}

	for _, tc := range []struct {
		name    string
		options StreamOptions
	}{
		{"rock ridge", StreamOptions{RockRidge: true}},
		{"joliet", StreamOptions{Joliet: true}},
		{"rock ridge and joliet", StreamOptions{RockRidge: true, Joliet: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, name := writeStream(t, files, tc.options)

			f, err := os.Open(name)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			info, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}

			fs, err := Read(file.New(f, true), info.Size(), 0, streamSector)
			if err != nil {
				t.Fatalf("reading: %v", err)
			}

			for i, sf := range files {
				got, err := fs.ReadFile(sf.Path)
				if err != nil {
					t.Fatalf("reading %s: %v", sf.Path, err)
				}
				if !bytes.Equal(got, streamContent(i, int(sf.Size))) {
					t.Errorf("%s read %d bytes, not its %d", sf.Path, len(got), sf.Size)
				}
			}

			entries, err := fs.ReadDir("cache/nar")
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != filepath.Base(files[1].Path) {
				t.Fatalf("cache/nar has %v", entries)
			}
			if info, _ := entries[0].Info(); info.Size() != files[1].Size {
				t.Errorf("%s is %d bytes, not %d", entries[0].Name(), info.Size(), files[1].Size)
			}
		})
	}
}

func TestStreamIsDeterministic(t *testing.T) {
	files := []StreamFile{{Path: "b/two", Size: 3000}, {Path: "a/one", Size: 10}, {Path: "c", Size: 1}}
	options := StreamOptions{RockRidge: true, Joliet: true, ModTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}

	first, a := writeStream(t, files, options)
	second, b := writeStream(t, files, options)

	if first.Size != second.Size {
		t.Fatalf("sizes %d and %d", first.Size, second.Size)
	}

	one, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	two, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one, two) {
		t.Error("the same files laid out the same way gave different images")
	}
}

func TestStreamRefuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		files   []StreamFile
		options StreamOptions
		want    error
	}{
		{"no names", []StreamFile{{Path: "a", Size: 1}}, StreamOptions{}, ErrStreamNoNames},
		{"twice", []StreamFile{{Path: "a", Size: 1}, {Path: "a", Size: 2}}, StreamOptions{Joliet: true}, ErrStreamName},
		{"file and directory", []StreamFile{{Path: "a", Size: 1}, {Path: "a/b", Size: 2}}, StreamOptions{Joliet: true}, ErrStreamName},
		{"long joliet name", []StreamFile{{Path: string(bytes.Repeat([]byte("x"), 65)), Size: 1}}, StreamOptions{Joliet: true}, ErrStreamName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LayoutStream(tc.files, tc.options); !errors.Is(err, tc.want) {
				t.Errorf("got %v, not %v", err, tc.want)
			}
		})
	}
}

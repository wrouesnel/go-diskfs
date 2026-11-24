package iso9660

import (
	"fmt"
	"io"
	iofs "io/fs"
	"os"

	"github.com/diskfs/go-diskfs/filesystem"
)

var _ filesystem.File = (*File)(nil)
var _ iofs.File = (*File)(nil)

// File represents a single file in an iso9660 filesystem
//
//	it is NOT used when working in a workspace, where we just use the underlying OS
type File struct {
	*directoryEntry
	isReadWrite bool
	isAppend    bool
	offset      int64
	closed      bool
}

// Read reads up to len(b) bytes from the File.
// It returns the number of bytes read and any error encountered.
// At end of file, Read returns 0, io.EOF
// reads from the last known offset in the file from last read or write
// use Seek() to set at a particular point
func (fl *File) Read(b []byte) (int, error) {
	if fl == nil || fl.closed {
		return 0, os.ErrClosed
	}
	// we have the DirectoryEntry, so we can get the file's extents: one,
	// or, for a multi-extent file, several, each contiguous
	fs := fl.filesystem
	size := fl.Size()

	// if there is nothing left to read, just return EOF
	if fl.offset >= size {
		return 0, io.EOF
	}

	// we stop when we hit the lesser of
	//   1- len(b)
	//   2- file end
	read := 0
	start := int64(0)
	for _, e := range fl.fileExtents() {
		end := start + int64(e.size)
		if read < len(b) && fl.offset < end {
			// read what's wanted of this extent, from our offset into it
			into := fl.offset - start
			n := int(min(int64(len(b)-read), end-fl.offset))
			_, err := fs.backend.ReadAt(b[read:read+n], int64(e.location)*fs.blocksize+into)
			if err != nil && err != io.EOF {
				return read, err
			}
			read += n
			fl.offset += int64(n)
		}
		start = end
	}

	var retErr error
	if fl.offset >= size {
		retErr = io.EOF
	}
	return read, retErr
}

// Write writes len(b) bytes to the File.
//
//	you cannot write to an iso, so this returns an error
func (fl *File) Write(_ []byte) (int, error) {
	return 0, filesystem.ErrReadonlyFilesystem
}

// Seek set the offset to a particular point in the file
func (fl *File) Seek(offset int64, whence int) (int64, error) {
	if fl == nil || fl.closed {
		return 0, os.ErrClosed
	}
	newOffset := int64(0)
	switch whence {
	case io.SeekStart:
		newOffset = offset
	case io.SeekEnd:
		newOffset = fl.Size() + offset
	case io.SeekCurrent:
		newOffset = fl.offset + offset
	}
	if newOffset < 0 {
		return fl.offset, fmt.Errorf("cannot set offset %d before start of file", offset)
	}
	fl.offset = newOffset
	return fl.offset, nil
}

func (fl *File) Location() uint32 {
	return fl.location
}

// Close close the file
func (fl *File) Close() error {
	fl.closed = true
	return nil
}

// Stat returns a fs.FileInfo structure describing file
func (fl *File) Stat() (iofs.FileInfo, error) {
	return fl.directoryEntry, nil
}

package iso9660

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/diskfs/go-diskfs/util/timestamp"
)

// Errors laying out a streamed image.
var (
	ErrStreamNoNames  = errors.New("a streamed image needs Rock Ridge or Joliet names, or both")
	ErrStreamName     = errors.New("a name a streamed image can't hold")
	ErrStreamTooLarge = errors.New("a streamed image would be bigger than ISO 9660 allows")
)

// StreamFile is a regular file in a streamed image.
type StreamFile struct {
	// Path is relative to the root, separated by "/". Parent directories are
	// created implicitly.
	Path string
	Size int64
}

// StreamOptions configure a streamed image.
type StreamOptions struct {
	// At least one of RockRidge and Joliet is required.
	RockRidge bool
	Joliet    bool
	// VolumeIdentifier defaults to "ISOIMAGE".
	VolumeIdentifier string
	// ModTime defaults to SOURCE_DATE_EPOCH, or the current time.
	ModTime time.Time
}

// StreamPiece is a run of the image: Data, Zeros zero bytes, or the contents
// of file index File. File is -1 unless it's file contents.
type StreamPiece struct {
	Data  []byte
	Zeros int64
	File  int
	Size  int64
}

// StreamLayout is the pieces of an image, in order.
type StreamLayout struct {
	Pieces []StreamPiece
	Size   int64
}

const (
	streamSystemArea = 16 // sectors before the volume descriptors
	streamSector     = 2048
	streamMaxRecord  = 255
	streamMaxJoliet  = 64
)

// streamMaxExtent is the largest extent: under 4 GiB, in whole sectors.
// Tests lower it.
var streamMaxExtent int64 = 0xFFFFF800

// dCharacters matches characters not allowed in primary volume identifiers.
var dCharacters = regexp.MustCompile(`[^A-Z0-9_]`)

type streamNode struct {
	name     string
	dir      bool
	file     int // the file's index, for a file
	size     int64
	parent   *streamNode
	children []*streamNode

	// number is the directory's path table number.
	isoName    string
	jolietName []byte
	number     int

	lba, jolietLBA      uint32
	dirSize, jolietSize uint32
	dataLBA             uint32
}

// LayoutStream plans an image from file paths and sizes alone, so it can be
// written front to back without a workspace or a seekable destination. The
// same input always gives the same bytes. Plain ISO 9660 names are numbered
// placeholders, so Rock Ridge or Joliet is required. Files of 4 GiB or more
// are split into multiple extents (ISO 9660 level 3).
func LayoutStream(files []StreamFile, options StreamOptions) (*StreamLayout, error) {
	if !options.RockRidge && !options.Joliet {
		return nil, ErrStreamNoNames
	}

	if options.ModTime.IsZero() {
		options.ModTime = timestamp.GetTime()
	}

	root := &streamNode{dir: true, file: -1}
	root.parent = root

	if err := buildStreamTree(root, files, options); err != nil {
		return nil, err
	}

	w := &streamWriter{options: options, root: root, dirs: numberStreamTree(root)}

	return w.lay()
}

// WriteTo writes the image to w, reading file contents with open. Each file
// must be the size it was laid out with.
func (l *StreamLayout) WriteTo(w io.Writer, open func(file int) (io.ReadCloser, error)) (int64, error) {
	var written int64

	zeros := make([]byte, streamSector)

	for _, p := range l.Pieces {
		var (
			n   int64
			err error
		)

		switch {
		case p.File >= 0:
			n, err = writeStreamFile(w, p, open)
		case p.Zeros > 0:
			for left := p.Zeros; left > 0 && err == nil; {
				var m int
				m, err = w.Write(zeros[:min(left, int64(len(zeros)))])
				n += int64(m)
				left -= int64(m)
			}
		default:
			var m int
			m, err = w.Write(p.Data)
			n = int64(m)
		}

		written += n
		if err != nil {
			return written, err
		}
	}

	return written, nil
}

func writeStreamFile(w io.Writer, p StreamPiece, open func(file int) (io.ReadCloser, error)) (int64, error) {
	r, err := open(p.File)
	if err != nil {
		return 0, err
	}
	defer r.Close()

	n, err := io.CopyN(w, r, p.Size)
	if errors.Is(err, io.EOF) {
		return n, fmt.Errorf("file %d is %d bytes, not the %d laid out", p.File, n, p.Size)
	}

	return n, err
}

func buildStreamTree(root *streamNode, files []StreamFile, options StreamOptions) error {
	for i, f := range files {
		if f.Size < 0 {
			return fmt.Errorf("%s: %w: a negative size", f.Path, ErrStreamName)
		}

		names := strings.Split(strings.Trim(path.Clean("/"+f.Path), "/"), "/")
		at := root

		for j, name := range names {
			if err := checkStreamName(name, options); err != nil {
				return fmt.Errorf("%s: %w", f.Path, err)
			}

			last := j == len(names)-1

			var next *streamNode

			for _, child := range at.children {
				if child.name == name {
					next = child
				}
			}

			switch {
			case next == nil:
				next = &streamNode{name: name, dir: !last, parent: at, file: -1}
				if last {
					next.file, next.size = i, f.Size
				}

				at.children = append(at.children, next)
			case last || !next.dir:
				return fmt.Errorf("%s: %w: it's there twice", f.Path, ErrStreamName)
			}

			at = next
		}
	}

	return nil
}

func checkStreamName(name string, options StreamOptions) error {
	switch {
	case name == "" || name == "." || name == "..":
		return fmt.Errorf("%w: %q", ErrStreamName, name)
	case options.Joliet && len(utf16.Encode([]rune(name))) > streamMaxJoliet:
		return fmt.Errorf("%w: %q is longer than Joliet's %d characters", ErrStreamName, name, streamMaxJoliet)
	}

	return nil
}

// numberStreamTree numbers directories in path table order (breadth first,
// by parent then name) and assigns placeholder primary names in the order of
// the real names, so both trees sort the same way.
func numberStreamTree(root *streamNode) []*streamNode {
	var (
		dirs  = []*streamNode{root}
		dirN  = 0
		fileN = 0
	)

	root.number = 1

	for i := 0; i < len(dirs); i++ {
		d := dirs[i]

		sort.Slice(d.children, func(a, b int) bool {
			return bytes.Compare(ucs2Name(d.children[a].name), ucs2Name(d.children[b].name)) < 0
		})

		for _, child := range d.children {
			child.jolietName = ucs2Name(child.name)

			if child.dir {
				dirN++
				child.isoName = fmt.Sprintf("D%07d", dirN)
				dirs = append(dirs, child)
				child.number = len(dirs)
			} else {
				fileN++
				child.isoName = fmt.Sprintf("F%07d.;1", fileN)
			}
		}
	}

	return dirs
}

func ucs2Name(name string) []byte {
	units := utf16.Encode([]rune(name))
	out := make([]byte, 2*len(units))

	for i, u := range units {
		binary.BigEndian.PutUint16(out[2*i:], u)
	}

	return out
}

type streamWriter struct {
	options StreamOptions
	root    *streamNode
	dirs    []*streamNode

	pathTableSize, jolietTableSize uint32
	// tables are the L, M, Joliet L and Joliet M path table locations.
	tables  [4]uint32
	erLBA   uint32
	sectors uint32
}

// lay assigns locations: system area, volume descriptors, the Rock Ridge ER
// sector, path tables, primary then Joliet directories, then file data in
// input order.
func (w *streamWriter) lay() (*StreamLayout, error) {
	next := uint32(streamSystemArea + 2) // the primary descriptor, and the terminator

	if w.options.Joliet {
		next++
	}

	if w.options.RockRidge {
		w.erLBA = next
		next++
	}

	w.pathTableSize = w.pathTable(false, false, nil)
	w.jolietTableSize = w.pathTable(true, false, nil)

	for i, size := range []uint32{w.pathTableSize, w.pathTableSize, w.jolietTableSize, w.jolietTableSize} {
		if i >= 2 && !w.options.Joliet {
			break
		}

		w.tables[i] = next
		next += streamSectors(int64(size))
	}

	for _, joliet := range w.trees() {
		for _, d := range w.dirs {
			extent, err := w.dirExtent(d, joliet)
			if err != nil {
				return nil, err
			}

			if joliet {
				d.jolietLBA, d.jolietSize = next, uint32(len(extent))
			} else {
				d.lba, d.dirSize = next, uint32(len(extent))
			}

			next += streamSectors(int64(len(extent)))
		}
	}

	files := w.files()
	total := uint64(next)

	for _, f := range files {
		if f.size > 0 {
			f.dataLBA = uint32(total)
			total += uint64(streamSectors(f.size))
		}

		if total > 1<<32-1 {
			return nil, ErrStreamTooLarge
		}
	}

	w.sectors = uint32(total)

	return w.pieces(files)
}

func (w *streamWriter) trees() []bool {
	if w.options.Joliet {
		return []bool{false, true}
	}

	return []bool{false}
}

func (w *streamWriter) files() []*streamNode {
	var files []*streamNode

	for _, d := range w.dirs {
		for _, child := range d.children {
			if !child.dir {
				files = append(files, child)
			}
		}
	}

	sort.Slice(files, func(a, b int) bool { return files[a].file < files[b].file })

	return files
}

func streamSectors(size int64) uint32 {
	return uint32((size + streamSector - 1) / streamSector)
}

func (w *streamWriter) pieces(files []*streamNode) (*StreamLayout, error) {
	l := &StreamLayout{}

	add := func(p StreamPiece) {
		l.Pieces = append(l.Pieces, p)
		l.Size += p.Size
	}

	zeros := func(n int64) {
		if n > 0 {
			add(StreamPiece{Zeros: n, File: -1, Size: n})
		}
	}

	sectors := func(b []byte) {
		add(StreamPiece{Data: b, File: -1, Size: int64(len(b))})
		zeros(int64(streamSectors(int64(len(b))))*streamSector - int64(len(b)))
	}

	zeros(streamSystemArea * streamSector)
	sectors(w.volumeDescriptor(false))

	if w.options.Joliet {
		sectors(w.volumeDescriptor(true))
	}

	sectors(streamTerminator())

	if w.options.RockRidge {
		sectors(streamER())
	}

	for i := range 4 {
		if i >= 2 && !w.options.Joliet {
			break
		}

		var table []byte

		w.pathTable(i >= 2, i%2 == 1, &table)
		sectors(table)
	}

	for _, joliet := range w.trees() {
		for _, d := range w.dirs {
			extent, err := w.dirExtent(d, joliet)
			if err != nil {
				return nil, err
			}

			sectors(extent)
		}
	}

	for _, f := range files {
		if f.size == 0 {
			continue
		}

		add(StreamPiece{File: f.file, Size: f.size})
		zeros(int64(streamSectors(f.size))*streamSector - f.size)
	}

	if l.Size != int64(w.sectors)*streamSector {
		return nil, fmt.Errorf("laid out %d bytes, not the %d sectors planned", l.Size, w.sectors)
	}

	return l, nil
}

// pathTable returns a path table's size, and appends it to out if non-nil.
func (w *streamWriter) pathTable(joliet, bigEndian bool, out *[]byte) uint32 {
	var size uint32

	order := binary.ByteOrder(binary.LittleEndian)
	if bigEndian {
		order = binary.BigEndian
	}

	for _, d := range w.dirs {
		id := []byte{0}
		if d != w.root {
			id = []byte(d.isoName)
			if joliet {
				id = d.jolietName
			}
		}

		entry := make([]byte, 8+len(id)+len(id)%2)
		entry[0] = byte(len(id))

		lba := d.lba
		if joliet {
			lba = d.jolietLBA
		}

		order.PutUint32(entry[2:], lba)
		order.PutUint16(entry[6:], uint16(d.parent.number))
		copy(entry[8:], id)
		size += uint32(len(entry))

		if out != nil {
			*out = append(*out, entry...)
		}
	}

	return size
}

// dirExtent returns a directory's records, sorted by identifier (ECMA-119
// 9.3), with none crossing a sector.
func (w *streamWriter) dirExtent(d *streamNode, joliet bool) ([]byte, error) {
	children := append([]*streamNode{}, d.children...)
	if !joliet {
		sort.Slice(children, func(a, b int) bool { return children[a].isoName < children[b].isoName })
	}

	records := [][]byte{
		w.record(d, []byte{0}, joliet, d == w.root && !joliet, ""),
		w.record(d.parent, []byte{1}, joliet, false, ""),
	}

	for _, child := range children {
		id := []byte(child.isoName)
		if joliet {
			id = child.jolietName
		}

		if child.dir || child.size <= streamMaxExtent {
			records = append(records, w.record(child, id, joliet, false, child.name))
			continue
		}

		// One record per extent, all but the last flagged (ECMA-119 9.1.6).
		for at := int64(0); at < child.size; at += streamMaxExtent {
			part := *child
			part.dataLBA = child.dataLBA + uint32(at/streamSector)
			part.size = min(child.size-at, streamMaxExtent)
			record := w.record(&part, id, joliet, false, child.name)

			if at+streamMaxExtent < child.size {
				record[25] |= 0x80
			}

			records = append(records, record)
		}
	}

	var out []byte

	for _, r := range records {
		if len(r) > streamMaxRecord {
			return nil, fmt.Errorf("%w: %q makes too long a directory record", ErrStreamName, d.name)
		}

		if used := len(out) % streamSector; used+len(r) > streamSector {
			out = append(out, make([]byte, streamSector-used)...)
		}

		out = append(out, r...)
	}

	return append(out, make([]byte, int(streamSectors(int64(len(out))))*streamSector-len(out))...), nil
}

// record returns a directory record. With Rock Ridge, primary records get PX
// and NM, and the root's "." also gets SP and a CE pointing at the ER entry.
func (w *streamWriter) record(n *streamNode, id []byte, joliet, rootSelf bool, name string) []byte {
	var lba, size uint32

	switch {
	case n.dir && joliet:
		lba, size = n.jolietLBA, n.jolietSize
	case n.dir:
		lba, size = n.lba, n.dirSize
	default:
		lba, size = n.dataLBA, uint32(n.size)
	}

	var use []byte

	if !joliet && w.options.RockRidge {
		if rootSelf {
			use = append(use, 'S', 'P', 7, 1, 0xBE, 0xEF, 0)
		}

		use = append(use, streamPX(n.dir)...)

		if name != "" {
			use = append(use, append([]byte{'N', 'M', byte(5 + len(name)), 1, 0}, name...)...)
		}

		// CE last, as xorriso writes it.
		if rootSelf {
			ce := make([]byte, 28)
			copy(ce, "CE")
			ce[2], ce[3] = 28, 1
			streamBoth32(ce[4:], w.erLBA)
			streamBoth32(ce[12:], 0)
			streamBoth32(ce[20:], uint32(len(streamER())))
			use = append(use, ce...)
		}
	}

	// Records are an even length.
	idEnd := 33 + len(id) + (1 - len(id)%2)
	length := idEnd + len(use)
	length += length % 2

	r := make([]byte, length)
	r[0] = byte(length)
	streamBoth32(r[2:], lba)
	streamBoth32(r[10:], size)
	copy(r[18:25], timeToBytes(w.options.ModTime))

	if n.dir {
		r[25] = 0x02
	}

	streamBoth16(r[28:], 1)
	r[32] = byte(len(id))
	copy(r[33:], id)
	copy(r[idEnd:], use)

	return r
}

// streamPX is an RRIP 1.10 PX entry: read-only, owned by root.
func streamPX(dir bool) []byte {
	mode, links := uint32(0o100444), uint32(1)
	if dir {
		mode, links = 0o40555, 2
	}

	e := make([]byte, 36)
	copy(e, "PX")
	e[2], e[3] = 36, 1
	streamBoth32(e[4:], mode)
	streamBoth32(e[12:], links)

	return e
}

// streamER is the RRIP 1.10 ER entry.
func streamER() []byte {
	const (
		id   = rockRidge110
		des  = "THE ROCK RIDGE INTERCHANGE PROTOCOL PROVIDES SUPPORT FOR POSIX FILE SYSTEM SEMANTICS"
		src  = "PLEASE CONTACT DISC PUBLISHER FOR SPECIFICATION SOURCE.  SEE PUBLISHER IDENTIFIER IN PRIMARY VOLUME DESCRIPTOR FOR CONTACT INFORMATION."
		head = 8
	)

	e := make([]byte, head, head+len(id)+len(des)+len(src))
	copy(e, "ER")
	e[2], e[3] = byte(head+len(id)+len(des)+len(src)), 1
	e[4], e[5], e[6], e[7] = byte(len(id)), byte(len(des)), byte(len(src)), 1

	return append(append(append(e, id...), des...), src...)
}

func (w *streamWriter) volumeDescriptor(joliet bool) []byte {
	d := make([]byte, streamSector)
	d[0] = 1
	copy(d[1:6], "CD001")
	d[6] = 1

	text := func(at, size int, s string) {
		if joliet {
			copy(d[at:at+size], streamUCS2Field(s, size/2))
			return
		}

		copy(d[at:at+size], fmt.Sprintf("%-*s", size, s)[:size])
	}

	volumeID := w.options.VolumeIdentifier
	if volumeID == "" {
		volumeID = "ISOIMAGE"
	}

	if !joliet {
		volumeID = dCharacters.ReplaceAllString(strings.ToUpper(volumeID), "_")
	}

	tableSize, root, rootSize, lTable, mTable := w.pathTableSize, w.root.lba, w.root.dirSize, w.tables[0], w.tables[1]

	if joliet {
		d[0] = 2
		copy(d[88:91], "%/E")
		tableSize, root, rootSize, lTable, mTable = w.jolietTableSize, w.root.jolietLBA, w.root.jolietSize, w.tables[2], w.tables[3]
	}

	text(8, 32, "")
	text(40, 32, volumeID)
	streamBoth32(d[80:], w.sectors)
	streamBoth16(d[120:], 1)
	streamBoth16(d[124:], 1)
	streamBoth16(d[128:], streamSector)
	streamBoth32(d[132:], tableSize)
	binary.LittleEndian.PutUint32(d[140:], lTable)
	binary.BigEndian.PutUint32(d[148:], mTable)

	r := d[156:190]
	r[0] = 34
	streamBoth32(r[2:], root)
	streamBoth32(r[10:], rootSize)
	copy(r[18:25], timeToBytes(w.options.ModTime))
	r[25] = 0x02
	streamBoth16(r[28:], 1)
	r[32] = 1

	for _, at := range []struct{ off, size int }{{190, 128}, {318, 128}, {446, 128}, {574, 128}, {702, 37}, {739, 37}, {776, 37}} {
		text(at.off, at.size, "")
	}

	copy(d[813:830], streamVolumeTime(w.options.ModTime))
	copy(d[830:847], streamVolumeTime(w.options.ModTime))
	copy(d[847:864], streamVolumeTime(time.Time{}))
	copy(d[864:881], streamVolumeTime(time.Time{}))
	d[881] = 1

	return d
}

func streamUCS2Field(s string, units int) []byte {
	out := make([]byte, 2*units)
	for i := range units {
		binary.BigEndian.PutUint16(out[2*i:], ' ')
	}

	encoded := utf16.Encode([]rune(s))
	for i := 0; i < len(encoded) && i < units; i++ {
		binary.BigEndian.PutUint16(out[2*i:], encoded[i])
	}

	return out
}

func streamTerminator() []byte {
	d := make([]byte, streamSector)
	d[0] = 255
	copy(d[1:6], "CD001")
	d[6] = 1

	return d
}

// streamVolumeTime formats a volume descriptor date; zero means unset.
func streamVolumeTime(t time.Time) []byte {
	if t.IsZero() {
		return append([]byte("0000000000000000"), 0)
	}

	return append([]byte(t.UTC().Format("20060102150405")+"00"), 0)
}

func streamBoth16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b, v)
	binary.BigEndian.PutUint16(b[2:], v)
}

func streamBoth32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b, v)
	binary.BigEndian.PutUint32(b[4:], v)
}

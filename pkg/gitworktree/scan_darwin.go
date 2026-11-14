//go:build darwin

package gitworktree

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

// getattrlist(2) attributes and options that x/sys does not name.
const (
	attrCmnExtPrivateSize = 0x00000008
	attrCmnExtCloneID     = 0x00000100
	attrCmnExtCloneRefcnt = 0x00001000
	fsoptPackInvalAttrs   = 0x00000008
)

// Object types from sys/vnode.h.
const (
	vreg = 1
	vdir = 2
)

// An entry in getattrlistbulk's buffer, with invalid attributes packed too, holds its length (4), the attributes
// returned (5 words of 4), the error (4), the name reference (8) and the object type (4). A file's entry then holds
// its allocated size (8); a folder's has no file attributes, even packed. Both end with the private size (8), the
// clone ID (8) and the clone count (4), then the name.
const (
	headSize    = 4 + 5*4 + 4 + 8 + 4
	extSize     = 8 + 8 + 4
	fileAttrLen = 8
)

// errNoPrivateSize is returned on a volume that does not track which blocks a file shares, such as one that is not
// APFS.
var errNoPrivateSize = fmt.Errorf("%w: the volume does not report a file's private size", ErrUnsupported)

// scanTree calls visit for every regular file under root, with what APFS reports about its blocks. It reads each
// folder with getattrlistbulk(2), which returns a whole batch of entries and their attributes in one call, instead of
// a stat and a getattrlist(2) for each file. It does not follow symbolic links.
func scanTree(ctx context.Context, root string, visit func(fileBlocks)) error {
	list := unix.Attrlist{
		Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr:  unix.ATTR_CMN_RETURNED_ATTRS | unix.ATTR_CMN_NAME | unix.ATTR_CMN_OBJTYPE | unix.ATTR_CMN_ERROR,
		Fileattr:    unix.ATTR_FILE_ALLOCSIZE,
		// With FSOPT_ATTR_CMN_EXTENDED, the fork attributes are read as the extended common ones.
		Forkattr: attrCmnExtPrivateSize | attrCmnExtCloneID | attrCmnExtCloneRefcnt,
	}
	buf := make([]byte, 256<<10)
	dirs := []string{root}
	for len(dirs) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir := dirs[len(dirs)-1]
		dirs = dirs[:len(dirs)-1]
		subdirs, err := scanDir(dir, &list, buf, visit)
		// A folder removed while the scan runs, as an install in progress does, is skipped.
		if errors.Is(err, unix.ENOENT) && dir != root {
			continue
		}
		if err != nil {
			return err
		}
		dirs = append(dirs, subdirs...)
	}
	return nil
}

// scanDir reads one folder's entries, calls visit for each regular file, and returns its subfolders.
func scanDir(dir string, list *unix.Attrlist, buf []byte, visit func(fileBlocks)) ([]string, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(fd) }()
	var subdirs []string
	for {
		// Neither x/sys nor the standard library wraps getattrlistbulk(2). On darwin Syscall6 still goes through
		// libSystem's syscall() rather than a raw trap.
		// #nosec G103 -- getattrlistbulk(2) takes C pointers; each lives for the length of the call.
		n, _, errno := unix.Syscall6(unix.SYS_GETATTRLISTBULK, //nolint:staticcheck // no libSystem wrapper exists to call instead.
			uintptr(fd), uintptr(unsafe.Pointer(list)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
			uintptr(fsoptPackInvalAttrs|unix.FSOPT_ATTR_CMN_EXTENDED), 0)
		if errno != 0 {
			return nil, errno
		}
		if n == 0 {
			return subdirs, nil
		}
		off := 0
		for range n {
			entry, name, ok := readEntry(buf[off:])
			if !ok {
				return nil, errors.New("getattrlistbulk returned an entry too short to read")
			}
			off += int(binary.NativeEndian.Uint32(buf[off:]))
			switch {
			case entry.err:
				continue
			case entry.objType == vdir:
				subdirs = append(subdirs, filepath.Join(dir, name))
			case entry.objType == vreg:
				if !entry.hasPrivate {
					return nil, errNoPrivateSize
				}
				visit(entry.fileBlocks)
			}
		}
	}
}

// bulkEntry is one decoded getattrlistbulk entry.
type bulkEntry struct {
	fileBlocks
	err        bool
	objType    uint32
	hasPrivate bool
}

// readEntry decodes the entry at the start of b, laid out as headSize describes.
func readEntry(b []byte) (bulkEntry, string, bool) {
	ne := binary.NativeEndian
	if len(b) < headSize {
		return bulkEntry{}, "", false
	}
	length := int(ne.Uint32(b))
	if length < headSize || length > len(b) {
		return bulkEntry{}, "", false
	}
	forkReturned := ne.Uint32(b[4+4*4:])
	const nameAt = 4 + 5*4 + 4
	nameStart := nameAt + int(int32(ne.Uint32(b[nameAt:]))) // #nosec G115 -- a signed offset in a 32-bit field.
	nameEnd := nameStart + int(ne.Uint32(b[nameAt+4:])) - 1 // the length counts the closing NUL.
	if nameStart < nameAt || nameEnd < nameStart || nameEnd > length {
		return bulkEntry{}, "", false
	}
	e := bulkEntry{
		err:        ne.Uint32(b[4+5*4:]) != 0,
		objType:    ne.Uint32(b[nameAt+8:]),
		hasPrivate: forkReturned&attrCmnExtPrivateSize != 0,
	}
	name := string(b[nameStart:nameEnd])
	if e.objType != vreg {
		return e, name, true
	}
	if length < headSize+fileAttrLen+extSize {
		return bulkEntry{}, "", false
	}
	e.Alloc = int64(ne.Uint64(b[headSize:]))               // #nosec G115 -- an off_t, which is signed.
	e.Private = int64(ne.Uint64(b[headSize+fileAttrLen:])) // #nosec G115 -- an off_t, which is signed.
	if forkReturned&attrCmnExtCloneID != 0 && forkReturned&attrCmnExtCloneRefcnt != 0 {
		e.CloneID = ne.Uint64(b[headSize+fileAttrLen+8:])
		e.Clones = ne.Uint32(b[headSize+fileAttrLen+16:])
	}
	return e, name, true
}

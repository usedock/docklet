//go:build !linux

package disk

// Extent is one mapped extent of a file, tagged with whether its blocks are
// still shared with another file (a reflink copy) and whether their contents
// are unwritten.
type Extent struct {
	Start     int64 // logical offset in bytes
	End       int64 // exclusive
	Physical  int64 // physical offset of the first block
	Shared    bool
	Unwritten bool
}

// Extents reports the mapped extents of path. Non-linux hosts have no FIEMAP,
// so they cannot tell and the caller falls back to comparing bytes.
func Extents(path string) ([]Extent, error) {
	return nil, nil
}

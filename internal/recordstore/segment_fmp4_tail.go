package recordstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
)

// The end of the file is read backwards in chunks while looking for the last moof box.
// A segment that was not closed (the one being recorded, or one cut by a crash)
// has no duration in mvhd, and walking every part from the start costs one
// random read per part, which is slow on a cold cache and on HDDs.
var (
	segmentFMP4TailChunk int64 = 1 << 20
	segmentFMP4TailMax   int64 = 64 << 20
)

// segmentFMP4ReadDurationFromTail finds the last part by scanning the end of
// the file for a moof box, then follows the moof/mdat chain to its end with the
// same rules as segmentFMP4ReadDurationFromParts.
func segmentFMP4ReadDurationFromTail(
	ctx context.Context,
	f *os.File,
	init *fmp4.Init,
) (time.Duration, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := fi.Size()

	dataStart, err := segmentFMP4DataStart(f)
	if err != nil {
		return 0, err
	}

	// a box header that starts in a chunk can end in the next one
	const overlap = 7
	buf := make([]byte, segmentFMP4TailChunk+overlap)
	lowest := max(size-segmentFMP4TailMax, dataStart)

	for end := size; end > lowest; {
		if err = ctx.Err(); err != nil {
			return 0, err
		}

		from := max(end-segmentFMP4TailChunk, lowest)
		n := end - from + min(overlap, size-end)

		_, err = f.ReadAt(buf[:n], from)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}

		pos, ok := segmentFMP4FindMoofBackwards(f, buf[:n], from, size)
		if ok {
			pos = segmentFMP4FollowChain(f, pos, size)
			return segmentFMP4ReadMoofDuration(f, init, pos)
		}

		end = from
	}

	return 0, fmt.Errorf("no moof boxes found in the tail")
}

// segmentFMP4DataStart returns the position after ftyp and moov.
func segmentFMP4DataStart(r io.ReaderAt) (int64, error) {
	var hdr [8]byte
	pos := int64(0)

	for _, typ := range []string{"ftyp", "moov"} {
		_, err := r.ReadAt(hdr[:], pos)
		if err != nil {
			return 0, err
		}
		if string(hdr[4:]) != typ {
			return 0, fmt.Errorf("%s box not found", typ)
		}
		pos += int64(binary.BigEndian.Uint32(hdr[:4]))
	}

	return pos, nil
}

// segmentFMP4IsPart reports whether a moof box starts at pos and is followed
// by the header of an mdat box, which is what segmentFMP4ReadDurationFromParts
// accepts as a part.
func segmentFMP4IsPart(r io.ReaderAt, pos int64, size int64) (int64, bool) {
	var hdr [32]byte
	if pos+int64(len(hdr)) > size {
		return 0, false
	}
	_, err := r.ReadAt(hdr[:], pos)
	if err != nil {
		return 0, false
	}

	moofSize := int64(binary.BigEndian.Uint32(hdr[:4]))

	if !bytes.Equal(hdr[4:8], []byte("moof")) ||
		binary.BigEndian.Uint32(hdr[8:12]) != 16 ||
		!bytes.Equal(hdr[12:16], []byte("mfhd")) ||
		!bytes.Equal(hdr[28:32], []byte("traf")) ||
		moofSize < 32 {
		return 0, false
	}

	var mdat [8]byte
	if pos+moofSize+8 > size {
		return 0, false
	}
	_, err = r.ReadAt(mdat[:], pos+moofSize)
	if err != nil || !bytes.Equal(mdat[4:], []byte("mdat")) {
		return 0, false
	}

	mdatSize := int64(binary.BigEndian.Uint32(mdat[:4]))
	if mdatSize < 8 {
		return 0, false
	}

	return pos + moofSize + mdatSize, true
}

// segmentFMP4FindMoofBackwards returns the position of the last part whose moof box
// starts inside buf, which holds the file from position from to the end.
func segmentFMP4FindMoofBackwards(r io.ReaderAt, buf []byte, from int64, size int64) (int64, bool) {
	for i := len(buf) - 8; i >= 0; i-- {
		if buf[i+4] != 'm' || !bytes.Equal(buf[i+4:i+8], []byte("moof")) {
			continue
		}

		pos := from + int64(i)
		if _, ok := segmentFMP4IsPart(r, pos, size); ok {
			return pos, true
		}
	}

	return 0, false
}

// segmentFMP4FollowChain follows moof/mdat pairs from pos and returns the position
// of the last one.
func segmentFMP4FollowChain(r io.ReaderAt, pos int64, size int64) int64 {
	for {
		next, ok := segmentFMP4IsPart(r, pos, size)
		if !ok {
			return pos
		}
		if _, ok = segmentFMP4IsPart(r, next, size); !ok {
			return pos
		}
		pos = next
	}
}

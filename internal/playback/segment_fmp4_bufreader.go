package playback

import (
	"errors"
	"io"
)

// go-mp4 unmarshals boxes (moof, trun entries) field by field, and every field
// is a separate Read of a few bytes. On an *os.File each Read is a system call:
// a segment with 1 s parts costs thousands of calls per minute of video, which is
// slow even from the cache and very slow on a busy hard disk.
//
// bufReader serves those small reads from a buffer filled with a single ReadAt,
// and turns seeks over mdat into a change of position without system calls.
// ReadAt (sample payloads) goes to the underlying reader unchanged.
//
// The buffer is large enough to hold several parts: on a hard disk that is
// also busy recording, the cost is the number of reads, not their size.
//
// A request covers several segments, each one with its own bufReader: with
// format=mp4, sample payloads are read through ReadAt of that reader after all
// segments have been parsed. The buffer is shared by the readers of a request,
// so that a request allocates it once. Only the reader that filled it last can
// use its content.

const bufReaderSize = 4 * 1024 * 1024

type bufReaderBuffer struct {
	data  []byte
	owner *bufReader
}

func newBufReaderBuffer() *bufReaderBuffer {
	return &bufReaderBuffer{data: make([]byte, bufReaderSize)}
}

type bufReader struct {
	r        readSeekerAt
	pos      int64
	buf      *bufReaderBuffer
	bufStart int64
	bufLen   int
}

func newBufReader(r readSeekerAt, buf *bufReaderBuffer) *bufReader {
	return &bufReader{r: r, buf: buf}
}

func (b *bufReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	// another reader has filled the buffer since
	if b.buf.owner != b {
		b.bufLen = 0
	}

	if b.pos < b.bufStart || b.pos >= b.bufStart+int64(b.bufLen) {
		// large reads skip the buffer
		if len(p) >= len(b.buf.data) {
			n, err := b.r.ReadAt(p, b.pos)
			b.pos += int64(n)
			if n > 0 && errors.Is(err, io.EOF) {
				err = nil
			}
			return n, err
		}

		n, err := b.r.ReadAt(b.buf.data, b.pos)
		b.buf.owner = b
		b.bufStart = b.pos
		b.bufLen = n
		if n == 0 {
			if err == nil {
				err = io.EOF
			}
			return 0, err
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
	}

	n := copy(p, b.buf.data[b.pos-b.bufStart:b.bufLen])
	b.pos += int64(n)
	return n, nil
}

func (b *bufReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = b.pos + offset
	case io.SeekEnd:
		end, err := b.r.Seek(0, io.SeekEnd)
		if err != nil {
			return 0, err
		}
		abs = end + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("negative position")
	}
	b.pos = abs
	return abs, nil
}

func (b *bufReader) ReadAt(p []byte, off int64) (int, error) {
	return b.r.ReadAt(p, off)
}

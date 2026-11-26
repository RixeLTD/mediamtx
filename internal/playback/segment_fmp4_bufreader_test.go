package playback

import (
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// bufReader must return exactly what the file returns, for any mix of reads and seeks.
func TestBufReaderSameAsFile(t *testing.T) {
	data := make([]byte, 3*bufReaderSize+123)
	rnd := rand.New(rand.NewSource(1))
	rnd.Read(data)
	fpath := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(fpath, data, 0o644))

	f1, err := os.Open(fpath)
	require.NoError(t, err)
	defer f1.Close()
	f2, err := os.Open(fpath)
	require.NoError(t, err)
	defer f2.Close()

	b := newBufReader(f2, newBufReaderBuffer())

	for i := range 5000 {
		switch rnd.Intn(4) {
		case 0, 1:
			n := rnd.Intn(16) + 1
			if rnd.Intn(50) == 0 {
				n = bufReaderSize + rnd.Intn(1000)
			}
			p1, p2 := make([]byte, n), make([]byte, n)
			n1, err1 := io.ReadFull(f1, p1)
			n2, err2 := io.ReadFull(b, p2)
			require.Equal(t, n1, n2, "step %d", i)
			require.Equal(t, err1, err2, "step %d", i)
			require.Equal(t, p1[:n1], p2[:n2], "step %d", i)
		case 2:
			off := int64(rnd.Intn(len(data)+100)) - 50
			whence := []int{io.SeekStart, io.SeekCurrent, io.SeekEnd}[rnd.Intn(3)]
			if whence == io.SeekEnd {
				off = -int64(rnd.Intn(len(data)))
			}
			if whence == io.SeekCurrent {
				off = int64(rnd.Intn(2000)) - 1000
			}
			p1, err1 := f1.Seek(off, whence)
			p2, err2 := b.Seek(off, whence)
			require.Equal(t, err1 == nil, err2 == nil, "step %d", i)
			if err1 == nil {
				require.Equal(t, p1, p2, "step %d", i)
			} else {
				_, err = f1.Seek(b.pos, io.SeekStart)
				require.NoError(t, err)
			}
		case 3:
			p1, p2 := make([]byte, 20), make([]byte, 20)
			off := int64(rnd.Intn(len(data)))
			n1, _ := f1.ReadAt(p1, off)
			n2, _ := b.ReadAt(p2, off)
			require.Equal(t, p1[:n1], p2[:n2])
		}
	}
}

// Readers of the segments of a request share a buffer. A reader must never get
// data that another reader has put in it, and ReadAt (sample payloads with
// format=mp4) must keep working after the buffer has been reused.
func TestBufReaderSharedBuffer(t *testing.T) {
	dir := t.TempDir()
	rnd := rand.New(rand.NewSource(3))

	var data [2][]byte
	var readers [2]*bufReader
	buf := newBufReaderBuffer()

	for i := range data {
		data[i] = make([]byte, 100000)
		rnd.Read(data[i])
		fpath := filepath.Join(dir, string(rune('a'+i)))
		require.NoError(t, os.WriteFile(fpath, data[i], 0o644))

		f, err := os.Open(fpath)
		require.NoError(t, err)
		defer f.Close()

		readers[i] = newBufReader(f, buf)
	}

	p := make([]byte, 1000)
	for step := range 20 {
		i := step % 2
		_, err := io.ReadFull(readers[i], p)
		require.NoError(t, err)
		off := (step / 2) * len(p)
		require.Equal(t, data[i][off:off+len(p)], p)
	}

	_, err := readers[0].ReadAt(p, 5000)
	require.NoError(t, err)
	require.Equal(t, data[0][5000:6000], p)
}

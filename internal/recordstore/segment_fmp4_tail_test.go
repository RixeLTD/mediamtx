package recordstore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

// writeTailSegment writes a segment without duration in mvhd, with n parts of one second,
// whose payload looks like the start of a moof box, followed by tail.
func writeTailSegment(t *testing.T, fpath string, n int, tail []byte) {
	t.Helper()

	init := fmp4.Init{
		Tracks: []*fmp4.InitTrack{{
			ID:        1,
			TimeScale: 90000,
			Codec:     &mcodecs.H264{SPS: test.FormatH264.SPS, PPS: test.FormatH264.PPS},
		}},
	}

	var buf seekablebuffer.Buffer
	require.NoError(t, init.Marshal(&buf))

	fake := []byte{0, 0, 0, 0x40, 'm', 'o', 'o', 'f', 0, 0, 0, 0x10, 'm', 'f', 'h', 'd'}

	for i := range n {
		part := fmp4.Part{
			SequenceNumber: uint32(i),
			Tracks: []*fmp4.PartTrack{{
				ID:       1,
				BaseTime: uint64(i) * 90000,
				Samples: []*fmp4.Sample{{
					Duration: 90000,
					Payload:  bytes.Repeat(fake, 4),
				}},
			}},
		}
		require.NoError(t, part.Marshal(&buf))
	}

	require.NoError(t, os.WriteFile(fpath, append(buf.Bytes(), tail...), 0o644))
}

func TestSegmentFMP4ReadDurationFromTail(t *testing.T) {
	truncatedMoof := []byte{0, 0, 0x10, 0, 'm', 'o', 'o', 'f', 0, 0, 0, 0x10, 'm', 'f', 'h', 'd'}

	for _, ca := range []struct {
		name  string
		tail  []byte
		chunk int64
		max   int64
	}{
		{"clean", nil, 0, 0},
		{"zeros", make([]byte, 5000), 0, 0},
		{"truncated part", truncatedMoof, 0, 0},
		{"limit smaller than a part", make([]byte, 100), 0, 50},
	} {
		t.Run(ca.name, func(t *testing.T) {
			if ca.chunk != 0 {
				prev := segmentFMP4TailChunk
				segmentFMP4TailChunk = ca.chunk
				defer func() { segmentFMP4TailChunk = prev }()
			}
			if ca.max != 0 {
				prev := segmentFMP4TailMax
				segmentFMP4TailMax = ca.max
				defer func() { segmentFMP4TailMax = prev }()
			}

			fpath := filepath.Join(t.TempDir(), "segment.mp4")
			writeTailSegment(t, fpath, 5, ca.tail)

			f, err := os.Open(fpath)
			require.NoError(t, err)
			defer f.Close()

			init, d, err := SegmentFMP4ReadHeader(f)
			require.NoError(t, err)
			require.Zero(t, d)

			parts, err := segmentFMP4ReadDurationFromParts(context.Background(), f, init)
			require.NoError(t, err)
			require.Equal(t, 5*time.Second, parts)

			d, err = segmentFMP4ReadDurationFromTail(context.Background(), f, init)
			if ca.max != 0 {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, parts, d)
		})
	}
}

// every chunk size, so that the moof header of the last part also falls across chunk borders
func TestSegmentFMP4ReadDurationFromTailChunks(t *testing.T) {
	fpath := filepath.Join(t.TempDir(), "segment.mp4")
	writeTailSegment(t, fpath, 5, make([]byte, 30))

	f, err := os.Open(fpath)
	require.NoError(t, err)
	defer f.Close()

	init, _, err := SegmentFMP4ReadHeader(f)
	require.NoError(t, err)

	prev := segmentFMP4TailChunk
	defer func() { segmentFMP4TailChunk = prev }()

	for chunk := int64(1); chunk <= 300; chunk++ {
		segmentFMP4TailChunk = chunk
		d, err2 := segmentFMP4ReadDurationFromTail(context.Background(), f, init)
		require.NoError(t, err2, "chunk %d", chunk)
		require.Equal(t, 5*time.Second, d, "chunk %d", chunk)
	}
}

func TestSegmentFMP4ReadDurationFromTailCanceledContext(t *testing.T) {
	fpath := filepath.Join(t.TempDir(), "segment.mp4")
	writeTailSegment(t, fpath, 5, nil)

	f, err := os.Open(fpath)
	require.NoError(t, err)
	defer f.Close()

	init, _, err := SegmentFMP4ReadHeader(f)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = segmentFMP4ReadDurationFromTail(ctx, f, init)
	require.ErrorIs(t, err, context.Canceled)
}

package playback

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recordstore"
	"github.com/bluenviron/mediamtx/internal/test"
)

const (
	seekTestFPS        = 25
	seekTestPartFrames = 50 // 2 s parts
	seekTestParts      = 150
)

type seekTestAudio int

const (
	seekTestNoAudio seekTestAudio = iota
	seekTestAudioSamples
	seekTestAudioDeclared // declared in the init segment, without samples
)

// writeSeekTestSegment writes a 300 s segment: H264 at 25 fps with a GOP that
// does not line up with parts, AAC audio, and payloads that contain fake
// "moof" boxes so that the forward scan meets false candidates.
func writeSeekTestSegment(t *testing.T, fpath string, audio seekTestAudio, gop int) {
	t.Helper()

	init := fmp4.Init{
		Tracks: []*fmp4.InitTrack{{
			ID:        1,
			TimeScale: 90000,
			Codec:     &mcodecs.H264{SPS: test.FormatH264.SPS, PPS: test.FormatH264.PPS},
		}},
	}
	if audio != seekTestNoAudio {
		init.Tracks = append(init.Tracks, &fmp4.InitTrack{
			ID:        2,
			TimeScale: 48000,
			Codec: &mcodecs.MPEG4Audio{
				Config: mpeg4audio.AudioSpecificConfig{
					Type:          mpeg4audio.ObjectTypeAACLC,
					SampleRate:    48000,
					ChannelCount:  2, //nolint:staticcheck
					ChannelConfig: 2,
				},
			},
		})
	}

	var buf seekablebuffer.Buffer
	err := init.Marshal(&buf)
	require.NoError(t, err)

	rnd := rand.New(rand.NewSource(1))
	payload := func(n int, i int) []byte {
		p := make([]byte, n)
		rnd.Read(p)
		if i%7 == 0 { // fake box header inside media data
			copy(p[10:], []byte{0, 0, 0, 0x20, 'm', 'o', 'o', 'f', 0, 0, 0, 0x10, 'm', 'f', 'h', 'd'})
		}
		return p
	}

	frameDur := uint32(90000 / seekTestFPS)
	const audioPartTicks = 2 * 48000 // audio parts last exactly 2 s, like video
	frame := 0
	audioTime := uint64(0)

	for part := range seekTestParts {
		videoTrack := &fmp4.PartTrack{ID: 1, BaseTime: uint64(frame) * uint64(frameDur)}
		for range seekTestPartFrames {
			videoTrack.Samples = append(videoTrack.Samples, &fmp4.Sample{
				Duration:        frameDur,
				IsNonSyncSample: frame%gop != 0,
				Payload:         payload(200+(frame*37)%1500, frame), // variable bitrate
			})
			frame++
		}
		p := fmp4.Part{SequenceNumber: uint32(part), Tracks: []*fmp4.PartTrack{videoTrack}}

		if audio == seekTestAudioSamples {
			audioTrack := &fmp4.PartTrack{ID: 2, BaseTime: audioTime}
			for i, left := 0, uint32(audioPartTicks); left > 0; i++ {
				d := min(uint32(1024), left)
				audioTrack.Samples = append(audioTrack.Samples, &fmp4.Sample{
					Duration: d,
					Payload:  payload(40, i),
				})
				audioTime += uint64(d)
				left -= d
			}
			p.Tracks = append(p.Tracks, audioTrack)
		}

		err = p.Marshal(&buf)
		require.NoError(t, err)
	}

	err = os.WriteFile(fpath, buf.Bytes(), 0o644)
	require.NoError(t, err)
}

func muxSegmentErr(seg *recordstore.Segment, start time.Time, dur time.Duration, format string) ([]byte, error) {
	var out bytes.Buffer
	var m muxer
	if format == "mp4" {
		m = &muxerMP4{w: &out}
	} else {
		m = &muxerFMP4{w: &out}
	}

	err := seekAndMux(context.Background(), conf.RecordFormatFMP4, []*recordstore.Segment{seg}, start, dur, m)
	return out.Bytes(), err
}

func muxSegment(t *testing.T, seg *recordstore.Segment, start time.Time, dur time.Duration, format string) []byte {
	t.Helper()

	var out bytes.Buffer
	var m muxer
	if format == "mp4" {
		m = &muxerMP4{w: &out}
	} else {
		m = &muxerFMP4{w: &out}
	}

	err := seekAndMux(context.Background(), conf.RecordFormatFMP4, []*recordstore.Segment{seg}, start, dur, m)
	require.NoError(t, err)
	return out.Bytes()
}

// The output with seeking must be identical to the output of the full walk.
func TestSeekSameOutputAsFullWalk(t *testing.T) {
	oldLimit := seekScanLimit
	seekScanLimit = 256 * 1024
	defer func() { seekScanLimit = oldLimit }()

	for _, ca := range []struct {
		name  string
		audio seekTestAudio
		gop   int
	}{
		{"video", seekTestNoAudio, 37}, // GOP not a divisor of the part length
		{"video+audio", seekTestAudioSamples, 37},
		{"video+audio long GOP", seekTestAudioSamples, 400}, // 16 s, longer than seekLandBefore
		{"video+declared audio", seekTestAudioDeclared, 37},
	} {
		t.Run(ca.name, func(t *testing.T) {
			fpath := filepath.Join(t.TempDir(), "seg.mp4")
			writeSeekTestSegment(t, fpath, ca.audio, ca.gop)

			seg := &recordstore.Segment{
				Fpath: fpath,
				Start: time.Date(2008, 11, 7, 11, 22, 0, 0, time.Local),
			}

			offsets := []time.Duration{
				0, 40 * time.Millisecond, 1480 * time.Millisecond, 2 * time.Second,
				37 * time.Second, 100*time.Second + 20*time.Millisecond, 151 * time.Second,
				250 * time.Second, 299 * time.Second,
			}

			for _, off := range offsets {
				for _, format := range []string{"mp4", "fmp4"} {
					start := seg.Start.Add(off)

					seekDisabled = true
					want := muxSegment(t, seg, start, 10*time.Second, format)
					seekDisabled = false
					got := muxSegment(t, seg, start, 10*time.Second, format)

					require.Equal(t, want, got, "offset %v format %s", off, format)
				}
			}
		})
	}
}

// Seeking must land after the beginning of the file for a late start, and on a
// part whose GOP contains the start.
func TestSeekFindsLatePart(t *testing.T) {
	oldLimit := seekScanLimit
	seekScanLimit = 256 * 1024
	defer func() { seekScanLimit = oldLimit }()

	fpath := filepath.Join(t.TempDir(), "seg.mp4")
	writeSeekTestSegment(t, fpath, seekTestAudioSamples, 37)

	f, err := os.Open(fpath)
	require.NoError(t, err)
	defer f.Close()

	init, _, err := recordstore.SegmentFMP4ReadHeader(f)
	require.NoError(t, err)

	firstPartOffset, err := f.Seek(0, 1)
	require.NoError(t, err)
	st, err := f.Stat()
	require.NoError(t, err)

	target := 250 * time.Second
	off := seekFMP4Part(f, st.Size(), firstPartOffset, init.Tracks, target)
	require.Greater(t, off, firstPartOffset)

	p, ok := seekReadPart(f, st.Size(), off, init.Tracks)
	require.True(t, ok)
	require.LessOrEqual(t, p.start, target)
	require.Greater(t, p.start, target-20*time.Second) // close to the target, not near the beginning
	require.True(t, seekHasSyncBefore(p, target, init.Tracks))
}

// A truncated last part (file still being written) must not break seeking.
func TestSeekTruncatedFile(t *testing.T) {
	oldLimit := seekScanLimit
	seekScanLimit = 256 * 1024
	defer func() { seekScanLimit = oldLimit }()

	fpath := filepath.Join(t.TempDir(), "seg.mp4")
	writeSeekTestSegment(t, fpath, seekTestAudioSamples, 37)

	data, err := os.ReadFile(fpath)
	require.NoError(t, err)
	err = os.WriteFile(fpath, data[:len(data)-5000], 0o644)
	require.NoError(t, err)

	seg := &recordstore.Segment{Fpath: fpath, Start: time.Date(2008, 11, 7, 11, 22, 0, 0, time.Local)}

	for _, off := range []time.Duration{100 * time.Second, 297 * time.Second} {
		for _, format := range []string{"mp4", "fmp4"} {
			start := seg.Start.Add(off)
			seekDisabled = true
			want, wantErr := muxSegmentErr(seg, start, 10*time.Second, format)
			seekDisabled = false
			got, gotErr := muxSegmentErr(seg, start, 10*time.Second, format)
			t.Logf("offset %v format %s: full walk err=%v, seek err=%v", off, format, wantErr, gotErr)
			require.Equal(t, wantErr, gotErr, "offset %v format %s", off, format)
			require.Equal(t, want, got, "offset %v format %s", off, format)
		}
	}
}

type countingReaderAt struct {
	r     io.ReaderAt
	calls int
	bytes int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.calls++
	n, err := c.r.ReadAt(p, off)
	c.bytes += int64(n)
	return n, err
}

// Seeking must cost far fewer reads than a walk from the segment start.
func TestSeekReadsLittle(t *testing.T) {
	fpath := filepath.Join(t.TempDir(), "seg.mp4")
	writeSeekTestSegment(t, fpath, seekTestAudioSamples, 37)

	f, err := os.Open(fpath)
	require.NoError(t, err)
	defer f.Close()

	init, _, err := recordstore.SegmentFMP4ReadHeader(f)
	require.NoError(t, err)
	firstPartOffset, err := f.Seek(0, io.SeekCurrent)
	require.NoError(t, err)
	st, err := f.Stat()
	require.NoError(t, err)

	for _, target := range []time.Duration{150 * time.Second, 290 * time.Second} {
		c := &countingReaderAt{r: f}
		off := seekFMP4Part(c, st.Size(), firstPartOffset, init.Tracks, target)
		require.Greater(t, off, firstPartOffset)
		// cost of walking part by part from the first part to target
		walk := &countingReaderAt{r: f}
		for p, ok := seekReadPart(walk, st.Size(), firstPartOffset, init.Tracks); ok && p.start < target; {
			p, ok = seekReadPart(walk, st.Size(), p.offset+p.size, init.Tracks)
		}

		t.Logf("target %v: seek %d reads / %d bytes, walk %d reads / %d bytes",
			target, c.calls, c.bytes, walk.calls, walk.bytes)
		// on a hard disk the cost is the number of scattered reads (seeks);
		// a probe reads a 64 KiB window at once, so bytes are not comparable here
		require.Less(t, c.calls, walk.calls/2)
	}
}

type countingFile struct {
	*os.File
	minOff int64
	bytes  int64
}

func (c *countingFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.File.ReadAt(p, off)
	if n > 0 && (c.bytes == 0 || off < c.minOff) {
		c.minOff = off
	}
	c.bytes += int64(n)
	return n, err
}

// After seekToStart, parsing must start at the part found, not at the beginning
// of the file (amp4.ReadBoxStructure seeks to 0), and must read only the range.
func TestSeekParsingStartsAtPart(t *testing.T) {
	oldLimit := seekScanLimit
	seekScanLimit = 256 * 1024
	defer func() { seekScanLimit = oldLimit }()

	fpath := filepath.Join(t.TempDir(), "seg.mp4")
	writeSeekTestSegment(t, fpath, seekTestAudioSamples, 37)

	f, err := os.Open(fpath)
	require.NoError(t, err)
	defer f.Close()
	st, err := f.Stat()
	require.NoError(t, err)

	init, _, err := recordstore.SegmentFMP4ReadHeader(f)
	require.NoError(t, err)
	firstPartOffset, err := f.Seek(0, io.SeekCurrent)
	require.NoError(t, err)

	target := 250 * time.Second
	partOff := seekFMP4Part(f, st.Size(), firstPartOffset, init.Tracks, target)
	require.Greater(t, partOff, st.Size()/2)

	c := &countingFile{File: f}
	r, err := seekToStart(c, init.Tracks, target)
	require.NoError(t, err)
	c.bytes = 0 // count only the parsing

	var out bytesBuffer
	m := &muxerMP4{w: &out}
	m.writeInit(init)
	_, err = segmentFMP4MuxParts(context.Background(), r, -target, 10*time.Second, init.Tracks, m)
	require.NoError(t, err)
	require.NoError(t, m.flush())

	require.GreaterOrEqual(t, c.minOff, partOff)
	require.Less(t, c.bytes, st.Size()/4)
}

type bytesBuffer struct{ n int }

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.n += len(p)
	return len(p), nil
}

// A track declared in the init segment without samples (the audio track of a
// camera that doesn't send audio) must not prevent seeking.
func TestSeekDeclaredTrackWithoutSamples(t *testing.T) {
	oldLimit := seekScanLimit
	seekScanLimit = 256 * 1024
	defer func() { seekScanLimit = oldLimit }()

	fpath := filepath.Join(t.TempDir(), "seg.mp4")
	writeSeekTestSegment(t, fpath, seekTestAudioDeclared, 37)

	f, err := os.Open(fpath)
	require.NoError(t, err)
	defer f.Close()

	init, _, err := recordstore.SegmentFMP4ReadHeader(f)
	require.NoError(t, err)
	require.Len(t, init.Tracks, 2)

	firstPartOffset, err := f.Seek(0, io.SeekCurrent)
	require.NoError(t, err)
	st, err := f.Stat()
	require.NoError(t, err)

	target := 250 * time.Second
	off := seekFMP4Part(f, st.Size(), firstPartOffset, init.Tracks, target)
	require.Greater(t, off, st.Size()/2)
}

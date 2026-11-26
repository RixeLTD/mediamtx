package playback

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"time"

	amp4 "github.com/abema/go-mp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
)

// Finding the part that contains the requested start without walking every
// part from the beginning of the segment.
//
// segmentFMP4MuxParts reads every moof of a segment, so the cost of a request
// grows with the distance between the segment start and the requested start.
// On a busy hard disk, with several requests at once, each moof is a seek.
//
// seekFMP4Part estimates the offset of the requested start from the bitrate of
// the known parts (interpolation, a few probes): at a probe offset it scans
// forward for the next part (a moof followed by an mdat) and reads its decode
// time from tfdt. Near the target it walks forward part by part. It returns the offset
// of a part from which muxing gives the same result as muxing from the first
// part: a part that starts at or before the requested start and contains, for
// every track, a sync sample before the requested start. The muxers keep only
// the samples from the last sync sample before start, so earlier parts do not
// change the output.
//
// Any doubt (no valid part found, unexpected structure) falls back to the first
// part, i.e. to the previous behavior.

const (
	seekScanWindow   = 64 * 1024
	seekMaxMoofSize  = 16 * 1024 * 1024
	seekMaxProbes    = 8
	seekMaxStepsBack = 8
	// aim this much before the requested start, so that the walk forward
	// usually meets the sync sample of the GOP that contains it
	seekLandBefore = 10 * time.Second
	// stop probing when the walk forward is shorter than this
	seekWalkSpan = 20 * time.Second
)

// how far a probe scans for the next part; a variable so that tests can use small files
var seekScanLimit int64 = 8 * 1024 * 1024

var moofType = []byte{'m', 'o', 'o', 'f'}

type seekTraf struct {
	trackID   int
	timeScale uint32
	baseTime  int64
	entries   []amp4.TrunEntry
}

type seekPart struct {
	offset int64
	size   int64         // moof + mdat
	start  time.Duration // earliest decode time among tracks, from segment start
	end    time.Duration // latest end of the samples among tracks
	trafs  []seekTraf
}

func readUint32At(r io.ReaderAt, off int64) (uint32, []byte, error) {
	buf := make([]byte, 8)
	_, err := r.ReadAt(buf, off)
	if err != nil {
		return 0, nil, err
	}
	return binary.BigEndian.Uint32(buf[:4]), buf[4:], nil
}

// seekReadPart reads and validates the part (moof + mdat) at off.
func seekReadPart(
	r io.ReaderAt,
	fileSize int64,
	off int64,
	tracks []*fmp4.InitTrack,
) (*seekPart, bool) {
	moofSize, typ, err := readUint32At(r, off)
	if err != nil || !bytes.Equal(typ, moofType) ||
		moofSize < 16 || moofSize > seekMaxMoofSize || off+int64(moofSize)+8 > fileSize {
		return nil, false
	}

	// the moof must be followed by a complete mdat
	mdatOff := off + int64(moofSize)
	mdatSize32, typ, err := readUint32At(r, mdatOff)
	if err != nil || !bytes.Equal(typ, []byte{'m', 'd', 'a', 't'}) {
		return nil, false
	}
	mdatSize := int64(mdatSize32)
	if mdatSize32 == 1 {
		buf := make([]byte, 8)
		_, err = r.ReadAt(buf, mdatOff+8)
		if err != nil {
			return nil, false
		}
		mdatSize = int64(binary.BigEndian.Uint64(buf))
	}
	if mdatSize < 8 || mdatOff+mdatSize > fileSize {
		return nil, false
	}

	moof := make([]byte, moofSize)
	_, err = r.ReadAt(moof, off)
	if err != nil || !bytes.Equal(moof[12:16], []byte{'m', 'f', 'h', 'd'}) {
		return nil, false
	}

	var trafs []seekTraf
	var cur *seekTraf

	_, err = amp4.ReadBoxStructure(bytes.NewReader(moof), func(h *amp4.ReadHandle) (any, error) {
		switch h.BoxInfo.Type.String() {
		case "moof", "traf":
			return h.Expand()

		case "tfhd":
			box, _, err2 := h.ReadPayload()
			if err2 != nil {
				return nil, err2
			}
			track := findInitTrack(tracks, int(box.(*amp4.Tfhd).TrackID))
			if track == nil {
				return nil, errTerminated
			}
			trafs = append(trafs, seekTraf{trackID: track.ID, timeScale: track.TimeScale})
			cur = &trafs[len(trafs)-1]

		case "tfdt":
			if cur == nil {
				return nil, errTerminated
			}
			box, _, err2 := h.ReadPayload()
			if err2 != nil {
				return nil, err2
			}
			cur.baseTime = int64(box.(*amp4.Tfdt).BaseMediaDecodeTimeV1)

		case "trun":
			if cur == nil {
				return nil, errTerminated
			}
			box, _, err2 := h.ReadPayload()
			if err2 != nil {
				return nil, err2
			}
			cur.entries = box.(*amp4.Trun).Entries
		}
		return nil, nil
	})
	if err != nil || len(trafs) == 0 {
		return nil, false
	}

	p := &seekPart{offset: off, size: int64(moofSize) + mdatSize, trafs: trafs}
	for i, t := range trafs {
		s := durationMp4ToGo(t.baseTime, t.timeScale)
		var d int64
		for _, e := range t.entries {
			d += int64(e.SampleDuration)
		}
		e := durationMp4ToGo(t.baseTime+d, t.timeScale)
		if i == 0 || s < p.start {
			p.start = s
		}
		if i == 0 || e > p.end {
			p.end = e
		}
	}
	return p, true
}

// seekFindPartFrom returns the first valid part with offset in [from, end).
func seekFindPartFrom(
	r io.ReaderAt,
	fileSize int64,
	from int64,
	end int64,
	tracks []*fmp4.InitTrack,
) (*seekPart, bool) {
	limit := min(end, from+seekScanLimit, fileSize)
	buf := make([]byte, seekScanWindow+3)

	for pos := from; pos < limit; pos += seekScanWindow {
		n, err := r.ReadAt(buf[:min(int64(len(buf)), fileSize-pos)], pos)
		if n == 0 && err != nil {
			return nil, false
		}
		window := buf[:n]

		for i := 0; ; {
			j := bytes.Index(window[i:], moofType)
			if j < 0 {
				break
			}
			off := pos + int64(i+j) - 4 // the size field precedes the type
			if off >= from && off < limit {
				if p, ok := seekReadPart(r, fileSize, off, tracks); ok {
					return p, true
				}
			}
			i += j + 1
		}
	}
	return nil, false
}

// seekHasSyncBefore checks that, for every track, the part contains a sync sample
// that starts before target. "Before" uses the same arithmetic as
// segmentFMP4MuxParts, where a sample is before start when
// tfdt + durationGoToMp4(-target) < 0.
func seekHasSyncBefore(p *seekPart, target time.Duration, tracks []*fmp4.InitTrack) bool {
	for _, track := range tracks {
		found := false
		for _, t := range p.trafs {
			if t.trackID != track.ID {
				continue
			}
			offset := durationGoToMp4(-target, t.timeScale)
			dts := t.baseTime
			for _, e := range t.entries {
				if dts+offset >= 0 {
					break
				}
				if (e.SampleFlags & sampleFlagIsNonSyncSample) == 0 {
					found = true
					break
				}
				dts += int64(e.SampleDuration)
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// seekDisabled turns seeking off; used by tests that compare against the full walk.
var seekDisabled = false

// seekToStart returns a reader of the segment f, positioned right after the moov
// box, that starts at the part from which muxing must start for a requested
// start at target from the segment start.
//
// amp4.ReadBoxStructure() always seeks to the beginning of its reader, therefore
// seeking f is not enough: the reader shows the file as if it began at that part.
// Box offsets seen by the parser, and the sample offsets computed from them, are
// relative to that part, and ReadAt translates them back.
func seekToStart(f readSeekerAt, tracks []*fmp4.InitTrack, target time.Duration) (readSeekerAt, error) {
	firstPartOffset, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}

	fileSize, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}

	off := seekFMP4Part(f, fileSize, firstPartOffset, tracks, target)

	// the segment may still be being written, do not limit the reader to its current size
	return io.NewSectionReader(f, off, math.MaxInt64-off), nil
}

// seekLocate returns a known part that starts at or before aim, as close to it
// as a few interpolation probes allow. lo must start at or before aim.
func seekLocate(
	r io.ReaderAt,
	fileSize int64,
	lo *seekPart,
	tracks []*fmp4.InitTrack,
	aim time.Duration,
) *seekPart {
	var hi *seekPart // a part known to start after aim
	hiOff := fileSize

	for range seekMaxProbes {
		if aim-lo.end < seekWalkSpan {
			return lo // close enough: walk the rest
		}

		// bytes per second, from the two closest known points
		var rate float64
		switch {
		case hi != nil && hi.start > lo.start:
			rate = float64(hi.offset-lo.offset) / (hi.start - lo.start).Seconds()
		case lo.end > lo.start:
			rate = float64(lo.size) / (lo.end - lo.start).Seconds()
		default:
			return lo
		}

		est := lo.offset + lo.size + int64(rate*(aim-lo.end).Seconds())
		est = max(est, lo.offset+lo.size)
		if est >= hiOff {
			est = lo.offset + lo.size + (hiOff-lo.offset-lo.size)/2
		}
		if est >= hiOff {
			return lo
		}

		p, ok := seekFindPartFrom(r, fileSize, est, hiOff, tracks)
		switch {
		case !ok:
			hiOff = est
		case p.start <= aim:
			lo = p
		default:
			hi = p
			hiOff = est
		}
	}
	return lo
}

// seekFMP4Part returns the offset to start muxing from, for a requested start at
// target (time from the segment start). firstPartOffset is the offset of the
// first part, right after the moov box.
func seekFMP4Part(
	r io.ReaderAt,
	fileSize int64,
	firstPartOffset int64,
	tracks []*fmp4.InitTrack,
	target time.Duration,
) int64 {
	first, ok := seekReadPart(r, fileSize, firstPartOffset, tracks)
	if !ok || first.start >= target {
		return firstPartOffset
	}

	// land a bit before target, then walk forward part by part and keep the
	// last part that has a sync sample of every track before target
	margin := seekLandBefore
	for range seekMaxStepsBack {
		lo := seekLocate(r, fileSize, first, tracks, target-margin)

		best := int64(-1)
		for p := lo; p != nil && p.start < target; {
			if seekHasSyncBefore(p, target, tracks) {
				best = p.offset
			}
			next, ok2 := seekReadPart(r, fileSize, p.offset+p.size, tracks)
			if !ok2 {
				break
			}
			p = next
		}

		if best >= 0 {
			return best
		}
		if lo.offset == firstPartOffset {
			return firstPartOffset
		}

		// the GOP that contains target starts before lo
		margin = target - lo.start + seekLandBefore
	}

	return firstPartOffset
}

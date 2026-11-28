package playback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	amp4 "github.com/abema/go-mp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recordstore"
	"github.com/bluenviron/mediamtx/internal/test"
)

// fifoSegments creates segments backed by FIFOs: opening one for reading
// blocks until a writer shows up, like opening a file on slow storage.
func fifoSegments(t *testing.T, count int) []*recordstore.Segment {
	t.Helper()
	dir := t.TempDir()
	segments := make([]*recordstore.Segment, count)

	for i := range segments {
		fpath := filepath.Join(dir, strconv.Itoa(i))
		err := syscall.Mkfifo(fpath, 0o600)
		require.NoError(t, err)
		segments[i] = &recordstore.Segment{Fpath: fpath, Start: time.Unix(int64(i), 0)}
	}

	return segments
}

// openWriters opens the write end of every FIFO that has a reader and is
// not in skip. Readers stay blocked in read() until the returned files are
// closed, then they get EOF.
func openWriters(segments []*recordstore.Segment, skip map[string]*os.File) map[string]*os.File {
	out := make(map[string]*os.File)
	for _, seg := range segments {
		if _, ok := skip[seg.Fpath]; ok {
			continue
		}

		// fails with ENXIO when the FIFO has no reader.
		f, err := os.OpenFile(seg.Fpath, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			out[seg.Fpath] = f
		}
	}
	return out
}

func closeAll(files map[string]*os.File) {
	for _, f := range files {
		f.Close()
	}
}

func TestOnList(t *testing.T) {
	for _, ca := range []string{
		"unfiltered",
		"filtered",
		"filtered and gap",
		"different init",
		"start after duration",
		"start before first",
	} {
		t.Run(ca, func(t *testing.T) {
			dir := t.TempDir()

			err := os.Mkdir(filepath.Join(dir, "mypath"), 0o755)
			require.NoError(t, err)

			switch ca {
			case "unfiltered", "filtered", "start before first":
				writeSegment1(t, filepath.Join(dir, "mypath", "2008-11-07_11-22-00-500000.mp4"))
				writeSegment2(t, filepath.Join(dir, "mypath", "2008-11-07_11-23-02-500000.mp4"))
				writeSegment2(t, filepath.Join(dir, "mypath", "2009-11-07_11-23-02-500000.mp4"))

			case "filtered and gap":
				writeSegment1(t, filepath.Join(dir, "mypath", "2008-11-07_11-22-00-500000.mp4"))
				writeSegment2(t, filepath.Join(dir, "mypath", "2008-11-07_11-24-02-500000.mp4"))

			case "different init":
				writeSegment1(t, filepath.Join(dir, "mypath", "2008-11-07_11-22-00-500000.mp4"))
				writeSegment3(t, filepath.Join(dir, "mypath", "2008-11-07_11-23-02-500000.mp4"))

			case "start after duration":
				writeSegment1(t, filepath.Join(dir, "mypath", "2008-11-07_11-22-00-500000.mp4"))
			}

			checked := false

			s := &Server{
				Address:      "127.0.0.1:9996",
				ReadTimeout:  conf.Duration(10 * time.Second),
				WriteTimeout: conf.Duration(10 * time.Second),
				PathConfs: map[string]*conf.Path{
					"mypath": {
						Name:         "mypath",
						RecordPath:   filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
						RecordFormat: conf.RecordFormatFMP4,
					},
				},
				AuthManager: &test.AuthManager{
					AuthenticateImpl: func(req *auth.Request) (*auth.Result, *auth.Error) {
						require.Equal(t, conf.AuthActionPlayback, req.Action)
						require.Equal(t, "myuser", req.Credentials.User)
						require.Equal(t, "mypass", req.Credentials.Pass)
						checked = true
						return &auth.Result{User: req.Credentials.User}, nil
					},
				},
				Parent: test.NilLogger,
			}
			err = s.Initialize()
			require.NoError(t, err)
			defer s.Close()

			u, err := url.Parse("http://myuser:mypass@localhost:9996/list?start=")
			require.NoError(t, err)

			v := url.Values{}
			v.Set("path", "mypath")

			switch ca {
			case "filtered":
				v.Set("start", time.Date(2008, 11, 7, 11, 22, 1, 500000000, time.Local).Format(time.RFC3339Nano))
				v.Set("end", time.Date(2009, 11, 7, 11, 23, 4, 500000000, time.Local).Format(time.RFC3339Nano))

			case "filtered and gap":
				v.Set("start", time.Date(2008, 11, 7, 11, 23, 20, 500000000, time.Local).Format(time.RFC3339Nano))
				v.Set("end", time.Date(2009, 11, 7, 11, 23, 4, 500000000, time.Local).Format(time.RFC3339Nano))

			case "start after duration":
				v.Set("start", time.Date(2010, 11, 7, 11, 23, 20, 500000000, time.Local).Format(time.RFC3339Nano))

			case "start before first":
				v.Set("start", time.Date(2007, 11, 7, 11, 23, 20, 500000000, time.Local).Format(time.RFC3339Nano))
			}

			u.RawQuery = v.Encode()

			req, err := http.NewRequest(http.MethodGet, u.String(), nil)
			require.NoError(t, err)

			res, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer res.Body.Close()

			if ca == "start after duration" {
				require.Equal(t, http.StatusNotFound, res.StatusCode)
				return
			}

			require.Equal(t, http.StatusOK, res.StatusCode)

			var out any
			err = json.NewDecoder(res.Body).Decode(&out)
			require.NoError(t, err)

			switch ca {
			case "unfiltered", "start before first":
				require.Equal(t, []any{
					map[string]any{
						"duration": float64(66),
						"start":    time.Date(2008, 11, 7, 11, 22, 0, 500000000, time.Local).Format(time.RFC3339Nano),
						"url": "http://localhost:9996/get?duration=66&path=mypath&start=" +
							url.QueryEscape(time.Date(2008, 11, 7, 11, 22, 0, 500000000, time.Local).Format(time.RFC3339Nano)),
					},
					map[string]any{
						"duration": float64(4),
						"start":    time.Date(2009, 11, 7, 11, 23, 2, 500000000, time.Local).Format(time.RFC3339Nano),
						"url": "http://localhost:9996/get?duration=4&path=mypath&start=" +
							url.QueryEscape(time.Date(2009, 11, 7, 11, 23, 2, 500000000, time.Local).Format(time.RFC3339Nano)),
					},
				}, out)

			case "filtered":
				require.Equal(t, []any{
					map[string]any{
						"duration": float64(65),
						"start":    time.Date(2008, 11, 7, 11, 22, 1, 500000000, time.Local).Format(time.RFC3339Nano),
						"url": "http://localhost:9996/get?duration=65&path=mypath&start=" +
							url.QueryEscape(time.Date(2008, 11, 7, 11, 22, 1, 500000000, time.Local).Format(time.RFC3339Nano)),
					},
					map[string]any{
						"duration": float64(2),
						"start":    time.Date(2009, 11, 7, 11, 23, 2, 500000000, time.Local).Format(time.RFC3339Nano),
						"url": "http://localhost:9996/get?duration=2&path=mypath&start=" +
							url.QueryEscape(time.Date(2009, 11, 7, 11, 23, 2, 500000000, time.Local).Format(time.RFC3339Nano)),
					},
				}, out)

			case "filtered and gap":
				require.Equal(t, []any{
					map[string]any{
						"duration": float64(4),
						"start":    time.Date(2008, 11, 7, 11, 24, 2, 500000000, time.Local).Format(time.RFC3339Nano),
						"url": "http://localhost:9996/get?duration=4&path=mypath&start=" +
							url.QueryEscape(time.Date(2008, 11, 7, 11, 24, 2, 500000000, time.Local).Format(time.RFC3339Nano)),
					},
				}, out)

			case "different init":
				require.Equal(t, []any{
					map[string]any{
						"duration": float64(62),
						"start":    time.Date(2008, 11, 7, 11, 22, 0, 500000000, time.Local).Format(time.RFC3339Nano),
						"url": "http://localhost:9996/get?duration=62&path=mypath&start=" +
							url.QueryEscape(time.Date(2008, 11, 7, 11, 22, 0, 500000000, time.Local).Format(time.RFC3339Nano)),
					},
					map[string]any{
						"duration": float64(1),
						"start":    time.Date(2008, 11, 7, 11, 23, 2, 500000000, time.Local).Format(time.RFC3339Nano),
						"url": "http://localhost:9996/get?duration=1&path=mypath&start=" +
							url.QueryEscape(time.Date(2008, 11, 7, 11, 23, 2, 500000000, time.Local).Format(time.RFC3339Nano)),
					},
				}, out)
			}

			require.True(t, checked)
		})
	}
}

func writeDuration(f io.ReadWriteSeeker, d time.Duration) error {
	_, err := f.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}

	// check and skip ftyp header and content

	buf := make([]byte, 8)
	_, err = io.ReadFull(f, buf)
	if err != nil {
		return err
	}

	if !bytes.Equal(buf[4:], []byte{'f', 't', 'y', 'p'}) {
		return fmt.Errorf("ftyp box not found")
	}

	ftypSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

	_, err = f.Seek(int64(ftypSize), io.SeekStart)
	if err != nil {
		return err
	}

	// check and skip moov header

	_, err = io.ReadFull(f, buf)
	if err != nil {
		return err
	}

	if !bytes.Equal(buf[4:], []byte{'m', 'o', 'o', 'v'}) {
		return fmt.Errorf("moov box not found")
	}

	moovSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

	moovPos, err := f.Seek(8, io.SeekCurrent)
	if err != nil {
		return err
	}

	var mvhd amp4.Mvhd
	_, err = amp4.Unmarshal(f, uint64(moovSize-8), &mvhd, amp4.Context{})
	if err != nil {
		return err
	}

	mvhd.DurationV0 = uint32(d / time.Millisecond)

	_, err = f.Seek(moovPos, io.SeekStart)
	if err != nil {
		return err
	}

	_, err = amp4.Marshal(f, &mvhd, amp4.Context{})
	if err != nil {
		return err
	}

	return nil
}

func TestOnListInvalidPath(t *testing.T) {
	s := &Server{
		Address:      "127.0.0.1:9996",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		PathConfs: map[string]*conf.Path{
			"all_others": {
				Name:         "all_others",
				RecordPath:   filepath.Join(t.TempDir(), "mypath/%Y-%m-%d_%H-%M-%S-%f"),
				RecordFormat: conf.RecordFormatFMP4,
			},
		},
		AuthManager: test.NilAuthManager,
		Parent:      test.NilLogger,
	}
	err := s.Initialize()
	require.NoError(t, err)
	defer s.Close()

	u, err := url.Parse("http://localhost:9996/list")
	require.NoError(t, err)

	v := url.Values{}
	v.Set("path", "group/../cam1")
	u.RawQuery = v.Encode()

	res, err := http.Get(u.String())
	require.NoError(t, err)
	defer res.Body.Close()

	require.Equal(t, http.StatusBadRequest, res.StatusCode)
}

func TestOnListCachedDuration(t *testing.T) {
	dir := t.TempDir()

	err := os.Mkdir(filepath.Join(dir, "mypath"), 0o755)
	require.NoError(t, err)

	func() {
		var f *os.File
		f, err = os.Create(filepath.Join(dir, "mypath", "2008-11-07_11-22-00-500000.mp4"))
		require.NoError(t, err)
		defer f.Close()

		init := fmp4.Init{
			Tracks: []*fmp4.InitTrack{
				{
					ID:        1,
					TimeScale: 90000,
					Codec: &mcodecs.H264{
						SPS: test.FormatH264.SPS,
						PPS: test.FormatH264.PPS,
					},
				},
			},
		}

		err = init.Marshal(f)
		require.NoError(t, err)

		err = writeDuration(f, 50*time.Second)
		require.NoError(t, err)
	}()

	s := &Server{
		Address:      "127.0.0.1:9996",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		PathConfs: map[string]*conf.Path{
			"mypath": {
				Name:         "mypath",
				RecordPath:   filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
				RecordFormat: conf.RecordFormatFMP4,
			},
		},
		AuthManager: test.NilAuthManager,
		Parent:      test.NilLogger,
	}
	err = s.Initialize()
	require.NoError(t, err)
	defer s.Close()

	u, err := url.Parse("http://myuser:mypass@localhost:9996/list")
	require.NoError(t, err)

	v := url.Values{}
	v.Set("path", "mypath")
	u.RawQuery = v.Encode()

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	require.NoError(t, err)

	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	require.Equal(t, http.StatusOK, res.StatusCode)

	var out any
	err = json.NewDecoder(res.Body).Decode(&out)
	require.NoError(t, err)

	require.Equal(t, []any{
		map[string]any{
			"duration": float64(50),
			"start":    time.Date(2008, 11, 7, 11, 22, 0, 500000000, time.Local).Format(time.RFC3339Nano),
			"url": "http://localhost:9996/get?duration=50&path=mypath&start=" +
				url.QueryEscape(time.Date(2008, 11, 7, 11, 22, 0, 500000000, time.Local).Format(time.RFC3339Nano)),
		},
	}, out)
}

func TestOnListXForwardedProto(t *testing.T) {
	dir := t.TempDir()

	err := os.Mkdir(filepath.Join(dir, "mypath"), 0o755)
	require.NoError(t, err)

	writeSegment1(t, filepath.Join(dir, "mypath", "2008-11-07_11-22-00-500000.mp4"))

	var trustedProxies conf.IPNetworks
	err = json.Unmarshal([]byte(`["127.0.0.0/8"]`), &trustedProxies)
	require.NoError(t, err)

	s := &Server{
		Address:        "127.0.0.1:9996",
		ReadTimeout:    conf.Duration(10 * time.Second),
		WriteTimeout:   conf.Duration(10 * time.Second),
		TrustedProxies: trustedProxies,
		PathConfs: map[string]*conf.Path{
			"mypath": {
				Name:         "mypath",
				RecordPath:   filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
				RecordFormat: conf.RecordFormatFMP4,
			},
		},
		AuthManager: test.NilAuthManager,
		Parent:      test.NilLogger,
	}
	err = s.Initialize()
	require.NoError(t, err)
	defer s.Close()

	u, err := url.Parse("http://localhost:9996/list")
	require.NoError(t, err)

	v := url.Values{}
	v.Set("path", "mypath")
	u.RawQuery = v.Encode()

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	require.NoError(t, err)

	req.Header.Set("X-Forwarded-Proto", "https")

	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	require.Equal(t, http.StatusOK, res.StatusCode)

	var out any
	err = json.NewDecoder(res.Body).Decode(&out)
	require.NoError(t, err)

	require.Equal(t, []any{
		map[string]any{
			"duration": float64(62),
			"start":    time.Date(2008, 11, 7, 11, 22, 0, 500000000, time.Local).Format(time.RFC3339Nano),
			"url": "https://localhost:9996/get?duration=62&path=mypath&start=" +
				url.QueryEscape(time.Date(2008, 11, 7, 11, 22, 0, 500000000, time.Local).Format(time.RFC3339Nano)),
		},
	}, out)
}

func TestOnListCanceledContext(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "2008-11-07_11-22-00-500000.mp4")
	writeSegment1(t, fpath)

	segments := []*recordstore.Segment{{
		Fpath: fpath,
		Start: time.Date(2008, 11, 7, 11, 22, 0, 500000000, time.Local),
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := parseAndConcatenate(ctx, conf.RecordFormatFMP4, segments)
	require.ErrorIs(t, err, context.Canceled)
}

func TestParseSegmentsCanceled(t *testing.T) {
	fpath := filepath.Join(t.TempDir(), "2008-11-07_11-22-00-500000.mp4")
	writeSegment1(t, fpath)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := parseSegments(ctx, []*recordstore.Segment{{
		Fpath: fpath,
		Start: time.Date(2008, 11, 7, 11, 22, 0, 500000000, time.Local),
	}})
	require.ErrorIs(t, err, context.Canceled)
}

func TestParseSegmentsMaxParallel(t *testing.T) {
	segments := fifoSegments(t, 4*maxParallelSegmentReads)

	done := make(chan struct{})
	go func() {
		// every read fails with EOF, since nothing is written to the FIFOs.
		_, _ = parseSegments(context.Background(), segments)
		close(done)
	}()

	maxReaders := 0

	for {
		time.Sleep(50 * time.Millisecond)

		writers := openWriters(segments, nil)
		maxReaders = max(maxReaders, len(writers))
		closeAll(writers)

		select {
		case <-done:
			require.Greater(t, maxReaders, 1)
			require.LessOrEqual(t, maxReaders, maxParallelSegmentReads)
			return
		default:
		}
	}
}

func TestParseSegmentsCanceledWhileReading(t *testing.T) {
	segments := fifoSegments(t, 4*maxParallelSegmentReads)
	ctx, cancel := context.WithCancel(context.Background())

	var err error
	done := make(chan struct{})
	go func() {
		_, err = parseSegments(ctx, segments)
		close(done)
	}()

	// wait until all reads in progress are blocked.
	writers := make(map[string]*os.File)
	for len(writers) < maxParallelSegmentReads {
		time.Sleep(10 * time.Millisecond)
		maps.Copy(writers, openWriters(segments, writers))
	}

	cancel()
	closeAll(writers)

	// any read started after cancel() blocks in open() forever.
	select {
	case <-done:
		require.Error(t, err)

	case <-time.After(2 * time.Second):
		for {
			closeAll(openWriters(segments, nil))
			select {
			case <-done:
				t.Fatal("segments were read after the context was canceled")
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
}

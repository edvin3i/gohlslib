package gohlslib

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
)

// a real MPEG-TS capture in which a mid-stream IDR is lost and the stream
// resumes on a P-frame (same SPS). Before the fix, the H264 DTS extractor
// returns "too many reordered frames" and gohlslib treats it as fatal, killing
// the muxer.
//
//go:embed testdata/h264_dropped_idr.ts
var testTSDroppedIDR []byte

type recoveryAU struct {
	pts int64
	au  [][]byte
}

// TestMuxerH264RecoverFromLostIDR feeds the capture above into a muxer. With the
// fix, the muxer drops access units until the next IDR and re-anchors the DTS
// extractor, so no WriteH264 call fails and the stream keeps flowing.
func TestMuxerH264RecoverFromLostIDR(t *testing.T) {
	r, err := mpegts.NewReader(bytes.NewReader(testTSDroppedIDR))
	require.NoError(t, err)

	var h264Track *mpegts.Track
	for _, tr := range r.Tracks() {
		if _, ok := tr.Codec.(*mpegts.CodecH264); ok {
			h264Track = tr
			break
		}
	}
	require.NotNil(t, h264Track)

	var aus []recoveryAU
	r.OnDataH264(h264Track, func(pts int64, _ int64, au [][]byte) error {
		cp := make([][]byte, len(au))
		for i, nalu := range au {
			cp[i] = append([]byte(nil), nalu...)
		}
		aus = append(aus, recoveryAU{pts: pts, au: cp})
		return nil
	})
	for {
		err = r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
	}
	require.Greater(t, len(aus), 30, "fixture must contain enough access units")

	// initialize the track from the in-band SPS/PPS.
	var sps, pps []byte
	for _, e := range aus {
		for _, nalu := range e.au {
			switch h264.NALUType(nalu[0] & 0x1f) {
			case h264.NALUTypeSPS:
				if sps == nil {
					sps = nalu
				}
			case h264.NALUTypePPS:
				if pps == nil {
					pps = nalu
				}
			}
		}
		if sps != nil && pps != nil {
			break
		}
	}
	require.NotNil(t, sps)
	require.NotNil(t, pps)

	track := &Track{
		Codec:     &codecs.H264{SPS: sps, PPS: pps},
		ClockRate: 90000,
	}

	m := &Muxer{
		Variant:            MuxerVariantMPEGTS,
		SegmentCount:       30,
		SegmentMinDuration: 1 * time.Second,
		Tracks:             []*Track{track},
	}
	require.NoError(t, m.Start())
	defer m.Close()

	written := 0
	for _, e := range aus {
		ntp := testTime.Add(time.Duration(e.pts) * time.Second / time.Duration(track.ClockRate))
		err := m.WriteH264(track, ntp, e.pts, e.au)
		require.NoError(t, err,
			"WriteH264 must survive a mid-stream lost IDR (au #%d, pts=%d)", written, e.pts)
		written++
	}
	require.Greater(t, written, 30)
}

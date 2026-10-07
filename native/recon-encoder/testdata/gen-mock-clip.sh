#!/usr/bin/env bash
# Regenerates mock_clip.h264: the canned H.264 stream that the mock backend's
# "replay encoder" plays back frame by frame (it is compiled into
# recon-encoder.exe, see CMakeLists.txt).
#
# Requirements on the clip (src/mock/replay_encoder.cpp relies on them):
#   - 320x180, exactly 60 frames, Annex-B byte stream
#   - closed GOP: frame 0 is an IDR with SPS/PPS, frames 1..59 are P frames
#     (no B frames, one reference), so jumping back to frame 0 is always legal
#   - an access unit delimiter (NAL type 9) starts every frame: that is how the
#     replay encoder splits the stream into frames
#   - Constrained Baseline, so every WebCodecs H.264 decoder accepts it
#
# Usage: native/recon-encoder/testdata/gen-mock-clip.sh   (needs ffmpeg with libx264)
set -euo pipefail
cd "$(dirname "$0")"

out=mock_clip.h264
ffmpeg -hide_banner -loglevel error -y \
	-f lavfi -i "testsrc2=size=320x180:rate=60" -frames:v 60 \
	-c:v libx264 -threads 1 -preset veryfast -tune zerolatency \
	-profile:v baseline -pix_fmt yuv420p \
	-g 60 -keyint_min 60 -sc_threshold 0 -bf 0 -refs 1 -crf 30 \
	-x264-params "aud=1:repeat-headers=1:open-gop=0:scenecut=0" \
	-f h264 "$out"

# Sanity checks: frame count, key frames, size budget.
frames=$(ffprobe -v error -select_streams v:0 -count_frames -show_entries stream=nb_read_frames -of csv=p=0 "$out")
keys=$(ffprobe -v error -select_streams v:0 -show_frames -show_entries frame=key_frame -of csv=p=0 "$out" | grep -c '^1')
size=$(stat -c %s "$out")
if [ "$frames" != 60 ] || [ "$keys" != 1 ] || [ "$size" -gt 200000 ]; then
	echo "unexpected clip: frames=$frames keyframes=$keys bytes=$size" >&2
	exit 1
fi
echo "$out: $frames frames, $keys key frame, $size bytes"

// Dirty-area fraction of a captured image (GUIDE 5 "present-synced capture
// with flip timestamps + dirty rects": the rate controller can cut the bitrate
// on a static desktop). The captures report rectangles that changed since the
// last image they handed out (DXGI DDA GetFrameMoveRects / GetFrameDirtyRects,
// AMD Direct Capture AMF_DISPLAYCAPTURE_DIRTY_RECTS); this turns them into the
// share of the picture they cover. No OS or GPU API here; checked by
// --self-test-pacer (capture/pacer.cpp).
#pragma once

#include <cstddef>
#include <cstdint>

namespace recon {

// A rectangle in pixels, right / bottom exclusive: the layout of the Win32
// RECT and of AMFRect, so their arrays can be passed as they are.
struct DirtyRect {
    int32_t left = 0, top = 0, right = 0, bottom = 0;
};

// Rect lists longer than this are summed instead of united (the union costs
// O(n^2 log n)): an upper bound, which only matters on a busy screen anyway.
constexpr size_t kMaxDirtyUnionRects = 256;

// The share (0..1) of a width x height picture covered by the union of the
// rects, each clipped to the picture (empty and inverted ones count nothing).
// Overlapping rects (a move rect's destination that is also dirty, the same
// caret reported twice) are counted once. 0 for an empty list or picture.
double dirtyFraction(const DirtyRect* rects, size_t count, uint32_t width, uint32_t height);

// Two deliveries merged into one (the pacer kept only the newest image, so
// the encoder sees both changes): their sum, at most 1 (an upper bound: the
// rects of the dropped image are gone). -1 (unknown) if either is unknown.
inline float mergeDirty(float a, float b) {
    if (a < 0 || b < 0) return -1;
    const float s = a + b;
    return s > 1 ? 1.0f : s;
}

}  // namespace recon

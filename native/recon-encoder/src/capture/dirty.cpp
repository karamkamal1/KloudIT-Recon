#include "capture/dirty.hpp"

#include <algorithm>
#include <utility>
#include <vector>

namespace recon {

double dirtyFraction(const DirtyRect* rects, size_t count, uint32_t width, uint32_t height) {
    if (!rects || !count || !width || !height) return 0;
    const int64_t w = width, h = height;
    // Clipped, non-empty rects.
    std::vector<DirtyRect> r;
    r.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        DirtyRect c;
        c.left = int32_t(std::clamp<int64_t>(rects[i].left, 0, w));
        c.right = int32_t(std::clamp<int64_t>(rects[i].right, 0, w));
        c.top = int32_t(std::clamp<int64_t>(rects[i].top, 0, h));
        c.bottom = int32_t(std::clamp<int64_t>(rects[i].bottom, 0, h));
        if (c.left < c.right && c.top < c.bottom) r.push_back(c);
    }
    const double total = double(w) * double(h);
    if (r.size() > kMaxDirtyUnionRects) {
        double sum = 0;
        for (const DirtyRect& c : r) sum += double(c.right - c.left) * double(c.bottom - c.top);
        return std::min(1.0, sum / total);
    }
    // Union area by vertical slabs: between two neighbouring x edges, the
    // covered height is the union of the y ranges of the rects spanning it.
    std::vector<int32_t> xs;
    xs.reserve(r.size() * 2);
    for (const DirtyRect& c : r) {
        xs.push_back(c.left);
        xs.push_back(c.right);
    }
    std::sort(xs.begin(), xs.end());
    xs.erase(std::unique(xs.begin(), xs.end()), xs.end());
    double area = 0;
    std::vector<std::pair<int32_t, int32_t>> ys;
    for (size_t i = 0; i + 1 < xs.size(); ++i) {
        const int32_t x0 = xs[i], x1 = xs[i + 1];
        ys.clear();
        for (const DirtyRect& c : r) {
            if (c.left <= x0 && c.right >= x1) ys.emplace_back(c.top, c.bottom);
        }
        if (ys.empty()) continue;
        std::sort(ys.begin(), ys.end());
        int64_t covered = 0;
        int32_t from = ys[0].first, to = ys[0].second;
        for (size_t k = 1; k < ys.size(); ++k) {
            if (ys[k].first > to) {
                covered += to - from;
                from = ys[k].first;
                to = ys[k].second;
            } else {
                to = std::max(to, ys[k].second);
            }
        }
        covered += to - from;
        area += double(x1 - x0) * double(covered);
    }
    return std::min(1.0, area / total);
}

}  // namespace recon

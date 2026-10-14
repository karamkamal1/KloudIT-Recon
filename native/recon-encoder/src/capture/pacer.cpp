#include "capture/pacer.hpp"

#include <algorithm>
#include <cmath>
#include <cstdio>
#include <limits>
#include <vector>

#include "capture/dirty.hpp"
#include "selftest.hpp"

namespace recon {

void FramePacer::reset(int64_t qpcFrequency, int fps, int idleRepeatMs, int64_t now) {
    freq_ = qpcFrequency > 0 ? qpcFrequency : 1;
    idle_ = freq_ * (idleRepeatMs > 0 ? idleRepeatMs : 100) / 1000;
    setFps(fps);
    nextDue_ = now;
    lastDelivered_ = now;
    pendingAt_ = lastArrival_ = 0;
    nIntervals_ = nextInterval_ = 0;
    interval_ = predict_ = 0;
    regular_ = false;
}

void FramePacer::setFps(int fps) {
    if (fps <= 0) return;
    period_ = (freq_ + fps - 1) / fps;
    early_ = period_ * 3 / 8;
    late_ = period_ / 8;
}

void FramePacer::arrived(int64_t presentQpc, int presents) {
    // The interval since the previous image, per present it stands for.
    // Only intervals up to two frame intervals say how fast the source
    // presents for rule 3; a longer one (a pause, a slow game) starts over.
    const int64_t gap = lastArrival_ ? presentQpc - lastArrival_ : 0;
    if (gap > 0) {
        const int64_t each = gap / std::max(1, presents);
        if (each <= 2 * period_) {
            intervals_[nextInterval_] = each;
            nextInterval_ = (nextInterval_ + 1) % kIntervals;
            nIntervals_ = std::min(nIntervals_ + 1, kIntervals);
        } else {
            nIntervals_ = nextInterval_ = 0;
        }
    }
    if (presentQpc > lastArrival_) lastArrival_ = presentQpc;
    pendingAt_ = presentQpc;
    interval_ = predict_ = 0;
    regular_ = false;
    if (nIntervals_ >= 3) {
        const int n = nIntervals_;
        int64_t v[kIntervals];
        std::copy(intervals_, intervals_ + n, v);
        std::sort(v, v + n);
        interval_ = v[n / 2];
        predict_ = v[n - 1 - n / 8];
        // A game on the display's vblank grid that misses some refreshes
        // presents one or two refreshes apart: not regular, its next present
        // cannot be predicted (rule 3 keeps out).
        regular_ = v[n - 1] * 2 <= interval_ * 3;
    }
}

FramePacer::Decision FramePacer::decide(int64_t now, bool havePending, bool haveLast) const {
    const int64_t slot = nextDue_ - early_;
    if (havePending) {
        if (now < slot) return {Decision::Wait, slot, false};
        // Rule 3: the image came before its slot opened and the source
        // presents regularly and faster than fps. If its next present is due
        // less than half an interval after the slot (by the 7/8 quantile of
        // the source's present intervals), wait for that one instead of
        // sending this one late (it would make the next one wait for the
        // following slot, and so on: the beat of the two rates). The waiting
        // image still goes out should that present not come: a little after
        // it was due (a quarter of its interval, at most 2 ms), never later
        // than one interval after its own present, and never so late that
        // the slot schedule is lost (late_ after the slot).
        if (pendingAt_ < slot && presentsFaster()) {
            const int64_t due = pendingAt_ + predict_;
            if (due > slot && due - slot < period_ / 2) {
                const int64_t until = std::min({due + std::min(interval_ / 4, freq_ * 2 / 1000), pendingAt_ + period_, nextDue_ + late_});
                if (now < until) return {Decision::Wait, until, true};
            }
        }
        return {Decision::DeliverPending, 0, false};
    }
    if (haveLast) {
        const int64_t repeatAt = std::max(lastDelivered_ + std::max(idle_, period_), slot);
        if (now >= repeatAt) return {Decision::DeliverRepeat, 0, false};
        return {Decision::Wait, repeatAt, false};
    }
    return {Decision::Wait, std::numeric_limits<int64_t>::max(), false};
}

void FramePacer::delivered(int64_t now, bool repeat) {
    // Repeats take no slot (policy 4). A new image at most late_ after its
    // slot keeps the schedule (policy 2: a held image of rule 3, or a present
    // that came just after the slot was due, costs no output slot).
    if (!repeat) nextDue_ = now > nextDue_ && now - nextDue_ <= late_ ? nextDue_ + period_ : std::max(nextDue_, now) + period_;
    lastDelivered_ = now;
}

void FirstImageWatchdog::reset(int64_t qpcFrequency, int64_t now) {
    freq_ = qpcFrequency > 0 ? qpcFrequency : 1;
    start_ = now;
    next_ = now + freq_ * kFirstMs / 1000;
    reported_ = done_ = false;
}

int64_t FirstImageWatchdog::due(int64_t now) {
    if (done_ || now < next_) return 0;
    reported_ = true;
    next_ = now + freq_ * kEveryMs / 1000;  // from now: a capture thread held up long reports once
    return std::max<int64_t>(1, (now - start_) * 1000 / freq_);
}

int64_t FirstImageWatchdog::image(int64_t now) {
    if (done_) return 0;
    done_ = true;
    return reported_ ? std::max<int64_t>(1, (now - start_) * 1000 / freq_) : 0;
}

// --- self-test ----------------------------------------------------------------------

namespace {

struct Delivery {
    int64_t at;
    int64_t present;
    bool repeat;
};

// Drives the pacer like PacedCapture::next() does, on a simulated clock (1 tick
// = 1 us): presents arrive at the given times. While a pending image waits
// asleep for its slot, the presents meanwhile are taken when it wakes up (the
// newest wins, standing for all of them: DDA's AccumulatedFrames); in a
// watching wait (rule 3) and with nothing pending, each present is taken the
// moment it comes.
std::vector<Delivery> simulate(const std::vector<int64_t>& presents, int fps, int idleMs, int64_t end) {
    FramePacer p;
    p.reset(1000000, fps, idleMs, 0);
    std::vector<Delivery> out;
    size_t next = 0;
    bool pending = false, haveLast = false;
    int64_t pendingPresent = 0, t = 0;
    while (t <= end) {
        const auto d = p.decide(t, pending, haveLast);
        if (d.kind == FramePacer::Decision::DeliverPending) {
            out.push_back({t, pendingPresent, false});
            p.delivered(t, false);
            pending = false;
            haveLast = true;
            continue;
        }
        if (d.kind == FramePacer::Decision::DeliverRepeat) {
            out.push_back({t, 0, true});
            p.delivered(t, true);
            continue;
        }
        if (pending && !d.watch) {
            t = d.until;
            int n = 0;
            while (next < presents.size() && presents[next] <= t) pendingPresent = presents[next++], ++n;
            if (n) p.arrived(pendingPresent, n);
            continue;
        }
        const int64_t nextPresent = next < presents.size() ? presents[next] : std::numeric_limits<int64_t>::max();
        if (nextPresent <= d.until) {
            t = std::max(t, nextPresent);
            pending = true;
            pendingPresent = nextPresent;
            ++next;
            p.arrived(nextPresent, 1);
        } else {
            t = d.until;
        }
    }
    return out;
}

// The rule before the second hardware run, for comparison: slots one
// interval apart with a quarter interval of slack, an image presented before
// its slot waits for it and goes out when it opens (the newest presented by
// then). No idle repeats (the comparisons are of steady presents).
std::vector<Delivery> simulateSlotRule(const std::vector<int64_t>& presents, int fps, int64_t end) {
    const int64_t period = (1000000 + fps - 1) / fps, early = period / 4;
    std::vector<Delivery> out;
    int64_t nextDue = 0;
    for (size_t i = 0; i < presents.size() && presents[i] <= end;) {
        const int64_t at = std::max(presents[i], nextDue - early);
        while (i + 1 < presents.size() && presents[i + 1] <= at) ++i;  // newest wins
        out.push_back({at, presents[i], false});
        nextDue = std::max(nextDue, at) + period;
        ++i;
    }
    return out;
}

std::vector<int64_t> steady(double hz, int64_t from, int64_t to, int jitterUs = 0) {
    std::vector<int64_t> v;
    uint32_t seed = 7;
    for (int i = 0;; ++i) {
        int64_t t = from + int64_t(i * 1e6 / hz);
        if (jitterUs) {
            seed = seed * 1664525u + 1013904223u;
            t = std::max(from, t + int64_t(seed >> 8) % (2 * jitterUs + 1) - jitterUs);
        }
        if (t > to) break;
        v.push_back(t);
    }
    std::sort(v.begin(), v.end());
    return v;
}

int failures = 0;

void expect(bool ok, const char* name, const char* what) {
    if (!ok) {
        ++failures;
        std::printf("  %s: FAIL %s\n", name, what);
    }
}

// How long delivered new images waited between their present and going out
// (us), and how many went out: what rule 3 cuts is the second hardware run's
// 9 ms p50 / 15.5 ms p95 at 60 fps on a 75 Hz display, without fewer frames.
struct Waits {
    int64_t mean = 0, p50 = 0, p95 = 0, p99 = 0;
    size_t frames = 0;  // new images delivered
};

Waits waits(const std::vector<Delivery>& d) {
    std::vector<int64_t> w;
    for (const auto& x : d) {
        if (!x.repeat) w.push_back(x.at - x.present);
    }
    if (w.empty()) return {};
    std::sort(w.begin(), w.end());
    int64_t sum = 0;
    for (const int64_t v : w) sum += v;
    const auto at = [&w](size_t pct) { return w[std::min(w.size() - 1, w.size() * pct / 100)]; };
    return {sum / int64_t(w.size()), at(50), at(95), at(99), w.size()};
}

// The share (percent) of steps between consecutive new images that advance
// the source by exactly `step` presents: at twice the stream's rate every
// step should be 2 (one present left out each time); a 1 or a 3 is a visible
// judder.
int stepShare(const std::vector<int64_t>& presents, const std::vector<Delivery>& d, int64_t step) {
    size_t steps = 0, hits = 0;
    int64_t last = -1;
    for (const auto& x : d) {
        if (x.repeat) continue;
        const int64_t i = std::lower_bound(presents.begin(), presents.end(), x.present) - presents.begin();
        if (last >= 0) {
            ++steps;
            if (i - last == step) ++hits;
        }
        last = i;
    }
    return steps ? int(hits * 100 / steps) : 0;
}

// Common properties: in any stretch of time no more new images than fps
// allows (plus the one frame the early allowance can add: 3/8 of an interval
// early, 1/8 late keeping the schedule), new images at least half an interval
// apart, a repeat only after max(idle, interval)
// without any delivery, every delivered image the newest one presented at that
// time and never older than one frame interval. (A new image right after a
// repeat is allowed: repeats take no slot.)
Waits common(const char* name, const std::vector<int64_t>& presents, const std::vector<Delivery>& d, int fps,
             int64_t duration, int idleMs = 100) {
    const int64_t period = (1000000 + fps - 1) / fps, early = period / 2;
    const int64_t idle = std::max<int64_t>(int64_t(idleMs) * 1000, period);
    size_t n = 0;
    int64_t minGap = std::numeric_limits<int64_t>::max(), lastNew = -1;
    for (size_t i = 0; i < d.size(); ++i) {
        if (d[i].repeat) {
            if (i) expect(d[i].at - d[i - 1].at >= idle, name, "repeat sooner than the idle interval");
            continue;
        }
        if (d[i].at <= duration) ++n;
        if (lastNew >= 0) minGap = std::min(minGap, d[i].at - lastNew);
        lastNew = d[i].at;
        const auto it = std::upper_bound(presents.begin(), presents.end(), d[i].at);
        const int64_t newest = *(it - 1);
        expect(d[i].present == newest, name, "delivered an image while a newer one was waiting");
        expect(d[i].at - d[i].present <= period, name, "delivered image older than one frame interval");
    }
    expect(int64_t(n) <= (duration + early) / period + 1, name, "more new images than fps allows");
    if (minGap != std::numeric_limits<int64_t>::max()) {
        expect(minGap >= period - early, name, "new images closer than half a frame interval");
    } else {
        minGap = 0;
    }
    const Waits w = waits(d);
    std::printf("  %-44s %zu frames (%zu repeats), min gap %lld us, wait mean %lld us p50 %lld p95 %lld p99 %lld\n", name,
                d.size(), size_t(std::count_if(d.begin(), d.end(), [](const Delivery& x) { return x.repeat; })),
                static_cast<long long>(minGap), static_cast<long long>(w.mean), static_cast<long long>(w.p50),
                static_cast<long long>(w.p95), static_cast<long long>(w.p99));
    return w;
}

// The slot rule's waits on the same presents (simulateSlotRule), printed for
// comparison.
Waits slotRule(const std::vector<int64_t>& presents, int fps, int64_t end) {
    const Waits w = waits(simulateSlotRule(presents, fps, end));
    std::printf("  %-44s %zu frames with the slot rule alone: wait mean %lld us p50 %lld p95 %lld p99 %lld\n", "", w.frames,
                static_cast<long long>(w.mean), static_cast<long long>(w.p50), static_cast<long long>(w.p95),
                static_cast<long long>(w.p99));
    return w;
}

// A game's presents: frame times of meanUs +- about spreadUs (the sum of three
// uniform draws, roughly normal), at least 2 ms.
std::vector<int64_t> game(int64_t meanUs, int64_t spreadUs, int64_t to) {
    std::vector<int64_t> v;
    uint32_t seed = 11;
    const auto draw = [&seed, spreadUs] {
        seed = seed * 1664525u + 1013904223u;
        return int64_t(seed >> 8) % (2 * spreadUs + 1) - spreadUs;
    };
    for (int64_t t = 0; t <= to;) {
        v.push_back(t);
        t += std::max<int64_t>(2000, meanUs + (draw() + draw() + draw()) / 3);
    }
    return v;
}

// A game rendering at gameFps (+- jitterUs per frame) on a display refreshing
// at gridHz with vsync or through the compositor: each frame is presented at
// the next refresh after it is done (at least one refresh after the last), so
// the intervals are one or two refreshes (a game slower than the display
// misses some), plus up to 0.2 ms of capture latency.
std::vector<int64_t> grid(double gameFps, double gridHz, int jitterUs, int64_t to) {
    std::vector<int64_t> v;
    uint32_t seed = 3;
    const double refresh = 1e6 / gridHz;
    int64_t last = -1;
    for (int64_t i = 0;; ++i) {
        seed = seed * 1664525u + 1013904223u;
        const double done = double(i) * 1e6 / gameFps + double(int64_t(seed >> 8) % (2 * jitterUs + 1) - jitterUs);
        int64_t t = int64_t(std::ceil(done / refresh) * refresh);
        if (t <= last) t = int64_t((double(std::llround(double(last) / refresh)) + 1) * refresh);
        if (t > to) break;
        seed = seed * 1664525u + 1013904223u;
        v.push_back(t + int64_t(seed >> 8) % 200);
        last = t;
    }
    return v;
}

// A game vsynced to a gridHz display that misses `missPct` percent of the
// refreshes (each missed one makes an interval of two refreshes).
std::vector<int64_t> missedRefreshes(double gridHz, int missPct, int64_t to) {
    std::vector<int64_t> v;
    uint32_t seed = 5;
    for (int64_t i = 0;; ++i) {
        const int64_t t = int64_t(double(i) * 1e6 / gridHz);
        if (t > to) break;
        seed = seed * 1664525u + 1013904223u;
        if (i && int64_t(seed >> 8) % 100 < missPct) continue;
        seed = seed * 1664525u + 1013904223u;
        v.push_back(t + int64_t(seed >> 8) % 200);
    }
    return v;
}

// The dirty-area share of capture/dirty.hpp (GUIDE 5: the rate controller
// cuts the bitrate on a static desktop): union, clipping, the long-list
// fallback, a present that names no region, and merging two deliveries.
void testDirty() {
    const char* name = "dirty area (union of rects)";
    const auto near = [](double a, double b) { return a > b - 1e-9 && a < b + 1e-9; };
    const DirtyRect quarter{0, 0, 960, 540};
    expect(dirtyFraction(nullptr, 0, 1920, 1080) == 0 && dirtyFraction(&quarter, 1, 0, 1080) == 0, name, "empty list / picture not 0");
    expect(near(dirtyFraction(&quarter, 1, 1920, 1080), 0.25), name, "one quarter rect");
    const DirtyRect twice[] = {quarter, quarter};
    expect(near(dirtyFraction(twice, 2, 1920, 1080), 0.25), name, "the same rect twice counted twice");
    // Overlapping (a move rect's destination that is also dirty): 100x100 +
    // 100x100 overlapping in 50x50 = 17500 pixels.
    const DirtyRect overlap[] = {{0, 0, 100, 100}, {50, 50, 150, 150}};
    expect(near(dirtyFraction(overlap, 2, 200, 200), 17500.0 / 40000.0), name, "overlap counted twice");
    // An L shape and a rect inside it, clipping, empty / inverted rects.
    const DirtyRect shapes[] = {{0, 0, 100, 10}, {0, 0, 10, 100}, {5, 5, 8, 8}, {-50, 90, 5, 150}, {60, 60, 60, 80}, {70, 70, 40, 90}};
    expect(near(dirtyFraction(shapes, 6, 100, 100), (1000.0 + 900.0) / 10000.0), name, "L shape, nested, clipped, empty");
    // A text caret: tiny but not 0 (stats dirtyPct reports it as 1).
    const DirtyRect caret{500, 300, 502, 320};
    const double c = dirtyFraction(&caret, 1, 1920, 1080);
    expect(c > 0 && near(c, 40.0 / (1920.0 * 1080.0)), name, "caret");
    // More rects than kMaxDirtyUnionRects: summed (an upper bound), at most 1.
    std::vector<DirtyRect> many(kMaxDirtyUnionRects + 1, DirtyRect{0, 0, 100, 100});
    expect(dirtyFraction(many.data(), many.size(), 200, 200) == 1.0, name, "long list not capped at 1");
    many.assign(kMaxDirtyUnionRects + 1, DirtyRect{0, 0, 1, 1});
    expect(near(dirtyFraction(many.data(), many.size(), 1000, 1000), double(kMaxDirtyUnionRects + 1) / 1e6), name, "long list not summed");
    // A presented image whose capture names no region (a game flipping
    // past the compositor: DDA hands out its presents without metadata) is
    // the whole picture, not an unchanged one; named regions count as above.
    const DirtyRect nothing[] = {{60, 60, 60, 80}, {-20, -20, -10, -10}};
    expect(presentedDirtyFraction(nullptr, 0, 1920, 1080) == 1.0, name, "a present without metadata not the whole picture");
    expect(presentedDirtyFraction(nothing, 2, 100, 100) == 1.0, name, "a present with only empty rects not the whole picture");
    expect(near(presentedDirtyFraction(&caret, 1, 1920, 1080), c), name, "a present with a caret not the caret");
    expect(near(presentedDirtyFraction(overlap, 2, 200, 200), 17500.0 / 40000.0), name, "a present's rects not their union");
    expect(presentedDirtyFraction(nullptr, 0, 0, 1080) == -1, name, "an empty picture not unknown");
    expect(mergeDirty(0.25f, 0.5f) == 0.75f && mergeDirty(0.75f, 0.5f) == 1.0f && mergeDirty(-1, 0.5f) == -1 && mergeDirty(0, 0) == 0, name,
           "merging two deliveries");
    std::printf("  %-36s ok\n", name);
}

// The first-image watchdog: nothing before 2 s, then a report every 10 s
// while no image comes (a display that is off: the first hardware run's 54 s),
// the first image with the time it took; an image within 2 s reports nothing.
void testFirstImage() {
    const char* name = "first-image watchdog";
    const int64_t ms = 1000;  // ticks per millisecond (1 tick = 1 us)
    FirstImageWatchdog w;
    w.reset(1000000, 5 * ms);
    std::vector<int64_t> reports;
    for (int64_t t = 5 * ms; t <= 30 * 1000 * ms; t += 100 * ms) {
        if (const int64_t r = w.due(t)) reports.push_back(r);
    }
    expect(reports.size() == 3 && reports[0] == 2000 && reports[1] == 12000 && reports[2] == 22000, name,
           "not reported at 2 s, 12 s and 22 s");
    expect(w.image(54 * 1000 * ms) == 54000 - 5 && w.image(55 * 1000 * ms) == 0 && w.due(90 * 1000 * ms) == 0, name,
           "the first image after a report not with its time, or reports after it");
    w.reset(1000000, 0);
    expect(w.due(1999 * ms) == 0 && w.image(1500 * ms) == 0 && w.due(60 * 1000 * ms) == 0, name,
           "an image within 2 s reported");
    w.reset(1000000, 0);
    expect(w.due(40 * 1000 * ms) == 40000 && w.due(41 * 1000 * ms) == 0 && w.due(50 * 1000 * ms) == 50000, name,
           "a late look not reported once, then every 10 s");
    std::printf("  %-36s ok\n", name);
}

}  // namespace

int runPacerSelfTest() {
    failures = 0;
    testDirty();
    testFirstImage();
    const int64_t sec = 1000000;
    {
        const char* name = "144 Hz game at 120 fps";
        auto pr = steady(144, 0, 10 * sec);
        auto d = simulate(pr, 120, 100, 10 * sec);
        common(name, pr, d, 120, 10 * sec);
        expect(d.size() >= 1190, name, "fewer than ~120 fps delivered");
    }
    {
        const char* name = "60 Hz +-1 ms jitter at 60 fps";
        auto pr = steady(60, 0, 10 * sec, 1000);
        auto d = simulate(pr, 60, 100, 10 * sec);
        common(name, pr, d, 60, 10 * sec);
        size_t immediate = 0;
        for (const auto& x : d) immediate += !x.repeat && x.at == x.present;
        expect(immediate * 100 >= pr.size() * 99, name, "presents were delayed or dropped");
    }
    {
        const char* name = "59.94 Hz display at 60 fps";
        auto pr = steady(59.94, 0, 10 * sec);
        auto d = simulate(pr, 60, 100, 10 * sec);
        common(name, pr, d, 60, 10 * sec);
        expect(d.size() == pr.size(), name, "not every present was delivered at once");
    }
    {
        const char* name = "30 Hz game at 60 fps";
        auto pr = steady(30, 0, 10 * sec);
        auto d = simulate(pr, 60, 100, 10 * sec);
        common(name, pr, d, 60, 10 * sec);
        bool allNow = d.size() == pr.size();
        for (const auto& x : d) allNow = allNow && x.at == x.present;
        expect(allNow, name, "presents not delivered immediately");
    }
    {
        const char* name = "static after 1 s (idle repeats)";
        auto pr = steady(60, 0, sec);
        auto d = simulate(pr, 60, 100, 3 * sec);
        common(name, pr, d, 60, 3 * sec);
        int64_t lastReal = 0;
        std::vector<int64_t> repeats;
        for (const auto& x : d) {
            if (!x.repeat) lastReal = x.at;
            else repeats.push_back(x.at);
        }
        expect(repeats.size() >= 19 && repeats.size() <= 21, name, "not one repeat per 100 ms");
        expect(!repeats.empty() && repeats.front() - lastReal == 100000, name, "first repeat not 100 ms after the last image");
        for (size_t i = 1; i < repeats.size(); ++i) expect(repeats[i] - repeats[i - 1] == 100000, name, "repeat interval");
    }
    {
        const char* name = "5 fps stream, static";
        auto pr = steady(5, 0, sec / 2);
        auto d = simulate(pr, 5, 100, 3 * sec);
        common(name, pr, d, 5, 3 * sec);
        for (size_t i = 1; i < d.size(); ++i) expect(d[i].at - d[i - 1].at >= 200000, name, "faster than 5 fps");
    }
    {
        // The first change after an idle period (a click on a static desktop)
        // must not wait for the slot an idle repeat would have used.
        const char* name = "present 1 ms after an idle repeat";
        auto pr = steady(60, 0, sec / 2);
        const int64_t lastPresent = pr.back();
        pr.push_back(lastPresent + 100000 + 1000);  // the first repeat comes 100 ms after the last image
        auto d = simulate(pr, 60, 100, sec);
        common(name, pr, d, 60, sec);
        bool sawRepeat = false, ok = false;
        for (const auto& x : d) {
            if (x.repeat) sawRepeat = true;
            else if (sawRepeat && x.present == pr.back()) ok = x.at == x.present;
        }
        expect(sawRepeat, name, "no idle repeat before the late present");
        expect(ok, name, "the present after a repeat was not delivered at once");
    }
    {
        // The second hardware run: a 75 Hz display (game or desktop) streamed
        // at 60 fps. With the slot rule alone every image waited for the beat
        // of the two rates (9 ms p50, 15.5 ms p95 between capture and encoder
        // in the log); rule 3 sends four of five presents on arrival.
        const char* name = "75 Hz display at 60 fps (rule 3)";
        auto pr = steady(75, 0, 10 * sec, 300);
        auto d = simulate(pr, 60, 100, 10 * sec);
        const Waits w = common(name, pr, d, 60, 10 * sec), old = slotRule(pr, 60, 10 * sec);
        expect(w.frames >= 590, name, "fewer than ~60 fps delivered");
        expect(w.mean <= 1500 && w.p95 <= 5000 && w.mean * 4 <= old.mean, name,
               "images still wait for their slots (mean > 1.5 ms or a quarter of the slot rule's, or p95 > 5 ms)");
    }
    {
        // Games that do not wait for the display (no vsync, or a variable
        // refresh rate) at 62-70 fps streamed at 60: their frame times jitter,
        // the next present is predicted from the last 16.
        struct Game {
            const char* name;
            int64_t meanUs, spreadUs;
        };
        for (const Game& g : {Game{"game at ~70 fps (14.3 +- 3 ms) at 60 fps", 14300, 3000},
                              Game{"game at ~65 fps (15.4 +- 2 ms) at 60 fps", 15400, 2000},
                              Game{"game at ~62 fps (16.1 +- 2 ms) at 60 fps", 16100, 2000}}) {
            auto pr = game(g.meanUs, g.spreadUs, 10 * sec);
            auto d = simulate(pr, 60, 100, 10 * sec);
            const Waits w = common(g.name, pr, d, 60, 10 * sec), old = slotRule(pr, 60, 10 * sec);
            expect(w.frames * 100 >= old.frames * 99, g.name, "fewer frames than the slot rule");
            expect(w.mean <= 3000 && w.p95 <= 8500 && w.p95 + 5000 <= old.p95, g.name,
                   "images wait more than 3 ms on average, p95 above 8.5 ms or not 5 ms below the slot rule's");
        }
    }
    {
        const char* name = "144 Hz display at 60 fps";
        auto pr = steady(144, 0, 10 * sec, 300);
        auto d = simulate(pr, 60, 100, 10 * sec);
        const Waits w = common(name, pr, d, 60, 10 * sec), old = slotRule(pr, 60, 10 * sec);
        expect(w.frames >= 595, name, "fewer than ~60 fps delivered");
        expect(w.mean <= 1000 && w.mean * 2 <= old.mean, name, "images wait more than 1 ms or half the slot rule's on average");
    }
    {
        // A source at twice the stream's rate (the fps ladder's 30 on a 60 Hz
        // display, 120 Hz at 60): every other present, always the same ones.
        // Half an interval of slack put the slot on the left-out presents, and
        // jitter flipped which one went out: steps of 1 and 3, a judder. With
        // 1 ms of present jitter (12% of a 120 Hz interval; a compositor's
        // presents jitter far less) a few steps still flip.
        struct Twice {
            const char* name;
            double hz;
            int fps, jitterUs, minShare;
        };
        for (const Twice& c : {Twice{"120 Hz +- 0.3 ms at 60 fps", 120, 60, 300, 98}, Twice{"120 Hz +- 1 ms at 60 fps", 120, 60, 1000, 95},
                               Twice{"60 Hz +- 0.3 ms at 30 fps", 60, 30, 300, 98}, Twice{"144 Hz +- 0.3 ms at 72 fps", 144, 72, 300, 98},
                               Twice{"240 Hz +- 0.3 ms at 120 fps", 240, 120, 300, 98}}) {
            auto pr = steady(c.hz, 0, 10 * sec, c.jitterUs);
            auto d = simulate(pr, c.fps, 100, 10 * sec);
            common(c.name, pr, d, c.fps, 10 * sec);
            const int share = stepShare(pr, d, 2);
            std::printf("  %-44s %d%% of the steps 2 presents\n", "", share);
            expect(share >= c.minShare, c.name, "too few steps between images are 2 presents (judder)");
            expect(waits(d).frames * 100 >= size_t(c.fps) * 10 * 98, c.name, "fewer than 98% of fps delivered");
        }
    }
    {
        // Games on the display's refresh grid (vsync or the compositor) that
        // miss some refreshes: intervals of one and two refreshes, the next
        // present unpredictable. The second run's game at 60 fps on 75 Hz
        // likely looked like this. No fewer frames than min(fps, game) allows
        // (98%), and no image waits longer than with the slot rule (p95).
        struct Grid {
            const char* name;
            double gameFps, hz;
            int fps;
        };
        for (const Grid& g : {Grid{"60 fps game on a 75 Hz grid at 60 fps", 60, 75, 60}, Grid{"62 fps game on a 75 Hz grid at 60 fps", 62, 75, 60},
                              Grid{"64 fps game on a 75 Hz grid at 60 fps", 64, 75, 60}, Grid{"66 fps game on a 75 Hz grid at 60 fps", 66, 75, 60},
                              Grid{"68 fps game on a 75 Hz grid at 60 fps", 68, 75, 60}, Grid{"70 fps game on a 75 Hz grid at 60 fps", 70, 75, 60},
                              Grid{"72 fps game on a 75 Hz grid at 60 fps", 72, 75, 60}, Grid{"120 fps game on a 144 Hz grid at 120", 120, 144, 120}}) {
            auto pr = grid(g.gameFps, g.hz, g.hz > 100 ? 500 : 1000, 10 * sec);
            auto d = simulate(pr, g.fps, 100, 10 * sec);
            const Waits w = common(g.name, pr, d, g.fps, 10 * sec), old = slotRule(pr, g.fps, 10 * sec);
            expect(double(w.frames) >= 0.98 * 10 * std::min<double>(g.fps, g.gameFps), g.name, "fewer than 98% of min(fps, game) delivered");
            expect(w.p95 <= old.p95, g.name, "p95 wait above the slot rule's");
        }
        for (const int miss : {5, 8, 12}) {
            char name[64];
            std::snprintf(name, sizeof name, "75 Hz vsync missing %d%% at 60 fps", miss);
            auto pr = missedRefreshes(75, miss, 10 * sec);
            auto d = simulate(pr, 60, 100, 10 * sec);
            const Waits w = common(name, pr, d, 60, 10 * sec), old = slotRule(pr, 60, 10 * sec);
            expect(w.frames * 100 >= old.frames * 98, name, "fewer than 98% of the slot rule's frames");
            expect(w.p95 + 1500 <= old.p95 && w.mean * 3 <= old.mean * 2, name,
                   "p95 wait not 1.5 ms below the slot rule's, or mean above 2/3 of it");
        }
    }
    {
        // Rule 3 waits for a present that is due; when the source stops (the
        // last image of a movement), the waiting image still goes out, at
        // most one interval after its present (common checks that).
        const char* name = "75 Hz that stops, at 60 fps";
        for (int64_t stop = sec / 2; stop < sec / 2 + 40000; stop += 1000) {
            auto pr = steady(75, 0, stop);
            auto d = simulate(pr, 60, 100, sec);
            bool last = false;
            for (const auto& x : d) last = last || (!x.repeat && x.present == pr.back());
            expect(last, name, "the last present never went out");
            if (stop == sec / 2) common(name, pr, d, 60, sec);
            else {
                for (const auto& x : d) {
                    if (!x.repeat && x.at - x.present > 16667) expect(false, name, "an image went out more than one interval late");
                }
            }
        }
    }
    {
        const char* name = "1000 Hz burst at 60 fps";
        auto pr = steady(1000, 0, sec / 10);
        auto d = simulate(pr, 60, 100, sec / 10);
        common(name, pr, d, 60, sec / 10);
    }
    std::printf("self-test-pacer: %s\n", failures ? "FAIL" : "ok");
    return failures ? 1 : 0;
}

}  // namespace recon

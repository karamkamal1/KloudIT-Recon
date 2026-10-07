#include "capture/pacer.hpp"

#include <algorithm>
#include <cstdio>
#include <limits>
#include <vector>

#include "selftest.hpp"

namespace recon {

void FramePacer::reset(int64_t qpcFrequency, int fps, int idleRepeatMs, int64_t now) {
    freq_ = qpcFrequency > 0 ? qpcFrequency : 1;
    idle_ = freq_ * (idleRepeatMs > 0 ? idleRepeatMs : 100) / 1000;
    setFps(fps);
    nextDue_ = now;
    lastDelivered_ = now;
}

void FramePacer::setFps(int fps) {
    if (fps <= 0) return;
    period_ = (freq_ + fps - 1) / fps;
    early_ = period_ / 4;
}

FramePacer::Decision FramePacer::decide(int64_t now, bool havePending, bool haveLast) const {
    const int64_t slot = nextDue_ - early_;
    if (havePending) {
        if (now >= slot) return {Decision::DeliverPending, 0};
        return {Decision::Wait, slot};
    }
    if (haveLast) {
        const int64_t repeatAt = std::max(lastDelivered_ + std::max(idle_, period_), slot);
        if (now >= repeatAt) return {Decision::DeliverRepeat, 0};
        return {Decision::Wait, repeatAt};
    }
    return {Decision::Wait, std::numeric_limits<int64_t>::max()};
}

void FramePacer::delivered(int64_t now, bool repeat) {
    if (!repeat) nextDue_ = std::max(nextDue_, now) + period_;  // repeats take no slot (policy 4)
    lastDelivered_ = now;
}

// --- self-test ----------------------------------------------------------------------

namespace {

struct Delivery {
    int64_t at;
    int64_t present;
    bool repeat;
};

// Drives the pacer like PacedCapture::next() does, on a simulated clock (1 tick
// = 1 us): presents arrive at the given times; while a pending image waits for
// its slot, newer presents replace it.
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
        const int64_t nextPresent = next < presents.size() ? presents[next] : std::numeric_limits<int64_t>::max();
        if (nextPresent <= d.until) {
            t = std::max(t, nextPresent);
            pending = true;
            pendingPresent = nextPresent;
            ++next;
        } else {
            t = d.until;
        }
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

// Common properties: in any stretch of time no more new images than fps
// allows (plus the one frame the early allowance can add), new images at
// least 3/4 of an interval apart, a repeat only after max(idle, interval)
// without any delivery, every delivered image the newest one presented at that
// time and never older than one frame interval. (A new image right after a
// repeat is allowed: repeats take no slot.)
void common(const char* name, const std::vector<int64_t>& presents, const std::vector<Delivery>& d, int fps,
            int64_t duration, int idleMs = 100) {
    const int64_t period = (1000000 + fps - 1) / fps, early = period / 4;
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
        expect(minGap >= period - period / 4, name, "new images closer than 3/4 of a frame interval");
    } else {
        minGap = 0;
    }
    std::printf("  %-36s %zu frames (%zu repeats), min gap %lld us\n", name, d.size(),
                size_t(std::count_if(d.begin(), d.end(), [](const Delivery& x) { return x.repeat; })),
                static_cast<long long>(minGap));
}

}  // namespace

int runPacerSelfTest() {
    failures = 0;
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
        const char* name = "1000 Hz burst at 60 fps";
        auto pr = steady(1000, 0, sec / 10);
        auto d = simulate(pr, 60, 100, sec / 10);
        common(name, pr, d, 60, sec / 10);
    }
    std::printf("self-test-pacer: %s\n", failures ? "FAIL" : "ok");
    return failures ? 1 : 0;
}

}  // namespace recon

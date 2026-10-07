// recon-encoder --self-test-encoder: the encoder-independent logic of the
// encoder backends, without a GPU: the LTR recovery policy (codec/ltr.hpp)
// driven like an encoder that marks and references exactly as asked,
// parameter-set detection and insertion on real H.264 access units (the mock
// clip) and on synthetic HEVC / AV1 units, ROI importance maps and the coded
// size alignment.
#include <algorithm>
#include <cstdio>
#include <string>
#include <vector>

#include "codec/bitstream.hpp"
#include "codec/ltr.hpp"
#include "mock/mock.hpp"
#include "selftest.hpp"

// Generated from testdata/mock_clip.h264 by cmake/embed.cmake.
extern const unsigned char kMockClip[];
extern const std::size_t kMockClipSize;

namespace recon {

namespace {

int failures = 0;

void expect(bool cond, const char* test, const std::string& what) {
    if (cond) return;
    ++failures;
    std::printf("  %s: FAIL %s\n", test, what.c_str());
}

// An encoder that does what it is told: marks the planned slot, references
// the planned mask, outputs a key frame for an IDR. Frames come out
// `delay` frames after they go in (the encoder's pipeline).
struct SimEncoder {
    explicit SimEncoder(LtrTracker& tracker, int pipelineDelay = 1) : t(tracker), delay(pipelineDelay) {}
    LtrTracker& t;
    int delay = 1;
    std::vector<std::pair<uint64_t, LtrTracker::Plan>> queue;
    std::vector<std::pair<uint64_t, LtrTracker::Plan>> out;  // what came out, in order

    void frame(uint64_t id, int64_t now, bool idr, std::vector<bool>* recoveryOk = nullptr) {
        const LtrTracker::Plan p = t.plan(id, now, idr);
        t.submitted(id, p, now);
        queue.emplace_back(id, p);
        while (int(queue.size()) > delay) {
            const auto [fid, plan] = queue.front();
            queue.erase(queue.begin());
            LtrTracker::Output o;
            o.key = plan.idr;
            o.intra = plan.idr;
            o.markedSlot = plan.markSlot;
            o.refMask = plan.refMask;
            const bool ok = t.output(fid, o, plan, now);
            if (recoveryOk) recoveryOk->push_back(ok);
            out.emplace_back(fid, plan);
        }
    }
};

void testLtr() {
    const char* name = "LTR marks alternate, newest ACKed slot kept";
    {
        LtrTracker t;
        t.reset({2, 6, 1000});
        SimEncoder enc(t);
        // Frame 1 is the IDR; marks every 6 frames from frame 2; the client
        // ACKs every marked frame 3 frames after it came out.
        std::vector<uint64_t> marked;
        for (uint64_t id = 1; id <= 60; ++id) {
            // The slot holding the newest ACKed LTR is never the one marked.
            int keep = -1;
            uint64_t newest = 0;
            const auto before = t.slots();
            for (size_t i = 0; i < before.size(); ++i) {
                if (before[i].acked && before[i].frameId > newest) newest = before[i].frameId, keep = int(i);
            }
            enc.frame(id, int64_t(id), id == 1);
            const int markedSlot = enc.queue.back().second.markSlot;
            expect(keep < 0 || markedSlot != keep, name, "frame " + std::to_string(id) + " overwrites the newest ACKed LTR " + std::to_string(newest));
            for (const auto& [fid, p] : enc.out) {
                if (p.markSlot >= 0 && fid + 3 == id) t.ack(fid);
            }
        }
        int markCount = 0;
        int lastSlot = -1;
        for (const auto& [fid, p] : enc.out) {
            if (fid == 1) expect(p.idr && p.markSlot < 0, name, "frame 1 not a plain IDR");
            if (p.markSlot < 0) continue;
            ++markCount;
            expect(p.markSlot != lastSlot, name, "two marks in a row into slot " + std::to_string(p.markSlot) + " at frame " + std::to_string(fid));
            lastSlot = p.markSlot;
            marked.push_back(fid);
        }
        expect(markCount >= 8, name, "only " + std::to_string(markCount) + " marks in 60 frames");
        expect(!marked.empty() && marked.front() == 2, name, "the frame after the IDR was not marked");
        const auto slots = t.slots();
        expect(slots.size() == 2 && slots[0].frameId && slots[1].frameId, name, "slots not both used");
        std::printf("  %-44s %d marks, slots hold %llu%s / %llu%s\n", name, markCount, static_cast<unsigned long long>(slots[0].frameId),
                    slots[0].acked ? " (acked)" : "", static_cast<unsigned long long>(slots[1].frameId), slots[1].acked ? " (acked)" : "");
    }

    name = "loss: recovery from the newest ACKed LTR";
    {
        LtrTracker t;
        t.reset({2, 6, 1000});
        SimEncoder enc(t);
        for (uint64_t id = 1; id <= 40; ++id) {
            enc.frame(id, int64_t(id), id == 1);
            for (const auto& [fid, p] : enc.out) {
                if (p.markSlot >= 0 && fid + 2 == id) t.ack(fid);
            }
        }
        // Newest ACKed LTR below 41: the marks went to 2, 8, 14, 20, 26, 32, 38;
        // ACKed are those that came out >= 2 frames before frame 40.
        uint64_t newestAcked = 0;
        for (const auto& v : t.slots()) {
            if (v.acked && v.frameId < 41) newestAcked = std::max(newestAcked, v.frameId);
        }
        expect(newestAcked >= 26, name, "newest ACKed LTR " + std::to_string(newestAcked));
        expect(t.recover(41, std::nullopt), name, "recover() refused with ACKed LTRs present");
        std::vector<bool> ok;
        enc.frame(41, 41, false, &ok);
        enc.frame(42, 42, false, &ok);
        enc.frame(43, 43, false, &ok);
        const auto it = std::find_if(enc.out.begin(), enc.out.end(), [](const auto& x) { return x.first == 41; });
        expect(it != enc.out.end() && it->second.recovery && it->second.refFloor == newestAcked && !it->second.idr, name,
               "frame 41 is not a recovery frame from " + std::to_string(newestAcked));
        expect(it != enc.out.end() && it->second.refMask && (it->second.refMask & (it->second.refMask - 1)) == 0, name,
               "recovery references more than one slot");
        expect(!ok.empty() && ok.front(), name, "recovery output not accepted");
        const auto next = std::find_if(enc.out.begin(), enc.out.end(), [](const auto& x) { return x.first == 42; });
        expect(next != enc.out.end() && !next->second.recovery && next->second.refMask == 0, name, "frame 42 still forced to the LTR");
        std::printf("  %-44s refFloor %llu, mask 0x%x\n", name, static_cast<unsigned long long>(newestAcked),
                    it != enc.out.end() ? it->second.refMask : 0u);
    }

    name = "loss before any ACK: IDR";
    {
        LtrTracker t;
        t.reset({2, 6, 1000});
        SimEncoder enc(t);
        for (uint64_t id = 1; id <= 20; ++id) enc.frame(id, int64_t(id), id == 1);
        expect(!t.recover(21, std::nullopt), name, "recover() without ACKs did not fall back to an IDR");
        expect(t.stats().idrFallbacks == 1, name, "fallback not counted");
        std::printf("  %-44s ok\n", name);
    }

    name = "unACKed marks wait for the ACK timeout";
    {
        LtrTracker t;
        t.reset({2, 3, 100});  // ACK timeout 100 ticks, 1 tick per frame
        SimEncoder enc(t);
        std::vector<uint64_t> marks;
        for (uint64_t id = 1; id <= 300; ++id) enc.frame(id, int64_t(id), id == 1);
        for (const auto& [fid, p] : enc.out) {
            if (p.markSlot >= 0) marks.push_back(fid);
        }
        // Without ACKs: two marks fill both slots, then none until the first
        // is 100 ticks old, then about one per timeout.
        expect(marks.size() >= 3 && marks[0] == 2 && marks[1] == 5, name, "first marks not at frames 2 and 5");
        expect(marks.size() >= 3 && marks[2] >= 102, name, "a slot was overwritten before its ACK timed out");
        expect(marks.size() <= 8, name, std::to_string(marks.size()) + " marks: the timeout does not limit them");
        std::printf("  %-44s %zu marks in 300 frames\n", name, marks.size());
    }

    name = "lost ACKs never cost the ACKed LTR";
    {
        // Only the first LTR is ever ACKed; later marks time out. They must
        // rotate through the other slot and leave the ACKed one alone.
        LtrTracker t;
        t.reset({2, 4, 20});
        SimEncoder enc(t);
        for (uint64_t id = 1; id <= 200; ++id) {
            enc.frame(id, int64_t(id), id == 1);
            if (id == 4) t.ack(2);
        }
        bool held = false;
        for (const auto& v : t.slots()) held = held || (v.frameId == 2 && v.acked);
        expect(held, name, "the only ACKed LTR (frame 2) was overwritten");
        expect(t.recover(150, std::nullopt), name, "no recovery after lost ACKs");
        std::printf("  %-44s ok (%llu marks)\n", name, static_cast<unsigned long long>(t.stats().marks));
    }

    name = "a key frame clears the slots";
    {
        LtrTracker t;
        t.reset({2, 4, 1000});
        SimEncoder enc(t);
        for (uint64_t id = 1; id <= 30; ++id) {
            enc.frame(id, int64_t(id), id == 1 || id == 20);
            for (const auto& [fid, p] : enc.out) {
                if (p.markSlot >= 0 && fid + 1 == id) t.ack(fid);
            }
        }
        for (const auto& v : t.slots()) expect(v.frameId == 0 || v.frameId > 20, name, "slot still holds " + std::to_string(v.frameId));
        // A loss from before the key frame cannot use an LTR from before it.
        expect(!t.recover(19, std::nullopt), name, "recovery from an LTR older than the last key frame");
        std::printf("  %-44s ok\n", name);
    }

    name = "recovery frame not coded from the LTR";
    {
        LtrTracker t;
        t.reset({2, 2, 1000});
        SimEncoder enc(t);
        for (uint64_t id = 1; id <= 12; ++id) {
            enc.frame(id, int64_t(id), id == 1);
            for (const auto& [fid, p] : enc.out) {
                if (p.markSlot >= 0 && fid + 1 == id) t.ack(fid);
            }
        }
        expect(t.recover(13, std::nullopt), name, "recover() refused");
        const LtrTracker::Plan p = t.plan(13, 13, false);
        t.submitted(13, p, 13);
        LtrTracker::Output o;  // a P frame that ignored the forced reference
        expect(!t.output(13, o, p, 13), name, "accepted a recovery frame that did not reference the LTR");
        o.intra = true;  // "Referring to a LTR frame not existing in LTR slot will generate an Intra only frame"
        expect(t.output(13, o, p, 13), name, "rejected an intra-only recovery frame");
        std::printf("  %-44s ok\n", name);
    }

    name = "recon-host's ackedLtr and in-flight marks";
    {
        LtrTracker t;
        t.reset({2, 5, 1000});
        SimEncoder enc(t, 3);  // a deeper encoder pipeline
        for (uint64_t id = 1; id <= 30; ++id) enc.frame(id, int64_t(id), id == 1);
        // Nothing ACKed by the tracker; recon-host names one it saw ACKed.
        uint64_t held = 0;
        for (const auto& v : t.slots()) {
            if (v.frameId && (!held || v.frameId < held)) held = v.frameId;
        }
        const bool r = t.recover(29, held);
        // The older slot may already have a newer mark on its way in: then it
        // must not be used.
        bool inflightToHeld = false;
        for (const auto& q : enc.queue) {
            const auto slots = t.slots();
            if (q.second.markSlot >= 0 && slots[size_t(q.second.markSlot)].frameId == held) inflightToHeld = true;
        }
        expect(r != inflightToHeld, name, inflightToHeld ? "used a slot that is being overwritten" : "ignored recon-host's ACKed LTR");
        std::printf("  %-44s %s\n", name, r ? "recovers from it" : "IDR (slot being overwritten)");
    }

    name = "LTR off";
    {
        LtrTracker t;
        t.reset({0, 6, 1000});
        const LtrTracker::Plan p = t.plan(1, 1, true);
        expect(p.idr && p.markSlot < 0 && !p.recovery, name, "plan with LTR off");
        expect(!t.recover(5, std::nullopt), name, "recover() with LTR off");
        std::printf("  %-44s ok\n", name);
    }
}

// Annex-B access units of the mock clip (AUD, SPS, PPS, IDR / AUD, P).
std::vector<uint8_t> au(size_t i) {
    const auto aus = ReplayEncoder::splitAccessUnits(kMockClip, kMockClipSize);
    if (i >= aus.size()) return {};
    return std::vector<uint8_t>(kMockClip + aus[i].first, kMockClip + aus[i].first + aus[i].second);
}

std::vector<uint8_t> nal(std::initializer_list<uint8_t> bytes) {
    std::vector<uint8_t> v{0, 0, 0, 1};
    v.insert(v.end(), bytes);
    return v;
}

std::vector<uint8_t> cat(std::initializer_list<std::vector<uint8_t>> parts) {
    std::vector<uint8_t> v;
    for (const auto& p : parts) v.insert(v.end(), p.begin(), p.end());
    return v;
}

void testBitstream() {
    const char* name = "parameter sets: H.264 (mock clip)";
    {
        const std::vector<uint8_t> idr = au(0), p = au(1);
        expect(hasParameterSets(Codec::H264, idr.data(), idr.size()), name, "SPS + PPS not found in the IDR");
        expect(!hasParameterSets(Codec::H264, p.data(), p.size()), name, "a P frame has parameter sets");
        // The extradata: SPS + PPS cut from the IDR access unit (after its AUD).
        std::vector<uint8_t> extra;
        size_t sps = 0, slice = 0;
        for (size_t i = 0; i + 4 < idr.size(); ++i) {
            if (idr[i] == 0 && idr[i + 1] == 0 && idr[i + 2] == 1) {
                const int t = idr[i + 3] & 0x1f;
                const size_t start = i > 0 && idr[i - 1] == 0 ? i - 1 : i;
                if (t == 7 && !sps) sps = start;
                if (t == 5 && !slice) slice = start;
            }
        }
        expect(sps && slice > sps, name, "cannot cut SPS/PPS from the clip");
        if (sps && slice > sps) extra.assign(idr.begin() + long(sps), idr.begin() + long(slice));
        const std::vector<uint8_t> fixed = withParameterSets(Codec::H264, p.data(), p.size(), extra.data(), extra.size());
        expect(hasParameterSets(Codec::H264, fixed.data(), fixed.size()), name, "insertion did not add SPS + PPS");
        // The access unit delimiter stays first.
        size_t first = 0;
        while (first < fixed.size() && fixed[first] == 0) ++first;
        expect(first + 1 < fixed.size() && fixed[first] == 1 && (fixed[first + 1] & 0x1f) == 9 && fixed.size() == p.size() + extra.size(),
               name, "AUD no longer first");
        expect(withParameterSets(Codec::H264, p.data(), p.size(), p.data(), p.size()).empty(), name, "accepted extradata without SPS");
        std::printf("  %-44s ok (IDR %zu bytes, extradata %zu bytes)\n", name, idr.size(), extra.size());
    }
    name = "parameter sets: HEVC";
    {
        const auto vps = nal({0x40, 0x01, 0x0c}), sps = nal({0x42, 0x01, 0x01}), pps = nal({0x44, 0x01, 0xc1});
        const auto idr = nal({0x26, 0x01, 0xaf}), trail = nal({0x02, 0x01, 0xd0}), aud = nal({0x46, 0x01, 0x10});
        const auto key = cat({aud, vps, sps, pps, idr});
        expect(hasParameterSets(Codec::Hevc, key.data(), key.size()), name, "VPS/SPS/PPS not found");
        const auto noVps = cat({sps, pps, idr});
        expect(!hasParameterSets(Codec::Hevc, noVps.data(), noVps.size()), name, "found without VPS");
        const auto bare = cat({aud, idr}), extra = cat({vps, sps, pps});
        const auto fixed = withParameterSets(Codec::Hevc, bare.data(), bare.size(), extra.data(), extra.size());
        expect(fixed == cat({aud, vps, sps, pps, idr}), name, "not inserted after the AUD");
        const auto noAud = withParameterSets(Codec::Hevc, trail.data(), trail.size(), extra.data(), extra.size());
        expect(noAud == cat({vps, sps, pps, trail}), name, "not inserted first without an AUD");
        std::printf("  %-44s ok\n", name);
    }
    name = "parameter sets: AV1 OBUs";
    {
        // obu_header: type << 3 | has_size_field (0x02); then leb128 size.
        const std::vector<uint8_t> td{0x12, 0x00}, seq{0x0a, 0x03, 0x00, 0x00, 0x00}, frame{0x32, 0x02, 0xaa, 0xbb};
        const auto key = cat({td, seq, frame}), inter = cat({td, frame});
        expect(hasParameterSets(Codec::Av1, key.data(), key.size()), name, "sequence header not found");
        expect(!hasParameterSets(Codec::Av1, inter.data(), inter.size()), name, "found in a frame without one");
        const auto fixed = withParameterSets(Codec::Av1, inter.data(), inter.size(), seq.data(), seq.size());
        expect(fixed == key, name, "not inserted after the temporal delimiter");
        const std::vector<uint8_t> bad{0x0a, 0x05, 0x00};  // size runs past the end
        expect(!hasParameterSets(Codec::Av1, bad.data(), bad.size()), name, "accepted a truncated OBU");
        const std::vector<uint8_t> multi{0x0a, 0x83, 0x00, 0x01, 0x02, 0x03};  // 2-byte leb128 size (3), 3-byte payload
        expect(hasParameterSets(Codec::Av1, multi.data(), multi.size()), name, "multi-byte leb128 size not parsed");
        const std::vector<uint8_t> noSize{0x12, 0x00, 0x08, 0x01, 0x02};  // last OBU without size field runs to the end
        expect(hasParameterSets(Codec::Av1, noSize.data(), noSize.size()), name, "OBU without size field not parsed");
        std::printf("  %-44s ok\n", name);
    }
}

void testRoi() {
    const char* name = "ROI importance map";
    const RoiMap none = roiImportanceMap(1920, 1080, 64, {});
    expect(none.cols == 30 && none.rows == 17, name, "1920x1080 is not 30x17 blocks of 64");
    bool uniform = true;
    for (uint32_t v : none.values) uniform = uniform && v == kRoiBackground;
    expect(uniform, name, "background not uniform");
    // A crosshair region (+10) overlapping a de-emphasised HUD (-10): the
    // overlap takes the higher importance; outside both: background.
    const RoiMap m = roiImportanceMap(256, 128, 64, {RoiRect{64, 0, 64, 64, 10}, RoiRect{100, 0, 156, 128, -10}});
    expect(m.cols == 4 && m.rows == 2, name, "256x128 is not 4x2 blocks");
    expect(m.values[0] == 5 && m.values[1] == 10 && m.values[2] == 0 && m.values[3] == 0, name, "row 0 importance wrong");
    expect(m.values[4] == 5 && m.values[5] == 0 && m.values[6] == 0, name, "row 1 importance wrong");
    const RoiMap h264 = roiImportanceMap(64, 32, 16, {RoiRect{20, 20, 8, 8, 3}});
    expect(h264.cols == 4 && h264.rows == 2 && h264.values[5] == 7 && h264.values[0] == 5, name, "16x16 blocks wrong");
    const RoiMap clipped = roiImportanceMap(100, 100, 64, {RoiRect{90, 90, 500, 500, 10}});
    expect(clipped.values[3] == 10 && clipped.values[0] == 5, name, "clipping wrong");
    std::printf("  %-44s ok\n", name);

    name = "coded size alignment";
    expect(alignUp(1920, 64) == 1920 && alignUp(1080, 16) == 1088 && alignUp(3440, 64) == 3456 && alignUp(1440, 16) == 1440 &&
               alignUp(1080, 1) == 1080,
           name, "alignUp");
    std::printf("  %-44s ok\n", name);
}

}  // namespace

int runEncoderSelfTest() {
    failures = 0;
    testLtr();
    testBitstream();
    testRoi();
    std::printf("self-test-encoder: %s\n", failures ? "FAIL" : "ok");
    return failures ? 1 : 0;
}

}  // namespace recon

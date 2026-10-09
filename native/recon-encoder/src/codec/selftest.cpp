// recon-encoder --self-test-encoder: the encoder-independent logic of the
// encoder backends, without a GPU: the LTR recovery policy (codec/ltr.hpp)
// driven like an encoder that marks and references exactly as asked, the
// reference-invalidation policy (codec/rfi.hpp), parameter-set detection and
// insertion on real H.264 access units (the mock clip) and on synthetic HEVC /
// AV1 units, the level and reference frames read from SPS NAL units (x264 /
// x265 output and hand-written ones), ROI importance and QP delta maps, the
// coded size alignment, the NVENC settings that need no driver
// (nvenc/nvenc_policy.hpp), and the HDR10 metadata and its encoder units
// (codec/hdr.hpp). Phase 5: the LTR policy with temporal SVC (marks and
// recoveries on base-layer frames, the layer prediction), temporal ids and the
// discardable flag from H.264 / HEVC / AV1 units, the sub-frame output
// assembler, the cursor / crosshair ROI maps of encoder.FocusROI written into
// a pitched GRAY32 plane, and the NVENC re-encode limits and QP maps.
#include <algorithm>
#include <cstdio>
#include <cstring>
#include <string>
#include <vector>

#include "codec/bitstream.hpp"
#include "codec/hdr.hpp"
#include "codec/ltr.hpp"
#include "codec/rfi.hpp"
#include "codec/slices.hpp"
#include "mock/mock.hpp"
#include "nvenc/nvenc_policy.hpp"
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
    uint64_t switchAt = 0;  // this frame comes out as an AV1 switch frame (made by the encoder on its own)
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
            o.clearsSlots = fid == switchAt;
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

    name = "an AV1 switch frame clears the slots";
    {
        // Frame 20 comes out as a switch frame the encoder inserted on its own:
        // the ACKed LTRs before it are gone, so a loss at 21 needs an IDR.
        // The same run without the switch frame recovers from frame 18.
        for (const uint64_t sw : {uint64_t(0), uint64_t(20)}) {
            LtrTracker t;
            t.reset({2, 4, 1000});
            SimEncoder enc(t);
            enc.switchAt = sw;
            for (uint64_t id = 1; id <= 21; ++id) {
                enc.frame(id, int64_t(id), id == 1);
                for (const auto& [fid, p] : enc.out) {
                    if (p.markSlot >= 0 && fid + 1 == id) t.ack(fid);
                }
            }
            bool empty = true;
            for (const auto& v : t.slots()) empty = empty && v.frameId == 0;
            if (sw) {
                expect(empty, name, "a slot survived the switch frame");
                expect(!t.recover(21, std::nullopt), name, "recovery from an LTR the switch frame cleared");
            } else {
                expect(!empty && t.recover(21, std::nullopt), name, "no LTR recovery without the switch frame");
            }
        }
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
    name = "SPS: level and reference frames";
    {
        // SPS NAL units (header byte(s) first) written by libx264 / libx265
        // (FFmpeg 8.1: H.264 High 3840x2160 -refs 5, Main 1920x1080 -refs 6
        // with pic_order_cnt_type 0; HEVC 3840x2160 ref=5, 1920x1080 with a
        // temporal sub-layer), and two written for this test (H.264 with
        // scaling lists, pic_order_cnt_type 1 and an emulation prevention byte
        // before max_num_ref_frames; HEVC with three sub-layers, sub-layer
        // profile / level info and sps_max_dec_pic_buffering_minus1 for the
        // highest only). FFmpeg's trace_headers bitstream filter decodes each
        // to the values expected here.
        const std::vector<uint8_t> x264Uhd{0x67, 0x64, 0x00, 0x34, 0xac, 0xd9, 0x80, 0x3c, 0x00, 0x43, 0xec, 0x04, 0x40,
                                           0x00, 0x00, 0x03, 0x00, 0x40, 0x00, 0x00, 0x1e, 0x03, 0xc6, 0x0c, 0x66, 0x80};
        const std::vector<uint8_t> x264Fhd{0x67, 0x4d, 0x40, 0x32, 0xec, 0xe0, 0x3c, 0x01, 0x13, 0xf2, 0xe0, 0x22, 0x00,
                                           0x00, 0x03, 0x00, 0x02, 0x00, 0x00, 0x03, 0x00, 0xf0, 0x1e, 0x30, 0x63, 0x3c};
        const std::vector<uint8_t> synH264{0x67, 0x64, 0x00, 0x33, 0xad, 0x95, 0x24, 0x92, 0x49, 0x24, 0x92, 0x50, 0x88, 0x42,
                                           0x09, 0x24, 0x90, 0xd4, 0x92, 0x48, 0x6a, 0x49, 0x24, 0x35, 0x24, 0x92, 0x1a, 0x92,
                                           0x49, 0x0d, 0x49, 0x24, 0x86, 0xa4, 0x92, 0x43, 0x52, 0x49, 0x21, 0xa9, 0x24, 0x90,
                                           0xd5, 0x00, 0x00, 0x03, 0x01, 0x00, 0x00, 0x0a, 0x66, 0x12, 0x50, 0x1e, 0x00, 0x89,
                                           0xf9, 0x50};
        const std::vector<uint8_t> x265Uhd{0x42, 0x01, 0x01, 0x01, 0x60, 0x00, 0x00, 0x03, 0x00, 0x90, 0x00, 0x00, 0x03, 0x00, 0x00,
                                           0x03, 0x00, 0x99, 0xa0, 0x01, 0xe0, 0x20, 0x02, 0x1c, 0x59, 0x66, 0x65, 0x4a, 0x4c, 0x2f,
                                           0x01, 0x68, 0x08, 0x00, 0x00, 0x03, 0x00, 0x08, 0x00, 0x00, 0x03, 0x01, 0xe0, 0x40};
        const std::vector<uint8_t> x265Layers{0x42, 0x01, 0x02, 0x01, 0x60, 0x00, 0x00, 0x03, 0x00, 0x90, 0x00, 0x00,
                                              0x03, 0x00, 0x00, 0x03, 0x00, 0x7b, 0x00, 0x00, 0xa0, 0x03, 0xc0, 0x80,
                                              0x10, 0xe5, 0x96, 0x56, 0x62, 0xb3, 0x49, 0x26, 0x57, 0x80, 0xb4, 0x04,
                                              0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xf0, 0x20};
        const std::vector<uint8_t> synHevc{0x42, 0x01, 0x05, 0x01, 0x60, 0x00, 0x00, 0x03, 0x00, 0x90, 0x00, 0x00, 0x03,
                                           0x00, 0x00, 0x03, 0x00, 0x96, 0xd0, 0x00, 0x01, 0x60, 0x00, 0x00, 0x03, 0x00,
                                           0x90, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x90, 0x93, 0xa0, 0x01, 0xe0,
                                           0x20, 0x02, 0x20, 0x7c, 0x4e, 0x51, 0xb9, 0x24, 0xc2, 0x08};
        struct S {
            const char* what;
            Codec c;
            std::vector<uint8_t> data;  // Annex-B
            int level, refs;
            const char* levelName;
        };
        const auto pps264 = nal({0x68, 0xce, 0x38, 0x80}), pps265 = nal({0x44, 0x01, 0xc1, 0x72, 0xb4, 0x62, 0x40});
        const S cases[] = {
            {"x264 3840x2160 High", Codec::H264, cat({nal({}), x264Uhd, pps264}), 52, 5, "5.2"},
            {"x264 1920x1080 Main", Codec::H264, cat({nal({}), x264Fhd}), 50, 6, "5.0"},
            {"H.264 scaling lists", Codec::H264, cat({nal({}), synH264}), 51, 4, "5.1"},
            {"mock clip IDR (Baseline)", Codec::H264, au(0), 21, 1, "2.1"},
            {"x265 3840x2160", Codec::Hevc, cat({nal({0x40, 0x01, 0x0c}), nal({}), x265Uhd, pps265}), 153, 5, "5.1"},
            {"x265 sub-layer", Codec::Hevc, cat({nal({}), x265Layers}), 123, 4, "4.1"},
            {"HEVC sub-layer info", Codec::Hevc, cat({nal({}), synHevc}), 150, 5, "5.0"},
        };
        for (const S& k : cases) {
            SpsInfo info;
            const bool ok = parseSps(k.c, k.data.data(), k.data.size(), info);
            expect(ok && info.levelIdc == k.level && info.refFrames == k.refs && levelText(k.c, info.levelIdc) == k.levelName, name,
                   std::string(k.what) + ": " + (ok ? "level " + std::to_string(info.levelIdc) + " (" + levelText(k.c, info.levelIdc) +
                                                          "), " + std::to_string(info.refFrames) + " reference frames"
                                                    : "not parsed"));
        }
        SpsInfo info;
        const std::vector<uint8_t> p = au(1), truncated = cat({nal({}), std::vector<uint8_t>(x264Uhd.begin(), x264Uhd.begin() + 6)});
        std::vector<uint8_t> badLayers = cat({nal({}), synHevc});
        badLayers[6] = uint8_t((badLayers[6] & 0xf1) | (7 << 1));  // sps_max_sub_layers_minus1 7
        const std::vector<uint8_t> av1{0x0a, 0x03, 0x00, 0x00, 0x00};
        expect(!parseSps(Codec::H264, p.data(), p.size(), info), name, "a P frame has an SPS");
        expect(!parseSps(Codec::H264, truncated.data(), truncated.size(), info), name, "a truncated SPS was parsed");
        expect(!parseSps(Codec::Hevc, badLayers.data(), badLayers.size(), info), name, "sps_max_sub_layers_minus1 7 accepted");
        expect(!parseSps(Codec::Av1, av1.data(), av1.size(), info) && !parseSps(Codec::Hevc, nullptr, 0, info), name, "AV1 / empty");
        std::printf("  %-44s ok\n", name);
    }
}

// An encoder with temporal SVC as AMF documents it: the dyadic pattern after
// every key frame, "the request to mark the current picture as LTR would be
// delayed to the next base temporal layer picture if the current picture is in
// an enhancement layer", FORCE_LTR_REFERENCE_BITFIELD honoured on the frame it
// is set on. keyAt: a key frame it makes on its own (its pattern restarts).
struct SvcEncoder {
    SvcEncoder(LtrTracker& tracker, int numLayers, int pipelineDelay) : t(tracker), layers(numLayers), delay(pipelineDelay) {}
    struct Out {
        uint64_t id;
        int layer;
        LtrTracker::Plan plan;
        LtrTracker::Output o;
        bool ok;
    };
    LtrTracker& t;
    int layers;
    int delay;
    uint64_t keyAt = 0;
    uint64_t pos = 0;
    int pendingMark = -1;
    std::vector<std::pair<uint64_t, LtrTracker::Plan>> queue;
    std::vector<LtrTracker::Output> queued;
    std::vector<Out> out;

    void frame(uint64_t id, int64_t now, bool idr) {
        const LtrTracker::Plan p = t.plan(id, now, idr);
        t.submitted(id, p, now);
        LtrTracker::Output o;
        o.key = o.intra = p.idr || id == keyAt;
        if (o.key) pos = 0, pendingMark = -1;
        o.temporalLayer = LtrTracker::layerAt(pos++, layers);
        int mark = o.key ? -1 : p.markSlot;
        if (mark >= 0 && o.temporalLayer != 0) {
            pendingMark = mark;  // delayed to the next base-layer frame
            mark = -1;
        } else if (o.temporalLayer == 0) {
            if (mark < 0) mark = pendingMark;
            pendingMark = -1;
        }
        o.markedSlot = mark;
        o.refMask = p.refMask;
        queue.emplace_back(id, p);
        queued.push_back(o);
        while (int(queue.size()) > delay) {
            const auto [fid, plan] = queue.front();
            const LtrTracker::Output oo = queued.front();
            queue.erase(queue.begin());
            queued.erase(queued.begin());
            const bool ok = t.output(fid, oo, plan, now);
            out.push_back({fid, oo.temporalLayer, plan, oo, ok});
        }
    }
    void ackMarks(uint64_t current, uint64_t after) {
        for (const Out& x : out) {
            if (x.o.markedSlot >= 0 && x.id + after == current) t.ack(x.id);
        }
    }
};

void testLtrSvc() {
    const char* name = "SVC: LTR marks / recovery on base layer";
    {
        expect(LtrTracker::layerAt(0, 2) == 0 && LtrTracker::layerAt(1, 2) == 1 && LtrTracker::layerAt(2, 2) == 0 &&
                   LtrTracker::layerAt(1, 3) == 2 && LtrTracker::layerAt(2, 3) == 1 && LtrTracker::layerAt(4, 3) == 0 &&
                   LtrTracker::layerAt(6, 4) == 2 && LtrTracker::layerAt(5, 1) == 0,
               name, "layerAt pattern");
        LtrTracker t;
        LtrTracker::Config c{2, 6, 1000};
        c.layers = 2;
        t.reset(c);
        SvcEncoder enc(t, 2, 0);
        for (uint64_t id = 1; id <= 41; ++id) {
            enc.frame(id, int64_t(id), id == 1);
            enc.ackMarks(id, 2);
        }
        int marks = 0;
        for (const SvcEncoder::Out& x : enc.out) {
            if (x.plan.markSlot >= 0) {
                ++marks;
                expect(x.layer == 0, name, "mark planned on frame " + std::to_string(x.id) + " in layer " + std::to_string(x.layer));
            }
            expect(x.o.markedSlot < 0 || x.layer == 0, name, "frame " + std::to_string(x.id) + " stored as LTR in an enhancement layer");
            expect(x.plan.layer == x.layer, name, "frame " + std::to_string(x.id) + " predicted in layer " + std::to_string(x.plan.layer));
        }
        expect(marks >= 5, name, "only " + std::to_string(marks) + " marks in 41 frames");
        // Frames 40 and 41 lost; 42 is an enhancement-layer frame (odd
        // position): no recovery there, 43 (base layer) recovers.
        expect(t.recover(40, std::nullopt), name, "no ACKed LTR to recover from");
        enc.frame(42, 42, false);
        enc.frame(43, 43, false);
        const SvcEncoder::Out& e42 = enc.out[enc.out.size() - 2];
        const SvcEncoder::Out& e43 = enc.out.back();
        expect(e42.layer == 1 && !e42.plan.recovery && !e42.plan.idr, name, "the enhancement frame 42 was planned as the recovery");
        expect(e43.layer == 0 && e43.plan.recovery && e43.plan.refFloor < 40 && e43.ok, name,
               "base frame 43: recovery " + std::to_string(e43.plan.recovery) + ", refFloor " + std::to_string(e43.plan.refFloor));
    }
    std::printf("  %-44s ok\n", name);

    name = "SVC: layer prediction follows the encoder";
    {
        // The encoder makes a key frame on its own at an odd position: its
        // pattern restarts, the tracker learns it from the output (one frame
        // in the encoder at a time, so frame 23 is planned before 22's layer
        // is known: a mark there is delayed by the encoder, and tracked).
        LtrTracker t;
        LtrTracker::Config c{2, 3, 1000};
        c.layers = 2;
        t.reset(c);
        SvcEncoder enc(t, 2, 1);
        enc.keyAt = 22;
        for (uint64_t id = 1; id <= 60; ++id) {
            enc.frame(id, int64_t(id), id == 1);
            enc.ackMarks(id, 2);
        }
        expect(t.stats().layerResyncs >= 1, name, "no resync after the encoder's own key frame");
        int wrong = 0;
        for (const SvcEncoder::Out& x : enc.out) {
            expect(x.o.markedSlot < 0 || x.layer == 0, name, "LTR in an enhancement layer at " + std::to_string(x.id));
            if (x.id > 24) wrong += x.plan.layer != x.layer;
        }
        expect(wrong == 0, name, std::to_string(wrong) + " frames after 24 predicted in the wrong layer");
        bool slotsHeld = false;
        for (const auto& v : t.slots()) slotsHeld = slotsHeld || (v.frameId > 22 && v.acked);
        expect(slotsHeld, name, "no ACKed LTR after the unplanned key frame");
        // A recovery frame that comes out in an enhancement layer is refused.
        LtrTracker::Plan rp;
        rp.recovery = true;
        rp.refMask = 1;
        LtrTracker::Output ro;
        ro.refMask = 1;
        ro.temporalLayer = 1;
        expect(!t.output(1000, ro, rp, 1000), name, "a recovery frame in layer 1 was accepted");
        ro.temporalLayer = 0;
        expect(t.output(1001, ro, rp, 1001), name, "a base-layer recovery frame was refused");
    }
    std::printf("  %-44s ok\n", name);

    name = "SVC: planned IDR with frames in flight";
    {
        // An unplanned key frame at an odd position (frame 4) shifts the
        // prediction; a planned IDR (frame 9) restarts it while frames planned
        // under the shift are still in the encoder (two at a time): their
        // outputs must not shift the prediction after the IDR again.
        LtrTracker t;
        LtrTracker::Config c{2, 3, 1000};
        c.layers = 2;
        t.reset(c);
        SvcEncoder enc(t, 2, 2);
        enc.keyAt = 4;
        for (uint64_t id = 1; id <= 30; ++id) {
            enc.frame(id, int64_t(id), id == 1 || id == 9);
            enc.ackMarks(id, 2);
        }
        int wrong = 0;
        for (const SvcEncoder::Out& x : enc.out) {
            if (x.id >= 9) wrong += x.plan.layer != x.layer;
        }
        expect(wrong == 0, name, std::to_string(wrong) + " frames from the IDR on predicted in the wrong layer");
        expect(t.stats().layerResyncs == 1, name, std::to_string(t.stats().layerResyncs) + " resyncs, want 1");
        // Frames 29 and 30 lost: the recovery is planned on a base-layer
        // frame and accepted (no extra IDR).
        expect(t.recover(29, std::nullopt), name, "no ACKed LTR to recover from");
        for (uint64_t id = 31; id <= 34; ++id) enc.frame(id, int64_t(id), false);
        int recoveries = 0;
        for (const SvcEncoder::Out& x : enc.out) {
            if (!x.plan.recovery) continue;
            ++recoveries;
            expect(x.layer == 0 && x.ok, name, "recovery frame " + std::to_string(x.id) + " in layer " + std::to_string(x.layer) + " refused");
        }
        expect(recoveries == 1 && t.stats().failedRecoveries == 0 && t.stats().idrFallbacks == 0, name,
               std::to_string(recoveries) + " recoveries, " + std::to_string(t.stats().failedRecoveries) + " failed");
    }
    std::printf("  %-44s ok\n", name);
}

std::vector<uint8_t> bytes(std::initializer_list<int> b) {
    std::vector<uint8_t> v;
    for (int x : b) v.push_back(uint8_t(x));
    return v;
}

void testLayers() {
    const char* name = "temporal layers / discardable frames";
    // H.264: SVC prefix NAL (14, svc_extension_flag, temporal_id 1) + a
    // non-reference slice (nal_ref_idc 0); a reference slice; an IDR.
    const std::vector<uint8_t> h264Enh = bytes({0, 0, 0, 1, 0x0e, 0x80, 0x00, 0x20, 0, 0, 1, 0x01, 0x88, 0x84});
    const std::vector<uint8_t> h264Ref = bytes({0, 0, 0, 1, 0x41, 0x9a, 0x02});
    const std::vector<uint8_t> h264Idr = bytes({0, 0, 0, 1, 0x67, 0x64, 0, 0, 1, 0x68, 0xee, 0, 0, 1, 0x65, 0x88});
    LayerInfo li = layerInfo(Codec::H264, h264Enh.data(), h264Enh.size(), 1);
    expect(li.temporalId == 1 && li.reference == 0 && isDiscardable(false, li, 2, 1), name, "h264 enhancement frame");
    li = layerInfo(Codec::H264, h264Ref.data(), h264Ref.size(), 1);
    expect(li.temporalId == -1 && li.reference == 1 && !isDiscardable(false, li, 2, 1), name, "h264 reference frame in layer 1 discardable");
    li = layerInfo(Codec::H264, h264Idr.data(), h264Idr.size(), 1);
    expect(li.reference == 1 && !isDiscardable(true, li, 2, 0), name, "h264 IDR");
    // HEVC: TRAIL_N in the top layer; TRAIL_N in a lower layer (a higher one
    // may reference it); TRAIL_R in the top layer; IDR_W_RADL.
    const std::vector<uint8_t> trailN1 = bytes({0, 0, 1, 0x00, 0x02, 0xaf}), trailN0 = bytes({0, 0, 1, 0x00, 0x01, 0xaf}),
                               trailR1 = bytes({0, 0, 1, 0x02, 0x02, 0xaf}), idr = bytes({0, 0, 0, 1, 0x40, 0x01, 0x0c, 0, 0, 1, 0x26, 0x01, 0xaf});
    li = layerInfo(Codec::Hevc, trailN1.data(), trailN1.size(), 1);
    expect(li.temporalId == 1 && li.reference == 0 && isDiscardable(false, li, 2, 1), name, "hevc TRAIL_N top layer");
    li = layerInfo(Codec::Hevc, trailN0.data(), trailN0.size(), 1);
    expect(li.temporalId == 0 && li.reference == 1 && !isDiscardable(false, li, 2, 0), name, "hevc TRAIL_N below the top layer");
    li = layerInfo(Codec::Hevc, trailR1.data(), trailR1.size(), 1);
    expect(li.temporalId == 1 && li.reference == 1 && !isDiscardable(false, li, 2, 1), name, "hevc TRAIL_R top layer");
    li = layerInfo(Codec::Hevc, idr.data(), idr.size(), 1);
    expect(li.temporalId == 0 && li.reference == 1, name, "hevc IDR (after a VPS)");
    // AV1: temporal delimiter, then OBU_FRAME with an extension (temporal_id
    // 1) and a two-byte leb128 size; one without an extension.
    std::vector<uint8_t> av1 = bytes({0x12, 0x00, 0x36, 0x20, 0x80, 0x01});
    av1.resize(av1.size() + 128, 0x55);
    li = layerInfo(Codec::Av1, av1.data(), av1.size(), 1);
    expect(li.temporalId == 1 && li.reference == -1 && isDiscardable(false, li, 2, 1) && !isDiscardable(false, li, 2, 0) &&
               !isDiscardable(false, li, 1, 0),
           name, "av1 OBU extension temporal_id " + std::to_string(li.temporalId));
    const std::vector<uint8_t> av1Plain = bytes({0x12, 0x00, 0x32, 0x02, 0x10, 0x20});
    li = layerInfo(Codec::Av1, av1Plain.data(), av1Plain.size(), 1);
    expect(li.temporalId == -1, name, "av1 without an extension");
    std::printf("  %-44s ok\n", name);

    // The mock backend's SVC enhancement layer: each canned P frame recoded
    // as a non-reference picture (h264AsNonReference). Whether it decodes
    // to the original's picture is checked with FFmpeg (VENDOR_NOTES Phase
    // 5 wiring A); here: every P frame converts, nal_ref_idc 0, the marking
    // bit gone (a hand-made slice), refusals.
    name = "non-reference copies of the mock clip";
    {
        const std::vector<uint8_t> clipIdr = au(0);
        size_t n = 0;
        for (size_t i = 1; i < ReplayEncoder::kClipFrames; ++i, ++n) {
            const std::vector<uint8_t> p = au(i);
            const std::vector<uint8_t> c = h264AsNonReference(p.data(), p.size(), clipIdr.data(), clipIdr.size());
            if (c.empty()) {
                expect(false, name, "frame " + std::to_string(i) + " not converted");
                break;
            }
            const LayerInfo lo = layerInfo(Codec::H264, p.data(), p.size(), 1), lc = layerInfo(Codec::H264, c.data(), c.size(), 1);
            expect(lo.reference == 1 && lc.reference == 0, name, "nal_ref_idc of frame " + std::to_string(i));
            expect(c.size() + 2 >= p.size() && c.size() <= p.size() + 1, name, "size of frame " + std::to_string(i));
        }
        // first_mb 0, P (5), PPS 0, frame_num 1 (4 bits), no override, no
        // list modification, adaptive_ref_pic_marking_mode_flag 0, stop bit.
        const std::vector<uint8_t> slice = bytes({0, 0, 0, 1, 0x41, 0x9a, 0x22});
        const std::vector<uint8_t> want = bytes({0, 0, 0, 1, 0x01, 0x9a, 0x24});
        expect(h264AsNonReference(slice.data(), slice.size(), clipIdr.data(), clipIdr.size()) == want, name, "hand-made slice");
        const std::vector<uint8_t> mmco = bytes({0, 0, 0, 1, 0x41, 0x9a, 0x26});  // adaptive_ref_pic_marking_mode_flag 1
        expect(h264AsNonReference(mmco.data(), mmco.size(), clipIdr.data(), clipIdr.size()).empty(), name, "converted a slice with MMCOs");
        expect(h264AsNonReference(clipIdr.data(), clipIdr.size(), clipIdr.data(), clipIdr.size()).empty(), name, "converted an IDR");
        const std::vector<uint8_t> p1 = au(1);
        expect(h264AsNonReference(p1.data(), p1.size(), p1.data(), p1.size()).empty(), name, "converted without SPS / PPS");
        std::printf("  %-44s ok (%zu P frames)\n", name, n);
    }

    name = "sub-frame output: slices put together";
    {
        SliceAssembler a;
        const uint8_t s1[] = {1, 2}, s2[] = {3}, s3[] = {4, 5, 6};
        SliceAssembler::Result r = a.add(SliceAssembler::Part::Frame, 4, s1, 2, 100);
        expect(r.complete && a.parts() == 1 && a.frame().size() == 2 && a.frameId() == 4 && a.firstQpc() == 100, name, "a whole frame");
        r = a.add(SliceAssembler::Part::Slice, 5, s1, 2, 200);
        expect(!r.complete && a.collecting(), name, "first slice completed a frame");
        r = a.add(SliceAssembler::Part::Slice, -1, s2, 1, 210);  // a part without the id
        r = a.add(SliceAssembler::Part::Last, 5, s3, 3, 230);
        expect(r.complete && a.parts() == 3 && a.frame() == std::vector<uint8_t>({1, 2, 3, 4, 5, 6}) && a.firstQpc() == 200 &&
                   a.frameId() == 5 && !a.collecting(),
               name, "three slices");
        // Frame 6 never finishes: frame 7's first part drops its two parts.
        a.add(SliceAssembler::Part::Slice, 6, s1, 2, 300);
        a.add(SliceAssembler::Part::Slice, 6, s2, 1, 310);
        r = a.add(SliceAssembler::Part::Slice, 7, s3, 3, 320);
        expect(!r.complete && r.droppedParts == 2, name, "an unfinished frame was not dropped");
        r = a.add(SliceAssembler::Part::Last, 7, s2, 1, 330);
        expect(r.complete && a.parts() == 2 && a.frame() == std::vector<uint8_t>({4, 5, 6, 3}) && a.firstQpc() == 320, name,
               "the frame after a dropped one");
        // A whole frame while slices are collecting.
        a.add(SliceAssembler::Part::Slice, 8, s1, 2, 400);
        r = a.add(SliceAssembler::Part::Frame, 9, s2, 1, 410);
        expect(r.complete && r.droppedParts == 1 && a.frameId() == 9 && a.frame().size() == 1 && a.droppedFrames() == 2, name,
               "a whole frame after an unfinished one");
    }
    std::printf("  %-44s ok\n", name);
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

    // The rects encoder.FocusROI (internal/host/encoder/focus.go) makes for a
    // 1920x1080 stream with the pointer at (100, 100), Background -2 (its Go
    // test pins the same numbers): the whole picture at -2, a 135x135 square
    // around the pointer at +6, a 180x180 one around the crosshair (the
    // centre) at +8; and the pointer square clipped at the bottom-left corner.
    name = "ROI maps of the cursor / crosshair rects";
    {
        const std::vector<RoiRect> focus = {RoiRect{0, 0, 1920, 1080, -2}, RoiRect{33, 33, 135, 135, 6}, RoiRect{870, 450, 180, 180, 8}};
        const auto count = [](const auto& values, auto v) { return size_t(std::count(values.begin(), values.end(), v)); };
        const RoiMap hevc = roiImportanceMap(1920, 1080, 64, focus);  // AMF HEVC / AV1: 64x64 blocks
        expect(hevc.cols == 30 && hevc.rows == 17 && count(hevc.values, 8u) == 9 && count(hevc.values, 9u) == 12 &&
                   count(hevc.values, 4u) == 510 - 21 && hevc.values[0] == 8 && hevc.values[2 * 30 + 2] == 8 && hevc.values[3 * 30 + 3] == 4 &&
                   hevc.values[7 * 30 + 13] == 9 && hevc.values[9 * 30 + 16] == 9 && hevc.values[10 * 30 + 16] == 4,
               name, "AMF HEVC importance map");
        const RoiMap mb = roiImportanceMap(1920, 1080, 16, focus);  // AMF H.264: macroblocks
        expect(mb.cols == 120 && mb.rows == 68 && count(mb.values, 8u) == 81 && count(mb.values, 9u) == 144, name,
               "AMF H.264 importance map");
        const nvenc::QpMap qh = nvenc::roiQpDeltaMap(Codec::Hevc, 1920, 1080, focus);  // NVENC HEVC: 32x32
        expect(qh.cols == 60 && qh.rows == 34 && count(qh.values, int8_t(-6)) == 25 && count(qh.values, int8_t(-8)) == 36 &&
                   count(qh.values, int8_t(2)) == 60 * 34 - 61,
               name, "NVENC HEVC QP delta map");
        const nvenc::QpMap qa = nvenc::roiQpDeltaMap(Codec::Av1, 1920, 1080, focus);  // NVENC AV1: 64x64, x4
        expect(count(qa.values, int8_t(-24)) == 9 && count(qa.values, int8_t(-32)) == 12 && count(qa.values, int8_t(8)) == 510 - 21, name,
               "NVENC AV1 QP delta map");
        const RoiMap corner = roiImportanceMap(1920, 1080, 64, {RoiRect{0, 1008, 73, 72, 6}});
        expect(count(corner.values, 8u) == 4 && corner.values[15 * 30] == 8 && corner.values[16 * 30 + 1] == 8, name, "clipped pointer square");
        // Into a pitched host-memory GRAY32 plane (AMF ROI_DATA), padding untouched.
        const size_t pitch = hevc.cols * 4 + 12;
        std::vector<uint8_t> plane(pitch * hevc.rows, 0xee);
        expect(writeRoiPlane(hevc, plane.data(), pitch), name, "writeRoiPlane refused a valid pitch");
        bool rowsOk = true, padOk = true;
        for (uint32_t y = 0; y < hevc.rows; ++y) {
            for (uint32_t x = 0; x < hevc.cols; ++x) {
                uint32_t v;
                std::memcpy(&v, &plane[y * pitch + x * 4], 4);
                rowsOk = rowsOk && v == hevc.values[size_t(y) * hevc.cols + x];
            }
            for (size_t b = hevc.cols * 4; b < pitch; ++b) padOk = padOk && plane[y * pitch + b] == 0xee;
        }
        expect(rowsOk && padOk, name, "GRAY32 plane rows / padding");
        expect(!writeRoiPlane(hevc, plane.data(), hevc.cols * 4 - 4), name, "a pitch smaller than a row accepted");
    }
    std::printf("  %-44s ok\n", name);

    name = "coded size alignment";
    expect(alignUp(1920, 64) == 1920 && alignUp(1080, 16) == 1088 && alignUp(3440, 64) == 3456 && alignUp(1440, 16) == 1440 &&
               alignUp(1080, 1) == 1080,
           name, "alignUp");
    std::printf("  %-44s ok\n", name);
}


// Reference frame invalidation (NVENC recovery), driven like the backend:
// plan, invalidate, submit.
struct RfiSim {
    explicit RfiSim(int dpb) { t.reset(dpb); }
    RfiTracker t;
    uint64_t next = 1;
    std::vector<RfiTracker::Plan> plans;  // by frame id - 1
    RfiTracker::Plan frame(bool idr = false) {
        const RfiTracker::Plan p = t.plan(next, idr || next == 1);
        t.submitted(next, p);
        plans.push_back(p);
        ++next;
        return p;
    }
    void until(uint64_t last) {
        while (next <= last) frame();
    }
};

std::string ids(const std::vector<uint64_t>& v) {
    std::string out;
    for (uint64_t x : v) out += (out.empty() ? "" : ",") + std::to_string(x);
    return out;
}

void testRfi() {
    const char* name = "invalidation: range, refFloor, DPB window";
    {
        RfiSim s(6);
        s.until(25);
        s.t.recover(22);
        RfiTracker::Plan p = s.frame();  // 26
        expect(p.recovery && !p.idr && p.refFloor == 21 && p.invalidate == std::vector<uint64_t>{22, 23, 24, 25}, name,
               "loss at 22: refFloor " + std::to_string(p.refFloor) + ", invalidate " + ids(p.invalidate));
        p = s.frame();  // 27: back to normal
        expect(!p.recovery && !p.idr && p.invalidate.empty(), name, "frame after the recovery frame");
        s.until(40);
        s.t.recover(36);  // 36..40 = 5 frames: 35 still in the 6-frame window
        p = s.frame();  // 41
        expect(p.recovery && p.refFloor == 35 && p.invalidate.size() == 5, name, "loss of 5 frames: refFloor " + std::to_string(p.refFloor));
        s.until(50);
        s.t.recover(45);  // 45..50 = 6 frames: nothing valid left (Sunshine: "rfi request too large")
        p = s.frame();  // 51
        expect(p.idr && !p.recovery && p.invalidate.empty(), name, "loss of the whole DPB: IDR");
        s.until(55);
        s.t.recover(51);  // the key frame itself
        p = s.frame();
        expect(p.idr, name, "loss of the key frame: IDR");
        s.until(70);
        s.t.recover(80);  // never submitted
        p = s.frame();
        expect(p.idr, name, "loss of a frame not submitted yet: IDR");
    }
    std::printf("  %-44s ok\n", name);

    name = "invalidation: repeated and overlapping losses";
    {
        RfiSim s(6);
        s.until(60);
        s.t.recover(58);
        RfiTracker::Plan p = s.frame();  // 61 references 57
        expect(p.recovery && p.refFloor == 57, name, "first loss");
        s.t.recover(61);  // the recovery frame lost as well
        p = s.frame();    // 62: 58..60 stay invalid, so 57 again
        expect(p.recovery && p.refFloor == 57 && p.invalidate == std::vector<uint64_t>{61}, name,
               "recovery frame lost: refFloor " + std::to_string(p.refFloor) + ", invalidate " + ids(p.invalidate));
        // Losses reported between plan and submit: one inside the range the
        // planned frame invalidates is covered by it; one of the planned
        // frame itself stays pending and the next frame recovers again.
        s.until(70);
        s.t.recover(68);
        const RfiTracker::Plan q = s.t.plan(71, false);
        s.t.recover(69);
        s.t.recover(71);
        s.t.submitted(71, q);
        s.plans.push_back(q);
        ++s.next;
        expect(q.recovery && q.refFloor == 67 && q.invalidate == std::vector<uint64_t>{68, 69, 70}, name, "loss at 68");
        expect(s.t.pending(), name, "the loss of the planned frame was dropped");
        p = s.frame();  // 72: 71 lost too, 68..70 still invalid
        expect(p.recovery && p.refFloor == 67 && p.invalidate == std::vector<uint64_t>{71} && !s.t.pending(), name,
               "loss of the planned frame: refFloor " + std::to_string(p.refFloor) + ", invalidate " + ids(p.invalidate));
        // Only a covered loss arrives meanwhile: nothing stays pending (else
        // the next frame would invalidate the good recovery frame again).
        s.until(75);
        s.t.recover(74);
        const RfiTracker::Plan q2 = s.t.plan(76, false);
        s.t.recover(75);
        s.t.submitted(76, q2);
        s.plans.push_back(q2);
        ++s.next;
        expect(q2.recovery && !s.t.pending(), name, "a loss inside the planned range was not covered");
        // A failed submit (no submitted()) plans the same recovery again.
        s.until(80);
        s.t.recover(78);
        const RfiTracker::Plan a = s.t.plan(81, false);
        const RfiTracker::Plan b = s.t.plan(81, false);
        expect(a.recovery && b.recovery && a.invalidate == b.invalidate && a.refFloor == b.refFloor, name, "plan changed state");
        // A forced IDR wins and covers the loss.
        p = s.frame(true);
        expect(p.idr && !p.recovery && !s.t.pending(), name, "forced IDR over a pending loss");
        // An unplanned key frame (the encoder made one) covers older losses
        // and is a new floor.
        s.until(90);
        s.t.recover(88);
        s.t.unplannedKey(89);
        expect(!s.t.pending(), name, "an unplanned key frame did not cover an older loss");
        s.until(92);
        s.t.recover(90);
        p = s.frame();  // 93: 89 is the key frame, 90.. lost: refFloor 89
        expect(p.recovery && p.refFloor == 89, name, "unplanned key frame as reference: " + std::to_string(p.refFloor));
        s.t.recover(89);
        p = s.frame();
        expect(p.idr, name, "loss of the unplanned key frame: IDR");
        const RfiTracker::Stats st = s.t.stats();
        expect(st.recoveries >= 4 && st.idrFallbacks >= 2, name, "stats");
    }
    std::printf("  %-44s ok\n", name);

    name = "invalidation: window resized to the encoder's";
    {
        // The encoder's SPS says it keeps 3 reference frames, not 6: frames
        // older than the last 3 are gone, whatever was planned with 6.
        RfiSim s(6);
        s.until(20);
        s.t.recover(19);
        s.t.resize(3);  // window 18, 19, 20; the pending loss stays
        expect(s.t.dpbSize() == 3 && s.t.pending(), name, "resize lost the pending loss or the size");
        RfiTracker::Plan p = s.frame();  // 21: references 18
        expect(p.recovery && p.refFloor == 18 && p.invalidate == std::vector<uint64_t>{19, 20}, name,
               "loss at 19: refFloor " + std::to_string(p.refFloor) + ", invalidate " + ids(p.invalidate));
        s.until(30);
        s.t.recover(28);  // 28..30 = the whole 3-frame window: IDR (with 6 it would have recovered from 27)
        p = s.frame();
        expect(p.idr && !p.recovery, name, "loss of 3 frames with a 3-frame window: no IDR");
        s.until(40);
        s.t.recover(39);  // 39, 40 lost: 38 is still kept
        p = s.frame();
        expect(p.recovery && p.refFloor == 38, name, "loss of 2 frames: refFloor " + std::to_string(p.refFloor));
        s.t.resize(0);  // clamped to 1
        expect(s.t.dpbSize() == 1, name, "resize(0)");
    }
    std::printf("  %-44s ok\n", name);
}

void testNvencPolicy() {
    const char* name = "NVENC API version negotiation";
    using namespace nvenc;
    const uint32_t built = apiVersion(13, 0);
    expect(versionProblem(apiVersion(13, 0), built).empty() && versionProblem(apiVersion(13, 2), built).empty() &&
               versionProblem(apiVersion(14, 0), built).empty(),
           name, "a newer driver refused");
    const std::string old = versionProblem(apiVersion(12, 2), built);
    expect(old.find("12.2") != std::string::npos && old.find("13.0") != std::string::npos && old.find("570.0") != std::string::npos, name,
           "old driver text: " + old);
    expect(!versionProblem(0, built).empty() && minimumDriver(apiVersion(12, 0)) == "522.25" && apiVersionText(0xd2) == "13.2", name,
           "version texts");
    std::printf("  %-44s ok\n", name);

    name = "NVENC preset by pixel rate";
    struct P {
        uint32_t w, h;
        int fps;
        const char* q;
        int want;
    };
    const P presets[] = {{1920, 1080, 60, "speed", 4},  {1920, 1080, 240, "speed", 4}, {2560, 1440, 120, "speed", 4},
                         {2560, 1440, 165, "speed", 3}, {3840, 2160, 60, "speed", 4},  {3840, 2160, 90, "speed", 2},
                         {3840, 2160, 120, "speed", 1}, {7680, 4320, 60, "speed", 1},  {1920, 1080, 60, "balanced", 5},
                         {1920, 1080, 60, "quality", 6}, {3840, 2160, 120, "quality", 3}};
    for (const P& x : presets) {
        const int got = presetFor(x.w, x.h, x.fps, x.q);
        expect(got == x.want, name, std::to_string(x.w) + "x" + std::to_string(x.h) + "@" + std::to_string(x.fps) + " " + x.q + ": P" +
                                        std::to_string(got) + ", expected P" + std::to_string(x.want));
    }
    std::printf("  %-44s ok\n", name);

    name = "NVENC reference frames by level";
    struct D {
        Codec c;
        uint32_t w, h;
        int want;
    };
    // H.264: level 5.1 / 5.2 MaxDpbMbs 184320 / frame macroblocks, at most 16
    // (A.3.1), above level 5's frame size level 6's 696320; HEVC: MaxDpbSize
    // (A.4.2) less the current picture; capped at kDpbFrames.
    const D dpbs[] = {{Codec::H264, 1920, 1080, 6}, {Codec::H264, 2560, 1440, 6}, {Codec::H264, 3840, 2160, 5},
                      {Codec::H264, 4096, 2160, 5}, {Codec::H264, 4096, 2304, 5}, {Codec::H264, 4096, 4096, 6},
                      {Codec::Hevc, 1920, 1080, 6}, {Codec::Hevc, 3440, 1440, 6}, {Codec::Hevc, 3840, 2160, 5},
                      {Codec::Hevc, 5120, 1440, 5}, {Codec::Hevc, 5120, 2880, 6}, {Codec::Hevc, 7680, 4320, 5},
                      {Codec::Av1, 3840, 2160, 6},  {Codec::Av1, 7680, 4320, 6},  {Codec::Hevc, 0, 0, kDpbFrames}};
    for (const D& x : dpbs) {
        const int got = dpbFramesFor(x.c, x.w, x.h);
        expect(got == x.want, name, std::string(codecName(x.c)) + " " + std::to_string(x.w) + "x" + std::to_string(x.h) + ": " +
                                        std::to_string(got) + ", expected " + std::to_string(x.want));
    }
    std::printf("  %-44s ok\n", name);

    name = "NVENC rate values and ROI QP delta maps";
    const Rate r = rateFor(20000, 1.0, 60), r2 = rateFor(50000, 1.5, 120);
    expect(r.average == 20000000 && r.max == 20000000 && r.vbv == 333333 && r2.vbv == 625000, name, "rate values");
    expect(qpMapBlock(Codec::H264) == 16 && qpMapBlock(Codec::Hevc) == 32 && qpMapBlock(Codec::Av1) == 64, name, "block sizes");
    const QpMap hevc = roiQpDeltaMap(Codec::Hevc, 1920, 1080, {RoiRect{100, 100, 64, 64, 10}});
    size_t nonzero = 0;
    for (int8_t v : hevc.values) nonzero += v != 0;
    expect(hevc.cols == 60 && hevc.rows == 34 && nonzero == 9 && hevc.values[3 * 60 + 3] == -10 && hevc.values[5 * 60 + 5] == -10 &&
               hevc.values[6 * 60 + 6] == 0,
           name, "hevc map");
    const QpMap av1 = roiQpDeltaMap(Codec::Av1, 1920, 1080, {RoiRect{0, 0, 64, 64, 5}, RoiRect{0, 0, 128, 64, -3}});
    expect(av1.cols == 30 && av1.rows == 17 && av1.values[0] == -20 && av1.values[1] == 12, name, "av1 map: highest weight wins");
    const QpMap h264 = roiQpDeltaMap(Codec::H264, 64, 32, {RoiRect{0, 0, 16, 16, -4}, RoiRect{200, 200, 16, 16, 9}});
    expect(h264.cols == 4 && h264.rows == 2 && h264.values[0] == 4 && h264.values[1] == 0, name, "h264 map, rect outside");
    std::printf("  %-44s ok\n", name);

    name = "NVENC re-encode limits and QP maps";
    expect(oversizeLimit(20000, 60, 4.0) == 166667 && oversizeLimit(8000, 30, 2.5) == 83333, name, "limits");
    expect(reencodeQpDelta(Codec::Hevc, 2.0) == 6 && reencodeQpDelta(Codec::H264, 1.05) == 2 && reencodeQpDelta(Codec::Hevc, 100) == 12 &&
               reencodeQpDelta(Codec::Hevc, 3.0) == 10 && reencodeQpDelta(Codec::Av1, 2.0) == 24 && reencodeQpDelta(Codec::Av1, 1000) == 48,
           name, "QP deltas");
    QpMap roi;
    roi.cols = roi.rows = 2;
    roi.values = {-10, 0, 0, -51};
    const QpMap up = offsetQpMap(Codec::Hevc, 64, 64, &roi, 6);
    expect(up.cols == 2 && up.rows == 2 && up.values == std::vector<int8_t>({-4, 6, 6, -45}), name, "ROI map plus the offset");
    expect(offsetQpMap(Codec::Hevc, 64, 64, &roi, 60).values[1] == 51 && offsetQpMap(Codec::Av1, 64, 64, nullptr, 200).values[0] == 127,
           name, "clamping");
    const QpMap plain = offsetQpMap(Codec::H264, 64, 32, &roi, 4);  // roi has the wrong size for 16x16 blocks: ignored
    expect(plain.cols == 4 && plain.rows == 2 && std::all_of(plain.values.begin(), plain.values.end(), [](int8_t v) { return v == 4; }), name,
           "uniform offset");
    std::printf("  %-44s ok\n", name);
}

void testHdrMetadata() {
    const char* name = "HDR10 metadata and its units";
    DisplayColor d;
    d.known = d.hdr = true;
    d.minLuminance = 0.005, d.maxLuminance = 1000, d.maxFullFrameLuminance = 400;
    d.red[0] = 0.64, d.red[1] = 0.33;  // the panel's (sRGB-like) primaries are not used
    HdrMetadata m = hdrMetadataFor(d);
    expect(m.red[0] == 0.708 && m.red[1] == 0.292 && m.green[0] == 0.170 && m.green[1] == 0.797 && m.blue[0] == 0.131 &&
               m.blue[1] == 0.046 && m.white[0] == 0.3127 && m.white[1] == 0.3290,
           name, "primaries: not BT.2020 / D65");
    expect(m.maxLuminance == 1000 && m.minLuminance == 0.005 && m.maxCll == 1000 && m.maxFall == 400, name, "luminance from the display");
    // Unknown, implausible or inconsistent values: a 1000 cd/m2 display.
    const HdrMetadata unknown = hdrMetadataFor(DisplayColor{});
    expect(unknown.maxLuminance == kDefaultHdrPeak && unknown.minLuminance == 0 && unknown.maxCll == 1000 && unknown.maxFall == 1000, name,
           "unknown display");
    d.maxLuminance = 50;
    expect(hdrMetadataFor(d).maxLuminance == kDefaultHdrPeak, name, "peak below 80 cd/m2 accepted");
    d.maxLuminance = 20000;
    expect(hdrMetadataFor(d).maxLuminance == kDefaultHdrPeak, name, "peak above 10000 cd/m2 accepted");
    d.maxLuminance = 600, d.maxFullFrameLuminance = 800, d.minLuminance = 7;
    m = hdrMetadataFor(d);
    expect(m.maxCll == 600 && m.maxFall == 600 && m.minLuminance == 0, name, "full-frame above peak / black level above 5 cd/m2");
    // HEVC SEI (and AMF): chromaticity x 50000, luminance x 10000; AV1: 0.16,
    // 24.8 and 18.14 fixed point.
    d.maxLuminance = 1000, d.maxFullFrameLuminance = 400, d.minLuminance = 0.005;
    m = hdrMetadataFor(d);
    const MasteringCodes h = masteringCodes(m, false), a = masteringCodes(m, true);
    expect(h.red[0] == 35400 && h.red[1] == 14600 && h.green[0] == 8500 && h.green[1] == 39850 && h.blue[0] == 6550 && h.blue[1] == 2300 &&
               h.white[0] == 15635 && h.white[1] == 16450 && h.maxLuminance == 10000000 && h.minLuminance == 50,
           name, "HEVC units");
    expect(a.red[0] == 46399 && a.red[1] == 19137 && a.green[0] == 11141 && a.green[1] == 52232 && a.blue[0] == 8585 && a.blue[1] == 3015 &&
               a.white[0] == 20493 && a.white[1] == 21561 && a.maxLuminance == 256000 && a.minLuminance == 82,
           name, "AV1 units");
    m.white[0] = 1.5;  // out of range: clamped
    expect(masteringCodes(m, false).white[0] == 50000 && masteringCodes(m, true).white[0] == 65535, name, "clamping");
    Started st;
    describeColor(st, m);
    expect(st.hdr && st.bitDepth == 10 && st.colorSpace == "bt2020-pq" && st.hdrMetadata, name, "started (HDR10)");
    describeColor(st, std::nullopt);
    expect(!st.hdr && st.bitDepth == 8 && st.colorSpace == "bt709" && !st.hdrMetadata, name, "started (SDR)");
    std::printf("  %-44s ok\n", name);
}

}  // namespace

int runEncoderSelfTest() {
    failures = 0;
    testLtr();
    testLtrSvc();
    testLayers();
    testRfi();
    testBitstream();
    testRoi();
    testNvencPolicy();
    testHdrMetadata();
    std::printf("self-test-encoder: %s\n", failures ? "FAIL" : "ok");
    return failures ? 1 : 0;
}

}  // namespace recon

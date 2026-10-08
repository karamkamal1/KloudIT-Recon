#include "ring.hpp"

#include <algorithm>
#include <atomic>
#include <cmath>
#include <bit>
#include <cstring>

#include "platform/platform.hpp"

namespace recon {

static_assert(std::endian::native == std::endian::little, "the ring layout is little-endian");

namespace {

template <typename T>
T load(const uint8_t* p) {
    T v;
    std::memcpy(&v, p, sizeof(v));
    return v;
}

template <typename T>
void store(uint8_t* p, T v) {
    std::memcpy(p, &v, sizeof(v));
}

// The counters are naturally aligned (8-byte offsets in a page-aligned view).
std::atomic_ref<uint64_t> counter(uint8_t* base, size_t off) {
    return std::atomic_ref<uint64_t>(*reinterpret_cast<uint64_t*>(base + off));
}

Status ringError(const std::string& text) { return Status::Error("ring", text, true); }

uint32_t dirtyPpm(float dirty) { return uint32_t(std::lround(std::clamp(double(dirty), 0.0, 1.0) * 1e6)); }

}  // namespace

RingWriter::~RingWriter() {
    if (base_) UnmapViewOfFile(base_);
}

Status RingWriter::attach(HANDLE mapping, uint64_t size, HANDLE event) {
    using namespace ring;
    if (size < kHeaderSize + 2ull * kMinSlotSize || size > (16ull << 30)) return ringError("ring size out of range");
    void* view = MapViewOfFile(mapping, FILE_MAP_READ | FILE_MAP_WRITE, 0, 0, static_cast<SIZE_T>(size));
    if (!view) return ringError("cannot map the ring: " + win32ErrorText(GetLastError()));
    base_ = static_cast<uint8_t*>(view);
    size_ = size;
    MEMORY_BASIC_INFORMATION mbi{};
    if (!VirtualQuery(base_, &mbi, sizeof(mbi)) || mbi.RegionSize < size) return ringError("ring mapping smaller than --ring-size");

    const uint8_t* h = base_;
    if (load<uint64_t>(h + kOffMagic) != kMagic) return ringError("bad ring magic");
    if (load<uint32_t>(h + kOffVersion) != kVersion) return ringError("unsupported ring layout version");
    if (load<uint32_t>(h + kOffHeaderSize) != kHeaderSize) return ringError("unexpected ring header size");
    if (load<uint32_t>(h + kOffSlotHeaderSize) != kSlotHeaderSize) return ringError("unexpected slot header size");
    const uint32_t slots = load<uint32_t>(h + kOffSlotCount);
    const uint32_t slotSize = load<uint32_t>(h + kOffSlotSize);
    if (slots < 2 || slots > kMaxSlots) return ringError("slot count out of range");
    if (slotSize < kMinSlotSize || slotSize % 4096 != 0) return ringError("slot size out of range");
    const uint64_t total = load<uint64_t>(h + kOffTotalSize);
    if (total != size || uint64_t(kHeaderSize) + uint64_t(slots) * slotSize != size) {
        return ringError("ring geometry does not match --ring-size");
    }
    if (counter(base_, kOffWriteCount).load() != 0 || counter(base_, kOffReadCount).load() != 0) {
        return ringError("ring is not fresh");
    }
    DWORD flags = 0;
    if (!event || !GetHandleInformation(event, &flags)) return ringError("bad --event-handle");

    slotCount_ = slots;
    slotSize_ = slotSize;
    event_ = event;
    store<int64_t>(base_ + kOffQpcFrequency, qpcFrequency());
    store<uint32_t>(base_ + kOffHelperPid, GetCurrentProcessId());
    std::atomic_thread_fence(std::memory_order_release);
    return Status::Ok();
}

WriteResult RingWriter::write(const EncodedFrame& f) {
    using namespace ring;
    const uint64_t read = counter(base_, kOffReadCount).load(std::memory_order_acquire);
    if (read > written_ || written_ - read > slotCount_) return WriteResult::Corrupt;
    const bool tooLarge = f.size > slotSize_ - kSlotHeaderSize;
    if (tooLarge || written_ - read == slotCount_) {
        ++dropped_;
        ++droppedPending_;
        counter(base_, kOffDropped).store(dropped_, std::memory_order_relaxed);
        return tooLarge ? WriteResult::TooLarge : WriteResult::Full;
    }

    uint8_t* s = base_ + kHeaderSize + (written_ % slotCount_) * uint64_t(slotSize_);
    std::memset(s, 0, kSlotHeaderSize);
    uint32_t flags = 0;
    if (f.key) flags |= kFlagKey;
    if (f.recovery) flags |= kFlagRecovery;
    if (droppedPending_) flags |= kFlagDroppedBefore;
    if (f.info.repeat) flags |= kFlagRepeat;
    if (f.info.dirty >= 0) flags |= kFlagDirty;
    if (f.discardable) flags |= kFlagDiscardable;
    store<uint64_t>(s + kSlotSeq, written_);
    store<uint64_t>(s + kSlotFrameId, f.info.frameId);
    store<uint32_t>(s + kSlotFlags, flags);
    store<uint32_t>(s + kSlotGen, f.gen);
    store<uint32_t>(s + kSlotPayloadOffset, kSlotHeaderSize);
    store<uint32_t>(s + kSlotPayloadSize, static_cast<uint32_t>(f.size));
    store<int64_t>(s + kSlotPresentQpc, f.info.presentQpc);
    store<int64_t>(s + kSlotCaptureQpc, f.info.captureQpc);
    store<int64_t>(s + kSlotSubmitQpc, f.info.submitQpc);
    store<int64_t>(s + kSlotOutputQpc, f.outputQpc);
    store<uint64_t>(s + kSlotRefFloor, f.recovery ? f.refFloor : 0);
    store<int32_t>(s + kSlotLtrSlot, f.ltrSlot);
    store<uint32_t>(s + kSlotTemporalLayer, f.temporalLayer);
    store<uint32_t>(s + kSlotRefLtrMask, f.refLtrMask);
    store<uint32_t>(s + kSlotDroppedBefore, droppedPending_);
    store<uint32_t>(s + kSlotWidth, f.width);
    store<uint32_t>(s + kSlotHeight, f.height);
    if (f.info.dirty >= 0) store<uint32_t>(s + kSlotDirtyPpm, dirtyPpm(f.info.dirty));
    if (f.size) std::memcpy(s + kSlotHeaderSize, f.data, f.size);

    // Publish: everything above happens-before recon-host's acquire load of writeCount.
    counter(base_, kOffWriteCount).store(written_ + 1, std::memory_order_release);
    ++written_;
    droppedPending_ = 0;
    SetEvent(event_);
    return WriteResult::Written;
}

}  // namespace recon

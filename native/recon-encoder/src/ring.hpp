// Shared-memory frame ring, producer side (layout version 1).
//
// recon-host creates an unnamed file mapping, writes the static header fields,
// and passes the (inherited) handle on the command line. The helper is the only
// writer of slots and of writeCount; recon-host is the only writer of readCount.
// The exact byte layout is in docs/HELPER_PROTOCOL.md and mirrored by
// internal/host/encoder/ring.go: keep all three in sync.
#pragma once

#include <windows.h>

#include <cstddef>
#include <cstdint>
#include <string>

#include "backend.hpp"
#include "types.hpp"

namespace recon {

namespace ring {
// "RECONRNG" read as a little-endian u64.
constexpr uint64_t kMagic = 0x474E524E4F434552ull;
constexpr uint32_t kVersion = 1;
constexpr uint32_t kHeaderSize = 4096;
constexpr uint32_t kSlotHeaderSize = 128;
constexpr uint32_t kMinSlotSize = 64 * 1024;
constexpr uint32_t kMaxSlots = 1024;

// Ring header offsets.
constexpr size_t kOffMagic = 0;           // u64
constexpr size_t kOffVersion = 8;         // u32
constexpr size_t kOffHeaderSize = 12;     // u32
constexpr size_t kOffSlotCount = 16;      // u32
constexpr size_t kOffSlotSize = 20;       // u32 (slot header + payload capacity)
constexpr size_t kOffTotalSize = 24;      // u64
constexpr size_t kOffSlotHeaderSize = 32; // u32
constexpr size_t kOffQpcFrequency = 40;   // i64, written by the helper on attach
constexpr size_t kOffHelperPid = 48;      // u32, written by the helper on attach
constexpr size_t kOffWriteCount = 64;     // u64 atomic, helper: slots published
constexpr size_t kOffReadCount = 128;     // u64 atomic, recon-host: slots consumed
constexpr size_t kOffDropped = 192;       // u64 atomic, helper: frames dropped (ring full / too large)

// Slot header offsets (from the start of the slot).
constexpr size_t kSlotSeq = 0;             // u64 write index this slot was written at
constexpr size_t kSlotFrameId = 8;         // u64
constexpr size_t kSlotFlags = 16;          // u32
constexpr size_t kSlotGen = 20;            // u32
constexpr size_t kSlotPayloadOffset = 24;  // u32 from slot start (>= kSlotHeaderSize)
constexpr size_t kSlotPayloadSize = 28;    // u32
constexpr size_t kSlotPresentQpc = 32;     // i64
constexpr size_t kSlotCaptureQpc = 40;     // i64
constexpr size_t kSlotSubmitQpc = 48;      // i64
constexpr size_t kSlotOutputQpc = 56;      // i64
constexpr size_t kSlotRefFloor = 64;       // u64 (valid with kFlagRecovery)
constexpr size_t kSlotLtrSlot = 72;        // i32, -1 = none
constexpr size_t kSlotTemporalLayer = 76;  // u32
constexpr size_t kSlotRefLtrMask = 80;     // u32
constexpr size_t kSlotDroppedBefore = 84;  // u32 frames dropped right before this one
constexpr size_t kSlotWidth = 88;          // u32
constexpr size_t kSlotHeight = 92;         // u32

constexpr uint32_t kFlagKey = 1u << 0;
constexpr uint32_t kFlagRecovery = 1u << 1;
constexpr uint32_t kFlagDroppedBefore = 1u << 2;
constexpr uint32_t kFlagRepeat = 1u << 3;  // idle re-submit of the previous image
constexpr uint32_t kFlagSeqStart = 1u << 4;  // key frame starting a sequence (stream start, or answering forceIdr)
}  // namespace ring

enum class WriteResult { Written, Full, TooLarge, Corrupt };

class RingWriter {
public:
    RingWriter() = default;
    ~RingWriter();
    RingWriter(const RingWriter&) = delete;
    RingWriter& operator=(const RingWriter&) = delete;

    // Maps the inherited section and validates the header written by recon-host.
    Status attach(HANDLE mapping, uint64_t size, HANDLE event);

    // Copies one frame into the next free slot and signals the event. Never
    // waits: a full ring drops this (the newest) frame. Output thread only.
    WriteResult write(const EncodedFrame& f);

    uint64_t droppedTotal() const { return dropped_; }
    uint32_t slotCount() const { return slotCount_; }
    uint32_t slotSize() const { return slotSize_; }

private:
    uint8_t* base_ = nullptr;
    uint64_t size_ = 0;
    HANDLE event_ = nullptr;
    uint32_t slotCount_ = 0;
    uint32_t slotSize_ = 0;
    uint64_t written_ = 0;        // local copy of writeCount
    uint64_t dropped_ = 0;        // total dropped
    uint32_t droppedPending_ = 0; // dropped since the last written slot
};

}  // namespace recon

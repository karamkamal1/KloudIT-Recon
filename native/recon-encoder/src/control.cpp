#include "control.hpp"

#include <chrono>
#include <cstdint>
#include <cstring>

#include "platform/platform.hpp"
#include "types.hpp"

namespace recon {

namespace {
// Per-frame stats beyond this many queued messages are dropped.
constexpr size_t kMaxQueuedDroppable = 256;
}  // namespace

ControlChannel::~ControlChannel() {
    // The threads may be blocked in ReadFile / WriteFile on pipes recon-host
    // still holds open; they end with the process, so do not wait for them.
    if (reader_.joinable()) reader_.detach();
    if (writer_.joinable()) writer_.detach();
}

void ControlChannel::start() {
    reader_ = std::thread([this] { readLoop(); });
    writer_ = std::thread([this] { writeLoop(); });
}

bool ControlChannel::readFull(void* buf, DWORD n) {
    auto* p = static_cast<uint8_t*>(buf);
    while (n > 0) {
        DWORD got = 0;
        if (!ReadFile(inPipe_, p, n, &got, nullptr) || got == 0) return false;
        p += got;
        n -= got;
    }
    return true;
}

void ControlChannel::readLoop() {
    for (;;) {
        uint8_t hdr[4];
        if (!readFull(hdr, 4)) break;
        uint32_t n;
        std::memcpy(&n, hdr, 4);
        if (n > kMaxControlMsg) {
            std::lock_guard<std::mutex> lock(mu_);
            protocolError_ = true;
            cv_.notify_all();
            return;
        }
        std::string msg(n, '\0');
        if (n && !readFull(msg.data(), n)) break;
        std::lock_guard<std::mutex> lock(mu_);
        queue_.push_back(std::move(msg));
        cv_.notify_all();
    }
    std::lock_guard<std::mutex> lock(mu_);
    eof_ = true;
    cv_.notify_all();
}

ControlChannel::Event ControlChannel::next(std::string& msg) {
    std::unique_lock<std::mutex> lock(mu_);
    cv_.wait(lock, [this] { return !queue_.empty() || eof_ || protocolError_ || woken_; });
    if (woken_) {
        woken_ = false;
        return Event::Woken;
    }
    if (protocolError_) return Event::ProtocolError;
    if (!queue_.empty()) {
        msg = std::move(queue_.front());
        queue_.pop_front();
        return Event::Message;
    }
    return Event::Eof;
}

void ControlChannel::wake() {
    std::lock_guard<std::mutex> lock(mu_);
    woken_ = true;
    cv_.notify_all();
}

bool ControlChannel::send(const std::string& json) { return enqueue(json, false); }

bool ControlChannel::sendDroppable(const std::string& json) { return enqueue(json, true); }

bool ControlChannel::enqueue(const std::string& json, bool droppable) {
    if (json.size() > kMaxControlMsg) {
        logf(LogLevel::Error, "control message too large (%zu bytes), not sent", json.size());
        return false;
    }
    std::string framed(4 + json.size(), '\0');
    const uint32_t n = static_cast<uint32_t>(json.size());
    std::memcpy(framed.data(), &n, 4);
    std::memcpy(framed.data() + 4, json.data(), json.size());

    std::lock_guard<std::mutex> lock(writeMu_);
    if (broken_) return false;
    if (droppable && outQueue_.size() >= kMaxQueuedDroppable) return true;  // recon-host is behind: skip
    outQueue_.push_back(std::move(framed));
    writeCv_.notify_all();
    return true;
}

void ControlChannel::writeLoop() {
    for (;;) {
        std::string msg;
        {
            std::unique_lock<std::mutex> lock(writeMu_);
            writeCv_.wait(lock, [this] { return !outQueue_.empty(); });
            msg = std::move(outQueue_.front());
            outQueue_.pop_front();
            writing_ = true;
        }
        const char* p = msg.data();
        DWORD left = static_cast<DWORD>(msg.size());
        bool ok = true;
        while (left > 0) {
            DWORD wrote = 0;
            if (!WriteFile(outPipe_, p, left, &wrote, nullptr) || wrote == 0) {
                ok = false;
                break;
            }
            p += wrote;
            left -= wrote;
        }
        std::lock_guard<std::mutex> lock(writeMu_);
        writing_ = false;
        if (!ok) {
            broken_ = true;
            outQueue_.clear();
            writeCv_.notify_all();
            std::lock_guard<std::mutex> l2(mu_);
            eof_ = true;  // recon-host is gone: shut down
            cv_.notify_all();
            return;
        }
        writeCv_.notify_all();
    }
}

void ControlChannel::flush(int timeoutMs) {
    std::unique_lock<std::mutex> lock(writeMu_);
    writeCv_.wait_for(lock, std::chrono::milliseconds(timeoutMs), [this] { return broken_ || (outQueue_.empty() && !writing_); });
}

}  // namespace recon

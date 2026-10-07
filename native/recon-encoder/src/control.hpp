// Control channel on the helper's stdin/stdout: u32 little-endian length +
// JSON payload in both directions (the same framing as proto.WriteMsg/ReadMsg).
//
// Anonymous pipes inherited from recon-host are used instead of a named pipe on
// purpose: no other process can open, squat or impersonate them, and the
// helper sees EOF on stdin as soon as recon-host exits, so it never outlives it.
#pragma once

#include <windows.h>

#include <condition_variable>
#include <deque>
#include <mutex>
#include <string>
#include <thread>

namespace recon {

class ControlChannel {
public:
    ControlChannel(HANDLE in, HANDLE out) : inPipe_(in), outPipe_(out) {}
    ~ControlChannel();
    ControlChannel(const ControlChannel&) = delete;
    ControlChannel& operator=(const ControlChannel&) = delete;

    // Starts the stdin reader and stdout writer threads.
    void start();

    enum class Event { Message, Eof, ProtocolError, Woken };
    // Waits for the next message (main thread). Eof: recon-host closed stdin or
    // stdout broke; ProtocolError: the framing is broken; Woken: wake() was called.
    Event next(std::string& msg);
    void wake();

    // Queues one message for the writer thread. Thread-safe, never blocks on
    // the pipe. Returns false once the pipe is broken (next() then reports Eof).
    bool send(const std::string& json);
    // Like send, but the message is dropped when recon-host is not keeping up
    // (per-frame stats): a slow reader must never stall the encoder threads.
    bool sendDroppable(const std::string& json);
    // Waits until everything queued was written (or the pipe broke), at most timeoutMs.
    void flush(int timeoutMs);

private:
    void readLoop();
    void writeLoop();
    bool readFull(void* buf, DWORD n);
    bool enqueue(const std::string& json, bool droppable);

    HANDLE inPipe_;
    HANDLE outPipe_;
    std::thread reader_;
    std::thread writer_;

    std::mutex mu_;
    std::condition_variable cv_;
    std::deque<std::string> queue_;
    bool eof_ = false;
    bool protocolError_ = false;
    bool woken_ = false;

    std::mutex writeMu_;
    std::condition_variable writeCv_;
    std::deque<std::string> outQueue_;  // framed messages waiting for the writer
    bool writing_ = false;
    bool broken_ = false;
};

}  // namespace recon

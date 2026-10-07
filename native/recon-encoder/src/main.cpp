// recon-encoder: native capture + encode helper for recon-host (GUIDE Phase 3).
//
// recon-host starts it once per streaming session with an inherited
// shared-memory ring and frame-ready event, talks to it over stdin/stdout
// (length-prefixed JSON), and restarts it if it exits or reports a fatal error.
// The protocol is specified in docs/HELPER_PROTOCOL.md.
#include <windows.h>

#include <atomic>
#include <cerrno>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <memory>
#include <mutex>
#include <string>
#include <thread>

#include "backend.hpp"
#include "control.hpp"
#include "d3d/convert.hpp"
#include "encode_test.hpp"
#include "pipeline.hpp"
#include "platform/platform.hpp"
#include "protocol.hpp"
#include "ring.hpp"
#include "selftest.hpp"
#include "stream.hpp"

using namespace recon;

namespace {

const char kUsage[] =
    "usage: recon-encoder --ring-handle=H --ring-size=N --event-handle=H [options]\n"
    "       recon-encoder --print-caps [--backend=B]\n"
    "       recon-encoder --self-test-convert | --self-test-pacer | --self-test-encoder | --self-test-nvenc[=DLL]\n"
    "       recon-encoder --encode-test=FILE [--backend=B] [encode test options]\n"
    "       recon-encoder --version\n"
    "\n"
    "Started by recon-host; speaks the protocol in docs/HELPER_PROTOCOL.md on stdin/stdout.\n"
    "\n"
    "  --ring-handle=H      inherited handle of the frame ring file mapping (0x... or decimal)\n"
    "  --ring-size=N        size of the mapping in bytes\n"
    "  --event-handle=H     inherited handle of the auto-reset frame-ready event\n"
    "  --backend=B          auto (default) | amf | nvenc | mock\n"
    "  --log-level=L        error | warn | info (default) | debug   (logs go to stderr)\n"
    "  --mock-error-at=N    mock only: report a non-fatal error when frame N is submitted\n"
    "  --mock-fatal-at=N    mock only: fail fatally when frame N is submitted\n"
    "  --mock-hang-at=N     mock only: never return from submitting frame N (a call stuck in the driver)\n"
    "  --dump-nv12=PATH     write converted frame 30 (raw NV12, encoded size) to PATH\n"
    "  --print-caps         print the capabilities JSON and exit\n"
    "  --self-test-convert[=warp|hw]  check the GPU colour conversion on a WARP device (default) or\n"
    "                       the default hardware adapter (exit 0 ok, 1 failed, 77 no device)\n"
    "  --self-test-pacer    check the frame pacing policy on simulated presents (exit 0 ok, 1 failed)\n"
    "  --self-test-encoder  check the encoder logic: LTR and invalidation recovery policies, parameter sets, ROI maps,\n"
    "                       NVENC settings (exit 0 / 1)\n"
    "  --self-test-nvenc[=DLL]  drive the NVENC backend: against DLL (the test double recon-fake-nvenc.dll, no GPU\n"
    "                       needed) or, without DLL, the NVIDIA driver (exit 0 ok, 1 failed, 77 no device / runtime)\n"
    "\n"
    "Encode test: one stream through the real capture, conversion, encoder and ring, without\n"
    "recon-host; the bitstream goes to FILE (Annex-B for h264/hevc, IVF for av1), a summary to stdout:\n"
    "  --codec=hevc|h264|av1  --capture=dda|amd-direct|wgc|synthetic-gpu|synthetic  --frames=N (300)\n"
    "  --width=W --height=H (0 = capture size)  --fps=N (60)  --kbps=N (20000)  --rc=cbr|vbr\n"
    "  --quality=speed|balanced|quality  --vbv=FRAMES (1.0)  --ltr-slots=N  --ltr-interval=N\n"
    "  --live-bitrate=seamless|flush  --instance=N  --zero-copy=0|1  --intra-refresh=N\n"
    "  --monitor=N  --hmonitor=H  --ack-delay=N (frames until an LTR frame is acknowledged, 2)\n"
    "  --at=N:EVENT  at frame id N: idr | loss | rate=KBPS | fps=FPS | roi=X,Y,W,H,WEIGHT | roi=off (repeatable)\n";

struct Args {
    bool printCaps = false;
    bool selfTestConvert = false;
    bool selfTestHardware = false;
    bool selfTestPacer = false;
    bool selfTestEncoder = false;
    bool selfTestNvenc = false;
    std::string selfTestNvencDll;  // --self-test-nvenc=DLL (test double), empty = the driver
    EncodeTestOptions encodeTest;
    std::string dumpNv12;
    bool version = false;
    bool help = false;
    std::string backend = "auto";
    uint64_t ringHandle = 0, ringSize = 0, eventHandle = 0;
    LogLevel logLevel = LogLevel::Info;
    MockOptions mock;
};

bool parseNumber(const std::string& s, uint64_t& out) {
    if (s.empty() || s[0] == '-') return false;
    char* end = nullptr;
    errno = 0;
    out = std::strtoull(s.c_str(), &end, 0);
    return errno == 0 && end && *end == '\0';
}

bool parseArgs(int argc, char** argv, Args& a, std::string& err) {
    for (int i = 1; i < argc; ++i) {
        std::string arg = argv[i];
        std::string key = arg, val;
        const size_t eq = arg.find('=');
        if (eq != std::string::npos) {
            key = arg.substr(0, eq);
            val = arg.substr(eq + 1);
        }
        bool ok = true;
        if (key == "--print-caps") a.printCaps = true;
        else if (key == "--self-test-convert") {
            a.selfTestConvert = true;
            ok = val.empty() || val == "warp" || val == "hw";
            a.selfTestHardware = val == "hw";
        }
        else if (key == "--self-test-pacer") a.selfTestPacer = true;
        else if (key == "--self-test-encoder") a.selfTestEncoder = true;
        else if (key == "--self-test-nvenc") {
            a.selfTestNvenc = true;
            a.selfTestNvencDll = val;
        }
        else if (key == "--encode-test") ok = !(a.encodeTest.output = val).empty();
        else if (encodeTestOption(key, val, a.encodeTest, ok)) {
        }
        else if (key == "--dump-nv12") ok = !(a.dumpNv12 = val).empty();
        else if (key == "--version") a.version = true;
        else if (key == "--help" || key == "-h") a.help = true;
        else if (key == "--backend") a.backend = val;
        else if (key == "--ring-handle") ok = parseNumber(val, a.ringHandle) && a.ringHandle != 0;
        else if (key == "--ring-size") ok = parseNumber(val, a.ringSize);
        else if (key == "--event-handle") ok = parseNumber(val, a.eventHandle) && a.eventHandle != 0;
        else if (key == "--log-level") ok = parseLogLevel(val, a.logLevel);
        else if (key == "--mock-error-at") ok = parseNumber(val, a.mock.errorAt);
        else if (key == "--mock-fatal-at") ok = parseNumber(val, a.mock.fatalAt);
        else if (key == "--mock-hang-at") ok = parseNumber(val, a.mock.hangAt);
        else {
            err = "unknown argument " + arg;
            return false;
        }
        if (!ok) {
            err = "bad value in " + arg;
            return false;
        }
    }
    if (a.backend != "auto" && a.backend != "amf" && a.backend != "nvenc" && a.backend != "mock") {
        err = "unknown backend " + a.backend;
        return false;
    }
    if ((a.mock.errorAt || a.mock.fatalAt || a.mock.hangAt) && a.backend != "mock") {
        err = "--mock-* options need --backend=mock";
        return false;
    }
    if (a.encodeTest.used && a.encodeTest.output.empty()) {
        err = "encode test options need --encode-test=FILE";
        return false;
    }
    const bool standalone =
        a.printCaps || a.version || a.help || a.selfTestConvert || a.selfTestPacer || a.selfTestEncoder || a.selfTestNvenc ||
        !a.encodeTest.output.empty();
    if (!standalone && (!a.ringHandle || !a.ringSize || !a.eventHandle)) {
        err = "--ring-handle, --ring-size and --event-handle are required";
        return false;
    }
    return true;
}

HANDLE toHandle(uint64_t v) { return reinterpret_cast<HANDLE>(static_cast<uintptr_t>(v)); }

// How long the helper may take to stop its threads and release the encoder
// once it has decided to exit.
constexpr DWORD kExitWatchdogMs = 500;

// Ends the process (exit code kExitStuck) kExitWatchdogMs after the helper
// decided to exit (fatal error, shutdown, stdin EOF) unless it has exited by
// then: a capture or encoder call stuck in the driver must not keep the helper,
// and its GPU encoder session, alive. recon-host kills a helper that reported a
// fatal error as well, but after stdin EOF nobody is left to do that.
void armExitWatchdog(ControlChannel& control) {
    static std::once_flag once;
    std::call_once(once, [&control] {
        std::thread([&control] {
            Sleep(kExitWatchdogMs);
            logf(LogLevel::Error, "threads did not stop within %lu ms of exiting, terminating", kExitWatchdogMs);
            control.flush(200);  // a fatal error must still reach recon-host
            TerminateProcess(GetCurrentProcess(), static_cast<UINT>(kExitStuck));
        }).detach();
    });
}

class MainReporter : public Reporter {
public:
    explicit MainReporter(ControlChannel& c) : c_(c) {}
    void stats(const FrameStats& s) override { c_.sendDroppable(encodeStats(s)); }
    void error(const Status& s, std::string_view re) override {
        logf(LogLevel::Warn, "%s: %s", s.code.c_str(), s.text.c_str());
        c_.send(encodeError(s, re));
    }
    void captureChanged(const CaptureEvent& ev) override { c_.send(encodeCaptureEvent(ev)); }
    void fatal(const Status& s) override {
        if (!fatal_.exchange(true)) {
            logf(LogLevel::Error, "fatal: %s: %s", s.code.c_str(), s.text.c_str());
            Status f = s;
            f.fatal = true;
            c_.send(encodeError(f, ""));
        }
        armExitWatchdog(c_);
        c_.wake();
    }
    bool fatalRaised() const { return fatal_; }

private:
    ControlChannel& c_;
    std::atomic<bool> fatal_{false};
};

// Desktop coordinates in physical pixels, and IDXGIOutput5::DuplicateOutput1
// needs a per-monitor DPI aware process (Sunshine display_base.cpp sets the
// same before display init). user32 entry point of Windows 10 1703+.
void setDpiAwareness() {
    using Fn = BOOL(WINAPI*)(HANDLE);
    HMODULE user32 = GetModuleHandleW(L"user32.dll");
    auto fn = user32 ? reinterpret_cast<Fn>(reinterpret_cast<void*>(GetProcAddress(user32, "SetProcessDpiAwarenessContext")))
                     : nullptr;
    if (fn) fn(reinterpret_cast<HANDLE>(static_cast<intptr_t>(-4)));  // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2
}

}  // namespace

int main(int argc, char** argv) {
    // Never block on a crash or "missing DLL" dialog: recon-host restarts us.
    SetErrorMode(SEM_FAILCRITICALERRORS | SEM_NOGPFAULTERRORBOX | SEM_NOOPENFILEERRORBOX);

    Args a;
    std::string err;
    if (!parseArgs(argc, argv, a, err)) {
        std::fprintf(stderr, "recon-encoder: %s\n\n%s", err.c_str(), kUsage);
        return kExitUsage;
    }
    if (a.help) {
        std::fputs(kUsage, stderr);
        return kExitOk;
    }
    if (a.version) {
        std::printf("recon-encoder %s (protocol %d)\n", RECON_ENCODER_VERSION, kProtocolVersion);
        return kExitOk;
    }
    setLogLevel(a.logLevel);
    setDpiAwareness();
    if (a.selfTestNvenc) {
        // Alone: it must choose the NVENC runtime before anything loads it.
        const int rc = runNvencSelfTest(fromUtf8(a.selfTestNvencDll));
        std::fflush(stdout);
        return rc;
    }
    if (a.selfTestConvert || a.selfTestPacer || a.selfTestEncoder) {
        int rc = 0;
        if (a.selfTestPacer) rc = runPacerSelfTest();
        if (a.selfTestEncoder) {
            const int e = runEncoderSelfTest();
            if (rc == 0) rc = e;
        }
        if (a.selfTestConvert) {
            const int c = runConvertSelfTest(a.selfTestHardware);
            if (rc == 0) rc = c;
        }
        std::fflush(stdout);
        return rc;
    }

    BackendChoice choice = chooseBackend(a.backend, a.mock);
    if (!a.encodeTest.output.empty()) return runEncodeTest(a.encodeTest, choice);
    if (a.printCaps) {
        std::printf("%s\n", encodeCaps(choice.caps, qpcFrequency()).c_str());
        return kExitOk;
    }

    // Intentionally never freed: its reader and writer threads can still be
    // blocked in ReadFile / WriteFile while the process exits.
    auto* control = new ControlChannel(GetStdHandle(STD_INPUT_HANDLE), GetStdHandle(STD_OUTPUT_HANDLE));
    // Armed from the reader thread too: the main thread may be the one stuck
    // (e.g. in Backend::init) when recon-host goes away.
    control->onClosed([control] { armExitWatchdog(*control); });

    control->start();

    RingWriter ring;
    Status rs = ring.attach(toHandle(a.ringHandle), a.ringSize, toHandle(a.eventHandle));
    if (!rs.ok) {
        logf(LogLevel::Error, "%s", rs.text.c_str());
        control->send(encodeError(rs, ""));
        control->flush(2000);
        return kExitUsage;
    }
    logf(LogLevel::Info, "recon-encoder %s: backend %s, vendor %s, ring %u x %u bytes", RECON_ENCODER_VERSION,
         choice.caps.backend.c_str(), choice.caps.vendor.c_str(), ring.slotCount(), ring.slotSize());
    for (const auto& [name, why] : choice.caps.unavailable) logf(LogLevel::Debug, "%s unavailable: %s", name.c_str(), why.c_str());

    control->send(encodeCaps(choice.caps, qpcFrequency()));

    MainReporter rep(*control);
    std::unique_ptr<Capture> capture;
    std::unique_ptr<Pipeline> pipeline;
    int exitCode = kExitOk;

    for (;;) {
        std::string msg;
        const auto ev = control->next(msg);
        if (ev == ControlChannel::Event::Woken) {
            if (rep.fatalRaised()) {
                exitCode = kExitFatal;
                break;
            }
            continue;
        }
        if (ev == ControlChannel::Event::Eof) {
            logf(LogLevel::Info, "control channel closed, exiting");
            break;
        }
        if (ev == ControlChannel::Event::ProtocolError) {
            rep.fatal(Status::Error("protocol", "control message larger than the limit", true));
            exitCode = kExitFatal;
            break;
        }

        ControlMsg m;
        Status ps = parseControl(msg, m);
        if (!ps.ok) {
            rep.error(ps, m.type);
            continue;
        }
        if (m.type == "shutdown") {
            logf(LogLevel::Info, "shutdown requested");
            break;
        }
        if (m.type == "start") {
            if (pipeline) {
                rep.error(Status::Error("already_started", "the helper encodes one stream; restart it to change"), m.type);
                continue;
            }
            StartResult sr;
            Started st;
            Status ss = startStream(m.start, choice, ring, rep, a.dumpNv12, sr, st);
            if (!ss.ok) {
                if (ss.fatal) {
                    rep.fatal(ss);
                    exitCode = kExitFatal;
                    break;
                }
                rep.error(ss, m.type);
                continue;
            }
            capture = std::move(sr.capture);
            pipeline = std::move(sr.pipeline);
            control->send(encodeStarted(st));
            pipeline->start();
            logf(LogLevel::Info, "started: %s %dx%d@%d %d kbps, capture %s %dx%d", st.codec.c_str(), st.width, st.height,
                 st.fps, st.kbps, st.capture.c_str(), st.captureWidth, st.captureHeight);
            continue;
        }
        if (!pipeline) {
            rep.error(Status::Error("not_started", m.type + " before start"), m.type);
            continue;
        }
        Status s;
        if (m.type == "forceIdr") s = choice.backend->forceIdr();
        else if (m.type == "recover") s = choice.backend->recover(m.lostFromFrameId, m.ackedLtrFrameId);
        else if (m.type == "setRate") s = pipeline->setRate(m.rate);
        else if (m.type == "setRoi") s = choice.backend->setRoi(m.rects);
        else if (m.type == "ack") s = choice.backend->ack(m.ackFrameId);
        if (!s.ok) {
            if (s.fatal) {
                rep.fatal(s);
                exitCode = kExitFatal;
                break;
            }
            rep.error(s, m.type);
        }
    }

    armExitWatchdog(*control);
    if (pipeline) pipeline->stop();  // wakes and joins the threads
    // Only now release the encoder and the capture (their destructors).
    pipeline.reset();
    choice.backend.reset();
    capture.reset();
    control->flush(2000);  // the last messages (a fatal error) must reach recon-host
    logf(LogLevel::Info, "exiting (%d)", exitCode);
    return exitCode;
}

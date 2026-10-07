#!/usr/bin/env python3
"""Click-to-photon latency rig: serial driver and analysis (docs/LATENCY_RIG.md).

Python 3.8+, standard library only (pyserial is used on Windows when installed).

  rig.py measure --port /dev/ttyACM0 --label recon-hevc-1080p120-lan [--samples 200]
      Calibrates the rig, collects samples into results/<label>-<time>.csv and
      prints the summary of that run.
  rig.py analyze results/*.csv [--baseline moonlight-...] [--json s.json] [--csv s.csv]
      Groups captures by configuration label (files with the same label are
      merged) and prints n / median / p95 / min / max per configuration.
      Also reads camera frame counts (columns id,click_frame,client_frame[,host_frame]
      plus --fps or a "# fps=240" line).
  rig.py camera --click led.txt --client client.txt [--host host.txt] --out cam.csv
      240 fps camera fallback: turns FFmpeg signalstats brightness logs of the
      marker LED and the screens into a capture that "analyze" reads.
  rig.py presentmon capture.csv
      Present modes per process from a PresentMon CSV (Composed vs Independent Flip).
  rig.py synth --out DIR / rig.py selftest
      Synthetic captures with known latencies; selftest analyzes them and checks the result.
  rig.py simulate
      (POSIX) a fake rig on a pseudo-terminal, to try "measure" without hardware.

Capture format (what the firmware prints): "id,click_us,client_us,host_us".
Fields are micros() of one 32-bit clock (wraps every ~71.6 min, deltas are taken
modulo 2^32); empty = sensor disabled, "timeout" = no light within the timeout.
Percentiles interpolate linearly between closest ranks (numpy's default, Excel
PERCENTILE.INC).
"""

from __future__ import annotations

import argparse
import csv
import datetime as _dt
import json
import math
import os
import random
import re
import statistics
import sys
import tempfile
import threading
import time
from dataclasses import dataclass, field
from typing import Dict, Iterable, List, Optional, Sequence, Tuple

WRAP = 1 << 32
TIMEOUT = "timeout"
MIN_SAMPLES = 200
METRICS = (
    # name, from field, to field, title
    ("click_to_client", "click", "client", "click->client"),
    ("click_to_host", "click", "host", "click->host"),
    ("host_to_client", "host", "client", "host->client"),
)
_STAMP_RE = re.compile(r"-\d{8}-\d{6}$")


# ---------------------------------------------------------------------------
# Parsing


@dataclass
class Row:
    id: int
    click: object  # microseconds (int, float for camera frames), None (sensor disabled) or TIMEOUT
    client: object
    host: object


@dataclass
class Capture:
    path: str
    label: str
    meta: Dict[str, str] = field(default_factory=dict)
    rows: List[Row] = field(default_factory=list)
    bad_lines: int = 0


def _field(value: Optional[str], fps: Optional[float]) -> object:
    if value is None:
        return None
    value = value.strip()
    if value == "":
        return None
    if value.lower() in (TIMEOUT, "na", "nan", "-"):
        return TIMEOUT
    if fps:  # camera frame number -> microseconds (float keeps sub-microsecond deltas exact)
        return float(value) * 1e6 / fps
    return int(value)


def label_from_path(path: str) -> str:
    stem = os.path.splitext(os.path.basename(path))[0]
    return _STAMP_RE.sub("", stem)


def read_capture(path: str, label: Optional[str] = None, fps: Optional[float] = None) -> Capture:
    """Reads a rig capture (firmware output, comments allowed) or camera frame counts."""
    cap = Capture(path=path, label="")
    header: Optional[List[str]] = None
    with open(path, encoding="utf-8", errors="replace") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            if line.startswith("#"):
                m = re.match(r"#\s*([A-Za-z_][\w.-]*)\s*=\s*(.*)$", line)
                if m:
                    cap.meta[m.group(1).lower()] = m.group(2).strip()
                continue
            cols = [c.strip() for c in line.split(",")]
            if not re.fullmatch(r"\d+", cols[0]):
                names = [c.lower() for c in cols]
                if "id" in names and ("click_us" in names or "click_frame" in names):
                    header = names
                else:
                    cap.bad_lines += 1  # noise on the serial line, not a header
                continue
            if header is None:
                header = ["id", "click_us", "client_us", "host_us"]
            rec = dict(zip(header, cols))
            frames = "click_frame" in header
            rate = None
            if frames:
                rate = fps or float(cap.meta.get("fps", "0") or 0)
                if not rate:
                    raise ValueError(f"{path}: frame counts need --fps or a '# fps=<n>' line")
            suffix = "_frame" if frames else "_us"
            try:
                cap.rows.append(Row(
                    id=int(rec["id"]),
                    click=_field(rec.get("click" + suffix), rate),
                    client=_field(rec.get("client" + suffix), rate),
                    host=_field(rec.get("host" + suffix), rate),
                ))
            except (KeyError, ValueError):
                cap.bad_lines += 1
    cap.label = label or cap.meta.get("label") or label_from_path(path)
    return cap


# ---------------------------------------------------------------------------
# Statistics


def signed_delta(a: int, b: int) -> int:
    """b - a in microseconds on a 32-bit wrapping clock."""
    d = (b - a) % WRAP
    return d - WRAP if d >= WRAP // 2 else d


def percentile(sorted_values: Sequence[float], p: float) -> float:
    """Linear interpolation between closest ranks (type 7)."""
    if not sorted_values:
        raise ValueError("no values")
    k = (len(sorted_values) - 1) * p / 100.0
    lo = math.floor(k)
    hi = min(lo + 1, len(sorted_values) - 1)
    return sorted_values[lo] + (sorted_values[hi] - sorted_values[lo]) * (k - lo)


def stats_ms(values_us: Iterable[int]) -> Optional[Dict[str, float]]:
    v = sorted(x / 1000.0 for x in values_us)
    if not v:
        return None
    return {
        "n": len(v),
        "median": statistics.median(v),
        "p95": percentile(v, 95),
        "min": v[0],
        "max": v[-1],
        "mean": statistics.fmean(v) if hasattr(statistics, "fmean") else statistics.mean(v),
        "stdev": statistics.stdev(v) if len(v) > 1 else 0.0,
    }


@dataclass
class Metric:
    values: List[int] = field(default_factory=list)  # microseconds
    timeouts: int = 0
    invalid: int = 0  # light before the click: sensor saw something else


def metrics_for(rows: Iterable[Row]) -> Dict[str, Metric]:
    out = {name: Metric() for name, _, _, _ in METRICS}
    for r in rows:
        for name, a, b, _ in METRICS:
            va, vb = getattr(r, a), getattr(r, b)
            if va is None or vb is None:
                continue  # sensor not fitted
            if va == TIMEOUT or vb == TIMEOUT:
                out[name].timeouts += 1
                continue
            d = signed_delta(va, vb)
            if d < 0 and a == "click":
                out[name].invalid += 1
                continue
            out[name].values.append(d)
    return out


def summarize(captures: Sequence[Capture], baseline: Optional[str] = None,
              min_samples: int = MIN_SAMPLES) -> Dict[str, object]:
    groups: Dict[str, List[Capture]] = {}
    for c in captures:
        groups.setdefault(c.label, []).append(c)
    configs = []
    for label, caps in groups.items():
        rows = [r for c in caps for r in c.rows]
        ms = metrics_for(rows)
        metrics = {}
        for name, m in ms.items():
            s = stats_ms(m.values)
            if s is None and m.timeouts == 0 and m.invalid == 0:
                continue  # sensor not used in this configuration
            metrics[name] = dict(s or {"n": 0}, timeouts=m.timeouts, invalid=m.invalid)
        n_main = metrics.get("click_to_client", {}).get("n", 0)
        configs.append({
            "label": label,
            "files": [c.path for c in caps],
            "rows": len(rows),
            "bad_lines": sum(c.bad_lines for c in caps),
            "enough_samples": n_main >= min_samples,
            "metrics": metrics,
        })
    configs.sort(key=lambda c: c["label"])
    if baseline:
        base = next((c for c in configs if c["label"] == baseline), None)
        if base is None:
            raise SystemExit(f"baseline {baseline!r} not among the configurations: "
                             + ", ".join(c["label"] for c in configs))
        for c in configs:
            if c is base:
                continue
            delta = {}
            for name, m in c["metrics"].items():
                b = base["metrics"].get(name)
                if b and b.get("n") and m.get("n"):
                    delta[name] = {"median": m["median"] - b["median"], "p95": m["p95"] - b["p95"]}
            c["vs_baseline"] = delta
    return {
        "tool": "recon-latency-rig",
        "generated": _dt.datetime.now(_dt.timezone.utc).isoformat(timespec="seconds"),
        "unit": "ms",
        "percentile": "linear interpolation between closest ranks",
        "min_samples": min_samples,
        "baseline": baseline,
        "configs": configs,
    }


def format_summary(summary: Dict[str, object]) -> str:
    lines = []
    head = f"{'configuration':<34} {'metric':<14} {'n':>5} {'t/o':>4} {'median':>8} {'p95':>8} {'min':>8} {'max':>8}  (ms)"
    lines.append(head)
    lines.append("-" * len(head))
    titles = {name: title for name, _, _, title in METRICS}
    for c in summary["configs"]:
        first = True
        for name, m in c["metrics"].items():
            lab = c["label"] if first else ""
            first = False
            if m.get("n"):
                lines.append(f"{lab:<34} {titles[name]:<14} {m['n']:>5} {m['timeouts']:>4} "
                             f"{m['median']:>8.2f} {m['p95']:>8.2f} {m['min']:>8.2f} {m['max']:>8.2f}")
            else:
                lines.append(f"{lab:<34} {titles[name]:<14} {0:>5} {m['timeouts']:>4}   (no samples)")
            if m.get("invalid"):
                lines.append(f"{'':<34} {'':<14} {m['invalid']} sample(s) with light before the click ignored")
        for name, d in c.get("vs_baseline", {}).items():
            lines.append(f"{'  vs ' + str(summary['baseline']):<34} {titles[name]:<14} {'':>5} {'':>4} "
                         f"{d['median']:>+8.2f} {d['p95']:>+8.2f}")
        if not c["enough_samples"]:
            n = c["metrics"].get("click_to_client", {}).get("n", 0)
            lines.append(f"{'':<34} only {n} click->client samples (< {summary['min_samples']} required)")
    return "\n".join(lines)


def write_summary_csv(summary: Dict[str, object], path: str) -> None:
    cols = ["label", "metric", "n", "timeouts", "invalid", "median_ms", "p95_ms", "min_ms", "max_ms",
            "mean_ms", "stdev_ms", "enough_samples", "delta_median_ms", "delta_p95_ms"]
    with open(path, "w", newline="", encoding="utf-8") as f:
        w = csv.writer(f)
        w.writerow(cols)
        for c in summary["configs"]:
            for name, m in c["metrics"].items():
                d = c.get("vs_baseline", {}).get(name, {})

                def r(x):
                    return "" if x is None else f"{x:.3f}"
                w.writerow([c["label"], name, m.get("n", 0), m["timeouts"], m["invalid"],
                            r(m.get("median")), r(m.get("p95")), r(m.get("min")), r(m.get("max")),
                            r(m.get("mean")), r(m.get("stdev")), int(c["enough_samples"]),
                            r(d.get("median")), r(d.get("p95"))])


def analyze_files(paths: Sequence[str], label: Optional[str] = None, fps: Optional[float] = None,
                  baseline: Optional[str] = None, min_samples: int = MIN_SAMPLES) -> Dict[str, object]:
    return summarize([read_capture(p, label=label, fps=fps) for p in paths], baseline, min_samples)


# ---------------------------------------------------------------------------
# Serial port (no pyserial needed)


class SerialPort:
    """Minimal line-oriented serial port: POSIX termios, or Win32 via pyserial/ctypes."""

    def __init__(self, port: str, baud: int = 115200):
        self.port = port
        self._buf = b""
        if os.name == "nt":
            self._impl = _open_windows(port, baud)
        else:
            self._impl = _PosixSerial(port, baud)

    def write_line(self, text: str) -> None:
        self._impl.write((text + "\n").encode())

    def readline(self, timeout: float) -> Optional[str]:
        deadline = time.monotonic() + timeout
        while b"\n" not in self._buf:
            left = deadline - time.monotonic()
            if left <= 0:
                return None
            self._buf += self._impl.read(min(left, 0.1))
        line, self._buf = self._buf.split(b"\n", 1)
        return line.decode("utf-8", "replace").rstrip("\r")

    def close(self) -> None:
        self._impl.close()


class _PosixSerial:
    def __init__(self, port: str, baud: int):
        import termios
        import tty

        self.fd = os.open(port, os.O_RDWR | os.O_NOCTTY | os.O_NONBLOCK)
        try:
            tty.setraw(self.fd)
            attrs = termios.tcgetattr(self.fd)
            speed = getattr(termios, f"B{baud}", termios.B115200)  # never 1200: that resets a Leonardo
            attrs[2] |= termios.CLOCAL | termios.CREAD
            attrs[4] = attrs[5] = speed
            termios.tcsetattr(self.fd, termios.TCSANOW, attrs)  # DTR is raised on open
        except Exception:
            os.close(self.fd)
            raise

    def write(self, data: bytes) -> None:
        import select

        while data:
            try:
                n = os.write(self.fd, data)
                data = data[n:]
            except BlockingIOError:
                select.select([], [self.fd], [], 1.0)

    def read(self, timeout: float) -> bytes:
        import select

        r, _, _ = select.select([self.fd], [], [], timeout)
        if not r:
            return b""
        try:
            data = os.read(self.fd, 4096)
        except BlockingIOError:
            return b""
        if not data:
            raise EOFError("serial port closed")
        return data

    def close(self) -> None:
        os.close(self.fd)


def _open_windows(port: str, baud: int):
    try:
        import serial  # type: ignore  # pyserial, optional
    except ImportError:
        return _Win32Serial(port, baud)

    class _PySerial:
        def __init__(self):
            self.s = serial.Serial(port, baud, timeout=0.1, dsrdtr=False, rtscts=False)
            self.s.dtr = True

        def write(self, data: bytes) -> None:
            self.s.write(data)

        def read(self, timeout: float) -> bytes:
            self.s.timeout = timeout
            return self.s.read(max(1, self.s.in_waiting))

        def close(self) -> None:
            self.s.close()

    return _PySerial()


try:
    import ctypes as _ct

    class _DCB(_ct.Structure):
        # Fixed-width types so the layout (28 bytes) matches Win32 on every OS.
        _fields_ = [
            ("DCBlength", _ct.c_uint32), ("BaudRate", _ct.c_uint32), ("fBits", _ct.c_uint32),
            ("wReserved", _ct.c_uint16), ("XonLim", _ct.c_uint16), ("XoffLim", _ct.c_uint16),
            ("ByteSize", _ct.c_uint8), ("Parity", _ct.c_uint8), ("StopBits", _ct.c_uint8),
            ("XonChar", _ct.c_char), ("XoffChar", _ct.c_char), ("ErrorChar", _ct.c_char),
            ("EofChar", _ct.c_char), ("EvtChar", _ct.c_char), ("wReserved1", _ct.c_uint16),
        ]

    class _COMMTIMEOUTS(_ct.Structure):
        _fields_ = [(n, _ct.c_uint32) for n in (
            "ReadIntervalTimeout", "ReadTotalTimeoutMultiplier", "ReadTotalTimeoutConstant",
            "WriteTotalTimeoutMultiplier", "WriteTotalTimeoutConstant")]
except ImportError:  # pragma: no cover
    _ct = None


class _Win32Serial:
    """COM port through kernel32 (CreateFileW / SetCommState / ReadFile)."""

    GENERIC_RW = 0x80000000 | 0x40000000
    OPEN_EXISTING = 3
    MAXDWORD = 0xFFFFFFFF
    SETDTR = 5
    # fBinary | fDtrControl=DTR_CONTROL_ENABLE (bits 4-5) | fRtsControl=RTS_CONTROL_ENABLE (bits 12-13)
    FBITS = 0x1 | (1 << 4) | (1 << 12)

    def __init__(self, port: str, baud: int):
        from ctypes import wintypes

        k = _ct.WinDLL("kernel32", use_last_error=True)
        k.CreateFileW.restype = wintypes.HANDLE
        k.CreateFileW.argtypes = [wintypes.LPCWSTR, wintypes.DWORD, wintypes.DWORD, wintypes.LPVOID,
                                  wintypes.DWORD, wintypes.DWORD, wintypes.HANDLE]
        for fn in ("GetCommState", "SetCommState", "SetCommTimeouts", "EscapeCommFunction", "ReadFile",
                   "WriteFile", "CloseHandle"):
            getattr(k, fn).restype = wintypes.BOOL
        self.k = k
        name = port if port.startswith("\\\\.\\") else "\\\\.\\" + port
        h = k.CreateFileW(name, self.GENERIC_RW, 0, None, self.OPEN_EXISTING, 0, None)
        if h is None or h == wintypes.HANDLE(-1).value:
            raise OSError(_ct.get_last_error(), f"cannot open {port}")
        self.h = h
        dcb = _DCB()
        dcb.DCBlength = _ct.sizeof(_DCB)
        if not k.GetCommState(_ct.c_void_p(h), _ct.byref(dcb)):
            self.close()
            raise OSError(_ct.get_last_error(), f"GetCommState {port}")
        dcb.BaudRate, dcb.ByteSize, dcb.Parity, dcb.StopBits = baud, 8, 0, 0
        dcb.fBits = self.FBITS
        to = _COMMTIMEOUTS(self.MAXDWORD, self.MAXDWORD, 100, 0, 2000)  # return as soon as bytes arrive
        if not (k.SetCommState(_ct.c_void_p(h), _ct.byref(dcb))
                and k.SetCommTimeouts(_ct.c_void_p(h), _ct.byref(to))):
            self.close()
            raise OSError(_ct.get_last_error(), f"configuring {port}")
        k.EscapeCommFunction(_ct.c_void_p(h), self.SETDTR)  # the 32U4 CDC only sends with DTR set

    def write(self, data: bytes) -> None:
        n = _ct.c_uint32(0)
        if not self.k.WriteFile(_ct.c_void_p(self.h), data, len(data), _ct.byref(n), None):
            raise OSError(_ct.get_last_error(), "WriteFile")

    def read(self, timeout: float) -> bytes:
        buf = _ct.create_string_buffer(4096)
        n = _ct.c_uint32(0)
        if not self.k.ReadFile(_ct.c_void_p(self.h), buf, 4096, _ct.byref(n), None):
            raise OSError(_ct.get_last_error(), "ReadFile")
        return buf.raw[: n.value]

    def close(self) -> None:
        if getattr(self, "h", None):
            self.k.CloseHandle(_ct.c_void_p(self.h))
            self.h = None


# ---------------------------------------------------------------------------
# Driving the rig


class RigError(RuntimeError):
    pass


class Rig:
    def __init__(self, port: SerialPort, log=None):
        self.port = port
        self.log = log  # file: the rig's comment lines, and data lines while record_data is set
        self.record_data = False  # so samples of a button-started run are not mixed in

    def _line(self, timeout: float) -> Optional[str]:
        line = self.port.readline(timeout)
        if line is not None and self.log and (self.record_data or line.startswith("#")):
            self.log.write(line + "\n")
            self.log.flush()
        return line

    def drain(self, quiet: float = 0.5, limit: float = 5.0) -> List[str]:
        """Reads until the rig is silent for `quiet` seconds."""
        out, end = [], time.monotonic() + limit
        while time.monotonic() < end:
            line = self._line(quiet)
            if line is None:
                break
            out.append(line)
        return out

    def command(self, cmd: str, idle_timeout: float = 5.0, on_line=None) -> List[str]:
        """Sends one command and returns its output up to the matching "# done <name>"."""
        names = {"c": "click", "click": "click", "r": "run", "run": "run", "x": "stop", "stop": "stop",
                 "i": "info", "info": "info", "?": "help"}
        verb = cmd.split()[0]
        name = names.get(verb, verb)
        self.port.write_line(cmd)
        out = []
        while True:
            line = self._line(idle_timeout)
            if line is None:
                raise RigError(f"no answer to {cmd!r} for {idle_timeout:.0f} s (wrong port? firmware flashed?)")
            if line.startswith("# done"):
                done = line[6:].strip()
                if done in (name, "error"):
                    if done == "error":
                        raise RigError(f"rig rejected {cmd!r}: " + "; ".join(out[-1:]))
                    return out
                continue  # a late "done" of an earlier command
            out.append(line)
            if on_line:
                on_line(line)


def _progress_printer(total: int, echo=print):
    vals: List[int] = []
    state = {"rows": 0, "timeouts": 0}

    def on_line(line: str) -> None:
        if line.startswith("#"):
            if line.startswith(("# err", "# warn", "# stopped")):
                echo("  rig: " + line[2:])
            return
        if not re.match(r"\d+,", line):
            return
        cols = (line.split(",") + ["", "", ""])[:4]
        state["rows"] += 1
        try:
            click, client = int(cols[1]), cols[2]
            if client == TIMEOUT:
                state["timeouts"] += 1
            elif client:
                vals.append(signed_delta(click, int(client)))
        except ValueError:
            pass
        if state["rows"] % 10 == 0 or state["rows"] == total:
            med = f"{statistics.median(vals) / 1000:.2f} ms" if vals else "-"
            echo(f"  {state['rows']:>4}/{total}  click->client median {med}"
                 f"{'  timeouts ' + str(state['timeouts']) if state['timeouts'] else ''}")

    return on_line


def cmd_measure(args) -> int:
    label = args.label
    if not re.fullmatch(r"[A-Za-z0-9][\w.+-]*", label):
        raise SystemExit("--label: letters, digits, '.', '_', '+', '-' only (it becomes a file name)")
    os.makedirs(args.out, exist_ok=True)
    stamp = _dt.datetime.now().strftime("%Y%m%d-%H%M%S")
    path = os.path.join(args.out, f"{label}-{stamp}.csv")
    port = SerialPort(args.port)
    try:
        with open(path, "w", encoding="utf-8") as log:
            log.write(f"# label={label}\n# started={_dt.datetime.now().astimezone().isoformat(timespec='seconds')}\n")
            log.write(f"# port={args.port}\n# samples_requested={args.samples}\n")
            if args.note:
                log.write(f"# note={args.note}\n")
            rig = Rig(port, log=log)
            port.write_line("x")  # stop whatever a button press may have started
            rig.drain()
            for line in rig.command("i"):
                print("  rig: " + line.lstrip("# "))
            rig.command("host " + ("on" if args.host_sensor else "off"))
            for key, value in (("timeout", args.timeout_ms), ("settle", args.settle_ms)):
                if value is not None:
                    errs = [l for l in rig.command(f"set {key} {value}") if l.startswith("# err")]
                    if errs:
                        raise RigError(f"set {key}: {errs[0][6:]}")
            if not args.no_cal:
                print("calibrating (the rig clicks 3 times; keep the flash page in front) ...")
                lines = rig.command("cal", idle_timeout=10)
                for line in lines:
                    print("  rig: " + line.lstrip("# "))
                errs = [l for l in lines if l.startswith("# err")]
                if errs:
                    raise RigError("calibration failed: " + errs[0][6:])
            print(f"measuring {args.samples} samples -> {path}  (Ctrl+C stops early)")
            # Longest silence: gap + up to 3 s waiting for dark screens + the sample timeout.
            idle = (args.gap_max + 3000 + (args.timeout_ms or 1000)) / 1000 + 5
            rig.record_data = True
            try:
                rig.command(f"r {args.samples} {args.gap_min} {args.gap_max}", idle_timeout=idle,
                            on_line=_progress_printer(args.samples))
            except KeyboardInterrupt:
                print("stopping ...")
                port.write_line("x")
                rig.drain(quiet=1.0)
            rig.record_data = False
    finally:
        port.close()
    summary = analyze_files([path], min_samples=args.min_samples)
    print()
    print(format_summary(summary))
    print(f"\nraw capture: {path}")
    return 0


# ---------------------------------------------------------------------------
# Fake rig (simulation over a pseudo-terminal) and synthetic captures


@dataclass
class Model:
    """Synthetic latency model in microseconds."""

    host_base: int = 6000      # input path + host page + host monitor
    host_refresh: int = 16667  # host monitor refresh (uniform phase)
    stream_base: int = 22000   # capture -> encode -> network -> decode -> display
    client_refresh: int = 8333
    jitter: int = 1500         # gaussian sigma
    timeout_every: int = 0     # every Nth sample the client sensor times out

    def draw(self, rnd: random.Random, i: int) -> Tuple[Optional[int], Optional[int]]:
        host = self.host_base + rnd.randrange(self.host_refresh)
        client = host + self.stream_base + rnd.randrange(self.client_refresh) + abs(int(rnd.gauss(0, self.jitter)))
        if self.timeout_every and i % self.timeout_every == 0:
            client = None
        return host, client


def synth_rows(model: Model, n: int, seed: int, start_us: int = WRAP - 2_000_000,
               gap_us: Tuple[int, int] = (250_000, 600_000)) -> Tuple[List[str], Dict[str, List[int]]]:
    """CSV lines as the firmware prints them, plus the true latencies."""
    rnd = random.Random(seed)
    t = start_us
    lines, truth = [], {"click_to_client": [], "click_to_host": [], "host_to_client": [], "timeouts": []}
    for i in range(1, n + 1):
        t = (t + rnd.randint(*gap_us)) % WRAP
        host, client = model.draw(rnd, i)
        host_f = str((t + host) % WRAP)
        client_f = TIMEOUT if client is None else str((t + client) % WRAP)
        lines.append(f"{i},{t},{client_f},{host_f}")
        truth["click_to_host"].append(host)
        if client is None:
            truth["timeouts"].append(i)
        else:
            truth["click_to_client"].append(client)
            truth["host_to_client"].append(client - host)
    return lines, truth


SYNTH_CONFIGS = {
    # label: (model, samples)
    "synthetic-moonlight": (Model(stream_base=18000), 240),
    "synthetic-recon": (Model(stream_base=24000, timeout_every=97), 240),
    "synthetic-short": (Model(), 150),  # fails the 200-sample acceptance
}


def write_synth(out_dir: str, seed: int = 1, samples: Optional[int] = None) -> Dict[str, Dict[str, List[int]]]:
    os.makedirs(out_dir, exist_ok=True)
    truths = {}
    for k, (label, (model, n)) in enumerate(SYNTH_CONFIGS.items()):
        n = samples or n
        lines, truth = synth_rows(model, n, seed + k)
        # Split each configuration over two files (A/B blocks) like real sessions.
        half = len(lines) // 2
        for part, chunk in (("a", lines[:half]), ("b", lines[half:])):
            with open(os.path.join(out_dir, f"{label}-2026010{k + 1}-12000{'0' if part == 'a' else '1'}.csv"),
                      "w", encoding="utf-8") as f:
                f.write(f"# label={label}\n# synthetic=1 seed={seed + k}\n")
                f.write("# recon-latency-rig v1 board=synthetic\nid,click_us,client_us,host_us\n")
                f.write("\n".join(chunk) + "\n# run complete\n")
        truths[label] = truth
    # Camera fallback: frame numbers at 240 fps for the moonlight model.
    rnd = random.Random(seed + 100)
    model = SYNTH_CONFIGS["synthetic-moonlight"][0]
    frames = []
    with open(os.path.join(out_dir, "camera-240fps.csv"), "w", encoding="utf-8") as f:
        f.write("# label=synthetic-camera\n# fps=240\nid,click_frame,client_frame\n")
        frame = 100
        for i in range(1, (samples or 220) + 1):
            frame += rnd.randint(60, 140)
            _, client = model.draw(rnd, i)
            cf = frame + round(client * 240 / 1e6)
            frames.append((cf - frame) * 1e6 / 240)
            f.write(f"{i},{frame},{cf}\n")
    truths["synthetic-camera"] = {"click_to_client": frames, "timeouts": []}
    return truths


def cmd_synth(args) -> int:
    write_synth(args.out, args.seed, args.samples)
    print(f"synthetic captures written to {args.out}")
    return 0


def run_selftest(verbose: bool = True) -> List[str]:
    """Analyzes synthetic captures and compares against the generator's ground truth."""
    problems: List[str] = []
    with tempfile.TemporaryDirectory(prefix="rig-selftest-") as d:
        truths = write_synth(d, seed=7)
        paths = sorted(os.path.join(d, p) for p in os.listdir(d))
        summary = analyze_files(paths, baseline="synthetic-moonlight")
        if verbose:
            print(format_summary(summary))
        by_label = {c["label"]: c for c in summary["configs"]}
        if sorted(by_label) != sorted(truths):
            problems.append(f"configurations {sorted(by_label)} != {sorted(truths)}")
        for label, truth in truths.items():
            c = by_label.get(label)
            if not c:
                continue
            for name in ("click_to_client", "click_to_host", "host_to_client"):
                if name not in truth:
                    continue
                vals = sorted(v / 1000 for v in truth[name])
                got = c["metrics"].get(name)
                if not got:
                    problems.append(f"{label}/{name}: missing")
                    continue
                want = {
                    "n": len(vals), "median": statistics.median(vals),
                    "p95": statistics.quantiles(vals, n=20, method="inclusive")[18],
                    "min": vals[0], "max": vals[-1],
                }
                for k, v in want.items():
                    if abs(got[k] - v) > 1e-9:
                        problems.append(f"{label}/{name}/{k}: got {got[k]} want {v}")
            want_to = len(truth["timeouts"])
            if c["metrics"]["click_to_client"]["timeouts"] != want_to:
                problems.append(f"{label}: timeouts {c['metrics']['click_to_client']['timeouts']} want {want_to}")
            if c["enough_samples"] != (len(truth["click_to_client"]) >= MIN_SAMPLES):
                problems.append(f"{label}: enough_samples flag wrong")
        rec = by_label.get("synthetic-recon", {})
        delta = rec.get("vs_baseline", {}).get("click_to_client", {}).get("median")
        if delta is None or not 3.0 < delta < 9.0:  # the models differ by 6 ms of stream latency
            problems.append(f"recon vs moonlight median delta {delta} not ~6 ms")
        # Round trip through the summary writers.
        write_summary_csv(summary, os.path.join(d, "summary.csv"))
        with open(os.path.join(d, "summary.csv"), encoding="utf-8") as f:
            if sum(1 for _ in f) < 2:
                problems.append("summary CSV empty")
        json.dumps(summary)
    return problems


def cmd_selftest(args) -> int:
    problems = run_selftest(verbose=not args.quiet)
    for p in problems:
        print("FAIL " + p)
    print("selftest " + ("FAILED" if problems else "passed"))
    return 1 if problems else 0


class FakeRig:
    """Speaks the firmware's serial protocol on a file descriptor (a pty master)."""

    def __init__(self, fd: int, model: Optional[Model] = None, seed: int = 1, gap_scale: float = 0.0,
                 host_sensor: bool = True, stale_on_stop: int = 0):
        # host_sensor: a host-monitor sensor is fitted; it is used after "host on".
        self.fd = fd
        self.model = model or Model(timeout_every=50)
        self.rnd = random.Random(seed)
        self.gap_scale = gap_scale  # real seconds slept per simulated second between samples
        self.fitted = host_sensor
        self.host = False
        self.stale_on_stop = stale_on_stop  # samples of a button-started run that "x" interrupts
        self.clock = WRAP - 3_000_000  # wraps during the first run
        self.id = 0
        self.stop = threading.Event()
        self._pending = b""
        self.commands: List[str] = []

    def _out(self, line: str) -> None:
        data = (line + "\r\n").encode()
        while data:
            try:
                n = os.write(self.fd, data)
                data = data[n:]
            except BlockingIOError:
                time.sleep(0.001)
            except OSError:
                return

    def _input_waiting(self) -> bool:
        import select

        return bool(self._pending) or bool(select.select([self.fd], [], [], 0)[0])

    def _readline(self) -> Optional[str]:
        import select

        while b"\n" not in self._pending:
            if self.stop.is_set():
                return None
            r, _, _ = select.select([self.fd], [], [], 0.05)
            if r:
                try:
                    data = os.read(self.fd, 1024)
                except OSError:
                    return None
                if not data:
                    return None
                self._pending += data
        line, self._pending = self._pending.split(b"\n", 1)
        return line.decode().strip()

    def _sample(self) -> None:
        self.id += 1
        self.clock = (self.clock + self.rnd.randint(250_000, 600_000)) % WRAP
        host, client = self.model.draw(self.rnd, self.id)
        c = TIMEOUT if client is None else str((self.clock + client) % WRAP)
        h = str((self.clock + host) % WRAP) if self.host else ""
        self._out(f"{self.id},{self.clock},{c},{h}")

    def serve(self) -> None:
        self._out("# recon-latency-rig v1 board=fake hid=none adc_bits=10")
        self._out("id,click_us,client_us,host_us")
        while not self.stop.is_set():
            line = self._readline()
            if line is None:
                return
            if not line:
                continue
            self.commands.append(line)
            argv = line.split()
            verb = argv[0]
            if verb in ("i", "info"):
                self._out("# recon-latency-rig v1 board=fake hid=none adc_bits=10")
                self._out("# settings timeout_ms=1000 settle_ms=40 debounce=3 gap_ms=250..600 count=200")
                self._out("# done info")
            elif verb == "cal":
                self._out("# cal client black=40 white=900 inverted=0 lo=255 hi=470 black_peak=48 white_dip=880")
                if not self.host:
                    self._out("# cal host: skipped (sensor off)")
                elif self.fitted:
                    self._out("# cal host black=30 white=700 inverted=0 lo=197 hi=365 black_peak=35 white_dip=690")
                else:
                    self._out("# cal host: no black/white difference (black=3 white=4): host sensor disabled")
                    self.host = False
                self._out("# done cal")
            elif verb in ("r", "run"):
                n = int(argv[1]) if len(argv) > 1 else 200
                self._out(f"# run n={n} gap_ms=250..600 timeout_ms=1000")
                self._out("id,click_us,client_us,host_us")
                done = 0
                for _ in range(n):
                    if self.gap_scale:
                        time.sleep(self.rnd.uniform(0.25, 0.6) * self.gap_scale)
                    if self._input_waiting():
                        self._out("# stopped")
                        break
                    self._sample()
                    done += 1
                self._out(f"# run complete samples={done}")
                self._out("# done run")
            elif verb in ("c", "click"):
                self._out("id,click_us,client_us,host_us")
                self._sample()
                self._out("# done click")
            elif verb in ("x", "stop"):
                if self.stale_on_stop:
                    self._out("id,click_us,client_us,host_us")
                    for _ in range(self.stale_on_stop):
                        self._sample()
                    self._out("# stopped")
                    self._out("# done run")
                    self.stale_on_stop = 0
                self._out("# done stop")
            elif verb == "host" and len(argv) > 1:
                self.host = argv[1] == "on"
                self._out(f"# host sensor {'on' if self.host else 'off'}")
                self._out("# done host")
            elif verb in ("set", "thr", "mon", "client", "help", "?"):
                self._out(f"# done {'help' if verb == '?' else verb}")
            else:
                self._out("# err unknown command (try help)")
                self._out("# done error")


def open_fake_pty(**kw) -> Tuple[str, FakeRig, threading.Thread]:
    import pty
    import tty

    master, slave = pty.openpty()
    tty.setraw(master)
    path = os.ttyname(slave)
    rig = FakeRig(master, **kw)
    t = threading.Thread(target=rig.serve, daemon=True)
    t.start()
    rig.slave_fd = slave  # keep the slave open so the master does not see EOF between opens
    return path, rig, t


def cmd_simulate(args) -> int:
    if os.name == "nt":
        raise SystemExit("simulate needs a POSIX pseudo-terminal")
    path, rig, t = open_fake_pty(gap_scale=args.gap_scale, host_sensor=not args.no_host)
    print("(the fake has a host-monitor sensor fitted: pass --host-sensor to measure to use it)")
    print(f"fake rig on {path}  (try: {sys.argv[0]} measure --port {path} --label dry-run); Ctrl+C ends")
    try:
        while t.is_alive():
            t.join(0.5)
    except KeyboardInterrupt:
        pass
    rig.stop.set()
    return 0


# ---------------------------------------------------------------------------
# Camera fallback: brightness logs from
#   ffmpeg -i clip.mp4 -vf "crop=W:H:X:Y,signalstats,metadata=mode=print:key=lavfi.signalstats.YAVG:file=r.txt" -f null -


def read_signalstats(path: str, key: str = "YAVG", fps: Optional[float] = None) -> List[Tuple[float, float]]:
    """(seconds, value) per frame from an FFmpeg metadata=print log. With fps the time is
    frame number / fps and the timestamps are ignored (slow-motion exports stretch them)."""
    raw, frames, cur = [], [], None
    pat = re.compile(r"lavfi\.signalstats\." + re.escape(key) + r"=([-+\d.eE]+)")
    with open(path, encoding="utf-8", errors="replace") as f:
        for line in f:
            m = re.search(r"(?:frame:\s*(\d+)\s+)?pts:\s*(-?\d+)\s+pts_time:\s*([-+\d.eE]+)", line)
            if m:
                cur = (m.group(1), int(m.group(2)), float(m.group(3)))
                continue
            m = pat.search(line)
            if m and cur is not None:
                frames.append(int(cur[0]) if cur[0] is not None else len(raw))
                raw.append((cur[1], cur[2], float(m.group(1))))
                cur = None
    if fps:
        return [(n / fps, v) for n, (_, _, v) in zip(frames, raw)]
    # pts_time is printed with 6 significant digits (100 us steps after 10 s, 1 ms
    # after 100 s): rebuild exact times from the integer pts and the time base,
    # estimated over all frames and snapped to 1/N when it is one.
    num = sum(pt for p, pt, _ in raw if p > 0)
    den = sum(p for p, pt, _ in raw if p > 0)
    if not den or num <= 0:
        return [(pt, v) for _, pt, v in raw]
    tb = num / den
    n = round(1 / tb)
    if n and abs(1 / tb - n) < 1e-4 * n:
        tb = 1.0 / n
    return [(p * tb, v) for p, _, v in raw]


def rising_edges(series: Sequence[Tuple[float, float]], rise: float = 0.5, fall: float = 0.25) -> List[float]:
    """Times where the region turns light: crosses 50 % of the black/white step after
    having been below 25 % (the same hysteresis as the firmware)."""
    if not series:
        return []
    vals = sorted(v for _, v in series)
    lo, hi = percentile(vals, 5), percentile(vals, 95)
    if hi - lo < 10:
        raise ValueError("no black/white change in this region (check the crop)")
    on, off = lo + (hi - lo) * rise, lo + (hi - lo) * fall
    light = series[0][1] >= on  # a clip that starts light has no edge there
    edges = []
    for t, v in series:
        if not light and v >= on:
            edges.append(t)
            light = True
        elif light and v < off:
            light = False
    return edges


def camera_rows(click: Sequence[float], client: Sequence[float],
                host: Optional[Sequence[float]] = None) -> List[str]:
    """Pairs each marker edge with the first screen edge before the next marker edge."""
    rows = []
    for i, tc in enumerate(click):
        nxt = click[i + 1] if i + 1 < len(click) else math.inf

        def after(edges):
            t = next((t for t in edges if tc <= t < nxt), None)
            return TIMEOUT if t is None else str(round(t * 1e6))
        h = after(host) if host is not None else ""
        rows.append(f"{i + 1},{round(tc * 1e6)},{after(client)},{h}")
    return rows


def cmd_camera(args) -> int:
    click = rising_edges(read_signalstats(args.click, fps=args.fps))
    client = rising_edges(read_signalstats(args.client, fps=args.fps))
    host = rising_edges(read_signalstats(args.host, fps=args.fps)) if args.host else None
    rows = camera_rows(click, client, host)
    label = args.label or label_from_path(args.out)
    times = f"frame/{args.fps:g}" if args.fps else "pts"
    with open(args.out, "w", encoding="utf-8") as f:
        f.write(f"# label={label}\n# source=camera times={times} click={args.click} client={args.client} "
                f"host={args.host or ''}\n")
        f.write("id,click_us,client_us,host_us\n" + "\n".join(rows) + "\n")
    print(f"{len(rows)} clicks, {len(client)} client edges"
          + (f", {len(host)} host edges" if host is not None else "") + f" -> {args.out}")
    print(format_summary(analyze_files([args.out], min_samples=args.min_samples)))
    return 0


# ---------------------------------------------------------------------------
# PresentMon


PRESENTMON_METRICS = ("MsBetweenPresents", "MsBetweenAppStart", "MsBetweenDisplayChange", "MsUntilDisplayed",
                      "MsDisplayLatency", "MsClickToPhotonLatency", "MsAllInputToPhotonLatency")


def presentmon_summary(path: str) -> List[Dict[str, object]]:
    groups: Dict[Tuple[str, str], Dict[str, object]] = {}
    with open(path, newline="", encoding="utf-8-sig", errors="replace") as f:
        for row in csv.DictReader(f):
            key = (row.get("Application", "?"), row.get("ProcessID", "?"))
            g = groups.setdefault(key, {"application": key[0], "pid": key[1], "presents": 0, "modes": {},
                                        "tearing": 0, "values": {m: [] for m in PRESENTMON_METRICS}})
            g["presents"] += 1
            mode = row.get("PresentMode") or "?"
            g["modes"][mode] = g["modes"].get(mode, 0) + 1
            if row.get("AllowsTearing", "0").strip() == "1":
                g["tearing"] += 1
            for m in PRESENTMON_METRICS:
                v = row.get(m)
                try:
                    g["values"][m].append(float(v))
                except (TypeError, ValueError):
                    pass  # "NA" for frames that never reached the screen
    out = []
    for g in sorted(groups.values(), key=lambda g: -g["presents"]):
        metrics = {}
        for m, vals in g.pop("values").items():
            if vals:
                vals.sort()
                metrics[m] = {"n": len(vals), "median": statistics.median(vals), "p95": percentile(vals, 95)}
        flips = sum(n for mode, n in g["modes"].items() if "Independent Flip" in mode)
        g["independent_flip_share"] = flips / g["presents"]
        g["metrics"] = metrics
        out.append(g)
    return out


def cmd_presentmon(args) -> int:
    for g in presentmon_summary(args.csv):
        print(f"{g['application']} (pid {g['pid']}): {g['presents']} presents, "
              f"independent flip {100 * g['independent_flip_share']:.1f} %, tearing allowed {g['tearing']}")
        for mode, n in sorted(g["modes"].items(), key=lambda kv: -kv[1]):
            print(f"    {100 * n / g['presents']:6.1f} %  {mode}")
        for m, s in g["metrics"].items():
            print(f"    {m:<26} median {s['median']:.2f} ms  p95 {s['p95']:.2f} ms  (n {s['n']})")
    return 0


# ---------------------------------------------------------------------------


def cmd_analyze(args) -> int:
    summary = analyze_files(args.files, label=args.label, fps=args.fps, baseline=args.baseline,
                            min_samples=args.min_samples)
    print(format_summary(summary))
    if args.json:
        with open(args.json, "w", encoding="utf-8") as f:
            json.dump(summary, f, indent=2)
            f.write("\n")
    if args.csv:
        write_summary_csv(summary, args.csv)
    ok = all(c["enough_samples"] for c in summary["configs"])
    return 0 if ok or not args.strict else 2


def main(argv: Optional[Sequence[str]] = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = p.add_subparsers(dest="cmd", required=True)

    m = sub.add_parser("measure", help="drive the rig over serial and record a configuration")
    m.add_argument("--port", required=True, help="serial port: /dev/ttyACM0, /dev/cu.usbmodem*, COM5")
    m.add_argument("--label", required=True, help="configuration name, e.g. recon-hevc-1080p120-lan-chrome")
    m.add_argument("--host-sensor", action="store_true", help="a second sensor on the host monitor is fitted (A1)")
    m.add_argument("--samples", type=int, default=MIN_SAMPLES)
    m.add_argument("--out", default="results", help="directory for raw captures (default: results)")
    m.add_argument("--gap-min", type=int, default=250, help="random gap between clicks, ms")
    m.add_argument("--gap-max", type=int, default=600)
    m.add_argument("--timeout-ms", type=int, help="per-sample timeout on the rig (default 1000)")
    m.add_argument("--settle-ms", type=int, help="dark time required before a click (default 40)")
    m.add_argument("--no-cal", action="store_true", help="keep the rig's current thresholds")
    m.add_argument("--note", help="free text stored in the capture header")
    m.add_argument("--min-samples", type=int, default=MIN_SAMPLES)
    m.set_defaults(fn=cmd_measure)

    a = sub.add_parser("analyze", help="summarize captured CSV files per configuration")
    a.add_argument("files", nargs="+")
    a.add_argument("--label", help="override the configuration label of all files")
    a.add_argument("--fps", type=float, help="camera frame rate for click_frame/client_frame files")
    a.add_argument("--baseline", help="configuration label to compare the others against")
    a.add_argument("--json", help="write the summary as JSON")
    a.add_argument("--csv", help="write the summary as CSV")
    a.add_argument("--min-samples", type=int, default=MIN_SAMPLES)
    a.add_argument("--strict", action="store_true", help="exit 2 if a configuration has too few samples")
    a.set_defaults(fn=cmd_analyze)

    s = sub.add_parser("synth", help="write synthetic captures with known latencies")
    s.add_argument("--out", required=True)
    s.add_argument("--seed", type=int, default=1)
    s.add_argument("--samples", type=int)
    s.set_defaults(fn=cmd_synth)

    t = sub.add_parser("selftest", help="analyze synthetic captures and check the numbers")
    t.add_argument("--quiet", action="store_true")
    t.set_defaults(fn=cmd_selftest)

    cam = sub.add_parser("camera", help="capture from FFmpeg signalstats logs of a slow-motion video")
    cam.add_argument("--click", required=True, help="log of the marker LED region")
    cam.add_argument("--client", required=True, help="log of the client screen region")
    cam.add_argument("--host", help="log of the host monitor region")
    cam.add_argument("--out", required=True, help="capture CSV to write")
    cam.add_argument("--label", help="configuration label (default: from --out)")
    cam.add_argument("--fps", type=float, help="capture frame rate: time = frame number / fps instead of the "
                     "video timestamps (for slow-motion exports, whose timestamps are stretched)")
    cam.add_argument("--min-samples", type=int, default=MIN_SAMPLES)
    cam.set_defaults(fn=cmd_camera)

    pm = sub.add_parser("presentmon", help="present modes per process from a PresentMon CSV")
    pm.add_argument("csv")
    pm.set_defaults(fn=cmd_presentmon)

    sim = sub.add_parser("simulate", help="fake rig on a pseudo-terminal (POSIX)")
    sim.add_argument("--gap-scale", type=float, default=1.0, help="1 = real-time gaps between samples")
    sim.add_argument("--no-host", action="store_true", help="simulate a rig without the host sensor")
    sim.set_defaults(fn=cmd_simulate)

    args = p.parse_args(argv)
    try:
        return args.fn(args)
    except RigError as e:
        print(f"error: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())

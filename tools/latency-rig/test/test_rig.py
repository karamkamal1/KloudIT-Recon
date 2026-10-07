"""Tests for tools/latency-rig/rig.py (standard library unittest).

  python3 -m unittest discover -s tools/latency-rig/test -v
"""

import contextlib
import csv
import io
import json
import math
import os
import random
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
import rig  # noqa: E402


def write(path, text):
    with open(path, "w", encoding="utf-8") as f:
        f.write(text)
    return path


class StatsTest(unittest.TestCase):
    def test_percentile_linear(self):
        v = list(range(1, 101))
        self.assertAlmostEqual(rig.percentile(v, 95), 95.05)
        self.assertAlmostEqual(rig.percentile(v, 50), 50.5)
        self.assertEqual(rig.percentile([7], 95), 7)
        self.assertEqual(rig.percentile([1, 2], 0), 1)
        self.assertEqual(rig.percentile([1, 2], 100), 2)

    def test_signed_delta_wraps(self):
        self.assertEqual(rig.signed_delta(rig.WRAP - 1000, 500), 1500)
        self.assertEqual(rig.signed_delta(500, rig.WRAP - 1000), -1500)
        self.assertEqual(rig.signed_delta(10, 25), 15)

    def test_stats_ms(self):
        s = rig.stats_ms([10000, 20000, 30000])
        self.assertEqual((s["n"], s["median"], s["min"], s["max"]), (3, 20.0, 10.0, 30.0))
        self.assertIsNone(rig.stats_ms([]))


class ParseTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.d = self.tmp.name

    def tearDown(self):
        self.tmp.cleanup()

    def test_firmware_output(self):
        p = write(os.path.join(self.d, "recon-lan-20261007-101500.csv"), "\n".join([
            "# recon-latency-rig v1 board=atmega32u4 hid=avr adc_bits=10",
            "# cal client black=40 white=900",
            "id,click_us,client_us,host_us",
            f"1,{rig.WRAP - 10000},20000,5000",        # wraps: 30 ms / 15 ms
            "2,1000000,1031000,timeout",
            "3,2000000,timeout,2012000",
            "# run complete samples=3",
            "id,click_us,client_us,host_us",          # header repeated by the next command
            "4,3000000,2990000,3010000",             # light before the click: invalid
            "garbage,line",                          # serial noise must not become a header
            "5,4000000,4040000,4012000",
            "6,not-a-number,1,2",
        ]) + "\n")
        cap = rig.read_capture(p)
        self.assertEqual(cap.label, "recon-lan")  # time stamp stripped from the file name
        self.assertEqual(len(cap.rows), 5)
        self.assertEqual(cap.bad_lines, 2)
        m = rig.metrics_for(cap.rows)
        self.assertEqual(m["click_to_client"].values, [30000, 31000, 40000])
        self.assertEqual(m["click_to_client"].timeouts, 1)
        self.assertEqual(m["click_to_client"].invalid, 1)
        self.assertEqual(m["click_to_host"].values, [15000, 12000, 10000, 12000])
        self.assertEqual(m["click_to_host"].timeouts, 1)
        self.assertEqual(m["host_to_client"].values, [15000, -20000, 28000])

    def test_label_meta_and_disabled_host(self):
        p = write(os.path.join(self.d, "x.csv"), "# label=moonlight-lan\n1,100,30100,\n2,200,32200,\n")
        cap = rig.read_capture(p)
        self.assertEqual(cap.label, "moonlight-lan")
        s = rig.summarize([cap], min_samples=2)
        c = s["configs"][0]
        self.assertEqual(list(c["metrics"]), ["click_to_client"])  # no host sensor: no host metrics
        self.assertTrue(c["enough_samples"])
        self.assertEqual(c["metrics"]["click_to_client"]["median"], 31.0)

    def test_camera_frames(self):
        p = write(os.path.join(self.d, "cam.csv"), "# fps=240\nid,click_frame,client_frame\n1,100,110\n2,300,309\n")
        cap = rig.read_capture(p)
        vals = rig.metrics_for(cap.rows)["click_to_client"].values
        self.assertAlmostEqual(vals[0], 10 * 1e6 / 240)
        self.assertAlmostEqual(vals[1], 9 * 1e6 / 240)
        p2 = write(os.path.join(self.d, "cam2.csv"), "id,click_frame,client_frame\n1,100,110\n")
        with self.assertRaises(ValueError):
            rig.read_capture(p2)
        self.assertAlmostEqual(rig.metrics_for(rig.read_capture(p2, fps=120).rows)["click_to_client"].values[0],
                               10 * 1e6 / 120)


class AnalyzeTest(unittest.TestCase):
    def test_selftest(self):
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(rig.run_selftest(verbose=False), [])

    def test_cli_analyze_writes_summaries(self):
        with tempfile.TemporaryDirectory() as d:
            rig.write_synth(d, seed=3)
            files = sorted(os.path.join(d, f) for f in os.listdir(d))
            js, cs = os.path.join(d, "s.json"), os.path.join(d, "s.csv")
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                code = rig.main(["analyze", *files, "--baseline", "synthetic-moonlight", "--json", js, "--csv", cs,
                                 "--strict"])
            self.assertEqual(code, 2)  # synthetic-short has 150 < 200 samples
            self.assertIn("only 150 click->client samples", out.getvalue())
            with open(js, encoding="utf-8") as f:
                summary = json.load(f)
            labels = [c["label"] for c in summary["configs"]]
            self.assertEqual(labels, ["synthetic-camera", "synthetic-moonlight", "synthetic-recon", "synthetic-short"])
            recon = summary["configs"][2]
            self.assertEqual(len(recon["files"]), 2)  # A/B blocks merged by label
            self.assertTrue(recon["enough_samples"])
            with open(cs, newline="", encoding="utf-8") as f:
                rows = list(csv.DictReader(f))
            r = next(r for r in rows if r["label"] == "synthetic-recon" and r["metric"] == "click_to_client")
            self.assertAlmostEqual(float(r["median_ms"]), recon["metrics"]["click_to_client"]["median"], delta=0.001)
            self.assertTrue(r["delta_median_ms"])

    def test_unknown_baseline(self):
        with tempfile.TemporaryDirectory() as d:
            p = write(os.path.join(d, "a.csv"), "1,0,1000,\n")
            with self.assertRaises(SystemExit):
                rig.analyze_files([p], baseline="nope")


class PresentMonTest(unittest.TestCase):
    def test_modes(self):
        with tempfile.TemporaryDirectory() as d:
            rows = ["Application,ProcessID,SwapChainAddress,PresentRuntime,SyncInterval,PresentFlags,AllowsTearing,"
                    "PresentMode,MsBetweenPresents,MsUntilDisplayed"]
            for i in range(90):
                rows.append(f"chrome.exe,4242,0x1,DXGI,0,0,1,Hardware: Independent Flip,8.33,{4 + i % 3}")
            for i in range(10):
                rows.append("chrome.exe,4242,0x1,DXGI,0,0,0,Composed: Flip,8.33,NA")
            rows.append("dwm.exe,100,0x2,DXGI,1,0,0,Composed: Flip,16.6,20")
            p = write(os.path.join(d, "pm.csv"), "\n".join(rows) + "\n")
            groups = rig.presentmon_summary(p)
            self.assertEqual(groups[0]["application"], "chrome.exe")
            self.assertAlmostEqual(groups[0]["independent_flip_share"], 0.9)
            self.assertEqual(groups[0]["tearing"], 90)
            self.assertEqual(groups[0]["metrics"]["MsUntilDisplayed"]["n"], 90)
            self.assertEqual(groups[0]["metrics"]["MsUntilDisplayed"]["median"], 5)
            self.assertEqual(groups[1]["independent_flip_share"], 0)


@unittest.skipUnless(shutil.which("ffmpeg"), "needs ffmpeg")
class CameraTest(unittest.TestCase):
    """Renders a 240 fps clip (marker LED, host and client screen regions), runs the
    FFmpeg commands from docs/LATENCY_RIG.md and checks rig.py camera against the truth."""

    FPS, N = 240, 30

    def test_video_pipeline(self):
        rnd = random.Random(5)
        events, t = [], 0.2
        for _ in range(self.N):
            t += rnd.uniform(0.3, 0.6)
            host = t + rnd.uniform(0.006, 0.023)
            client = host + rnd.uniform(0.020, 0.032)
            release = client + 0.05
            events.append((t, host, client, release))
        frames = math.ceil((events[-1][3] + 0.3) * self.FPS)

        def lit(i, start, end):
            return start <= i / self.FPS < end

        raw = bytearray()
        for i in range(frames):
            led = any(lit(i, e[0], e[3]) for e in events)
            host = any(lit(i, e[1], e[3] + 0.010) for e in events)
            client = any(lit(i, e[2], e[3] + 0.030) for e in events)
            row = bytes([255 if led else 0] * 16 + [255 if host else 0] * 16 + [255 if client else 0] * 16)
            raw += row * 16
        with tempfile.TemporaryDirectory() as d:
            clip = os.path.join(d, "clip.mp4")
            subprocess.run(["ffmpeg", "-v", "error", "-f", "rawvideo", "-pix_fmt", "gray", "-s", "48x16", "-r",
                            str(self.FPS), "-i", "-", "-c:v", "libx264", "-preset", "ultrafast", "-qp", "0",
                            "-pix_fmt", "yuv420p", clip], input=bytes(raw), check=True)
            cap = self.camera(d, clip, "camera-test")
            # A slow-motion export plays every frame 8x slower (30 fps): its timestamps are
            # stretched, so the times must come from the frame numbers (--fps).
            slow = os.path.join(d, "slow.mp4")
            subprocess.run(["ffmpeg", "-v", "error", "-i", clip, "-vf", "setpts=8*PTS", "-r", "30", "-c:v", "libx264",
                            "-preset", "ultrafast", "-qp", "0", slow], check=True)
            slow_cap = self.camera(d, slow, "camera-slow", "--fps", str(self.FPS))
        self.assertEqual(cap.label, "camera-test")
        # Each edge lands on the first frame that starts at or after the event.
        def q(x):
            return math.ceil(x * self.FPS - 1e-9)
        want_client = [round((q(e[2]) - q(e[0])) * 1e6 / self.FPS) for e in events]
        want_host = [round((q(e[1]) - q(e[0])) * 1e6 / self.FPS) for e in events]
        for c in (cap, slow_cap):
            m = rig.metrics_for(c.rows)
            self.assertEqual(len(m["click_to_client"].values), self.N, c.label)
            # The clip runs ~14 s, past the point where FFmpeg's 6-digit pts_time loses
            # precision; rig.py rebuilds times from pts. 1 us rounding per timestamp.
            for got, want in zip(m["click_to_client"].values, want_client):
                self.assertLessEqual(abs(got - want), 2, c.label)
            for got, want in zip(m["click_to_host"].values, want_host):
                self.assertLessEqual(abs(got - want), 2, c.label)

    def camera(self, d, clip, name, *extra):
        """Logs the brightness of each region with the FFmpeg command from the doc and runs
        rig.py camera on the logs; returns the capture it wrote."""
        logs = {}
        for region, x in (("led", 0), ("host", 16), ("client", 32)):
            logs[region] = os.path.join(d, f"{name}-{region}.txt")
            vf = (f"crop=16:16:{x}:0,signalstats,"
                  f"metadata=mode=print:key=lavfi.signalstats.YAVG:file={logs[region]}")
            subprocess.run(["ffmpeg", "-v", "error", "-i", clip, "-vf", vf, "-an", "-f", "null", "-"], check=True)
        out = os.path.join(d, name + ".csv")
        with contextlib.redirect_stdout(io.StringIO()):
            code = rig.main(["camera", "--click", logs["led"], "--client", logs["client"], "--host", logs["host"],
                             "--out", out, "--min-samples", str(self.N), *extra])
        self.assertEqual(code, 0)
        return rig.read_capture(out)

    def test_edges_and_pairing(self):
        series = [(i / 10, v) for i, v in enumerate([200, 10, 10, 200, 210, 15, 12, 205, 20, 18])]
        self.assertEqual(rig.rising_edges(series), [0.3, 0.7])  # starts light: no edge at 0
        rows = rig.camera_rows([1.0, 2.0, 3.0], [1.04, 2.05], None)
        self.assertEqual(rows, ["1,1000000,1040000,", "2,2000000,2050000,", "3,3000000,timeout,"])
        with self.assertRaises(ValueError):
            rig.rising_edges([(0, 5), (1, 6)])


class Win32LayoutTest(unittest.TestCase):
    def test_struct_sizes(self):
        # Must match the Win32 DCB (28 bytes) and COMMTIMEOUTS (20 bytes).
        import ctypes

        self.assertEqual(ctypes.sizeof(rig._DCB), 28)
        self.assertEqual(ctypes.sizeof(rig._COMMTIMEOUTS), 20)
        self.assertEqual(rig._Win32Serial.FBITS & 0x30, 0x10)    # DTR_CONTROL_ENABLE
        self.assertEqual(rig._Win32Serial.FBITS & 0x3000, 0x1000)  # RTS_CONTROL_ENABLE


@unittest.skipUnless(os.name == "posix", "needs a pseudo-terminal")
class MeasureOverPtyTest(unittest.TestCase):
    def run_measure(self, n, extra=(), **fake):
        path, fake_rig, thread = rig.open_fake_pty(**fake)
        self.addCleanup(fake_rig.stop.set)
        d = tempfile.mkdtemp()
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = rig.main(["measure", "--port", path, "--label", "pty-test", "--samples", str(n), "--out", d,
                             "--min-samples", str(n), "--gap-min", "0", "--gap-max", "0", "--timeout-ms", "800",
                             *extra])
        self.assertEqual(code, 0, out.getvalue())
        files = os.listdir(d)
        self.assertEqual(len(files), 1)
        return os.path.join(d, files[0]), fake_rig, out.getvalue()

    def test_measure_with_host_sensor(self):
        capture, fake, out = self.run_measure(60, extra=["--host-sensor"], stale_on_stop=4)
        self.assertEqual(fake.commands, ["x", "i", "host on", "set timeout 800", "cal", "r 60 0 0"])
        cap = rig.read_capture(capture)
        self.assertEqual(cap.label, "pty-test")
        self.assertEqual(len(cap.rows), 60)  # the 4 samples of the interrupted button run are not recorded
        self.assertEqual([r.id for r in cap.rows], list(range(5, 65)))
        m = rig.metrics_for(cap.rows)
        self.assertEqual(m["click_to_client"].timeouts, 1)  # the fake times out every 50th sample
        self.assertEqual(len(m["click_to_client"].values), 59)
        self.assertEqual(len(m["click_to_host"].values), 60)
        self.assertTrue(all(v > 0 for v in m["click_to_client"].values))  # micros() wrapped mid-run
        self.assertIn("click->client", out)
        self.assertIn("60/60", out)

    def test_measure_without_host_sensor(self):
        # Sensor fitted but not requested: the rig must not report host timings.
        capture, fake, out = self.run_measure(20)
        self.assertIn("host off", fake.commands)
        s = rig.analyze_files([capture], min_samples=19)
        self.assertEqual(list(s["configs"][0]["metrics"]), ["click_to_client"])
        self.assertIn("skipped (sensor off)", out)

    def test_host_sensor_requested_but_missing(self):
        capture, _, out = self.run_measure(20, extra=["--host-sensor"], host_sensor=False)
        s = rig.analyze_files([capture], min_samples=19)
        self.assertEqual(list(s["configs"][0]["metrics"]), ["click_to_client"])
        self.assertIn("host sensor disabled", out)

    def test_rejected_command(self):
        path, fake_rig, _ = rig.open_fake_pty()
        self.addCleanup(fake_rig.stop.set)
        port = rig.SerialPort(path)
        self.addCleanup(port.close)
        r = rig.Rig(port)
        r.drain(quiet=0.2)
        with self.assertRaises(rig.RigError):
            r.command("bogus", idle_timeout=2)
        self.assertEqual(r.command("i", idle_timeout=2)[0][:20], "# recon-latency-rig ")


if __name__ == "__main__":
    unittest.main()

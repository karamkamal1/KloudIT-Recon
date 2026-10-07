# Click-to-photon latency rig

Hardware and scripts to measure click-to-photon latency of Recon against native
Moonlight + Sunshine. The full guide (build, wiring, flashing, calibration and how to
measure) is [docs/LATENCY_RIG.md](../../docs/LATENCY_RIG.md).

| File | What it is |
|---|---|
| `firmware/latency_rig/latency_rig.ino` | Firmware for an Arduino Leonardo / Pro Micro (ATmega32U4) or an RP2040 board: a USB mouse that clicks and times one or two light sensors |
| `flash.html` | Host test page: black, white while a mouse button or key is held |
| `rig.py` | Python 3 (standard library only): drives the rig, analyzes captures, camera fallback, PresentMon summary |
| `test/test_rig.py` | Tests for `rig.py`: `python3 -m unittest discover -s tools/latency-rig/test` |
| `test/flash_smoke.mjs` | Headless Chromium test for `flash.html`: `node tools/latency-rig/test/flash_smoke.mjs` |

Try the analysis without hardware: `python3 tools/latency-rig/rig.py selftest`, or
`rig.py simulate` in one terminal and `rig.py measure --port <pty it prints> --label dry-run`
in another (Linux / macOS).

# Vendor notes

## 0.3 Physical click-to-photon rig

Rig, host page and analysis: `tools/latency-rig/`. How to build, calibrate and measure:
[LATENCY_RIG.md](LATENCY_RIG.md). Acceptance: ≥ 200 samples per configuration, median and
p95, and a native Moonlight + Sunshine baseline on the same hardware.

Hardware acceptance (none of it could run in the build sandbox: no GPU, no Windows, no
microcontroller):

- AMD RDNA3 (RX 7900 XT): unverified. Test: build the rig with two sensors (client screen on A0, host monitor on A1) and plug it into a 120 Hz Windows client on wired LAN. Open `tools/latency-rig/flash.html` fullscreen on the host. With identical settings in both (HEVC, 1920×1080, 120 fps, same bitrate, fullscreen; Moonlight V-Sync and frame pacing off), alternate 100-sample blocks of `python3 tools/latency-rig/rig.py measure --port COMx --host-sensor --label moonlight-hevc-1080p120-lan-amd --samples 100` and `... --label recon-hevc-1080p120-lan-chrome-amd ...` (Sunshine stream stopped while Recon runs and vice versa) until each label has ≥ 200 samples. Then run `rig.py analyze results/*.csv --baseline moonlight-hevc-1080p120-lan-amd --strict --json results/summary-amd.json` and paste the table here. Pass: exit code 0 (≥ 200 click→client samples each), timeouts 0 or explained, and Recon's click→client median within ~5–10 ms of Moonlight's (acceptance T2). Repeat for the wifi, wan and capdrop profiles once step 0.4 exists.
- NVIDIA: unverified (no NVIDIA host available). Test: the same procedure on an RTX 20/30/40/50 host (HEVC; also AV1 on RTX 40+), labels ending in `-nvidia`, baseline `moonlight-hevc-1080p120-lan-nvidia`. Same pass criteria.
- Rig firmware on an ATmega32U4 (Leonardo / Pro Micro): unverified (compiled only). Test: flash it, open a serial monitor at 115200 baud and send `i`: it must print `board=atmega32u4 hid=avr`. Send `mon` while covering/uncovering the sensor: the first `# lvl` value follows the light. From a CR+LF terminal (Arduino Serial Monitor *Both NL & CR*, or `python -m serial.tools.miniterm`), `r 5` must take 5 samples and `mon` must keep printing; neither may stop at once with `# stopped`. On the flash page, `cal` prints black/white with no `# err`, and `c` prints `id,click_us,client_us,` with a plausible client_us − click_us. `i` then prints `us per loop` (expect tens of µs with two sensors). The client must enumerate a HID mouse plus a COM port, and USB Device Tree Viewer must show the HID interrupt IN endpoint with bInterval 1 ms.
- Rig firmware on an RP2040 (both USB stacks: Pico SDK and Adafruit TinyUSB): unverified (compiled only). Test: the same checks as for the ATmega32U4 (`board=rp2040 hid=pico-sdk` / `hid=adafruit-tinyusb`). USB Device Tree Viewer must show bInterval 1 ms on the HID endpoint for both stacks. The core default is 10 ms, and this sketch overrides it.
- Sensors (BPW34 + 47 kΩ + 1 nF, TEMT6000 breakout): unverified. Test: `mon` on the flash page at 100 % monitor brightness: black near 0 and white ≥ 20 % of full scale (or saturated). `cal` must print no `noisy` or `flickers` warning. If it does, shield the sensor or check for a PWM backlight.
- Rig vs 240 fps camera cross-check: unverified. Test: during a 50-sample `r 50` run, film the rig's LED and the client screen at 240 fps. Run the FFmpeg signalstats commands from LATENCY_RIG.md on the original file, then `rig.py camera --click led.txt --client client.txt --out cam.csv`, and `rig.py analyze cam.csv <rig capture of the same run> --min-samples 50`. The two click→client medians must agree within one camera frame (4.2 ms). Repeat with the phone's slow-motion export of the same clip and `rig.py camera ... --fps 240`: the median must match the original file's.
- `rig.py` serial on Windows (built-in ctypes path without pyserial, and the pyserial path) and on macOS: unverified. Test: `python rig.py measure --port COM5 --label smoke --samples 5 --min-samples 5` on a Windows client without pyserial, then with `pip install pyserial`, and with `/dev/cu.usbmodem*` on macOS. Each run must print the rig info and calibration and record 5 samples. A Leonardo must not reset into its bootloader when the port opens.
- flash.html on the real host (Chrome/Edge on Windows 11): unverified. Test: open it fullscreen. The instructions must show `canvas-desynchronized`, and holding any key or mouse button (locally, through Moonlight and through Recon) must turn the whole host monitor white until release.
- PresentMon on the client browser GPU process: unverified. Test: while streaming fullscreen, run `PresentMon-2.x-x64.exe --process_name chrome.exe --output_file recon-chrome.csv --timed 30 --terminate_after_timed` (and `--process_name Moonlight.exe`), then `rig.py presentmon <csv>`. Record the PresentMode shares per renderer (2D desynchronized, WebGPU) next to the click-to-photon numbers. Expect Independent Flip for Moonlight fullscreen. For Recon, "Composed: Flip" means about one refresh of extra latency (find the cause per LATENCY_RIG.md).

Verified in the sandbox:

- `python3 -m unittest discover -s tools/latency-rig/test` (17 tests, Python 3.13) covers:
  capture parsing (comments, repeated headers, `timeout` and empty fields, `micros()` wrap at
  2^32, light-before-click rejected); percentiles cross-checked against
  `statistics.quantiles(method="inclusive")`; JSON/CSV summaries and baseline deltas; `--strict`
  exit code 2 for a 150-sample configuration; the PresentMon summary; and the Win32
  DCB/COMMTIMEOUTS layouts (28/20 bytes). It also runs `rig.py measure` end to end against a
  fake rig that speaks the firmware protocol over a pseudo-terminal: calibration, host sensor
  on/off/missing, samples of an interrupted button-started run excluded, and the clock
  wrapping mid-run. Finally it runs the camera fallback end to end: a synthetic 240 fps clip
  encoded with FFmpeg, brightness logged with the exact FFmpeg commands from the doc, and edges
  matched to the frame-quantized truth within 2 µs. The clip is longer than 10 s, which exposed
  and now covers FFmpeg's 6-digit `pts_time` (times are rebuilt from `pts`). The same clip
  exported 8× slowed down (`setpts=8*PTS`, 30 fps, like a phone's slow-motion export) gives
  the same numbers with `camera --fps 240` (without it every latency came out 8× too long).
  CI and `make test` run these tests (Python 3.11, 3.12 and 3.13 all pass); CI and `make e2e`
  run `flash_smoke.mjs`.
- `rig.py selftest`: synthetic captures (A/B blocks, timeouts, wrap) give exactly the
  generator's median, p95, min and max.
- `rig.py simulate` and `rig.py measure` run as separate processes collected 210 samples and
  printed the summary.
- `node tools/latency-rig/test/flash_smoke.mjs` (headless Chromium via Playwright 1.56, run
  both with `PLAYWRIGHT_MODULE` and through the default `test/e2e/node_modules` lookup) passes
  26 checks. It runs both page modes; `getContextAttributes().desynchronized` is true in
  Chromium. Screenshot pixels are black at rest, white while a mouse button or key is held
  (chorded buttons, key auto-repeat), and black after the last release and after a fast click.
  The counters match, there are no page errors and no external resources. The test caught a
  real bug: an author `display:flex` overrode the `hidden` attribute, so the instructions
  overlay never went away. Fixed.
- Firmware compiles with arduino-cli 1.5.2-rc.1 and `--warnings all` (no warnings from the
  sketch) for: `arduino:avr:leonardo` and `arduino:avr:micro` (AVR core 1.8.8 + Mouse 1.0.1:
  16.5 KB flash, 585 B RAM); `rp2040:rp2040:rpipico` with the Pico SDK stack and with
  `usbstack=tinyusb`; and `rp2040:rp2040:rpipico2` (core 6.2.0). ELF checks: the Pico SDK build
  holds `usb_hid_poll_interval = 1` (the core's weak default of 10 ms is overridden), and
  `-DHOST_SENSOR=1` via `compiler.cpp.extra_flags` takes effect.
- Firmware line input with LF, CR and CR+LF endings: the sketch's own `LineReader` and
  `inputPending()`, compiled on the host against a fake `Stream`. All three endings start
  `r 200`, `x` or an empty line still stops it, and a `\n` that arrives in a later USB packet
  than its `\r` is dropped too. This caught a real bug: the reader returned a CR+LF line at
  the `\r` and the queued `\n` stopped the run at once (`# stopped`; likewise `cal` and
  `mon`). Fixed.

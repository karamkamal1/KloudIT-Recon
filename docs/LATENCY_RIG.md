# Click-to-photon latency rig

This rig measures what a player feels: the time from a mouse click on the client to the
moment the result of that click lights up the client's screen. It measures Recon and native
Moonlight + Sunshine the same way, on the same PC, client and network, so the two can be
compared directly. It implements step 0.3 of the upgrade guide. Each configuration needs at
least 200 samples, reported as median and p95.

Files are in [`tools/latency-rig/`](../tools/latency-rig/): the firmware, the host test page
`flash.html`, and `rig.py` (Python 3, standard library only).

## What it measures

```
 rig (USB mouse on the CLIENT)
   click_us ──USB──> client OS ──> Recon tab / Moonlight ──network──> host injects the click
                                                                       │
                                            flash.html turns white <───┘
                                                       │
   host_us   <── sensor A1 on the host monitor ────────┤ (host display)
                                                       │ capture → encode → network
                                                       │ → decode → draw → client display
   client_us <── sensor A0 on the client screen ───────┘
```

All three times come from one microcontroller clock (`micros()`), so no clock sync is needed.
`rig.py` reports three metrics per configuration:

| Metric | Meaning |
|---|---|
| **click→client** | Click-to-photon, the headline number (acceptance T2: Recon's median within ~5–10 ms of Moonlight on a 120 Hz client over LAN). |
| click→host | Input path (client OS, browser or Moonlight, network, injection on the host) plus the flash page and the host monitor. Needs the optional second sensor. |
| host→client | Everything after the host shows the frame: capture, encode, network, decode, client presentation. Compare it only between runs that use the same host monitor, because that monitor's own delay is subtracted. |

## Parts

| Part | Notes |
|---|---|
| Arduino Leonardo, SparkFun Pro Micro 5 V/16 MHz (ATmega32U4), **or** Raspberry Pi Pico / any RP2040 board | The board must act as a USB HID device. Uno and Nano boards cannot do this. |
| Light sensor ×1 or ×2: **BPW34** photodiode + 47 kΩ resistor + 1 nF capacitor, **or** a **TEMT6000** breakout | Use one sensor for the client screen. Add a second for the host monitor (optional). |
| Momentary push button | Optional. Starts samples without a PC command. |
| LED + 1 kΩ | Only needed on a Pro Micro or Pico W, which have no usable built-in LED. Use it as the click marker for the camera method. |
| Shielded 2-core cable, black foam/tape, rubber band or suction holder | Use these to hold each sensor flat on its screen and keep ambient light out. |

## Wiring

| Signal | Leonardo / Pro Micro | RP2040 (Pico pin numbers) |
|---|---|---|
| Sensor supply | 5V | **3V3(OUT), pin 36**. Never 5 V: the RP2040 ADC tolerates 3.3 V at most. |
| Sensor ground | GND | AGND (pin 33) or GND |
| Client sensor | A0 | GP26 / ADC0 (pin 31) |
| Host sensor (optional) | A1 | GP27 / ADC1 (pin 32) |
| Button (to GND, internal pull-up) | D2 | GP2 (pin 4) |
| Click marker LED | D13 (built in on Leonardo) | GP25 (built in on Pico; on Pico W use an external LED and `-DPIN_MARK=<gpio>`) |
| UART log (optional) | TX pin 1 / RX pin 0 | GP0 TX (pin 1) / GP1 RX (pin 2) |

**BPW34 photodiode (recommended).** Wire it in reverse bias, which is fast and linear:

```
 VCC ───── BPW34 cathode (marked lead)
           BPW34 anode ──┬────────── A0 (or A1)
                         ├── 47 kΩ ── GND     load resistor: 22 kΩ – 220 kΩ
                         └── 1 nF ─── GND     ATmega32U4: recommended; RP2040: optional
```

The voltage across the load resistor is proportional to the light. With the sensor flat on a
white screen the photocurrent is roughly in the low tens of µA. With 47 kΩ that gives a few
hundred mV to about a volt. Check the levels with `mon` (see below), then adjust: use a
larger resistor if white reads below ~20 % of full scale, and a smaller one if black is not
near zero. Saturation on white does no harm to timing, but it hides PWM flicker. The 1 nF
capacitor gives the ATmega's ADC the low source impedance it is specified for (≤ 10 kΩ). Its
time constant with 47 kΩ is about 50 µs, far below the millisecond scale being measured, and
it is the same in every configuration.

**TEMT6000 breakout.** Connect VCC, GND and SIG to A0. The board already has a 10 kΩ resistor,
and its output rises with light. On a bright white screen it usually saturates, which is fine
for timing. If black does not read near zero, replace the resistor with 2.2–4.7 kΩ. A
phototransistor is slower (tens of µs) and less linear than the photodiode, but still far
faster than any display.

Either sensor can be wired "upside down" (output falls with light). Calibration detects this
and inverts it.

**Placement.** Displays scan out top to bottom, so a sensor near the bottom sees a new frame
up to one refresh later than a sensor near the top. Put both sensors at the **same relative
position** on their screens (centre is a good default), and keep it identical for every
configuration you compare. Press each sensor flat against the panel and shield it from room
light with black foam or tape. Keep the client's mouse pointer and any overlay away from the
sensor.

## Build and flash

The firmware is one sketch for both families: `tools/latency-rig/firmware/latency_rig/`.

**Arduino IDE.** Install the board package (*Arduino AVR Boards* for Leonardo / Pro Micro, or
*Raspberry Pi Pico/RP2040/RP2350* by Earle Philhower using the board manager URL below), and
the **Mouse** library (Library Manager, by Arduino). This library is needed for AVR only. Open
`latency_rig.ino`, pick the board and upload.
For a 5 V/16 MHz Pro Micro, choose *Arduino Leonardo*. On RP2040, **Tools → USB Stack** can be
*Pico SDK* (default) or *Adafruit TinyUSB*. Both work, and the sketch sets a 1 ms HID polling
interval on both. The Pico SDK stack defaults to 10 ms, which would add up to 10 ms of jitter.

**arduino-cli:**

```sh
# ATmega32U4
arduino-cli core install arduino:avr
arduino-cli lib install Mouse
arduino-cli compile --fqbn arduino:avr:leonardo tools/latency-rig/firmware/latency_rig
arduino-cli upload  --fqbn arduino:avr:leonardo -p /dev/ttyACM0 tools/latency-rig/firmware/latency_rig

# RP2040 (add usbstack=tinyusb to the FQBN for the Adafruit stack)
arduino-cli config add board_manager.additional_urls \
  https://github.com/earlephilhower/arduino-pico/releases/download/global/package_rp2040_index.json
arduino-cli core update-index && arduino-cli core install rp2040:rp2040
arduino-cli compile --fqbn rp2040:rp2040:rpipico --output-dir build tools/latency-rig/firmware/latency_rig
# hold BOOTSEL while plugging the Pico in, then copy build/latency_rig.ino.uf2 to the RPI-RP2 drive
```

Compile-time options go through
`--build-property "compiler.cpp.extra_flags=-DHOST_SENSOR=1 -DPIN_MARK=9"`.
`HOST_SENSOR=1` enables the A1 sensor at boot (`rig.py` sets it explicitly anyway).
`PIN_MARK` moves the marker LED.

The sketch compiles warning-free with arduino-cli 1.5, `arduino:avr` 1.8.8 + Mouse 1.0.1
(Leonardo, Micro) and `rp2040:rp2040` 6.2.0 (Pico with both USB stacks, Pico 2).

**Safety.** The board is a mouse that clicks by itself. It clicks only on a command, a button
press, or during `cal`/`run`. Any serial input or a button press stops a run. If a Leonardo ever
needs rescuing, double-tap its reset button to enter the bootloader before uploading.

**First check.** Open a serial monitor (115200 baud; LF, CR or CR+LF line ending) and type
`i`. The rig prints its board, thresholds and settings. Type `mon` and cover/uncover the
sensor: the first number of each `# lvl` line must follow the light.

## Serial protocol

| Command | Action |
|---|---|
| `c` / `click` | One sample. |
| `r [n] [min max]` / `run` | `n` samples (default 200) with a random gap of `min`–`max` ms (default 250–600). |
| `x` / `stop` | Stop a run. Any input stops it. |
| `cal` | Calibrate: press and release three times, measure black and white on each enabled sensor, set thresholds. |
| `mon [ms]` | Print raw levels (avg/min/max per 20 ms) for both inputs. |
| `i` / `info` | Board, USB stack, thresholds, settings, and the measured time per sampling loop. |
| `host on` / `host off` | Enable the host-monitor sensor on A1. It is off at boot because an unconnected analog pin floats and can mimic the client channel. |
| `client on` / `client off` | Enable the client sensor on A0 (`cal` turns it back on). With both sensors off, every click is a marker-only click: a 150 ms press with the LED lit (camera method). |
| `thr <0\|1> <lo> <hi>` | Set thresholds by hand (brightness counts as shown by `info`). |
| `set timeout\|settle\|debounce\|count <v>`, `set gap <min> <max>` | Per-sample timeout (ms, default 1000), dark time required before a click (40 ms), consecutive light samples (3), default run length, default gaps. |

Every command ends with `# done <command>`. Comments start with `#`; warnings are
`# warn …` and errors `# err …`. Each sample is one CSV line:

```
id,click_us,client_us,host_us
17,52345120,52391388,52370500
18,52945120,timeout,52970500
```

Times are `micros()` of a free-running 32-bit counter, which wraps every ~71.6 minutes;
`rig.py` subtracts modulo 2^32. An empty field means the sensor is off. `timeout` means it saw
no light within the timeout (counted and reported, not averaged). The button does a single
sample on a short press. A press of 0.6 s or longer starts or stops a 200-sample run.

The same lines are mirrored to the hardware UART. If the client is a phone or tablet (rig
plugged into it as a mouse), connect a 3.3 V USB-UART adapter to the TX pin and log on a
laptop. Commands are accepted from both ports.

## Calibrate

1. **Host:** open `tools/latency-rig/flash.html` in Chrome or Edge (double-click works, it
   needs no server). Press **Start fullscreen**. The screen is black, white while any mouse
   button or key is held, and the cursor is hidden. The page draws on a `desynchronized` 2D
   canvas, which skips some of the host compositor's queueing. `?mode=css` switches to a
   plain background colour. Use the same mode for every configuration.
2. **Client:** start the stream in fullscreen (Recon in the browser, or Moonlight). The client
   shows the host's black page. Click into the stream once by hand so it has input focus
   (Recon's pointer lock / fullscreen gesture, Moonlight's mouse capture). Then move the
   pointer away from the sensor. The rig never moves the mouse.
3. Fit the sensors (client screen on A0, host monitor on A1) and plug the rig into the client.
4. Run `cal` (with the second sensor fitted, send `host on` first). `rig.py measure` does both
   automatically before every configuration (`--host-sensor` for the second sensor). The rig
   clicks three times, holding ~0.7 s each, and prints per sensor:
   `# cal client black=… white=… inverted=… lo=… hi=… black_peak=… white_dip=…`.
   Thresholds: **light at 50 %** of the black→white step, **dark below 25 %** (hysteresis). Watch
   for these warnings:
   - `black is noisy`: ambient light reaches the sensor. Shield it better.
   - `white flickers below the threshold`: a PWM-dimmed backlight. Set the monitor to 100 %
     brightness (most monitors are flicker-free there).
   - `err cal client: no black/white difference`: the sensor is not over the flash, the stream
     did not receive the click, or the load resistor is far too small.
5. Re-run `cal` whenever you move a sensor or change brightness, HDR or display mode.

How a sample works: the rig waits a random 250–600 ms gap, then waits until every enabled
sensor has read dark for 40 ms (*settle*), then presses the button and polls the sensors in a
tight loop. A sensor counts as light after 3 consecutive readings at or above the threshold
(*debounce*, rejects single spikes). The **time of the first** of those readings is
reported, so the debounce adds no delay. The rig releases the button once every enabled sensor
has seen light, or after the timeout.

## Measure Recon against native Moonlight + Sunshine

Rules for a fair comparison:

- **Same everything except the streamer:** the same host PC, client device and display, network
  path (same switch port or Wi-Fi AP, network profile from step 0.4), resolution, frame rate,
  codec and bitrate, and client display mode (fullscreen). Keep the same host monitor refresh
  and the same Windows settings (HAGS, Game Mode, power plan). Turn VRR off on the client for
  the baseline; measure VRR as its own configuration.
- **One streamer at a time.** Sunshine and Recon can both be installed, but only one may be
  streaming. Both capture and encode, and a second one running skews the first.
- **One label per configuration**, changing one variable at a time, for example
  `moonlight-hevc-1080p120-lan`, `recon-hevc-1080p120-lan-chrome`,
  `recon-hevc-1080p120-lan-chrome-webgpu`.
- **At least 200 samples per configuration.** Interleave blocks (Moonlight 100, Recon 100,
  Moonlight 100, Recon 100) so drift (temperature, background tasks) hits both equally.
  `rig.py analyze` merges files with the same label.
- **Random gaps** (the default) spread the clicks over every phase of the host refresh, the
  encoder's frame clock and the client refresh. Never use a fixed interval.

Steps:

1. Host: Recon agent installed as in [INSTALL.md](INSTALL.md); install the current Sunshine
   release and pair Moonlight on the client.
2. Pick the settings and use them for both. In Moonlight: resolution, FPS, video bitrate,
   video codec, display mode *Fullscreen*, *V-Sync* off and *Frame pacing* off (lowest
   latency). In Recon's stream settings: the same codec, resolution, FPS and bitrate. Note
   them with `--note`.
3. Client: Python 3 (from python.org on Windows; `rig.py` is a single file and can be copied
   alone). Find the rig's port: *Device Manager → Ports* (`COM5`), `/dev/ttyACM0` on Linux (user
   in the `dialout` group), `/dev/cu.usbmodem*` on macOS.
4. **Moonlight block:** start the Moonlight stream (Desktop), bring `flash.html` fullscreen on
   the host through the stream, focus the stream (step 2 of Calibrate), then:

   ```sh
   python3 tools/latency-rig/rig.py measure --port COM5 --host-sensor \
       --label moonlight-hevc-1080p120-lan --samples 100 --note "Moonlight 6.x, 30 Mbps"
   ```

   Leave out `--host-sensor` without the second sensor. `measure` stops a button-started
   run, runs `cal`, collects the samples into `results/<label>-<time>.csv`, shows a running
   median and prints a summary. Ctrl+C stops early and keeps the samples.
5. Quit Moonlight (the stream, so Sunshine goes idle). **Recon block:** open Recon in the
   browser, connect, go fullscreen, check that `flash.html` is still in front, focus the
   stream, and run the same command with `--label recon-hevc-1080p120-lan-chrome`.
6. Repeat 4 and 5 until each label has ≥ 200 samples, then:

   ```sh
   python3 tools/latency-rig/rig.py analyze results/*.csv \
       --baseline moonlight-hevc-1080p120-lan --json results/summary.json --csv results/summary.csv
   ```

   Example output (an excerpt, from synthetic data):

   ```
   configuration                      metric             n  t/o   median      p95      min      max  (ms)
   ------------------------------------------------------------------------------------------------------
   moonlight-hevc-1080p120-lan        click->client    240    0    37.29    46.16    25.53    49.70
                                      click->host      240    0    13.75    21.69     6.04    22.65
                                      host->client     240    0    23.55    27.57    18.43    30.24
   recon-hevc-1080p120-lan-chrome     click->client    238    2    43.36    52.62    31.92    57.29
     vs moonlight-hevc-1080p120-lan   click->client                +6.07    +6.46
   ```

   `--strict` makes the command exit with status 2 if any configuration has fewer than 200
   click→client samples.
7. Record the table in [VENDOR_NOTES.md](VENDOR_NOTES.md) under the vendor (AMD RDNA3 /
   NVIDIA), with the host GPU and driver, client device, browser and display (size, refresh,
   panel type), network profile, codec/resolution/FPS/bitrate, the Recon commit and the
   Sunshine and Moonlight versions.

**Optional local baseline.** Plug the rig into the host itself, put the A0 sensor on the host
monitor and measure `local-host-chrome`. This is the host's own click-to-photon (USB, Windows,
browser, display) with no streaming. It is what click→host should approach, minus the
network and input forwarding.

**Reading the numbers.** The spread between min and max is normally about one host frame
plus one client frame, because the click lands at a random point of both refresh cycles. That
is why the median and p95 need hundreds of samples. A p95 much further above the median than
that points to jitter (network, decoder queue, frame drops). Timeouts are clicks that never
produced light within 1 s (a dropped click or a frozen stream). They are counted, not
averaged, and any non-zero count deserves a look.

## Accuracy and sampling rate

- **Clock:** `micros()`, in 4 µs steps on the ATmega32U4 and 1 µs on the RP2040.
- **Sampling rate:** the rig polls the ADC continuously. The ATmega's ADC is switched to a
  1 MHz clock, ~16 µs per conversion instead of ~112 µs at the Arduino default, which loses
  1–2 bits that a black/white step does not need. After every switch between the two inputs,
  one conversion is thrown away, because the sample-and-hold still carries the other
  channel's voltage. With two sensors that is four conversions per loop: tens of µs on the
  ATmega32U4, a few µs on the RP2040. After a sample, `i` prints the measured
  `us per loop`, which is the timing resolution. At the AVR default ADC clock the loop would
  approach half a millisecond, too coarse for a 4–8 ms frame time.
- **USB:** the click time is stamped when the HID report is queued. The client reads it at its
  next poll, ≤ 1 ms later (0.5 ms on average), exactly like a 1000 Hz gaming mouse. It is the
  same in every configuration.
- **Display:** the threshold sits at 50 % of the transition, so panel response time
  (grey-to-grey) is included the same way for every configuration. Scan-out position matters
  (see Placement). A PWM backlight, VRR, HDR tone mapping and an OLED's brightness limiter can
  change the levels, so re-calibrate after changing any of them.
- **Debounce:** 3 consecutive readings at tens of µs each. This rejects spikes without moving
  the reported time.

## 240 fps phone-camera fallback

Use the camera to cross-check the rig or when no rig is built. Film the click marker and the
client screen (and the host monitor, if it fits in the shot) with a phone's 240 fps slow motion.

- **Marker:** a board with this firmware lights its LED at the click instant, even without
  sensors fitted. Send `client off` (the host sensor is off by default) and then `r 200`.
  Each click is a 150 ms press with the LED lit. Film that. With no microcontroller, film the finger on a real
  mouse and step through the frames by hand. Each read is ±1–2 frames, so use this for spot
  checks of a few dozen samples, not 200.
- **Resolution** is one frame (4.17 ms at 240 fps) per timestamp. Over 200 samples the
  quantization averages out in the median, but p95/min/max stay coarse.
- **Automatic edges:** crop a small region over the LED and over each screen and let FFmpeg log
  their brightness. Then `rig.py camera` finds the rising edges (same 50 %/25 % hysteresis as
  the firmware) and pairs each click with the next screen edge:

  ```sh
  # crop=W:H:X:Y picks the region; repeat for the client screen (and host monitor)
  ffmpeg -i clip.mov -vf "crop=40:40:610:900,signalstats,metadata=mode=print:key=lavfi.signalstats.YAVG:file=led.txt" -an -f null -
  ffmpeg -i clip.mov -vf "crop=80:80:300:400,signalstats,metadata=mode=print:key=lavfi.signalstats.YAVG:file=client.txt" -an -f null -
  python3 tools/latency-rig/rig.py camera --click led.txt --client client.txt [--host host.txt] \
      --out results/recon-hevc-1080p120-lan-camera.csv
  ```

  Use the original 240 fps file. By default the times come from the video's own timestamps
  (right for a variable frame rate). FFmpeg prints `pts_time` with only six significant
  digits; `rig.py` rebuilds exact times from the integer `pts`. A slow-motion *export* (shared
  or saved with the slow-down baked in, e.g. 30 fps playback of 240 fps frames) has stretched
  timestamps, so every latency would come out 8× too long. For such a file pass `--fps 240`:
  the time of each frame is then its frame number / 240. This assumes the file holds every
  captured frame in order, which is not true where the clip plays at normal speed (the ramps
  at the start and end of a slo-mo clip), so keep the clicks inside the slow part. The output
  is a normal capture, so `analyze` reads it with the rig's captures.
- **Counting by hand:** write `id,click_frame,client_frame[,host_frame]` with a `# fps=240`
  line (or pass `--fps`) and `analyze` converts frame numbers to time.

## PresentMon: Composed vs Independent Flip

How the client presents frames can add a whole refresh. Check it with
[PresentMon](https://github.com/GameTechDev/PresentMon) (console build, run as administrator)
on the Windows client while the stream runs fullscreen:

```powershell
# Chrome/Edge present from their GPU process (chrome.exe --type=gpu-process); filtering by name
# captures it, because the other chrome.exe processes do not present.
.\PresentMon-2.x-x64.exe --process_name chrome.exe --output_file recon-chrome.csv --timed 30 --terminate_after_timed
.\PresentMon-2.x-x64.exe --process_name Moonlight.exe --output_file moonlight.csv --timed 30 --terminate_after_timed
python3 tools/latency-rig/rig.py presentmon recon-chrome.csv
```

(For PresentMon 1.x the options take one dash: `-process_name`. Use `msedge.exe` for Edge and
`firefox.exe` for Firefox. The GPU process ID can be found with
`Get-CimInstance Win32_Process -Filter "Name='chrome.exe'" | ? CommandLine -match 'gpu-process'`.)

`rig.py presentmon` prints, per process, the share of each `PresentMode` and the median/p95 of
the latency columns it finds (`MsBetweenPresents`, `MsUntilDisplayed`, `MsBetweenDisplayChange`,
…):

- **Hardware: Independent Flip** / **Hardware Composed: Independent Flip** (multiplane
  overlay): the swap chain is scanned out directly, with no DWM composition. This is what
  native Moonlight usually gets in fullscreen, and what Recon should get.
- **Composed: Flip:** DWM composes the browser's frame with the desktop. This usually costs about
  one refresh. Typical causes: something overlaps the canvas (browser UI, a notification, the
  volume OSD, any overlay), the window is not fullscreen, the canvas is scaled by CSS or is not
  sized to device pixels, or the driver does not offer an overlay plane.

Record the mode next to each click-to-photon configuration, for every renderer (2D
desynchronized canvas, WebGPU, WebGL2: guide step 4.3) and for Moonlight. A difference of about
one refresh between configurations should line up with a difference in present mode.

### Renderers (step 4.3)

Recon's stream settings (*Pipeline → Renderer*) pick the presentation path: *2D canvas*,
*WebGL2*, *WebGPU*, or *Auto* (the default), which measures the three on the live stream on the
first connection in a browser and keeps the one with the lowest draw + display time (Phase 0
stages). Auto's numbers come from inside the browser and cannot see the compositor; the rig and
PresentMon can. To compare the paths, set each renderer in turn (reconnect after each change),
close the performance overlay (it sits on the canvas and forces composition), go fullscreen and
measure one label per renderer, for example `recon-hevc-1080p120-lan-chrome-canvas2d`,
`...-webgl2`, `...-webgpu`, interleaving 100-sample blocks as above. Note for each label the
overlay's *Renderer → context* row (opened briefly before the block): whether the browser
granted `desynchronized` (`getContextAttributes().desynchronized`) and the canvas size, which
must equal the screen in device pixels in fullscreen. Then compare the winner with Auto's pick
(overlay *bake-off* rows, ★): if they differ, set the rig's winner in the settings and record
both in docs/VENDOR_NOTES.md (step 4.3).

## Without hardware

```sh
python3 tools/latency-rig/rig.py selftest          # synthetic captures, checked against ground truth
python3 tools/latency-rig/rig.py synth --out /tmp/rig-demo && python3 tools/latency-rig/rig.py analyze /tmp/rig-demo/*.csv
python3 tools/latency-rig/rig.py simulate           # Linux/macOS: prints a pty path; run "measure --port <path>" against it
python3 -m unittest discover -s tools/latency-rig/test
node tools/latency-rig/test/flash_smoke.mjs         # flash.html in headless Chromium (Playwright from test/e2e)
```

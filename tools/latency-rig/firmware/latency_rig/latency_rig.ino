// KloudIT Recon click-to-photon latency rig (guide step 0.3, docs/LATENCY_RIG.md).
//
// Plugged into the CLIENT as a USB mouse, the rig presses the left button, stamps
// the press with micros() and watches up to two light sensors that look at the
// host test page tools/latency-rig/flash.html (white while a button is held):
//   A0  client screen (required): click -> streamed photon = click-to-photon.
//   A1  host monitor (optional, "host on"): click -> local photon on the host, which
//       splits the total into "input + host" and "capture -> encode -> network -> display".
// Each sample is one CSV line on USB serial (mirrored to the hardware UART):
//
//   id,click_us,client_us,host_us
//
// Fields are micros() readings of one free-running clock (they wrap every ~71.6
// minutes; subtract modulo 2^32). An empty field means that sensor is disabled,
// "timeout" that it saw no light within the timeout. Lines starting with '#'
// are comments; every command ends with "# done <command>".
//
// Boards:
//   - Arduino Leonardo / Pro Micro (ATmega32U4): Arduino AVR core + "Mouse" library.
//     The core polls the HID endpoint every 1 ms.
//   - RP2040 (Raspberry Pi Pico and clones): Arduino-Pico core, either USB stack:
//     "Pico SDK" (default; the core's Mouse library) or "Adafruit TinyUSB". Both
//     are TinyUSB underneath; this sketch sets a 1 ms HID poll interval on both
//     (the Pico SDK stack defaults to 10 ms).
//
// Serial commands (115200 baud, one per line; any input also stops a run):
//   c | click            one sample
//   r | run [n] [min max] n samples (default 200) with a random min..max ms gap
//   x | stop             stop a run
//   cal                  calibrate black/white levels of the enabled sensors (press/release x3)
//   mon [ms]             print sensor levels every 20 ms (default 5000 ms)
//   i | info             board, thresholds and settings
//   host on|off          enable/disable the host-monitor sensor (off at boot unless HOST_SENSOR 1)
//   client on|off        enable/disable the client sensor; with both off the rig is a pure
//                        click marker (150 ms press, LED lit) for the 240 fps camera method
//   thr <0|1> <lo> <hi>  set thresholds by hand (brightness counts, see "info")
//   set timeout|settle|debounce|count <v>, set gap <min> <max>
// Button on PIN_BUTTON: short press = one sample, long press (>= 0.6 s) = start/stop a run.

#include <Arduino.h>
#include <stdarg.h>

// ---- configuration ----------------------------------------------------------
#define RIG_VERSION "1"
#define PIN_CLIENT A0  // client-screen sensor
#define PIN_HOST A1    // host-monitor sensor (optional)
#ifndef HOST_SENSOR
// Off by default: an unconnected analog pin floats and, right after the multiplexer
// switch, reads back the client channel's charge, which would look like a sensor.
#define HOST_SENSOR 0
#endif
#define PIN_BUTTON 2   // momentary button to GND (internal pull-up)
#ifndef PIN_MARK
// Lit while the mouse button is held: the click marker for the 240 fps camera
// method. Pro Micro has no LED on 13 and the Pico W LED sits behind the Wi-Fi
// chip (slow): use an external LED + 1 kOhm on a free pin there.
#define PIN_MARK LED_BUILTIN
#endif
#define LOG_UART 1  // mirror output to Serial1 and accept commands from it
#define UART_BAUD 115200
#define LONG_PRESS_MS 600
#define MARKER_HOLD_MS 150  // press length when no sensor is enabled
#define BUTTON_DEBOUNCE_MS 20

#if defined(ARDUINO_ARCH_RP2040)
#define RIG_BOARD "rp2040"
#define ADC_BITS 12
#elif defined(__AVR_ATmega32U4__)
#define RIG_BOARD "atmega32u4"
#define ADC_BITS 10
#else
#error "Unsupported board: use an ATmega32U4 (Leonardo / Pro Micro) or an RP2040"
#endif
#define ADC_MAX ((1 << ADC_BITS) - 1)
#define MIN_CONTRAST (ADC_MAX / 32)  // black/white difference needed to trust a sensor

// ---- USB HID mouse ----------------------------------------------------------
#if defined(ARDUINO_ARCH_RP2040) && defined(USE_TINYUSB)
#include <Adafruit_TinyUSB.h>
#define HID_STACK "adafruit-tinyusb"
static const uint8_t hidReport[] = {TUD_HID_REPORT_DESC_MOUSE()};
static Adafruit_USBD_HID usbHid;

static void hidBegin() {
  if (!TinyUSBDevice.isInitialized()) TinyUSBDevice.begin(0);
  usbHid.setBootProtocol(HID_ITF_PROTOCOL_MOUSE);
  usbHid.setPollInterval(1);
  usbHid.setReportDescriptor(hidReport, sizeof(hidReport));
  usbHid.setStringDescriptor("Recon latency rig");
  usbHid.begin();
  if (TinyUSBDevice.mounted()) {  // re-enumerate so the host sees the HID interface
    TinyUSBDevice.detach();
    delay(10);
    TinyUSBDevice.attach();
  }
}
// This stack runs the USB device task from yield(), not from an interrupt.
static inline void usbTask() { yield(); }
static void hidButton(bool down) {
  uint32_t start = millis();
  while (!usbHid.ready() && millis() - start < 20) yield();
  if (down) usbHid.mouseButtonPress(0, MOUSE_BUTTON_LEFT);
  else usbHid.mouseButtonRelease(0);
}
#else
#include <Mouse.h>
#if defined(ARDUINO_ARCH_RP2040)
#define HID_STACK "pico-sdk"
int usb_hid_poll_interval = 1;  // overrides the core's weak default of 10 ms
#else
#define HID_STACK "avr"
#endif
// AVR: interrupt driven; Arduino-Pico (Pico SDK stack): a 1 ms timer interrupt runs the task.
static inline void usbTask() {}
static void hidBegin() { Mouse.begin(); }
static void hidButton(bool down) {
  if (down) Mouse.press(MOUSE_LEFT);
  else Mouse.release(MOUSE_LEFT);
}
#endif

// ---- output -----------------------------------------------------------------
#ifdef __AVR__
#define VSNPRINTF vsnprintf_P
#else
#define VSNPRINTF vsnprintf
#endif
// Format strings stay in flash on AVR (2.5 KB of RAM).
#define OUTF(fmt, ...) outf(PSTR(fmt), ##__VA_ARGS__)

static void outf(PGM_P fmt, ...) {
  char buf[112];
  va_list ap;
  va_start(ap, fmt);
  VSNPRINTF(buf, sizeof(buf), fmt, ap);
  va_end(ap);
  Serial.println(buf);  // dropped while no terminal has the port open
#if LOG_UART
  Serial1.println(buf);
#endif
}

// ---- light sensors ----------------------------------------------------------
struct Channel {
  const char *name;
  uint8_t pin;
  bool enabled;
  bool inverted;     // reads lower when brighter (sensor wired to the other rail)
  bool calibrated;
  uint16_t black;    // calibrated levels, raw ADC counts
  uint16_t white;
  uint16_t lo;       // "dark" below this (brightness counts, after inversion)
  uint16_t hi;       // "light" at or above this
};
// Thresholds are placeholders until "cal".
static Channel ch[2] = {
  {"client", PIN_CLIENT, true, false, false, 0, ADC_MAX, ADC_MAX / 4, ADC_MAX / 2},
  {"host", PIN_HOST, HOST_SENSOR != 0, false, false, 0, ADC_MAX, ADC_MAX / 4, ADC_MAX / 2},
};

static struct {
  uint16_t timeoutMs = 1000;  // give up on a sensor this long after the click
  uint16_t settleMs = 40;     // both screens must read dark this long before a click
  uint8_t debounce = 3;       // consecutive samples at/above "hi" that count as light
  uint16_t gapMin = 250;      // random gap between samples of a run (ms); de-correlates
  uint16_t gapMax = 600;      // the click from vsync and the encoder frame clock
  uint16_t count = 200;
} cfg;

static uint32_t sampleId = 0;
static uint32_t lastLoops = 0, lastElapsedUs = 0;
static uint8_t lastPin = 0xff;

static uint16_t readRaw(uint8_t pin) {
  // After the multiplexer switches, the sample-and-hold capacitor still holds the
  // other channel's voltage; with a high-impedance sensor one conversion is not
  // enough to settle, so throw the first one away.
  if (pin != lastPin) {
    (void)analogRead(pin);
    lastPin = pin;
  }
  return analogRead(pin);
}

// Brightness in ADC counts: higher = brighter regardless of wiring.
static inline uint16_t level(uint8_t i) {
  uint16_t v = readRaw(ch[i].pin);
  return ch[i].inverted ? ADC_MAX - v : v;
}

static bool inputPending();
static bool buttonRaw();

// Waits until every enabled sensor has read dark for settleMs without a break.
static bool waitDark(uint16_t settleMs, uint16_t timeoutMs) {
  uint32_t start = millis(), since = start;
  for (;;) {
    uint32_t now = millis();
    for (uint8_t i = 0; i < 2; i++)
      if (ch[i].enabled && level(i) > ch[i].lo) since = now;
    if (now - since >= settleMs) return true;
    if (now - start >= timeoutMs) return false;
    usbTask();
  }
}

static void printField(char *p, size_t n, bool enabled, bool got, uint32_t t) {
  if (!enabled) p[0] = 0;
  else if (!got) strncpy(p, "timeout", n);
  else snprintf(p, n, "%lu", (unsigned long)t);
}

// No sensor enabled: click and light the marker LED only (camera method).
static void markerClick() {
  digitalWrite(PIN_MARK, HIGH);
  hidButton(true);
  const uint32_t t0 = micros();
  uint32_t start = millis();
  while (millis() - start < MARKER_HOLD_MS) usbTask();
  hidButton(false);
  digitalWrite(PIN_MARK, LOW);
  OUTF("%lu,%lu,,", (unsigned long)++sampleId, (unsigned long)t0);
}

// One measurement. Returns false if the screens never went dark (nothing clicked).
static bool sample() {
  if (!ch[0].enabled && !ch[1].enabled) {
    markerClick();
    return true;
  }
  if (!waitDark(cfg.settleMs, 3000)) {
    OUTF("# err screens not dark: release the button, bring the flash page to the front, run cal");
    return false;
  }
  const bool want0 = ch[0].enabled, want1 = ch[1].enabled;
  bool got0 = false, got1 = false;
  uint32_t first0 = 0, first1 = 0;
  uint8_t run0 = 0, run1 = 0;
  const uint32_t limit = (uint32_t)cfg.timeoutMs * 1000UL;
  uint32_t loops = 0;

  digitalWrite(PIN_MARK, HIGH);
  hidButton(true);
  // The report now sits in the IN endpoint; the host collects it at its next poll
  // (<= 1 ms later), exactly like a real 1000 Hz mouse.
  const uint32_t t0 = micros();
  uint32_t now = t0;
  while ((want0 && !got0) || (want1 && !got1)) {
    // The timestamp is taken before the conversion: resolution is one loop (see "info").
    if (want0 && !got0) {
      uint32_t t = micros();
      if (level(0) >= ch[0].hi) {
        if (run0++ == 0) first0 = t;
        got0 = run0 >= cfg.debounce;  // first sample of the run, so debounce adds no bias
      } else {
        run0 = 0;
      }
    }
    if (want1 && !got1) {
      uint32_t t = micros();
      if (level(1) >= ch[1].hi) {
        if (run1++ == 0) first1 = t;
        got1 = run1 >= cfg.debounce;
      } else {
        run1 = 0;
      }
    }
    loops++;
    now = micros();
    if (now - t0 >= limit) break;
    if ((loops & 63) == 0) usbTask();
  }
  hidButton(false);
  digitalWrite(PIN_MARK, LOW);
  lastLoops = loops;
  lastElapsedUs = now - t0;

  char f0[12], f1[12];
  printField(f0, sizeof(f0), want0, got0, first0);
  printField(f1, sizeof(f1), want1, got1, first1);
  OUTF("%lu,%lu,%s,%s", (unsigned long)++sampleId, (unsigned long)t0, f0, f1);
  return true;
}

static void header() { OUTF("id,click_us,client_us,host_us"); }

// Idles for ms; false if a command or a button press arrived.
static bool idleFor(uint32_t ms) {
  uint32_t start = millis();
  while (millis() - start < ms) {
    if (inputPending()) return false;
    usbTask();
  }
  return true;
}

static void doRun(uint32_t n, uint16_t gapMin, uint16_t gapMax) {
  if (gapMax < gapMin) gapMax = gapMin;
  OUTF("# run n=%lu gap_ms=%u..%u timeout_ms=%u", (unsigned long)n, (unsigned)gapMin,
       (unsigned)gapMax, (unsigned)cfg.timeoutMs);
  header();
  uint32_t ok = 0;
  uint8_t fails = 0;
  for (uint32_t k = 0; k < n; k++) {
    if (!idleFor(random(gapMin, (long)gapMax + 1))) {
      OUTF("# stopped");
      if (buttonRaw()) {  // wait for the release so it does not count as a new press
        while (buttonRaw()) usbTask();
        delay(BUTTON_DEBOUNCE_MS);
      }
      break;
    }
    if (sample()) {
      ok++;
      fails = 0;
    } else if (++fails >= 3) {
      OUTF("# err giving up");
      break;
    }
  }
  OUTF("# run complete samples=%lu", (unsigned long)ok);
}

// Averages each sensor over a 200 ms window; also records min/max to spot PWM flicker.
static void window(uint32_t sum[2], uint16_t mn[2], uint16_t mx[2], uint16_t &count) {
  for (count = 0; count < 250; count++) {
    for (uint8_t i = 0; i < 2; i++) {
      uint16_t v = readRaw(ch[i].pin);
      sum[i] += v;
      if (v < mn[i]) mn[i] = v;
      if (v > mx[i]) mx[i] = v;
    }
    delayMicroseconds(800);
    usbTask();
  }
}

static void calibrate() {
  // Press/release three times. While the button is held the flash page is white on
  // both monitors (700 ms covers any sane stream latency), black after release.
  uint32_t sumB[2] = {0, 0}, sumW[2] = {0, 0};
  uint16_t minB[2] = {ADC_MAX, ADC_MAX}, maxB[2] = {0, 0};
  uint16_t minW[2] = {ADC_MAX, ADC_MAX}, maxW[2] = {0, 0};
  uint16_t nB = 0, nW = 0, cnt = 0;
  OUTF("# cal: pressing the button 3 times, keep the flash page in front");
  for (uint8_t r = 0; r < 3; r++) {
    hidButton(false);
    digitalWrite(PIN_MARK, LOW);
    if (!idleFor(700)) break;
    window(sumB, minB, maxB, cnt);
    nB += cnt;
    hidButton(true);
    digitalWrite(PIN_MARK, HIGH);
    if (!idleFor(700)) break;
    window(sumW, minW, maxW, cnt);
    nW += cnt;
  }
  hidButton(false);
  digitalWrite(PIN_MARK, LOW);
  if (nB == 0 || nW == 0 || nB != nW) {
    OUTF("# err cal interrupted");
    return;
  }
  ch[0].enabled = true;  // the client sensor is always calibrated; the host one only when on
  for (uint8_t i = 0; i < 2; i++) {
    Channel &c = ch[i];
    if (!c.enabled) {
      OUTF("# cal %s: skipped (sensor off)", c.name);
      continue;
    }
    uint16_t b = sumB[i] / nB, w = sumW[i] / nW;
    uint16_t contrast = w > b ? w - b : b - w;
    if (contrast < MIN_CONTRAST) {
      c.enabled = false;
      if (i == 0) OUTF("# err cal client: no black/white difference (black=%u white=%u); check sensor position and the flash page", b, w);
      else OUTF("# cal host: no black/white difference (black=%u white=%u): host sensor disabled", b, w);
      continue;
    }
    c.inverted = w < b;
    c.black = b;
    c.white = w;
    c.calibrated = true;
    c.enabled = true;
    // Brightness domain (after inversion): light at 50 % of the step, dark below 25 %.
    uint16_t bl = c.inverted ? ADC_MAX - b : b, wl = c.inverted ? ADC_MAX - w : w;
    c.hi = bl + (wl - bl) / 2;
    c.lo = bl + (wl - bl) / 4;
    uint16_t blackPeak = c.inverted ? ADC_MAX - minB[i] : maxB[i];  // brightest black reading
    uint16_t whiteDip = c.inverted ? ADC_MAX - maxW[i] : minW[i];   // darkest white reading
    OUTF("# cal %s black=%u white=%u inverted=%d lo=%u hi=%u black_peak=%u white_dip=%u", c.name, b, w,
         c.inverted ? 1 : 0, c.lo, c.hi, blackPeak, whiteDip);
    if (blackPeak > c.lo) OUTF("# warn %s: black is noisy (ambient light? shield the sensor)", c.name);
    if (whiteDip < c.hi) OUTF("# warn %s: white flickers below the threshold (PWM backlight? set 100%% brightness)", c.name);
    if (c.inverted ? w <= 2 : w >= ADC_MAX - 2)
      OUTF("# note %s: white saturates the ADC; fine for timing, lower the load resistor for a true level", c.name);
  }
}

static void monitor(uint32_t ms) {
  OUTF("# mon: raw levels avg/min/max per 20 ms (client | host)");
  uint32_t start = millis();
  while (millis() - start < ms) {
    uint32_t sum[2] = {0, 0};
    uint16_t mn[2] = {ADC_MAX, ADC_MAX}, mx[2] = {0, 0}, n = 0;
    uint32_t t = millis();
    while (millis() - t < 20) {
      for (uint8_t i = 0; i < 2; i++) {
        uint16_t v = readRaw(ch[i].pin);
        sum[i] += v;
        if (v < mn[i]) mn[i] = v;
        if (v > mx[i]) mx[i] = v;
      }
      n++;
      usbTask();
    }
    OUTF("# lvl %u %u %u | %u %u %u", (unsigned)(sum[0] / n), mn[0], mx[0], (unsigned)(sum[1] / n), mn[1], mx[1]);
    if (inputPending()) break;
  }
}

static void info() {
  OUTF("# recon-latency-rig v" RIG_VERSION " board=" RIG_BOARD " hid=" HID_STACK " adc_bits=%d", ADC_BITS);
  for (uint8_t i = 0; i < 2; i++) {
    const Channel &c = ch[i];
    OUTF("# ch %u %s pin=%u enabled=%d calibrated=%d inverted=%d black=%u white=%u lo=%u hi=%u", i, c.name, c.pin,
         c.enabled ? 1 : 0, c.calibrated ? 1 : 0, c.inverted ? 1 : 0, c.black, c.white, c.lo, c.hi);
  }
  OUTF("# settings timeout_ms=%u settle_ms=%u debounce=%u gap_ms=%u..%u count=%u", cfg.timeoutMs, cfg.settleMs,
       cfg.debounce, cfg.gapMin, cfg.gapMax, cfg.count);
  if (lastLoops)
    OUTF("# last sample: %lu loops in %lu us = %lu us per loop (timing resolution)", (unsigned long)lastLoops,
         (unsigned long)lastElapsedUs, (unsigned long)(lastElapsedUs / lastLoops));
}

// ---- commands ---------------------------------------------------------------
static bool is(const char *a, PGM_P b) { return strcmp_P(a, b) == 0; }

static void command(char *line) {
  char *argv[5];
  uint8_t argc = 0;
  for (char *tok = strtok(line, " \t,"); tok && argc < 5; tok = strtok(nullptr, " \t,")) argv[argc++] = tok;
  if (argc == 0) return;
  const char *c = argv[0];
  long a1 = argc > 1 ? atol(argv[1]) : -1, a2 = argc > 2 ? atol(argv[2]) : -1, a3 = argc > 3 ? atol(argv[3]) : -1;
  PGM_P done;
  if (is(c, PSTR("c")) || is(c, PSTR("click"))) {
    header();
    sample();
    done = PSTR("click");
  } else if (is(c, PSTR("r")) || is(c, PSTR("run"))) {
    doRun(a1 > 0 ? a1 : cfg.count, a2 >= 0 ? a2 : cfg.gapMin, a3 >= 0 ? a3 : cfg.gapMax);
    done = PSTR("run");
  } else if (is(c, PSTR("x")) || is(c, PSTR("stop"))) {
    done = PSTR("stop");  // a run already stopped when this line arrived
  } else if (is(c, PSTR("cal"))) {
    calibrate();
    done = PSTR("cal");
  } else if (is(c, PSTR("mon"))) {
    monitor(a1 > 0 ? a1 : 5000);
    done = PSTR("mon");
  } else if (is(c, PSTR("i")) || is(c, PSTR("info"))) {
    info();
    done = PSTR("info");
  } else if ((is(c, PSTR("host")) || is(c, PSTR("client"))) && argc > 1) {
    Channel &chn = ch[is(c, PSTR("host")) ? 1 : 0];
    chn.enabled = is(argv[1], PSTR("on"));
    OUTF("# %s sensor %s", chn.name, chn.enabled ? "on" : "off");
    if (!ch[0].enabled && !ch[1].enabled) OUTF("# no sensor enabled: clicks are marker-only (LED, %u ms press)", MARKER_HOLD_MS);
    done = is(c, PSTR("host")) ? PSTR("host") : PSTR("client");
  } else if (is(c, PSTR("thr")) && argc > 3 && (a1 == 0 || a1 == 1) && a2 >= 0 && a3 > a2 && a3 <= ADC_MAX) {
    ch[a1].lo = a2;
    ch[a1].hi = a3;
    OUTF("# thr %s lo=%u hi=%u", ch[a1].name, ch[a1].lo, ch[a1].hi);
    done = PSTR("thr");
  } else if (is(c, PSTR("set")) && argc > 2 && a2 >= 0) {
    const char *k = argv[1];
    if (is(k, PSTR("timeout"))) cfg.timeoutMs = constrain(a2, 50, 10000);
    else if (is(k, PSTR("settle"))) cfg.settleMs = constrain(a2, 0, 2000);
    else if (is(k, PSTR("debounce"))) cfg.debounce = constrain(a2, 1, 50);
    else if (is(k, PSTR("count"))) cfg.count = constrain(a2, 1, 60000);
    else if (is(k, PSTR("gap")) && a3 >= a2) {
      cfg.gapMin = constrain(a2, 0, 60000);
      cfg.gapMax = constrain(a3, cfg.gapMin, 60000);
    } else OUTF("# err unknown setting");
    done = PSTR("set");
  } else if (is(c, PSTR("?")) || is(c, PSTR("help"))) {
    OUTF("# commands: c|click, r|run [n] [min_ms max_ms], x|stop, cal, mon [ms], i|info,");
    OUTF("#   host on|off, client on|off, thr <0|1> <lo> <hi>, set timeout|settle|debounce|count <v>,");
    OUTF("#   set gap <min> <max>");
    done = PSTR("help");
  } else {
    OUTF("# err unknown command (try help)");
    done = PSTR("error");
  }
  char name[8];
  strncpy_P(name, done, sizeof(name));
  name[sizeof(name) - 1] = 0;
  OUTF("# done %s", name);
}

struct LineReader {
  char buf[48];
  uint8_t len = 0;
  bool cr = false;  // last byte was '\r': the '\n' of a CRLF line may still be queued
  // Returns a complete line or nullptr. A line ends at '\r' or '\n' (LF, CR or CRLF).
  char *poll(Stream &s) {
    while (s.available()) {
      int c = s.read();
      cr = c == '\r';
      if (c == '\r' || c == '\n') {
        if (!len) continue;
        buf[len] = 0;
        len = 0;
        return buf;
      }
      if (len < sizeof(buf) - 1) buf[len++] = (char)c;
    }
    return nullptr;
  }
  // A byte is waiting on s. poll() returns a CRLF line at its '\r'; the '\n' behind it is
  // dropped here, or "r 200" from a CRLF terminal would stop its own run at once.
  bool pending(Stream &s) {
    if (cr && s.peek() == '\n') {
      s.read();
      cr = false;
    }
    return s.available() > 0;
  }
};
static LineReader usbLine;
#if LOG_UART
static LineReader uartLine;
#endif

// ---- button -----------------------------------------------------------------
static bool buttonRaw() { return digitalRead(PIN_BUTTON) == LOW; }

static bool inputPending() {
#if LOG_UART
  if (uartLine.pending(Serial1)) return true;
#endif
  return usbLine.pending(Serial) || buttonRaw();
}

static void pollButton() {
  static bool down = false;
  static uint32_t changed = 0, pressedAt = 0;
  bool raw = buttonRaw();
  uint32_t now = millis();
  if (raw == down) {
    changed = now;
    return;
  }
  if (now - changed < BUTTON_DEBOUNCE_MS) return;
  down = raw;
  changed = now;
  if (down) {
    pressedAt = now;
    return;
  }
  // Act on release, so the press that starts a run does not stop it again.
  if (now - pressedAt >= LONG_PRESS_MS) {
    doRun(cfg.count, cfg.gapMin, cfg.gapMax);
    OUTF("# done run");
  } else {
    header();
    sample();
    OUTF("# done click");
  }
}

// ---- setup / loop -----------------------------------------------------------
void setup() {
  pinMode(PIN_BUTTON, INPUT_PULLUP);
  pinMode(PIN_MARK, OUTPUT);
  digitalWrite(PIN_MARK, LOW);
#ifdef __AVR__
  // ADC clock 16 MHz / 16 = 1 MHz: ~16 us per conversion instead of ~112 us at the
  // core's /128. Costs ~1-2 bits of accuracy, irrelevant for a black/white step.
  ADCSRA = (ADCSRA & ~0x07) | 0x04;
#else
  analogReadResolution(ADC_BITS);
#endif
  hidBegin();
  Serial.begin(115200);
#if LOG_UART
  Serial1.begin(UART_BAUD);
#endif
  randomSeed(analogRead(PIN_CLIENT) ^ analogRead(PIN_HOST) ^ micros());
  info();
  header();
}

void loop() {
  usbTask();
  char *line = usbLine.poll(Serial);
#if LOG_UART
  if (!line) line = uartLine.poll(Serial1);
#endif
  if (line) command(line);
  pollButton();
}

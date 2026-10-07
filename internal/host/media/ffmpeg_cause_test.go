package media

import "testing"

func TestCauseLines(t *testing.T) {
	// FFmpeg 8.1 with an NVIDIA driver that is too old (CRLF, as on Windows).
	stderr := "[h264_nvenc @ 0x1] Driver does not support the required nvenc API version. Required: 13.0 Found: 12.2\r\n" +
		"[h264_nvenc @ 0x1] The minimum required Nvidia driver for nvenc is 570.0 or newer\r\n" +
		"[vost#0:0/h264_nvenc @ 0x2] Error while opening encoder - maybe incorrect parameters such as bit_rate, rate, width or height.\r\n" +
		"[vf#0:0 @ 0x3] Error sending frames to consumers: Function not implemented\r\n" +
		"[vf#0:0 @ 0x3] Task finished with error code: -38 (Function not implemented)\r\n" +
		"[out#0/null @ 0x4] Nothing was written into output file, because at least one of its streams received no packets.\r\n"
	want := "[h264_nvenc @ 0x1] Driver does not support the required nvenc API version. Required: 13.0 Found: 12.2 | " +
		"[h264_nvenc @ 0x1] The minimum required Nvidia driver for nvenc is 570.0 or newer"
	if got := causeLines(stderr, 3); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if got := causeLines("one\ntwo\nthree\nfour", 2); got != "one | two" {
		t.Fatalf("got %q", got)
	}
}

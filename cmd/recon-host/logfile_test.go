package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// host.log is rotated while the agent runs: past the limit it becomes
// host.log.old (replacing the previous one) and a new file starts, whole
// records on either side.
func TestLogFileRotates(t *testing.T) {
	defer func(n int64) { logMaxSize = n }(logMaxSize)
	logMaxSize = 1000
	p := filepath.Join(t.TempDir(), "host.log")
	if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 1500), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := openLogFile(p) // over the limit at the start: rotated at once
	if err != nil {
		t.Fatal(err)
	}
	if old, _ := os.ReadFile(p + ".old"); len(old) != 1500 {
		t.Fatalf("not rotated at the start: .old has %d bytes", len(old))
	}
	var want []string
	for i := 0; i < 50; i++ {
		line := fmt.Sprintf("record %02d %s\n", i, strings.Repeat("y", 40))
		want = append(want, line)
		if _, err := l.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	cur, _ := os.ReadFile(p)
	old, _ := os.ReadFile(p + ".old")
	if len(cur) > 1000 || len(old) > 1000 || len(old) == 1500 {
		t.Fatalf("host.log %d bytes, host.log.old %d bytes: want both under the limit", len(cur), len(old))
	}
	// The last two files hold the last records, whole and in order.
	got := string(old) + string(cur)
	if !strings.HasSuffix(strings.Join(want, ""), got) || !strings.HasPrefix(got, "record ") {
		t.Fatalf("records split or lost:\n%s", got)
	}
}

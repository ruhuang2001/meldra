package app

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestCommandCursorPreservesStreamingText(t *testing.T) {
	for _, test := range []struct{ name, raw, want string }{
		{"unicode", strings.Repeat("a", 65535) + "中🎉tail", strings.Repeat("a", 65535) + "中🎉tail"},
		{"csi", strings.Repeat("a", 65534) + "\x1b[31mred\x1b[0mtail", strings.Repeat("a", 65534) + "redtail"},
		{"osc", strings.Repeat("a", 65532) + "\x1b]0;hidden-title\x1b\\tail", strings.Repeat("a", 65532) + "tail"},
		{"invalid", strings.Repeat("a", 65535) + "\xff\xfe中\xe4", strings.Repeat("a", 65535) + "��中�"},
	} {
		t.Run(test.name, func(t *testing.T) {
			log, err := newCommandLog(nil, "")
			if err != nil {
				t.Fatal(err)
			}
			defer log.close()
			if _, err := log.Write([]byte(test.raw)); err != nil {
				t.Fatal(err)
			}
			log.finish()
			artifact, truncated, err := log.artifact()
			if err != nil || truncated || string(artifact) != test.raw {
				t.Fatalf("presentation changed raw evidence: truncated=%v err=%v", truncated, err)
			}
			var output strings.Builder
			cursor := int64(0)
			for cursor < int64(len(test.raw)) {
				text, next, truncated, err := log.read(cursor)
				if err != nil || truncated || next <= cursor || !utf8.ValidString(text) {
					t.Fatalf("cursor %d -> %d truncated=%v valid=%v err=%v", cursor, next, truncated, utf8.ValidString(text), err)
				}
				output.WriteString(text)
				cursor = next
			}
			if got := output.String(); got != test.want {
				t.Fatalf("decoded suffix %q, want %q", got[max(0, len(got)-30):], test.want[max(0, len(test.want)-30):])
			}
			if again, next, _, err := log.read(cursor); err != nil || again != "" || next != cursor {
				t.Fatalf("final cursor replayed output: %q %d %v", again, next, err)
			}
		})
	}
}

func TestCommandCursorDefersIncompleteRuneAcrossWrites(t *testing.T) {
	log, err := newCommandLog(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer log.close()
	for index, char := range []byte("中") {
		_, _ = log.Write([]byte{char})
		text, next, _, err := log.read(0)
		if err != nil {
			t.Fatal(err)
		}
		if index < 2 && (text != "" || next != 0) {
			t.Fatalf("partial rune consumed: %q cursor %d", text, next)
		}
		if index == 2 && (text != "中" || next != 3) {
			t.Fatalf("complete rune lost: %q cursor %d", text, next)
		}
	}
	_, _ = log.Write([]byte{0xe4})
	log.finish()
	text, next, _, err := log.read(3)
	if text != "�" || next != 4 || err != nil {
		t.Fatalf("unfinished final byte not represented: %q %d %v", text, next, err)
	}
}

func TestCommandTailRetainsEscapeStateAfterEviction(t *testing.T) {
	for _, oneWrite := range []bool{false, true} {
		t.Run(fmt.Sprint(oneWrite), func(t *testing.T) {
			log, err := newCommandLog(nil, "")
			if err != nil {
				t.Fatal(err)
			}
			defer log.close()
			raw := []byte("before\x1b]0;" + strings.Repeat("hidden", maxToolOutput/6+100) + "\x1b\\after中")
			if oneWrite {
				_, _ = log.Write(raw)
			} else {
				for len(raw) > 0 {
					n := min(len(raw), 997)
					_, _ = log.Write(raw[:n])
					raw = raw[n:]
				}
			}
			log.finish()
			var output strings.Builder
			var cursor int64
			for cursor < log.total {
				text, next, truncated, err := log.read(cursor)
				if err != nil || next <= cursor || cursor == 0 && !truncated {
					t.Fatalf("invalid retained cursor %d -> %d truncated=%v err=%v", cursor, next, truncated, err)
				}
				output.WriteString(text)
				cursor = next
			}
			if output.String() != "after中" {
				t.Fatalf("evicted escape leaked payload: %.80q", output.String())
			}
		})
	}
}

func TestCommandLiveOutputFlushesQuietTailAndClosesTimer(t *testing.T) {
	updates := make(chan CommandOutput, 8)
	var closed atomic.Bool
	var late atomic.Bool
	log, err := newCommandLog(func(update CommandOutput) {
		if closed.Load() {
			late.Store(true)
		}
		updates <- update
	}, "process-one")
	if err != nil {
		t.Fatal(err)
	}
	defer log.close()
	_, _ = log.Write([]byte("initial"))
	<-updates
	_, _ = log.Write([]byte("\x1b]0;secret"))
	_, _ = log.Write([]byte("\x1b\\quiet中"))
	select {
	case update := <-updates:
		if update.ProcessID != "process-one" || update.Text != "quiet中" || update.Truncated {
			t.Fatalf("invalid quiet-tail update: %+v", update)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quiet trailing output requires another write to be emitted")
	}
	_, _ = log.Write([]byte("final"))
	log.close()
	closed.Store(true)
	if update := <-updates; update.Text != "final" {
		t.Fatalf("close did not flush pending bytes: %+v", update)
	}
	time.Sleep(150 * time.Millisecond)
	if late.Load() {
		t.Fatal("timer delivered callback after close returned")
	}
	select {
	case update := <-updates:
		t.Fatalf("timer repeated final update: %+v", update)
	default:
	}
}

func TestCommandStreamingCallbacksAndReadsRemainSafeUnderConcurrentClose(t *testing.T) {
	var callbacks atomic.Int64
	log, err := newCommandLog(func(CommandOutput) { callbacks.Add(1) }, "race")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	workers.Go(func() {
		for range 500 {
			_, _ = log.Write([]byte("中\x1b[31mred\x1b[0m"))
		}
	})
	workers.Go(func() {
		for range 500 {
			_, _, _, _ = log.read(0)
		}
	})
	log.close()
	count := callbacks.Load()
	workers.Wait()
	if callbacks.Load() != count {
		t.Fatal("write or timer emitted after close")
	}
}

func TestTerminalStreamIsIndependentOfWriteBoundaries(t *testing.T) {
	raw := []byte("text中\x1b[31mred\x1b[0m\x1b]title\x1b\\after\r\n\xff\xe4")
	want := "text中redafter\n��"
	for size := 1; size <= len(raw); size++ {
		var decoder terminalStream
		var output strings.Builder
		for chunk := range slices.Chunk(raw, size) {
			decoder.append(chunk, false, &output)
		}
		decoder.append(nil, true, &output)
		if output.String() != want {
			t.Fatalf("chunk size %d produced %q", size, output.String())
		}
	}
}

package judge

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/autocheck-team/backend/internal/sandbox"
)

func TestVerdict(t *testing.T) {
	tests := []struct {
		name   string
		result sandbox.Result
		want   string
	}{
		{"ok", sandbox.Result{}, "OK"},
		{"oom_precedes_timeout", sandbox.Result{OOMKilled: true, TimedOut: true}, "ML"},
		{"timeout_precedes_exit", sandbox.Result{TimedOut: true, ExitCode: 137, PeakMemoryBytes: 1024}, "TL"},
		{"cpu_sigkill", sandbox.Result{ExitCode: 137, CPUTime: time.Second}, "TL"},
		{"cpu_sigxcpu", sandbox.Result{ExitCode: 152, CPUTime: time.Second}, "TL"},
		{"sigxcpu_without_cpu", sandbox.Result{ExitCode: 152}, "RE"},
		{"sigkill_near_memory", sandbox.Result{ExitCode: 137, PeakMemoryBytes: 950}, "ML"},
		{"sigkill_other", sandbox.Result{ExitCode: 137, PeakMemoryBytes: 100}, "RE"},
		{"output", sandbox.Result{OutputLimitExceeded: true}, "RE"},
		{"exit", sandbox.Result{ExitCode: 1}, "RE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Verdict(tt.result, time.Second, 1024); got != tt.want {
				t.Fatalf("verdict=%s want=%s", got, tt.want)
			}
		})
	}
}

func TestCleanTail(t *testing.T) {
	for _, input := range [][]byte{[]byte("a\x00b\xffc"), []byte(strings.Repeat("я", 40000)), []byte("\x00\xff"), []byte("hello")} {
		for _, limit := range []int{0, 1, 4, 4096, 65536} {
			got := cleanTail(input, limit)
			if !utf8.ValidString(got) || strings.ContainsRune(got, 0) || len(got) > limit {
				t.Fatalf("invalid text %q", got)
			}
		}
	}
	if got := cleanTail([]byte("a\x00b\xffc"), 10); got != "abc" {
		t.Fatalf("got %q", got)
	}
}

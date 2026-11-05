package judge

import (
	"strings"
	"time"

	"github.com/autocheck-team/backend/internal/sandbox"
)

// Verdict учитывает признаки в порядке надёжности: OOM и wall timeout важнее кода выхода.
func Verdict(r sandbox.Result, timeLimit time.Duration, memoryBytes int64) string {
	switch {
	case r.OOMKilled:
		return "ML"
	case r.TimedOut:
		return "TL"
	case (r.ExitCode == 137 || r.ExitCode == 152) && r.CPUTime >= timeLimit:
		return "TL"
	case r.ExitCode == 137 && r.PeakMemoryBytes >= memoryBytes-memoryBytes/10:
		return "ML"
	case r.OutputLimitExceeded, r.ExitCode != 0:
		return "RE"
	default:
		return "OK"
	}
}

// cleanTail удаляет NUL и невалидный UTF-8: PostgreSQL text их не принимает.
func cleanTail(b []byte, limit int) string {
	s := strings.ToValidUTF8(strings.ReplaceAll(string(b), "\x00", ""), "")
	if len(s) > limit {
		s = s[len(s)-limit:]
		s = strings.ToValidUTF8(s, "")
	}
	return s
}

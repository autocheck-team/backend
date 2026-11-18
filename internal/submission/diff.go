package submission

import "strings"

// lineDiff сохраняет окончания строк: отсутствие LF и CRLF влияют на exact-чекер.
func lineDiff(expected, actual string) string {
	split := func(text string) []string {
		if text == "" {
			return nil
		}
		result := strings.SplitAfter(text, "\n")
		if result[len(result)-1] == "" {
			result = result[:len(result)-1]
		}
		return result
	}
	var out strings.Builder
	write := func(prefix, line string) {
		out.WriteString(prefix)
		out.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			out.WriteString("\n\\ No newline at end of file\n")
		}
	}
	var diff func([]string, []string)
	diff = func(left, right []string) {
		if len(left) == 0 {
			for _, line := range right {
				write("+", line)
			}
			return
		}
		if len(right) == 0 {
			for _, line := range left {
				write("-", line)
			}
			return
		}
		if len(left) == 1 {
			index := -1
			for i, line := range right {
				if line == left[0] {
					index = i
					break
				}
			}
			if index < 0 {
				write("-", left[0])
			}
			for i, line := range right {
				if i == index {
					write(" ", line)
					continue
				}
				write("+", line)
			}
			return
		}
		middle := len(left) / 2
		forward := lcsLengths(left[:middle], right, false)
		backward := lcsLengths(left[middle:], right, true)
		best, split := -1, 0
		for i := range len(right) + 1 {
			if score := forward[i] + backward[len(right)-i]; score > best {
				best, split = score, i
			}
		}
		diff(left[:middle], right[:split])
		diff(left[middle:], right[split:])
	}
	diff(split(expected), split(actual))
	return out.String()
}

// lcsLengths хранит одну строку LCS, чтобы тысячи коротких строк не раздували память.
func lcsLengths(left, right []string, reverse bool) []int {
	row := make([]int, len(right)+1)
	for i := range left {
		diagonal := 0
		for j := range right {
			li, ri := i, j
			if reverse {
				li, ri = len(left)-1-i, len(right)-1-j
			}
			previous := row[j+1]
			if left[li] == right[ri] {
				row[j+1] = diagonal + 1
			} else {
				row[j+1] = max(row[j+1], row[j])
			}
			diagonal = previous
		}
	}
	return row
}

// Package judge проверяет посылки, сохраняя аренду задания за одним воркером.
package judge

import (
	"bytes"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// Check сравнивает только данные в памяти: эталон не попадает в workspace решения.
func Check(kind string, expected, actual []byte, epsilon float64) bool {
	if kind == "exact" {
		normalize := func(b []byte) []byte {
			return bytes.TrimRightFunc(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")), unicode.IsSpace)
		}
		return bytes.Equal(normalize(expected), normalize(actual))
	}
	a, b := strings.Fields(string(expected)), strings.Fields(string(actual))
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if kind == "float" {
			x, ex := strconv.ParseFloat(a[i], 64)
			y, ey := strconv.ParseFloat(b[i], 64)
			if ex == nil && ey == nil {
				if math.IsNaN(x) || math.IsNaN(y) {
					if !(math.IsNaN(x) && math.IsNaN(y)) {
						return false
					}
					continue
				}
				if math.IsInf(x, 0) || math.IsInf(y, 0) {
					if x != y {
						return false
					}
					continue
				}
				if math.Abs(x-y) > epsilon {
					return false
				}
				continue
			}
		}
		if a[i] != b[i] {
			return false
		}
	}
	return kind == "tokens" || kind == "float"
}

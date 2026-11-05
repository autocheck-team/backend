package judge

import "testing"

func TestCheck(t *testing.T) {
	tests := []struct {
		name, kind, expected, actual string
		epsilon                      float64
		want                         bool
	}{
		{"exact_crlf_tail", "exact", "a\r\nb \t\n", "a\nb", 0, true},
		{"exact_internal_space", "exact", "a b", "a  b", 0, false},
		{"exact_internal_cr", "exact", "a\rb", "a\nb", 0, false},
		{"exact_empty", "exact", "", " \n\t", 0, true},
		{"exact_leading", "exact", " a", "a", 0, false},
		{"tokens_whitespace", "tokens", "a\tb\n3", "a b 3\n", 0, true},
		{"tokens_count", "tokens", "a", "a b", 0, false},
		{"tokens_numbers", "tokens", "1.0", "1", 0, false},
		{"float_tolerance", "float", "1.00 a", "1.0001 a", .001, true},
		{"float_boundary", "float", "1", "1.5", .5, true},
		{"float_outside", "float", "1", "1.6", .5, false},
		{"float_text", "float", "abc", "abd", .1, false},
		{"float_nan", "float", "NaN", "NaN", .1, true},
		{"float_nan_number", "float", "NaN", "1", .1, false},
		{"float_inf", "float", "+Inf", "Inf", .1, true},
		{"float_inf_sign", "float", "+Inf", "-Inf", .1, false},
		{"float_inf_finite", "float", "Inf", "1", .1, false},
		{"float_scientific", "float", "1e2", "100.0", .1, true},
		{"float_empty", "float", "", "\n", .1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Check(tt.kind, []byte(tt.expected), []byte(tt.actual), tt.epsilon); got != tt.want {
				t.Fatalf("Check = %t, want %t", got, tt.want)
			}
		})
	}
}

package metrics

import (
	"fmt"
	"testing"
)

func TestStatusCategory(t *testing.T) {
	tests := []struct {
		code     int
		expected string
	}{
		{200, "2xx"},
		{201, "2xx"},
		{204, "2xx"},
		{299, "2xx"},
		{400, "4xx"},
		{401, "4xx"},
		{404, "4xx"},
		{499, "4xx"},
		{500, "5xx"},
		{502, "5xx"},
		{503, "5xx"},
		{599, "5xx"},
		{100, "unknown"},
		{301, "unknown"},
		{399, "unknown"},
		{0, "unknown"},
		{-1, "unknown"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("code_%d", tt.code), func(t *testing.T) {
			got := StatusCategory(tt.code)
			if got != tt.expected {
				t.Errorf("StatusCategory(%d) = %q, want %q", tt.code, got, tt.expected)
			}
		})
	}
}

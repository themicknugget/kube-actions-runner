package scaler

import (
	"reflect"
	"testing"
)

func TestParseLabelMatchers_Empty(t *testing.T) {
	matchers := ParseLabelMatchers("")
	if matchers != nil {
		t.Errorf("expected nil for empty config, got %v", matchers)
	}
}

func TestParseLabelMatchers_SingleLabel(t *testing.T) {
	matchers := ParseLabelMatchers("linux")
	if len(matchers) != 1 {
		t.Fatalf("expected 1 matcher, got %d", len(matchers))
	}
	if matchers[0].ExactMatch {
		t.Error("expected any-match mode for single label")
	}
	if !reflect.DeepEqual(matchers[0].Labels, []string{"linux"}) {
		t.Errorf("expected labels [linux], got %v", matchers[0].Labels)
	}
}

func TestParseLabelMatchers_AnyMatch(t *testing.T) {
	matchers := ParseLabelMatchers("linux,macos,windows")
	if len(matchers) != 1 {
		t.Fatalf("expected 1 matcher, got %d", len(matchers))
	}
	if matchers[0].ExactMatch {
		t.Error("expected any-match mode")
	}
	expected := []string{"linux", "macos", "windows"}
	if !reflect.DeepEqual(matchers[0].Labels, expected) {
		t.Errorf("expected labels %v, got %v", expected, matchers[0].Labels)
	}
}

func TestParseLabelMatchers_ExactMatch(t *testing.T) {
	matchers := ParseLabelMatchers("linux+large")
	if len(matchers) != 1 {
		t.Fatalf("expected 1 matcher, got %d", len(matchers))
	}
	if !matchers[0].ExactMatch {
		t.Error("expected exact-match mode")
	}
	expected := []string{"linux", "large"}
	if !reflect.DeepEqual(matchers[0].Labels, expected) {
		t.Errorf("expected labels %v, got %v", expected, matchers[0].Labels)
	}
}

func TestParseLabelMatchers_MultipleMatchers(t *testing.T) {
	matchers := ParseLabelMatchers("linux,macos;ubuntu+large")
	if len(matchers) != 2 {
		t.Fatalf("expected 2 matchers, got %d", len(matchers))
	}

	// First matcher: any-match
	if matchers[0].ExactMatch {
		t.Error("expected first matcher to be any-match")
	}
	if !reflect.DeepEqual(matchers[0].Labels, []string{"linux", "macos"}) {
		t.Errorf("first matcher labels: expected [linux macos], got %v", matchers[0].Labels)
	}

	// Second matcher: exact-match
	if !matchers[1].ExactMatch {
		t.Error("expected second matcher to be exact-match")
	}
	if !reflect.DeepEqual(matchers[1].Labels, []string{"ubuntu", "large"}) {
		t.Errorf("second matcher labels: expected [ubuntu large], got %v", matchers[1].Labels)
	}
}

func TestParseLabelMatchers_WhitespaceHandling(t *testing.T) {
	matchers := ParseLabelMatchers("  linux , macos  ; ubuntu + large  ")
	if len(matchers) != 2 {
		t.Fatalf("expected 2 matchers, got %d", len(matchers))
	}

	if !reflect.DeepEqual(matchers[0].Labels, []string{"linux", "macos"}) {
		t.Errorf("expected trimmed labels, got %v", matchers[0].Labels)
	}
	if !reflect.DeepEqual(matchers[1].Labels, []string{"ubuntu", "large"}) {
		t.Errorf("expected trimmed labels, got %v", matchers[1].Labels)
	}
}

func TestParseLabelMatchers_EmptyParts(t *testing.T) {
	matchers := ParseLabelMatchers(";;linux;;")
	if len(matchers) != 1 {
		t.Fatalf("expected 1 matcher, got %d", len(matchers))
	}
	if !reflect.DeepEqual(matchers[0].Labels, []string{"linux"}) {
		t.Errorf("expected labels [linux], got %v", matchers[0].Labels)
	}
}

func TestShouldHandle_RequiresSelfHosted(t *testing.T) {
	matchers := []LabelMatcher{}

	// Without self-hosted label
	if ShouldHandle([]string{"linux", "large"}, matchers) {
		t.Error("expected false without self-hosted label")
	}

	// With self-hosted label
	if !ShouldHandle([]string{"self-hosted", "linux"}, matchers) {
		t.Error("expected true with self-hosted label")
	}
}

func TestShouldHandle_NoMatchersHandlesAllSelfHosted(t *testing.T) {
	matchers := []LabelMatcher{}

	if !ShouldHandle([]string{"self-hosted"}, matchers) {
		t.Error("expected true for self-hosted with no matchers")
	}
	if !ShouldHandle([]string{"self-hosted", "linux", "large"}, matchers) {
		t.Error("expected true for any self-hosted job with no matchers")
	}
}

func TestShouldHandle_ExactMatchAllLabelsRequired(t *testing.T) {
	matchers := []LabelMatcher{
		{Labels: []string{"linux", "large"}, ExactMatch: true},
	}

	// Has all required labels
	if !ShouldHandle([]string{"self-hosted", "linux", "large"}, matchers) {
		t.Error("expected true when all labels match")
	}

	// Has extra labels (still matches)
	if !ShouldHandle([]string{"self-hosted", "linux", "large", "gpu"}, matchers) {
		t.Error("expected true when all required labels present with extras")
	}

	// Missing a required label
	if ShouldHandle([]string{"self-hosted", "linux"}, matchers) {
		t.Error("expected false when missing required label")
	}
}

func TestShouldHandle_AnyMatchOneLabel(t *testing.T) {
	matchers := []LabelMatcher{
		{Labels: []string{"linux", "macos"}, ExactMatch: false},
	}

	// Has one matching label
	if !ShouldHandle([]string{"self-hosted", "linux"}, matchers) {
		t.Error("expected true when one label matches")
	}

	// Has different matching label
	if !ShouldHandle([]string{"self-hosted", "macos"}, matchers) {
		t.Error("expected true when other label matches")
	}

	// Has both labels
	if !ShouldHandle([]string{"self-hosted", "linux", "macos"}, matchers) {
		t.Error("expected true when both labels present")
	}

	// Has no matching label
	if ShouldHandle([]string{"self-hosted", "windows"}, matchers) {
		t.Error("expected false when no labels match")
	}
}

func TestShouldHandle_MultipleMatchers(t *testing.T) {
	matchers := []LabelMatcher{
		{Labels: []string{"linux", "large"}, ExactMatch: true},
		{Labels: []string{"arm64"}, ExactMatch: false},
	}

	// Matches first matcher (exact)
	if !ShouldHandle([]string{"self-hosted", "linux", "large"}, matchers) {
		t.Error("expected true matching first (exact) matcher")
	}

	// Matches second matcher (any)
	if !ShouldHandle([]string{"self-hosted", "arm64"}, matchers) {
		t.Error("expected true matching second (any) matcher")
	}

	// Matches neither
	if ShouldHandle([]string{"self-hosted", "linux"}, matchers) {
		t.Error("expected false when neither matcher matches")
	}
}

func TestShouldHandle_EmptyJobLabels(t *testing.T) {
	matchers := []LabelMatcher{}

	if ShouldHandle([]string{}, matchers) {
		t.Error("expected false for empty job labels")
	}
}

func TestContains(t *testing.T) {
	tests := []struct {
		slice    []string
		val      string
		expected bool
	}{
		{[]string{"a", "b", "c"}, "b", true},
		{[]string{"a", "b", "c"}, "d", false},
		{[]string{}, "a", false},
		{[]string{"self-hosted"}, "self-hosted", true},
	}

	for _, tt := range tests {
		result := contains(tt.slice, tt.val)
		if result != tt.expected {
			t.Errorf("contains(%v, %q) = %v, want %v", tt.slice, tt.val, result, tt.expected)
		}
	}
}

func TestContainsAll(t *testing.T) {
	tests := []struct {
		jobLabels []string
		required  []string
		expected  bool
	}{
		{[]string{"a", "b", "c"}, []string{"a", "b"}, true},
		{[]string{"a", "b", "c"}, []string{"a", "d"}, false},
		{[]string{"a"}, []string{"a", "b"}, false},
		{[]string{"a", "b"}, []string{}, true},
		{[]string{}, []string{"a"}, false},
	}

	for _, tt := range tests {
		result := containsAll(tt.jobLabels, tt.required)
		if result != tt.expected {
			t.Errorf("containsAll(%v, %v) = %v, want %v", tt.jobLabels, tt.required, result, tt.expected)
		}
	}
}

func TestContainsAny(t *testing.T) {
	tests := []struct {
		jobLabels []string
		targets   []string
		expected  bool
	}{
		{[]string{"a", "b", "c"}, []string{"b", "d"}, true},
		{[]string{"a", "b", "c"}, []string{"d", "e"}, false},
		{[]string{"a"}, []string{"a"}, true},
		{[]string{"a", "b"}, []string{}, false},
		{[]string{}, []string{"a"}, false},
	}

	for _, tt := range tests {
		result := containsAny(tt.jobLabels, tt.targets)
		if result != tt.expected {
			t.Errorf("containsAny(%v, %v) = %v, want %v", tt.jobLabels, tt.targets, result, tt.expected)
		}
	}
}

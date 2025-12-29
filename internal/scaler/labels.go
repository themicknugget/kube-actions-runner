package scaler

import (
	"strings"
)

type LabelMatcher struct {
	Labels     []string
	ExactMatch bool
}

// ParseLabelMatchers parses label matchers from config string.
// Format: "label1,label2" (any-match) or "label1+label2" (exact-match), separated by ";"
func ParseLabelMatchers(config string) []LabelMatcher {
	if config == "" {
		return nil
	}

	var matchers []LabelMatcher
	for _, matcherStr := range strings.Split(config, ";") {
		matcherStr = strings.TrimSpace(matcherStr)
		if matcherStr == "" {
			continue
		}

		if strings.Contains(matcherStr, "+") {
			labels := strings.Split(matcherStr, "+")
			cleaned := make([]string, 0, len(labels))
			for _, l := range labels {
				if trimmed := strings.TrimSpace(l); trimmed != "" {
					cleaned = append(cleaned, trimmed)
				}
			}
			if len(cleaned) > 0 {
				matchers = append(matchers, LabelMatcher{Labels: cleaned, ExactMatch: true})
			}
		} else {
			labels := strings.Split(matcherStr, ",")
			cleaned := make([]string, 0, len(labels))
			for _, l := range labels {
				if trimmed := strings.TrimSpace(l); trimmed != "" {
					cleaned = append(cleaned, trimmed)
				}
			}
			if len(cleaned) > 0 {
				matchers = append(matchers, LabelMatcher{Labels: cleaned, ExactMatch: false})
			}
		}
	}
	return matchers
}

func ShouldHandle(jobLabels []string, matchers []LabelMatcher) bool {
	if !contains(jobLabels, "self-hosted") {
		return false
	}

	if len(matchers) == 0 {
		return true
	}

	for _, matcher := range matchers {
		if matcher.ExactMatch {
			if containsAll(jobLabels, matcher.Labels) {
				return true
			}
		} else {
			if containsAny(jobLabels, matcher.Labels) {
				return true
			}
		}
	}
	return false
}

func contains(slice []string, val string) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
	}
	return false
}

func containsAll(jobLabels, required []string) bool {
	for _, req := range required {
		if !contains(jobLabels, req) {
			return false
		}
	}
	return true
}

func containsAny(jobLabels, targets []string) bool {
	for _, target := range targets {
		if contains(jobLabels, target) {
			return true
		}
	}
	return false
}

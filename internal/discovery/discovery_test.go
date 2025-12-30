package discovery

import (
	"testing"

	"github.com/kube-actions-runner/kube-actions-runner/internal/logger"
)

func TestExtractSelfHostedLabels(t *testing.T) {
	log := logger.New()
	d := &Discoverer{logger: log}

	tests := []struct {
		name     string
		content  string
		expected []string
	}{
		{
			name: "array syntax with self-hosted",
			content: `
jobs:
  build:
    runs-on: [self-hosted, linux, x64]
    steps:
      - uses: actions/checkout@v4
`,
			expected: []string{"self-hosted", "linux", "x64"},
		},
		{
			name: "string syntax with self-hosted",
			content: `
jobs:
  build:
    runs-on: self-hosted
`,
			expected: []string{"self-hosted"},
		},
		{
			name: "no self-hosted label",
			content: `
jobs:
  build:
    runs-on: ubuntu-latest
`,
			expected: nil,
		},
		{
			name: "multiple jobs with mixed runners",
			content: `
jobs:
  build:
    runs-on: ubuntu-latest
  deploy:
    runs-on: [self-hosted, linux]
`,
			expected: []string{"self-hosted", "linux"},
		},
		{
			name: "quoted labels",
			content: `
jobs:
  build:
    runs-on: ["self-hosted", "linux", "x64"]
`,
			expected: []string{"self-hosted", "linux", "x64"},
		},
		{
			name: "single quoted labels",
			content: `
jobs:
  build:
    runs-on: ['self-hosted', 'linux']
`,
			expected: []string{"self-hosted", "linux"},
		},
		{
			name: "case insensitive self-hosted",
			content: `
jobs:
  build:
    runs-on: [Self-Hosted, Linux]
`,
			expected: []string{"Self-Hosted", "Linux"},
		},
		{
			name: "multiple runs-on declarations",
			content: `
jobs:
  build:
    runs-on: [self-hosted, linux]
  test:
    runs-on: [self-hosted, windows]
`,
			expected: []string{"self-hosted", "linux", "self-hosted", "windows"},
		},
		{
			name: "empty content",
			content:  "",
			expected: nil,
		},
		{
			name: "runs-on without value",
			content: `
jobs:
  build:
    runs-on:
`,
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.extractSelfHostedLabels(tt.content)

			if len(got) != len(tt.expected) {
				t.Errorf("extractSelfHostedLabels() got %v labels, want %v labels", len(got), len(tt.expected))
				t.Errorf("got: %v", got)
				t.Errorf("want: %v", tt.expected)
				return
			}

			for i, label := range got {
				if label != tt.expected[i] {
					t.Errorf("extractSelfHostedLabels() label[%d] = %v, want %v", i, label, tt.expected[i])
				}
			}
		})
	}
}

func TestRunsOnPatternMatches(t *testing.T) {
	tests := []struct {
		name    string
		content string
		matches bool
	}{
		{
			name:    "array syntax",
			content: "runs-on: [self-hosted, linux]",
			matches: true,
		},
		{
			name:    "string syntax",
			content: "runs-on: ubuntu-latest",
			matches: true,
		},
		{
			name:    "with extra spaces",
			content: "runs-on:   [self-hosted]",
			matches: true,
		},
		{
			name:    "no match",
			content: "name: Build",
			matches: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runsOnPattern.MatchString(tt.content)
			if got != tt.matches {
				t.Errorf("runsOnPattern.MatchString(%q) = %v, want %v", tt.content, got, tt.matches)
			}
		})
	}
}

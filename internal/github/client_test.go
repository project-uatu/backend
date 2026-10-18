// SPDX-License-Identifier: Apache-2.0

package github

import (
	"strings"
	"testing"
)

func TestValidLogin(t *testing.T) {
	tests := []struct {
		login string
		want  bool
	}{
		{"octocat", true},
		{"octo-cat", true},
		{"Octo123", true},
		{strings.Repeat("a", 39), true},
		{"", false},
		{strings.Repeat("a", 40), false},
		{"-octocat", false},
		{"octocat-", false},
		{"octo_cat", false},
		{"octo/cat", false},
		{"..", false},
	}
	for _, tt := range tests {
		if got := ValidLogin(tt.login); got != tt.want {
			t.Errorf("ValidLogin(%q) = %v, want %v", tt.login, got, tt.want)
		}
	}
}

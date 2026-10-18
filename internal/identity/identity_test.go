// SPDX-License-Identifier: Apache-2.0

package identity

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		kind     Kind
		value    string
		want     string
		wantErr  bool
	}{
		{name: "email domain is case insensitive", kind: KindEmail, value: " Alice@EXAMPLE.com ", want: "Alice@example.com"},
		{name: "email local part remains exact", kind: KindEmail, value: "Alice@example.com", want: "Alice@example.com"},
		{name: "GitHub login is case insensitive", provider: "github", kind: KindLogin, value: "OctoCat", want: "octocat"},
		{name: "Forgejo login is case insensitive", provider: "forgejo", kind: KindLogin, value: "Codeberg", want: "codeberg"},
		{name: "account ID remains exact", kind: KindAccountID, value: "GhID-AbC", want: "GhID-AbC"},
		{name: "invalid email", kind: KindEmail, value: "alice@", wantErr: true},
		{name: "invalid login", provider: "github", kind: KindLogin, value: " octocat", wantErr: true},
		{name: "unsupported provider", provider: "unknown", kind: KindLogin, value: "user", wantErr: true},
		{name: "unsupported kind", kind: "name", value: "Alice", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Normalize(tt.provider, tt.kind, tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Normalize(%q, %q) error = %v, wantErr %v", tt.kind, tt.value, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("Normalize(%q, %q) = %q, want %q", tt.kind, tt.value, got, tt.want)
			}
		})
	}
}

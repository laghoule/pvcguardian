// SPDX-License-Identifier: GPL-3.0-only
package k8s

import (
	"reflect"
	"testing"
)

func TestParseSelectorAnnotation(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "single pair",
			raw:  "app.kubernetes.io/part-of=coroot",
			want: map[string]string{"app.kubernetes.io/part-of": "coroot"},
		},
		{
			name: "multiple pairs with spaces",
			raw:  "app=web, tier=db",
			want: map[string]string{"app": "web", "tier": "db"},
		},
		{
			name: "empty value",
			raw:  "app=",
			want: map[string]string{"app": ""},
		},
		{
			name:    "missing equals",
			raw:     "app",
			wantErr: true,
		},
		{
			name:    "empty key",
			raw:     "=web",
			wantErr: true,
		},
		{
			name:    "duplicate key",
			raw:     "app=web,app=db",
			wantErr: true,
		},
		{
			name:    "invalid label value",
			raw:     "app=web=db",
			wantErr: true,
		},
		{
			name:    "empty selector",
			raw:     " , ",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSelectorAnnotation(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("expected %v, got %v", tt.want, got)
			}
		})
	}
}

package resticx

import "testing"

func TestAtLeastMinVersion(t *testing.T) {
	tests := []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{"0.17.0", true, false},
		{"0.18.1", true, false},
		{"1.0.0", true, false},
		{"0.16.5", false, false},
		{"0.16.99", false, false},
		{"0.9.6", false, false},
		{"0.17", true, false}, // missing patch defaults to 0
		{"0.18", true, false},
		{"0.16", false, false},
		{"0.18.1-dev", true, false}, // pre-release suffix ignored
		{"0.17.0-rc.1", true, false},
		{"0.18.1+build.7", true, false},
		{"  0.18.1  ", true, false}, // surrounding whitespace tolerated
		{"", false, true},           // empty is an error, not a pass
		{"garbage", false, true},
		{"0.x.1", false, true},
		{"0.18.1.2", false, true}, // too many components
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := AtLeastMinVersion(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("AtLeastMinVersion(%q) = (%v, nil), want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("AtLeastMinVersion(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("AtLeastMinVersion(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

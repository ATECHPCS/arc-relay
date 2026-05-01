package main

import "testing"

func TestSelfUpdateBinaryName(t *testing.T) {
	cases := []struct {
		goos, goarch string
		want         string
	}{
		{"linux", "amd64", "arc-sync-linux-amd64"},
		{"linux", "arm64", "arc-sync-linux-arm64"},
		{"darwin", "amd64", "arc-sync-darwin-amd64"},
		{"darwin", "arm64", "arc-sync-darwin-arm64"},
		{"windows", "amd64", "arc-sync-windows-amd64.exe"},
		{"windows", "arm64", "arc-sync-windows-arm64.exe"},
	}
	for _, tc := range cases {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			got := selfUpdateBinaryName(tc.goos, tc.goarch)
			if got != tc.want {
				t.Fatalf("selfUpdateBinaryName(%q,%q) = %q; want %q", tc.goos, tc.goarch, got, tc.want)
			}
		})
	}
}

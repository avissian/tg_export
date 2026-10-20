package main

import "testing"

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want cliOptions
	}{
		{name: "defaults"},
		{
			name: "no stickers",
			args: []string{"--no-stickers"},
			want: cliOptions{export: ExportOptions{NoStickers: true}},
		},
		{
			name: "no audio",
			args: []string{"--no-audio"},
			want: cliOptions{export: ExportOptions{NoAudio: true}},
		},
		{
			name: "no media",
			args: []string{"--no-media"},
			want: cliOptions{export: ExportOptions{NoMedia: true}},
		},
		{
			name: "combined with existing flags",
			args: []string{"--no-audio", "--code-auth", "--no-stickers", "--verbose", "--no-media"},
			want: cliOptions{
				useCodeAuth: true,
				verbose:     true,
				export:      ExportOptions{NoStickers: true, NoAudio: true, NoMedia: true},
			},
		},
		{
			name: "short verbose and repeated flag",
			args: []string{"--no-audio", "-v", "--no-audio"},
			want: cliOptions{verbose: true, export: ExportOptions{NoAudio: true}},
		},
		{
			name: "unknown arguments still ignored",
			args: []string{"--unknown", "--code-auth"},
			want: cliOptions{useCodeAuth: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseArgs(tt.args); got != tt.want {
				t.Fatalf("parseArgs(%q) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

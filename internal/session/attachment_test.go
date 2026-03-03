package session

import "testing"

func TestMediaPathUnquotedRe_UnixPaths(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/tmp/image.png", "/tmp/image.png"},
		{"./relative/photo.jpg", "./relative/photo.jpg"},
		{"../parent/video.mp4", "../parent/video.mp4"},
		{"~/Pictures/cat.gif", "~/Pictures/cat.gif"},
	}
	for _, tt := range tests {
		m := mediaPathUnquotedRe.FindString(tt.input)
		if m != tt.want {
			t.Errorf("input %q: got %q, want %q", tt.input, m, tt.want)
		}
	}
}

func TestMediaPathUnquotedRe_WindowsPaths(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`C:\Users\test\image.png`, `C:\Users\test\image.png`},
		{`D:\Photos\vacation.jpg`, `D:\Photos\vacation.jpg`},
		{`C:/Users/test/image.png`, `C:/Users/test/image.png`},
		{`.\subfolder\image.png`, `.\subfolder\image.png`},
		{`..\parent\photo.jpg`, `..\parent\photo.jpg`},
		// Mixed separators
		{`C:\Users/test\photo.jpeg`, `C:\Users/test\photo.jpeg`},
		// Case insensitive extension
		{`C:\TEMP\IMAGE.PNG`, `C:\TEMP\IMAGE.PNG`},
	}
	for _, tt := range tests {
		m := mediaPathUnquotedRe.FindString(tt.input)
		if m != tt.want {
			t.Errorf("input %q: got %q, want %q", tt.input, m, tt.want)
		}
	}
}

func TestMediaPathQuotedRe_WindowsPaths(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`"C:\Users\test\my image.png"`, `C:\Users\test\my image.png`},
		{`'D:\Photos\vacation pic.jpg'`, `D:\Photos\vacation pic.jpg`},
	}
	for _, tt := range tests {
		ms := mediaPathQuotedRe.FindStringSubmatch(tt.input)
		if len(ms) < 2 || ms[1] != tt.want {
			got := ""
			if len(ms) >= 2 {
				got = ms[1]
			}
			t.Errorf("input %q: got %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestMediaPathUnquotedRe_NoMatch(t *testing.T) {
	tests := []string{
		"no path here",
		"just-a-filename.png",
	}
	for _, input := range tests {
		m := mediaPathUnquotedRe.FindString(input)
		if m != "" {
			t.Errorf("input %q: expected no match, got %q", input, m)
		}
	}
}

func TestMediaPathUnquotedRe_InContext(t *testing.T) {
	// Paths embedded in typical CLI output
	tests := []struct {
		input string
		want  string
	}{
		{"Saved to C:\\Users\\test\\output.png successfully", `C:\Users\test\output.png`},
		{"Created /tmp/screenshot.jpg done", "/tmp/screenshot.jpg"},
		{"File: C:/temp/result.webp (1.2MB)", "C:/temp/result.webp"},
	}
	for _, tt := range tests {
		m := mediaPathUnquotedRe.FindString(tt.input)
		if m != tt.want {
			t.Errorf("input %q: got %q, want %q", tt.input, m, tt.want)
		}
	}
}

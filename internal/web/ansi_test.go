package web

import (
	"strings"
	"testing"
)

func TestANSIHTML(t *testing.T) {
	const esc = "\x1b"
	tests := []struct {
		name, in, want string
	}{
		{"plain text is escaped", `a <b> & "c"`, `a &lt;b&gt; &amp; &#34;c&#34;`},
		{"NestJS line", esc + "[32m[Nest] 7  - " + esc + "[39m11/18/2026 " + esc + "[32m    LOG" + esc + "[39m done",
			`<span class="an af2">[Nest] 7  - </span>11/18/2026 <span class="an af2">    LOG</span> done`},
		{"bold and bright, reset", esc + "[1;91mfail" + esc + "[0m ok", `<span class="an af9 abold">fail</span> ok`},
		{"empty SGR resets", esc + "[33mx" + esc + "[my", `<span class="an af3">x</span>y`},
		{"open span closed at the end", esc + "[4mlink", `<span class="an aund">link</span>`},
		{"background and default", esc + "[41;97m!" + esc + "[49m-", `<span class="an af15 ab1">!</span><span class="an af15">-</span>`},
		{"256 colors map to the nearest", esc + "[38;5;196mred" + esc + "[38;5;2mgreen", `<span class="an af9">red</span><span class="an af2">green</span>`},
		{"true color maps to the nearest", esc + "[38;2;0;0;250mblue", `<span class="an af4">blue</span>`},
		{"colon form", esc + "[38:5:11mx", `<span class="an af11">x</span>`},
		{"cursor movement removed", "a" + esc + "[2K" + esc + "[1Gb", "ab"},
		{"window title and hyperlink removed", esc + "]0;title\x07x" + esc + "]8;;http://e\x1b\\link" + esc + "]8;;" + esc + "\\", "xlink"},
		{"two-byte escape removed", esc + "7a" + esc + "8", "a"},
		{"progress bar: last segment", "10%\r50%\r100% done", "100% done"},
		{"control characters removed, tab kept", "a\x00b\x08c\td\x7f", "abc\td"},
		{"cut sequence at the end", "x" + esc + "[3", "x"},
		{"lone ESC", "x" + esc, "x"},
		{"markup inside a colored span is escaped", esc + "[31m<script>alert(1)</script>", `<span class="an af1">&lt;script&gt;alert(1)&lt;/script&gt;</span>`},
		{"invalid extended color ignored", esc + "[38;5;999mx", "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(ansiHTML(tt.in)); got != tt.want {
				t.Errorf("ansiHTML(%q)\n got  %s\n want %s", tt.in, got, tt.want)
			}
		})
	}
}

// Whatever the input, the output contains no tag other than our spans and
// no raw '<' from the input.
func TestANSIHTMLNoInjection(t *testing.T) {
	for _, in := range []string{
		"\x1b[31m<img src=x onerror=alert(1)>",
		"\x1b[<31m>x", // a '<' inside a CSI parameter
		"\x1b]8;;javascript:alert(1)\x07<a>",
		"<\x1b[0m/span><script>",
	} {
		out := string(ansiHTML(in))
		rest := strings.NewReplacer("</span>", "").Replace(out)
		for i := strings.Index(rest, "<"); i >= 0; i = strings.Index(rest, "<") {
			if !strings.HasPrefix(rest[i:], `<span class="an`) {
				t.Fatalf("ansiHTML(%q) = %s: unexpected markup", in, out)
			}
			rest = rest[i+1:]
		}
	}
}

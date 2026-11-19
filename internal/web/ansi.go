package web

import (
	"html/template"
	"strconv"
	"strings"
)

// ANSI colors in container logs (issue #29). Applications color their
// output with SGR escape sequences ("ESC [ 32 m"). ansiHTML turns them into
// <span> elements with fixed classes (style.css, "Container log colors"):
// no inline style (the CSP forbids it), and every piece of text is
// HTML-escaped, so a log line can never inject markup. All other escape and
// control sequences (cursor movement, window titles, hyperlinks, progress
// bars) are removed.

// ansiPalette are the RGB values of the 16 standard colors (xterm), used
// to map 256-color and true-color codes to the nearest of them.
var ansiPalette = [16][3]int{
	{0, 0, 0}, {205, 0, 0}, {0, 205, 0}, {205, 205, 0}, {0, 0, 238}, {205, 0, 205}, {0, 205, 205}, {229, 229, 229},
	{127, 127, 127}, {255, 0, 0}, {0, 255, 0}, {255, 255, 0}, {92, 92, 255}, {255, 0, 255}, {0, 255, 255}, {255, 255, 255},
}

// sgrState is the text style set by SGR sequences; -1 = default color.
type sgrState struct {
	fg, bg                       int
	bold, dim, italic, underline bool
}

func (s sgrState) plain() bool {
	return s == sgrState{fg: -1, bg: -1}
}

// classes returns the class attribute value of a span with this style.
func (s sgrState) classes() string {
	c := []string{"an"}
	if s.fg >= 0 {
		c = append(c, "af"+strconv.Itoa(s.fg))
	}
	if s.bg >= 0 {
		c = append(c, "ab"+strconv.Itoa(s.bg))
	}
	for _, f := range []struct {
		on   bool
		name string
	}{{s.bold, "abold"}, {s.dim, "adim"}, {s.italic, "aital"}, {s.underline, "aund"}} {
		if f.on {
			c = append(c, f.name)
		}
	}
	return strings.Join(c, " ")
}

// ansiHTML renders one log line as HTML: escaped text, SGR styles as
// spans (all closed at the end of the line), other sequences removed.
func ansiHTML(line string) template.HTML {
	// A carriage return without line feed redraws the line (progress
	// bars): only what was written last is visible in a terminal.
	if i := strings.LastIndexByte(line, '\r'); i >= 0 {
		line = line[i+1:]
	}
	var b strings.Builder
	state := sgrState{fg: -1, bg: -1}
	open := false
	text := func(s string) {
		if s == "" {
			return
		}
		if !open && !state.plain() {
			b.WriteString(`<span class="` + state.classes() + `">`)
			open = true
		}
		b.WriteString(template.HTMLEscapeString(s))
	}
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c == 0x1b: // ESC
			seq, params, final := escapeSequence(line[i:])
			i += seq
			if final == 'm' {
				next := applySGR(state, params)
				if next != state && open {
					b.WriteString("</span>")
					open = false
				}
				state = next
			}
		case c < 0x20 && c != '\t', c == 0x7f: // other control characters
			i++
		default:
			j := i
			for j < len(line) && line[j] != 0x1b && (line[j] >= 0x20 || line[j] == '\t') && line[j] != 0x7f {
				j++
			}
			text(line[i:j])
			i = j
		}
	}
	if open {
		b.WriteString("</span>")
	}
	return template.HTML(b.String()) //nolint:gosec // G203: every text piece is escaped above; only fixed span tags are added
}

// escapeSequence measures the escape sequence at the start of s (s[0] is
// ESC). For a CSI sequence ("ESC [ params final") it returns its
// parameters and final byte; other sequences (OSC "ESC ] ... BEL/ESC \",
// two-byte escapes) have final 0. An incomplete sequence at the end of the
// line (cut lines) is consumed completely.
func escapeSequence(s string) (n int, params string, final byte) {
	if len(s) < 2 {
		return len(s), "", 0
	}
	switch s[1] {
	case '[': // CSI: parameter and intermediate bytes 0x20-0x3f, final byte 0x40-0x7e
		for j := 2; j < len(s); j++ {
			if s[j] >= 0x40 && s[j] <= 0x7e {
				return j + 1, s[2:j], s[j]
			}
			if s[j] < 0x20 || s[j] > 0x3f {
				return j, "", 0 // malformed: drop what was read
			}
		}
		return len(s), "", 0
	case ']', 'P', '_', '^': // OSC, DCS, APC, PM: up to BEL or ST (ESC \)
		for j := 2; j < len(s); j++ {
			if s[j] == 0x07 {
				return j + 1, "", 0
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2, "", 0
			}
		}
		return len(s), "", 0
	default: // two-byte escape (e.g. ESC 7, ESC c)
		return 2, "", 0
	}
}

// applySGR returns state changed by the SGR parameters ("1;32", "" = reset).
func applySGR(state sgrState, params string) sgrState {
	codes := strings.FieldsFunc(params, func(r rune) bool { return r == ';' || r == ':' })
	if len(codes) == 0 {
		codes = []string{"0"}
	}
	num := func(i int) int {
		if i >= len(codes) {
			return -1
		}
		n, err := strconv.Atoi(codes[i])
		if err != nil {
			return -1
		}
		return n
	}
	for i := 0; i < len(codes); i++ {
		switch n := num(i); {
		case n == 0:
			state = sgrState{fg: -1, bg: -1}
		case n == 1:
			state.bold = true
		case n == 2:
			state.dim = true
		case n == 3:
			state.italic = true
		case n == 4:
			state.underline = true
		case n == 22:
			state.bold, state.dim = false, false
		case n == 23:
			state.italic = false
		case n == 24:
			state.underline = false
		case n >= 30 && n <= 37:
			state.fg = n - 30
		case n >= 90 && n <= 97:
			state.fg = n - 90 + 8
		case n == 39:
			state.fg = -1
		case n >= 40 && n <= 47:
			state.bg = n - 40
		case n >= 100 && n <= 107:
			state.bg = n - 100 + 8
		case n == 49:
			state.bg = -1
		case n == 38 || n == 48:
			color, used := extendedColor(num, i+1)
			i += used
			if n == 38 {
				state.fg = color
			} else {
				state.bg = color
			}
		}
	}
	return state
}

// extendedColor reads a 256-color ("5;n") or true-color ("2;r;g;b")
// argument starting at codes[i] and maps it to the nearest of the 16
// colors; it returns the color (-1 if invalid) and the codes it used.
func extendedColor(num func(int) int, i int) (color, used int) {
	switch num(i) {
	case 5:
		n := num(i + 1)
		switch {
		case n < 0 || n > 255:
			return -1, 2
		case n < 16:
			return n, 2
		case n < 232: // 6x6x6 cube
			n -= 16
			level := func(v int) int {
				if v == 0 {
					return 0
				}
				return 55 + v*40
			}
			return nearestColor(level(n/36), level(n/6%6), level(n%6)), 2
		default: // gray ramp
			g := 8 + (n-232)*10
			return nearestColor(g, g, g), 2
		}
	case 2:
		r, g, b := num(i+1), num(i+2), num(i+3)
		if r < 0 || g < 0 || b < 0 || r > 255 || g > 255 || b > 255 {
			return -1, 4
		}
		return nearestColor(r, g, b), 4
	default:
		return -1, 0
	}
}

func nearestColor(r, g, b int) int {
	best, bestDist := 0, -1
	for i, p := range ansiPalette {
		d := (p[0]-r)*(p[0]-r) + (p[1]-g)*(p[1]-g) + (p[2]-b)*(p[2]-b)
		if bestDist < 0 || d < bestDist {
			best, bestDist = i, d
		}
	}
	return best
}

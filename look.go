package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
)

// --- The look: what every command's output for people is made of. ---
//
// One header with the metro-lx mark, cards with rounded edges, line badges, part chips, check rows, and bars, laid out
// in 80 columns. The colors are the web app's (front/src/index.css). A color never goes without its glyph or its word,
// so the output says the same without color. Text that comes from the hub, from `claude` or from files is cleaned
// (clean) before it gets here.

// screenWidth is the width every screen fits in, with normal data; screenEnd is where right-aligned text ends.
const (
	screenWidth = 80
	screenEnd   = 78
)

// rgb is a color of the web app's palette.
type rgb struct{ r, g, b uint8 }

var (
	ink         = rgb{0x12, 0x12, 0x12}
	paper       = rgb{0xff, 0xff, 0xff}
	okGreen     = rgb{0x00, 0x80, 0x1c} // --ok: connected, set up, up to date, ok
	failRed     = rgb{0xe6, 0x00, 0x09} // --danger: failed, blocked
	skillPastel = rgb{0x9e, 0xdf, 0xfd} // --part-skill
	hookPastel  = rgb{0x9e, 0xeb, 0xc0} // --part-hook
	// The metro-lx mark: three strokes, as on the landing.
	markColors = [3]rgb{{0x6f, 0xa3, 0xff}, {0xf2, 0xc5, 0x00}, {0xf9, 0x7c, 0x2b}}
)

// metroLine is a line as the web app draws it.
type metroLine struct {
	route int
	color rgb
	digit rgb  // its badge's digit: paper, or ink on the orange and the yellow
	edged bool // a bar takes the line's darker edge: the yellow alone doesn't show on a light terminal
}

// metroLines are the lines the hub puts plugins on (GET /api/me/installed), by name.
var metroLines = map[string]metroLine{
	"engineering": {1, rgb{0x01, 0x4c, 0xc1}, paper, false},
	"product":     {2, rgb{0xb9, 0x33, 0xad}, paper, false},
	"marketing":   {3, rgb{0xf9, 0x7c, 0x2b}, ink, false},
	"sales":       {4, rgb{0xfb, 0xdc, 0x1a}, ink, true},
	"support":     {5, rgb{0x00, 0x80, 0x1c}, paper, false},
	"everyone":    {6, ink, paper, false},
}

// edge is c at 70% mixed with black: the edge of every line in the web app, which keeps the yellow visible on white.
func (c rgb) edge() rgb {
	dark := func(v uint8) uint8 { return uint8((int(v)*7 + 5) / 10) }
	return rgb{dark(c.r), dark(c.g), dark(c.b)}
}

// xterm256 is the closest of xterm's 256 colors to c: a color of the 6×6×6 cube, or one of the 24 grays.
func xterm256(c rgb) int {
	levels := [6]int{0, 95, 135, 175, 215, 255}
	dist := func(r, g, b int) int {
		dr, dg, db := int(c.r)-r, int(c.g)-g, int(c.b)-b
		return dr*dr + dg*dg + db*db
	}
	nearest := func(v uint8) int {
		best := 0
		for i, l := range levels {
			if abs(int(v)-l) < abs(int(v)-levels[best]) {
				best = i
			}
		}
		return best
	}
	r, g, b := nearest(c.r), nearest(c.g), nearest(c.b)
	best, bestDist := 16+36*r+6*g+b, dist(levels[r], levels[g], levels[b])
	for i := range 24 {
		if v := 8 + 10*i; dist(v, v, v) < bestDist {
			best, bestDist = 232+i, dist(v, v, v)
		}
	}
	return best
}

func abs(n int) int { return max(n, -n) }

// painter colors text for a terminal: with no color, 256 colors, or 24-bit color.
type painter int

const (
	noColor painter = iota
	colors256
	trueColor
)

// colorDepth is how a command colors f: not at all when it isn't a terminal or color isn't allowed, in 24-bit
// when COLORTERM says the terminal has it, else in xterm's 256 colors. Every command decides here.
func colorDepth(f *os.File) painter {
	info, err := f.Stat()
	switch ct := os.Getenv("COLORTERM"); {
	case err != nil || info.Mode()&os.ModeCharDevice == 0 || !colorAllowed():
		return noColor
	case ct == "truecolor" || ct == "24bit":
		return trueColor
	}
	return colors256
}

// colorAllowed is false when NO_COLOR (https://no-color.org) or TERM=dumb is set.
func colorAllowed() bool { return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" }

func (p painter) paint(code, s string) string {
	if p == noColor || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p painter) bold(s string) string  { return p.paint("1", s) }
func (p painter) dim(s string) string   { return p.paint("2", s) }
func (p painter) green(s string) string { return p.fg(okGreen, s) }
func (p painter) red(s string) string   { return p.fg(failRed, s) }

// color is c as a foreground (layer 38) or background (48), at p's depth.
func (p painter) color(layer int, c rgb) string {
	if p == trueColor {
		return fmt.Sprintf("%d;2;%d;%d;%d", layer, c.r, c.g, c.b)
	}
	return fmt.Sprintf("%d;5;%d", layer, xterm256(c))
}

func (p painter) fg(c rgb, s string) string { return p.paint(p.color(38, c), s) }

// on draws s in fg on bg, as a chip.
func (p painter) on(bg, fg rgb, s string) string {
	return p.paint(p.color(48, bg)+";"+p.color(38, fg), s)
}

// --- Segments and rows ---

// seg is a piece of a line: its text, and how it's painted (nil: as is).
type seg struct {
	text  string
	paint func(string) string
}

func plain(s string) seg { return seg{s, nil} }

func (s seg) String() string {
	if s.paint == nil {
		return s.text
	}
	return s.paint(s.text)
}

func segsWidth(segs []seg) int {
	n := 0
	for _, s := range segs {
		n += cols(s.text)
	}
	return n
}

// row builds one line, column by column.
type row struct {
	b strings.Builder
	n int // the columns it takes so far
}

func (r *row) add(segs ...seg) *row {
	for _, s := range segs {
		r.b.WriteString(s.String())
		r.n += cols(s.text)
	}
	return r
}

// at puts segs at column col: one space when the row is already past it.
func (r *row) at(col int, segs ...seg) *row {
	r.b.WriteString(strings.Repeat(" ", max(0, col-r.n)))
	if r.n > col && r.n > 0 {
		r.b.WriteString(" ")
		r.n++
	}
	r.n = max(r.n, col)
	return r.add(segs...)
}

// right puts segs so they end at column end, at least two spaces after what's there.
func (r *row) right(end int, segs ...seg) *row {
	gap := 0
	if r.n > 0 {
		gap = 2
	}
	return r.at(max(end-segsWidth(segs), r.n+gap), segs...)
}

func (r *row) String() string { return r.b.String() }

// flow lays words out in lines of at most width columns, breaking at spaces. A seg that starts with a space starts
// a word; one that doesn't sticks to the word before it, so "v1.0.0" and " → " keep their own paint. The words of one
// seg on one line stay one seg, painted once.
func flow(segs []seg, width int) [][]seg {
	type piece struct {
		from int // the seg it comes from
		seg
	}
	var words [][]piece
	for from, s := range segs {
		for i, w := range strings.Split(s.text, " ") {
			switch {
			case w == "":
				continue
			case i == 0 && len(words) > 0 && !strings.HasPrefix(s.text, " "):
				words[len(words)-1] = append(words[len(words)-1], piece{from, seg{w, s.paint}})
			default:
				words = append(words, []piece{{from, seg{w, s.paint}}})
			}
		}
	}
	var lines [][]seg
	var cur []piece
	used := 0
	end := func() {
		line := make([]seg, len(cur))
		for i, p := range cur {
			line[i] = p.seg
		}
		lines, cur, used = append(lines, line), nil, 0
	}
	for _, w := range words {
		n := 0
		for _, p := range w {
			n += cols(p.text)
		}
		if used > 0 && used+1+n > width {
			end()
		}
		switch {
		case used > 0 && cur[len(cur)-1].from == w[0].from:
			cur[len(cur)-1].text += " " + w[0].text
			cur = append(cur, w[1:]...)
		case used > 0:
			cur = append(cur, piece{-1, plain(" ")})
			cur = append(cur, w...)
		default:
			cur = append(cur, w...)
		}
		used += n + min(used, 1)
	}
	if len(cur) > 0 {
		end()
	}
	return lines
}

// paragraph is text from column col, wrapped to the screen.
func paragraph(col int, segs ...seg) []string {
	var out []string
	for _, l := range flow(segs, screenWidth-col) {
		out = append(out, new(row).at(col, l...).String())
	}
	return out
}

// --- The pieces ---

// header is the first line of a screen: the metro-lx mark, the title, and on the right, ending at column 78, who or
// what it's about. writeHeader puts a blank line before it, between the command and its output.
func header(c painter, title, right []seg) string {
	r := new(row).at(2)
	for _, m := range markColors {
		r.add(seg{"▌", func(s string) string { return c.fg(m, s) }})
	}
	r.add(plain(" "), seg{"metro-lx", c.bold})
	if len(title) > 0 {
		r.add(plain("  ")).add(title...)
	}
	if len(right) > 0 {
		r.right(screenEnd, right...)
	}
	return r.String()
}

func writeHeader(w io.Writer, c painter, title, right []seg) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, header(c, title, right))
}

// printLines writes lines on stdout, one each.
func printLines(lines []string) {
	for _, l := range lines {
		fmt.Println(l)
	}
}

// badge is a plugin's route on its line's color, 3 columns: " 1 " ((1) without color), ◎ for Claude Code's bundle,
// which isn't a team's line, and blank for a plugin the hub gives no line.
func (p painter) badge(line string, bundle bool) seg {
	l, ok := metroLines[line]
	switch {
	case bundle:
		return plain(" ◎ ")
	case !ok:
		return plain("   ")
	case p == noColor:
		return plain(fmt.Sprintf("(%d)", l.route))
	}
	code := "1;" + p.color(48, l.color) + ";" + p.color(38, l.digit)
	return seg{fmt.Sprintf(" %d ", l.route), func(s string) string { return p.paint(code, s) }}
}

// stroke paints s in a line's color, for a bar: the yellow with its darker edge, and the ink line (and no line) in the
// terminal's own text color, which is ink on a light terminal and still shows on a dark one.
func (p painter) stroke(line string) func(string) string {
	l, ok := metroLines[line]
	switch {
	case !ok || l.color == ink:
		return nil
	case l.edged:
		return func(s string) string { return p.fg(l.color.edge(), s) }
	}
	return func(s string) string { return p.fg(l.color, s) }
}

// chip is a skill's or a hook's sign, 3 columns: the part's shape in ink on its pastel, as on the website.
func (p painter) chip(kind string) seg {
	glyph, bg := " ● ", skillPastel
	if kind == "hook" {
		glyph, bg = " ▲ ", hookPastel
	}
	return seg{glyph, func(s string) string { return p.on(bg, ink, s) }}
}

// A check's outcome: its mark at the head of its row.
const (
	checkOK   = iota // ✓ green
	checkFail        // ✗ red
	checkWarn        // ! bold
	checkNone        // · dim: nothing to check, such as an agent that isn't here
)

func (p painter) mark(kind int) seg {
	switch kind {
	case checkOK:
		return seg{"✓", p.green}
	case checkFail:
		return seg{"✗", p.red}
	case checkWarn:
		return seg{"!", p.bold}
	}
	return seg{"·", p.dim}
}

// checkCol is where a check row's detail starts: after "✓  Claude Code  ".
const checkCol = 19

// checkRow is one check: its mark, its bold label, and its detail from checkCol, wrapped to the screen.
func checkRow(c painter, kind int, label string, detail ...seg) []string {
	return markedRow(c.mark(kind), c.bold, label, detail...)
}

func markedRow(mark seg, paint func(string) string, label string, detail ...seg) []string {
	head := new(row).at(2, mark).at(5, seg{label, paint})
	col := max(checkCol, head.n+2)
	lines := flow(detail, screenWidth-col)
	if len(lines) == 0 {
		return []string{head.String()}
	}
	out := []string{head.at(col, lines[0]...).String()}
	for _, l := range lines[1:] {
		out = append(out, new(row).at(col, l...).String())
	}
	return out
}

// fixRow is what to do about the check above it: a dim arrow and the fix, under its detail.
func fixRow(c painter, col int, fix string) []string {
	return paragraph(col, seg{"→", c.dim}, plain(" "+fix))
}

// noteRow is a dim line of help under a check, at its detail's column.
func noteRow(c painter, col int, text string) []string { return paragraph(col, seg{text, c.dim}) }

// card is a box with rounded edges: its title and its state in the top border, then its rows.
type card struct {
	title string
	state []seg
	rows  []*row
}

// cardInner is the narrowest a card's inside is: the card then fills 78 columns.
const cardInner = 70

// innerWidth is the inside width a card needs: its rows, and its title with its state.
func (k card) innerWidth() int {
	n := cols(k.title) + segsWidth(k.state) + 3
	for _, r := range k.rows {
		n = max(n, r.n)
	}
	return n
}

// render writes the card with inner columns inside: 2 of margin, the border, 2 of padding each side.
func (k card) render(w io.Writer, c painter, inner int) {
	dashes := inner + 6 - 8 - cols(k.title) - segsWidth(k.state)
	top := new(row).at(2, seg{"╭─ ", c.dim}, seg{k.title, c.bold}, plain(" "), seg{strings.Repeat("─", max(1, dashes)), c.dim}, plain(" "))
	fmt.Fprintln(w, top.add(k.state...).add(seg{" ─╮", c.dim}).String())
	side := func(r *row) {
		fmt.Fprintln(w, "  "+c.dim("│")+"  "+r.String()+strings.Repeat(" ", max(0, inner-r.n))+"  "+c.dim("│"))
	}
	side(new(row))
	for _, r := range k.rows {
		side(r)
	}
	side(new(row))
	fmt.Fprintln(w, "  "+c.dim("╰"+strings.Repeat("─", inner+4)+"╯"))
}

// renderCards writes cards one under the other, a blank line apart, all as wide as the widest.
func renderCards(w io.Writer, c painter, cards []card) {
	inner := cardInner
	for _, k := range cards {
		inner = max(inner, k.innerWidth())
	}
	for i, k := range cards {
		if i > 0 {
			fmt.Fprintln(w)
		}
		k.render(w, c, inner)
	}
}

// eighths draws a bar of n out of most in cells columns at most, to an eighth of a cell: at least an eighth.
func eighths(n, most, cells int) string {
	if most <= 0 {
		return ""
	}
	e := max(1, int(float64(n)/float64(most)*float64(cells*8)+0.5))
	bar := strings.Repeat("█", e/8)
	if e%8 > 0 {
		bar += string([]rune("▏▎▍▌▋▊▉")[e%8-1])
	}
	return bar
}

// --- The spinner ---

// spinner turns a braille wheel at the head of one check row while its work runs. It only runs on a terminal with
// color: elsewhere it draws nothing, and the output has no carriage return.
type spinner struct {
	c      painter
	w      io.Writer
	label  string
	mu     sync.Mutex
	detail string
	done   chan struct{}
	ended  sync.WaitGroup
}

var spinFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// spin starts the wheel for label, or does nothing when on is false.
func spin(w io.Writer, c painter, on bool, label string) *spinner {
	s := &spinner{c: c, w: w, label: label}
	if !on {
		return s
	}
	s.done = make(chan struct{})
	s.ended.Add(1)
	go func() {
		defer s.ended.Done()
		tick := time.NewTicker(80 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			s.draw(spinFrames[i%len(spinFrames)])
			select {
			case <-s.done:
				fmt.Fprint(s.w, "\r\x1b[K")
				return
			case <-tick.C:
			}
		}
	}()
	return s
}

// say changes what the row says the work is doing.
func (s *spinner) say(detail string) {
	s.mu.Lock()
	s.detail = detail
	s.mu.Unlock()
}

func (s *spinner) draw(frame rune) {
	s.mu.Lock()
	detail := s.detail
	s.mu.Unlock()
	r := new(row).at(2, seg{string(frame), s.c.bold}).at(5, seg{s.label, s.c.bold})
	if detail != "" {
		r.at(checkCol, plain(fit(detail, screenWidth-checkCol-1)), seg{"…", s.c.dim})
	}
	fmt.Fprint(s.w, "\r"+r.String()+"\x1b[K")
}

// stop clears the row, so what the work did can take its place.
func (s *spinner) stop() {
	if s.done == nil {
		return
	}
	close(s.done)
	s.ended.Wait()
	s.done = nil
}

// --- Text in columns ---

// fit cuts s to n columns, the last one an ellipsis when it was longer.
func fit(s string, n int) string {
	if cols(s) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := runeCols(r)
		if used+w > n-1 {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String() + "…"
}

// fitWords cuts s to n columns after a whole word, with an ellipsis, or inside one when the first word is too long.
func fitWords(s string, n int) string {
	if cols(s) <= n {
		return s
	}
	cut := ""
	for i, r := range s {
		if r == ' ' && cols(s[:i])+1 <= n {
			cut = s[:i]
		}
	}
	if cut == "" {
		return fit(s, n)
	}
	return cut + "…"
}

// pad fills s with spaces to n columns (fmt's %-*s counts bytes, not columns).
func pad(s string, n int) string {
	return s + strings.Repeat(" ", max(0, n-cols(s)))
}

// cols is how many columns s takes in a terminal: a wide character (CJK, most emoji) two, a combining mark none.
func cols(s string) int {
	n := 0
	for _, r := range s {
		n += runeCols(r)
	}
	return n
}

func runeCols(r rune) int {
	switch {
	case unicode.In(r, unicode.Mn, unicode.Me):
		return 0
	case wide(r):
		return 2
	}
	return 1
}

// wide is true for the East Asian wide and fullwidth blocks, and the emoji shown wide.
func wide(r rune) bool {
	switch {
	case r < 0x1100:
		return false
	case r <= 0x115F, // Hangul Jamo
		0x2E80 <= r && r <= 0x303E,                             // CJK radicals, symbols and punctuation
		0x3041 <= r && r <= 0x33FF,                             // kana, CJK compatibility
		0x3400 <= r && r <= 0x4DBF, 0x4E00 <= r && r <= 0x9FFF, // CJK ideographs
		0xA000 <= r && r <= 0xA4CF,                             // Yi
		0xAC00 <= r && r <= 0xD7A3,                             // Hangul syllables
		0xF900 <= r && r <= 0xFAFF,                             // CJK compatibility ideographs
		0xFE30 <= r && r <= 0xFE4F,                             // CJK compatibility forms
		0xFF00 <= r && r <= 0xFF60, 0xFFE0 <= r && r <= 0xFFE6, // fullwidth forms
		0x1F300 <= r && r <= 0x1F64F, 0x1F680 <= r && r <= 0x1F6FF, 0x1F900 <= r && r <= 0x1F9FF, // emoji
		0x1FA70 <= r && r <= 0x1FAFF,
		0x20000 <= r && r <= 0x3FFFD: // CJK extensions
		return true
	}
	return false
}

// tilde writes a path under the home folder as ~/….
func tilde(path string) string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" && strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}

// hostOf is a hub's URL without its https:// scheme, as people read it.
func hostOf(hub string) string { return strings.TrimPrefix(hub, "https://") }

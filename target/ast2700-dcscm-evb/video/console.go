// Framebuffer text console for the AST2700 display. Ported from the cmd/nats
// validation harness (console.go).

//go:build ast2700video

package video

import (
	_ "unsafe"

	"github.com/kyanitecomputer/aspeed-go/hal/framebuffer"
	"github.com/usbarmory/tamago/soc/aspeed/ast2700"
)

const (
	fontW    = 8 // glyph width in pixels
	fontH    = 8 // glyph height in pixels
	conScale = 1 // integer pixel scaling for each glyph
	tabWidth = 8
)

var (
	conFG = framebuffer.RGB(0xFF, 0xFF, 0xFF) // white text
	conBG = framebuffer.RGB(0x00, 0x00, 0x00) // black background
)

// con is the active framebuffer console. It is nil until Init brings up the
// display, so early-boot printk output still reaches the UART safely.
var con *fbConsole

// fbConsole renders a scrolling text console onto a linear framebuffer.
type fbConsole struct {
	fb           *framebuffer.Framebuffer
	cellW, cellH int // glyph cell size including scaling
	cols, rows   int // console dimensions in cells
	cx, cy       int // cursor position in cells
}

// newFBConsole creates a console covering fb using the bundled 8x8 font.
func newFBConsole(fb *framebuffer.Framebuffer, scale int) *fbConsole {
	if scale < 1 {
		scale = 1
	}
	cw := fontW * scale
	ch := fontH * scale
	return &fbConsole{
		fb:    fb,
		cellW: cw,
		cellH: ch,
		cols:  fb.Width / cw,
		rows:  fb.Height / ch,
	}
}

// writeByte renders one byte, handling newline, carriage return, tab and
// backspace control characters.
func (c *fbConsole) writeByte(b byte) {
	switch b {
	case '\n':
		c.newline()
	case '\r':
		c.cx = 0
	case '\t':
		for n := tabWidth - (c.cx % tabWidth); n > 0; n-- {
			c.putGlyph(' ')
		}
	case '\b':
		if c.cx > 0 {
			c.cx--
		}
	default:
		if b < 0x20 || b > 0x7E {
			return
		}
		c.putGlyph(b)
	}
}

// putGlyph draws a printable byte at the cursor and advances it, wrapping and
// scrolling as needed.
func (c *fbConsole) putGlyph(b byte) {
	if c.cx >= c.cols {
		c.newline()
	}
	c.drawCell(c.cx, c.cy, b)
	c.cx++
}

// newline moves the cursor to the start of the next row, scrolling when the
// bottom row is reached.
func (c *fbConsole) newline() {
	c.cx = 0
	c.cy++
	if c.cy >= c.rows {
		c.scroll()
		c.cy = c.rows - 1
	}
}

// drawCell paints a single glyph cell, including its black background, then
// flushes the touched scanlines so the (non-coherent) GFX scanout sees them.
func (c *fbConsole) drawCell(col, row int, b byte) {
	glyph := font8x8[b-0x20]
	px := col * c.cellW
	py := row * c.cellH
	scale := c.cellW / fontW

	for gy := 0; gy < fontH; gy++ {
		bits := glyph[gy]
		for gx := 0; gx < fontW; gx++ {
			color := conBG
			if bits&(1<<uint(gx)) != 0 {
				color = conFG
			}
			c.fb.FillRect(px+gx*scale, py+gy*scale, scale, scale, color)
		}
	}
	c.flushRows(py, c.cellH)
}

// scroll shifts the framebuffer up by one text row and clears the new bottom
// row, then flushes the whole buffer.
func (c *fbConsole) scroll() {
	stride := c.fb.Stride
	shift := c.cellH * stride
	buf := c.fb.Buf
	copy(buf, buf[shift:])
	for i := len(buf) - shift; i < len(buf); i++ {
		buf[i] = 0 // black in XRGB8888
	}
	ast2700.ARM.CleanDataCacheRange(c.fb.Addr, uintptr(len(buf)))
}

// flushRows cleans the data cache for the scanlines starting at pixel row y for
// h rows so the GFX scanout DMA observes the freshly written pixels.
func (c *fbConsole) flushRows(y, h int) {
	off := y * c.fb.Stride
	ast2700.ARM.CleanDataCacheRange(c.fb.Addr+uintptr(off), uintptr(h*c.fb.Stride))
}

// printk routes runtime console output to UART12 (matching the board default)
// and mirrors it onto the framebuffer console once the display is up. It
// replaces the board package's printk, which must be disabled with the
// `linkprintk` build tag when this console is compiled in.
//
//go:linkname printk runtime/goos.Printk
func printk(b byte) {
	if b == '\n' {
		ast2700.UART12.Tx('\r')
	}
	ast2700.UART12.Tx(b)

	if con != nil {
		con.writeByte(b)
	}
}

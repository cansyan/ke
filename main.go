package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cansyan/kero"
	"github.com/mattn/go-runewidth"
)

// parsePathArg parses an argument of the form "path", "path:row", or
// "path:row:col". row and col are 1-based and converted to 0-based.
func parsePathArg(arg string) (path string, row, col int) {
	parts := strings.Split(arg, ":")
	if len(parts) <= 1 {
		return arg, 0, 0
	}

	path = parts[0]
	if len(parts) > 1 {
		var errRow error
		row, errRow = strconv.Atoi(parts[1])
		if errRow != nil || row < 1 {
			return path, 0, 0
		}
	}

	if len(parts) > 2 {
		var errCol error
		col, errCol = strconv.Atoi(parts[2])
		if errCol != nil || col < 1 {
			return path, row - 1, 0
		}
	}
	return path, row - 1, col - 1
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	var path string
	var row, col int
	if len(os.Args) > 1 {
		path, row, col = parsePathArg(os.Args[1])
	}

	e := &Editor{}
	err := e.OpenFile(path)
	if err != nil {
		log.Fatal(err)
	}
	e.View().Cursor = e.Buf().Clamp(Position{Row: row, Col: col})

	f, err := os.OpenFile("/tmp/ke.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	log.SetOutput(f)

	// set FPS for refreshing diagnostic
	p := kero.New(e, kero.WithAltScreen(true), kero.WithKitty(true), kero.WithFPS(3), kero.WithMouse(true))
	if err := p.Run(); err != nil {
		log.Print(err)
	}
}

// Position represents a zero-indexed coordinate inside Buffer.
//
// In terminal UI rendering, double-width characters
// (like CJK characters or emojis) occupy 2 terminal columns despite being 1 rune.
// Separating Byte Offset (storage), Rune Offset (character count),
// and Visual Display Column (screen cell width) prevents layout corruption.
type Position struct {
	Row int // line index (0-based)
	Col int // byte offset within line (0-based)
}

// View holds UI-specific state for a single window/viewport.
// Responsible strictly for displaying Buffer lines and handling cursor movement inside text
type View struct {
	Buf    *Buffer
	Cursor Position

	// Viewport
	ScrollRow int // Topmost visible line index (0-based)
	ScrollCol int // Leftmost visible visual column (0-based)
	Width     int // Viewport width in terminal cells
	Height    int // Viewport height in terminal rows

	Selecting bool
	SelAnchor Position // selection at [e.view().SelAnchor, e.pos)
}

// SetSize updates the viewport dimensions and re-clamps scrolling.
func (v *View) SetSize(width, height int) {
	v.Width = width
	v.Height = height
	v.ShowCursorSmart()
}

// Editor implements kero.App interface
type Editor struct {
	views  []*View
	active int // index of active view

	message string

	// prompt for saving
	saveAs    bool
	saveInput TextInput

	// find mode opens a find line at the message area
	finding        bool
	findBlur       bool
	replacing      bool
	findInput      TextInput
	replaceInput   TextInput
	findMatch      bool
	findMatchStart Position
	findMatchEnd   Position

	palette Palette

	clipboard  string
	clipIsLine bool

	// optional: record the time of last key, make it expire after a while
	lastEvent kero.Event

	diags    []*scanner.Error
	diagChan chan []*scanner.Error

	// reports whether the key comes from a paste action,
	// to distinguish the manual KeyEnter or a pasted \n
	pasting bool

	jumps JumpList

	completion Completion

	renaming    bool
	renameInput TextInput

	ref ReferencesPanel
}

// View returns the currently focused View.
func (e *Editor) View() *View {
	if len(e.views) == 0 || e.active < 0 || e.active >= len(e.views) {
		return nil
	}
	return e.views[e.active]
}

// Buf is a convenient shortcut to the active View's Buffer.
func (e *Editor) Buf() *Buffer {
	v := e.View()
	if v == nil {
		return nil
	}
	return v.Buf
}

func (e *Editor) Init(ctx *kero.Context) error {
	e.diagChan = make(chan []*scanner.Error, 1)
	e.diagnose()
	return nil
}

func (e *Editor) LastEvent() string {
	if e.lastEvent == nil {
		return ""
	}
	if s, ok := e.lastEvent.(fmt.Stringer); ok {
		return s.String()
	}
	return fmt.Sprintf("%T", e.lastEvent)
}

func (e *Editor) Update(ctx *kero.Context, ev kero.Event) error {
	// refresh diagnostic as soon as possible
	e.applyDiagnostic()
	v := e.View()
	if v.Width == 0 || v.Height == 0 {
		_, textRect, _, _ := LayoutWindow(ctx.Width, ctx.Height, len(e.Buf().Lines))
		v.SetSize(textRect.W, textRect.H)
	}

	switch ev := ev.(type) {
	case kero.TickEvent:
		return nil
	case kero.ResizeEvent:
		_, textRect, _, _ := LayoutWindow(ev.Width, ev.Height, len(e.Buf().Lines))
		for _, v := range e.views {
			v.SetSize(textRect.W, textRect.H)
		}
	case kero.PasteStartEvent:
		e.pasting = true
	case kero.PasteEndEvent:
		e.pasting = false
	case kero.MouseEvent:
		e.handleMouse(ctx, ev)
	case kero.KeyEvent:
		e.handleKey(ctx, ev)
	}
	e.lastEvent = ev
	return nil
}

// convert mouse (x, y) to Positon of Buffer
func (e *Editor) mouseToPosition(m kero.MouseEvent, textRect kero.Rect) Position {
	row := m.Y - textRect.Y + e.View().ScrollRow
	if row >= len(e.Buf().Lines) {
		// out of viewport
		return e.View().Cursor
	}

	visualCol := m.X - textRect.X + e.View().ScrollCol
	col := e.Buf().VisualToByteCol(row, visualCol, 4)
	return Position{Row: row, Col: col}
}

func (e *Editor) handleMouse(ctx *kero.Context, m kero.MouseEvent) error {
	_, textRect, _, _ := LayoutWindow(ctx.Width, ctx.Height, len(e.Buf().Lines))
	paletteRect := LayoutPalatte(ctx.Width, ctx.Height, e.palette.VisibleRows()+1)
	point := kero.Point{X: m.X, Y: m.Y}
	switch m.Button {
	case kero.MouseWheelUp:
		if e.ref.Active {
			e.ref.ScrollRow = max(0, e.ref.ScrollRow-1)
			return nil
		}
		if textRect.Contains(point) {
			p := e.palette
			if p.Active && paletteRect.Contains(point) {
				e.palette.Offset = max(0, p.Offset-1)
				return nil
			}
			e.View().ScrollRow = max(0, e.View().ScrollRow-1)
			return nil
		}
	case kero.MouseWheelDown:
		if e.ref.Active {
			e.ref.ScrollRow = min(e.ref.ScrollRow+1, len(e.ref.Items)-e.ref.VisibleRows())
			return nil
		}
		if textRect.Contains(point) {
			p := e.palette
			if p.Active && paletteRect.Contains(kero.Point{X: m.X, Y: m.Y}) {
				e.palette.Offset = min(p.Offset+1, len(p.Items)-p.VisibleRows())
				return nil
			}
			e.View().ScrollRow = min(e.View().ScrollRow+1, len(e.Buf().Lines)-textRect.H)
			return nil
		}
	case kero.MouseLeft:
		switch m.Action {
		case kero.MousePress:
			if e.palette.Active {
				if paletteRect.Contains(kero.Point{X: m.X, Y: m.Y}) {
					index := m.Y - paletteRect.Y - 1 + e.palette.Offset
					if index < 0 || index >= len(e.palette.Items) {
						return nil
					}
					action := e.palette.Items[index].Action
					e.palette.Close()
					action(e)
					return nil
				}
				// close palette overlay when click outside
				e.palette.Close()
			}

			if e.ref.Active {
				if rect := LayoutRefPanel(ctx.Width, ctx.Height); rect.Contains(point) {
					index := m.Y - rect.Y - 1 + e.ref.ScrollRow // minus 1 for header
					if index < 0 || index >= len(e.ref.Items) {
						return nil
					}
					e.ref.Index = index
					e.gotoLocation(e.ref.Items[index])
					return nil
				}
			}

			if textRect.Contains(point) {
				e.View().Cursor = e.mouseToPosition(m, textRect)
				if e.hasSelect() {
					e.clearSelect()
				}
				if e.finding {
					// focus out
					e.findBlur = true
				}
				return nil
			}

		case kero.MouseRelease:
			if textRect.Contains(point) {
				// ctrl+mouse_left_release goto definition
				if m.Mod == kero.ModCtrl {
					GotoDefinition(e)
				}
				return nil
			}
		case kero.MouseDrag:
			if last, ok := e.lastEvent.(kero.MouseEvent); ok &&
				last.Button == kero.MouseLeft && last.Action == kero.MousePress {
				// first drag sets selection anchor
				e.View().Selecting = true
				e.View().SelAnchor = e.mouseToPosition(last, textRect)
			}
			// later drag expands selection
			e.View().Cursor = e.mouseToPosition(m, textRect)
			e.View().ShowCursorSmart()
		}
	}
	return nil
}

func (e *Editor) handleKey(ctx *kero.Context, key kero.KeyEvent) error {
	// can quit at anytime, first priority
	if key.String() == "ctrl+q" {
		quitAgain := e.LastEvent() == "ctrl+q"
		if e.Buf().Dirty && !quitAgain {
			e.message = "warn: unsaved changes, press ctrl+s to save or ctrl+q again to quit"
			return nil
		}
		ctx.Quit()
		return nil
	}

	defer e.View().ShowCursorSmart()
	if e.saveAs {
		return e.updateSaveAs(key)
	}
	if e.finding && !e.findBlur {
		return e.updateFind(key)
	}
	if e.palette.Active {
		e.updatePalette(key)
		return nil
	}
	if e.renaming {
		e.updateRename(key)
		return nil
	}

	var completing bool // mark whether showing completion on keystroke
	defer func() {
		e.completion.Active = completing
	}()

	v := e.View()
	buf := e.Buf()
	switch key.Key {
	case kero.KeyRune:
		switch key.String() {
		case "ctrl+n":
			start, end := buf.WordBounds(buf.PrevRunePos(v.Cursor))
			word := buf.TextRange(start, end)
			c := &e.completion
			c.Refresh(buf.NewReader(), word)
			if len(c.Items) == 1 {
				// only 1 candidates, apply it early
				cursor := buf.Delete(start, end)
				v.Cursor = buf.Insert(cursor, c.Items[c.Index].Name)
				e.markDirty()
				return nil
			}
			completing = len(c.Items) > 0
		case "ctrl+-":
			e.JumpBack()
		case "ctrl+shift+-", "ctrl+_":
			e.JumpForward()
		case "ctrl+w":
			if buf.Dirty && !(e.LastEvent() == "ctrl+w") {
				e.message = "warn: unsaved changes, press ctrl+s to save or ctrl+w again to close"
				return nil
			}
			if len(e.views) <= 1 {
				ctx.Quit()
				return nil
			}
			e.CloseBuffer()
		case "ctrl+r":
			e.palette.Open(e, "@")
		case "ctrl+g":
			GotoDefinition(e)
			return nil
		case "ctrl+p":
			e.palette.Open(e, "")
			return nil
		case "ctrl+shift+p":
			e.palette.Open(e, "/")
			return nil
		case "ctrl+[":
			e.gotoPrevDiag()
			return nil
		case "ctrl+]":
			e.gotoNextDiag()
			return nil
		case "ctrl+shift+]", "ctrl+}":
			if len(e.ref.Items) <= 1 {
				return nil
			}
			e.gotoLocation(e.ref.Next())
		case "ctrl+shift+[", "ctrl+{":
			if len(e.ref.Items) <= 1 {
				return nil
			}
			e.gotoLocation(e.ref.Prev())
		case "ctrl+s":
			if err := e.save(); err != nil {
				return err
			}
			// diagnostic doesn't reflect the buffer changes
			e.diagnose()
			return nil
		case "ctrl+f":
			e.startFind()
			return nil
		case "ctrl+c":
			e.copy()
			return nil
		case "ctrl+x":
			e.cut()
			return nil
		case "ctrl+v":
			e.paste()
			return nil
		case "ctrl+d":
			if e.hasSelect() {
				// duplicate selection not implemented yet
				return nil
			}
			if start, end := buf.WordBounds(v.Cursor); start != end {
				v.Selecting = true
				v.SelAnchor = start
				v.Cursor = end
			}
			return nil
		case "ctrl+l":
			// select the line under cursor
			e.selectLine()
			return nil
		case "ctrl+k":
			if e.hasSelect() {
				e.deleteSelect()
				return nil
			}
			v.Cursor = buf.Delete(v.Cursor, buf.LineEnd(v.Cursor))
			e.markDirty()
			return nil
		case "ctrl+a":
			p := buf.LineStartNonSpace(v.Cursor)
			if v.Cursor == p {
				v.Cursor.Col = 0
				return nil
			}
			v.Cursor = p
		case "ctrl+e":
			v.Cursor = buf.LineEnd(v.Cursor)
		}

		if key.Mod != 0 {
			break
		}
		if e.hasSelect() {
			e.deleteSelect()
		}
		v.Cursor = buf.Insert(v.Cursor, string([]rune{key.Rune}))
		e.markDirty()
		if e.completion.Active {
			start, end := buf.WordBounds(buf.PrevRunePos(v.Cursor))
			word := buf.TextRange(start, end)
			e.completion.Refresh(buf.NewReader(), word)
			completing = len(e.completion.Items) > 0
		}
	case kero.KeyEnter:
		if e.completion.Active {
			start, end := buf.WordBounds(buf.PrevRunePos(v.Cursor))
			cursor := buf.Delete(start, end)
			v.Cursor = buf.Insert(cursor, e.completion.Items[e.completion.Index].Name)
			e.markDirty()
			return nil
		}

		if e.hasSelect() {
			e.deleteSelect()
		}

		if e.pasting {
			v.Cursor = buf.Insert(v.Cursor, "\n")
			return nil
		}

		// compute indentation
		getIndent := func(line []byte, col int) string {
			if len(line) == 0 || col == 0 {
				return ""
			}
			// get indentation before the column
			var n int
			for n <= len(line) {
				r, size := utf8.DecodeRune(line[:col])
				if !unicode.IsSpace(r) {
					break
				}
				n += size
			}
			indent := string(line[:n])
			// indent on block start
			if col == len(line) && line[col-1] == '{' {
				indent += "\t"
			}
			return indent
		}
		// newline retain the previous line's indentation
		switch key.String() {
		case "ctrl+enter":
			// insert newline below
			line := buf.Lines[v.Cursor.Row]
			indent := getIndent(line, len(line))
			p := Position{Row: v.Cursor.Row, Col: len(line)}
			v.Cursor = buf.Insert(p, "\n"+indent)
		case "shift+enter":
			// insert newline above
			var prevIndent string
			if v.Cursor.Row > 0 {
				prevLine := buf.Lines[v.Cursor.Row-1]
				prevIndent = getIndent(prevLine, len(prevLine))
			}
			buf.Insert(Position{Row: v.Cursor.Row, Col: 0}, "\n")
			v.Cursor = buf.Insert(Position{Row: v.Cursor.Row, Col: 0}, prevIndent)
		default:
			// insert newline under cursor
			line := buf.Lines[v.Cursor.Row]
			indent := getIndent(line, v.Cursor.Col)
			v.Cursor = buf.Insert(v.Cursor, "\n"+string(indent))
		}
		e.markDirty()
	case kero.KeyTab:
		if e.completion.Active {
			start, end := buf.WordBounds(buf.PrevRunePos(v.Cursor))
			cursor := buf.Delete(start, end)
			v.Cursor = buf.Insert(cursor, e.completion.Items[e.completion.Index].Name)
			e.markDirty()
			break
		}

		if key.Mod&kero.ModShift != 0 {
			e.unindentSelectOrLine()
			break
		}
		if e.hasSelect() {
			start, end := orderPos(v.SelAnchor, v.Cursor)
			if start.Row != end.Row {
				e.indentSelect()
				break
			}
			e.deleteSelect()
		}
		v.Cursor = buf.Insert(v.Cursor, "\t")
		e.markDirty()
	case kero.KeyBackspace:
		if e.hasSelect() {
			e.deleteSelect()
			break
		}
		switch key.String() {
		case "ctrl+backspace", "cmd+backspace":
			// delete to line start
			v.Cursor = buf.Delete(Position{Row: v.Cursor.Row, Col: 0}, v.Cursor)
		case "alt+backspace":
			// delete word backwards
			prev := buf.WordStart(v.Cursor)
			v.Cursor = buf.Delete(prev, v.Cursor)
		case "cmd+shift+backspace":
			// delete whole line
			v.Cursor = buf.Delete(Position{Row: v.Cursor.Row}, Position{Row: v.Cursor.Row + 1})
		default:
			v.Cursor = buf.Delete(buf.PrevRunePos(v.Cursor), v.Cursor)
		}
		e.markDirty()
	case kero.KeyDelete:
		v.Cursor = buf.Delete(v.Cursor, buf.NextRunePos(v.Cursor))
		e.markDirty()
	case kero.KeyLeft:
		switch key.String() {
		case "alt+left":
			v.Cursor = buf.WordStart(v.Cursor)
		case "cmd+left":
			p := buf.LineStartNonSpace(v.Cursor)
			if v.Cursor == p {
				v.Cursor.Col = 0
			} else {
				v.Cursor = p
			}
		case "shift+left":
			// start selection
			if !v.Selecting {
				v.Selecting = true
				v.SelAnchor = v.Cursor
			}
			v.moveLeft()
		default:
			v.moveLeft()
		}
	case kero.KeyRight:
		switch key.String() {
		case "alt+right":
			v.Cursor = buf.WordEnd(v.Cursor)
		case "cmd+right":
			v.Cursor = buf.LineEnd(v.Cursor)
		case "shift+right":
			// start selection
			if !v.Selecting {
				v.Selecting = true
				v.SelAnchor = v.Cursor
			}
			v.moveRight()
		default:
			v.moveRight()
		}
	case kero.KeyUp:
		switch key.String() {
		case "cmd+up":
			e.recordJump()
			// file start
			v.Cursor.Row = 0
			v.Cursor.Col = 0
		case "shift+up":
			// start selection
			if !v.Selecting {
				v.Selecting = true
				v.SelAnchor = v.Cursor
			}
			v.moveUp()
		default:
			if e.completion.Active {
				e.completion.Prev()
				completing = true
				return nil
			}
			v.moveUp()
		}
	case kero.KeyDown:
		switch key.String() {
		case "cmd+down":
			e.recordJump()
			// file end
			v.Cursor.Row = len(buf.Lines) - 1
			v.Cursor = buf.LineEnd(v.Cursor)
		case "shift+down":
			// start selection
			if !v.Selecting {
				v.Selecting = true
				v.SelAnchor = v.Cursor
			}
			v.moveDown()
		default:
			if e.completion.Active {
				e.completion.Next()
				completing = true
				return nil
			}
			v.moveDown()
		}
	case kero.KeyHome:
		p := buf.LineStartNonSpace(v.Cursor)
		if v.Cursor == p {
			v.Cursor.Col = 0
			return nil
		}
		v.Cursor = p
	case kero.KeyEnd:
		v.Cursor = buf.LineEnd(v.Cursor)
	case kero.KeyPgUp:
		e.recordJump()
		v.Cursor.Row -= v.Height
		v.Cursor = buf.Clamp(v.Cursor)
	case kero.KeyPgDown:
		e.recordJump()
		v.Cursor.Row += v.Height
		v.Cursor = buf.Clamp(v.Cursor)
	case kero.KeyEsc:
		if e.completion.Active {
			e.completion.Active = false
			return nil
		}
		if e.ref.Active {
			e.ref.Active = false
			return nil
		}
		if e.finding {
			e.finding = false
			return nil
		}
		e.clearSelect()
	}
	return nil
}

func gutterWidth(lines int) int {
	width := 1
	for lines >= 10 {
		width++
		lines /= 10
	}
	return width + 2 // marker, line number, and separator
}

// LayoutWindow splits a total available screen Rect into component Rects.
func LayoutWindow(totalWidth, totalHeight, lineCount int) (gutterRect, textRect, msgRect, statusRect kero.Rect) {
	remaining := kero.Rect{W: totalWidth, H: totalHeight}

	remaining, statusRect = kero.SplitHorizontal(remaining, remaining.H-1)
	remaining, msgRect = kero.SplitHorizontal(remaining, remaining.H-1)

	gutterWidth := gutterWidth(lineCount)
	gutterRect, textRect = kero.SplitVertical(remaining, gutterWidth)

	return gutterRect, textRect, msgRect, statusRect
}

func LayoutPalatte(totalWidth, totalHeight, paletteHeight int) kero.Rect {
	rect := kero.Rect{X: (totalWidth - 60) / 2, Y: 2, W: 60, H: paletteHeight}
	if rect.X <= 0 {
		rect.X = totalWidth / 4
		rect.W = totalWidth / 2
	}
	return rect
}

// calculate the rectangle for the references overlay panel
func LayoutRefPanel(totalWidth, totalHeight int) kero.Rect {
	header := 1
	visibleRows := 10
	rect := kero.Rect{X: 0, W: totalWidth, H: visibleRows + header}
	statusBar := 1
	rect.Y = totalHeight - statusBar - rect.H
	return rect
}

func (e *Editor) Draw(ctx *kero.Context, f *kero.Frame) {
	statusStyle := kero.NewStyle().Reverse()
	gutterStyle := kero.NewStyle().Foreground(kero.ColorBlue).Dim()
	gutterActiveStyle := kero.NewStyle().Foreground(kero.ColorBlue)
	textStyle := kero.NewStyle()
	cursorStyle := textStyle.Reverse().Foreground(kero.ColorRed)
	selectStyle := textStyle.Reverse()
	messageStyle := kero.NewStyle()

	v := e.View()
	fSize := f.Size()
	gutterRect, textRect, msgRect, statusRect := LayoutWindow(fSize.Width, fSize.Height, len(v.Buf.Lines))

	// 1. Draw Gutter Area
	for i := range gutterRect.H {
		lineIdx := v.ScrollRow + i
		y := gutterRect.Y + i

		if lineIdx >= len(v.Buf.Lines) {
			f.Write(gutterRect.X, y, "~", gutterStyle)
			continue
		}

		gutterText := fmt.Sprintf(" %*d ", gutterRect.W-2, lineIdx+1)
		if lineIdx == v.Cursor.Row {
			f.Write(gutterRect.X, y, gutterText, gutterActiveStyle)
		} else {
			f.Write(gutterRect.X, y, gutterText, gutterStyle)
		}

		if v := e.diagnosticForLine(lineIdx); v != nil {
			red := kero.NewStyle().Foreground(kero.ColorRed)
			f.Set(gutterRect.X, y, 'x', red)
		}
	}

	// 2. Draw Text Viewport Area
	for i := range textRect.H {
		lineIdx := v.ScrollRow + i
		if lineIdx >= len(v.Buf.Lines) {
			break
		}

		y := textRect.Y + i
		line := v.Buf.Lines[lineIdx]
		fullPadded := padTab(string(line), 4)
		// visual part of the padded line
		var visPadded string
		if v.ScrollCol < len(fullPadded) {
			visPadded = fullPadded[v.ScrollCol:]
		}
		if len(visPadded) > textRect.W {
			visPadded = visPadded[:textRect.W]
		}

		f.Write(textRect.X, y, visPadded, textStyle)

		// show disanostic if appear
		if v := e.diagnosticForLine(lineIdx); v != nil {
			red := kero.NewStyle().Foreground(kero.ColorRed)
			dx := max(textRect.X+len(visPadded), fSize.Width-len(v.Msg))
			f.Write(dx, y, v.Msg, red)
		}

		// highlight selection if any
		if v.Selecting {
			start, end := orderPos(v.SelAnchor, v.Cursor)
			if lineIdx >= start.Row && lineIdx <= end.Row {
				var selStartCol, selEndCol int
				if start.Row == end.Row {
					selStartCol = start.Col
					selEndCol = end.Col
				} else if lineIdx == start.Row {
					selStartCol = start.Col
					selEndCol = len(line)
				} else if lineIdx == end.Row {
					selStartCol = 0
					selEndCol = end.Col
				} else {
					selStartCol = 0
					selEndCol = len(line)
				}

				visStartCol := v.Buf.ByteToVisualCol(Position{Row: lineIdx, Col: selStartCol}, 4)
				visEndCol := v.Buf.ByteToVisualCol(Position{Row: lineIdx, Col: selEndCol}, 4)

				startDisplay := max(0, min(visStartCol-v.ScrollCol, len(visPadded)))
				endDisplay := max(0, min(visEndCol-v.ScrollCol, len(visPadded)))

				for x := startDisplay; x < endDisplay; x++ {
					ch := rune(visPadded[x])
					f.Set(textRect.X+x, y, ch, selectStyle)
				}
			}
		}

		// highlight finding match
		if e.finding && e.findMatch && lineIdx == e.findMatchStart.Row && lineIdx == e.findMatchEnd.Row {
			visStartCol := e.Buf().ByteToVisualCol(Position{Row: lineIdx, Col: e.findMatchStart.Col}, 4)
			visEndCol := e.Buf().ByteToVisualCol(Position{Row: lineIdx, Col: e.findMatchEnd.Col}, 4)

			startDisplay := max(0, min(visStartCol-e.View().ScrollCol, len(visPadded)))
			endDisplay := max(0, min(visEndCol-e.View().ScrollCol, len(visPadded)))

			for x := startDisplay; x < endDisplay; x++ {
				ch := rune(visPadded[x])
				f.Set(textRect.X+x, y, ch, selectStyle)
			}
		}
	}

	cursorVisCol := v.Buf.ByteToVisualCol(v.Cursor, 4)
	cursorX := textRect.X + cursorVisCol - v.ScrollCol
	cursorY := 0 + v.Cursor.Row - v.ScrollRow
	if textRect.Contains(kero.Point{X: cursorX, Y: cursorY}) {
		fullLinePadded := padTab(string(v.Buf.Lines[v.Cursor.Row]), 4)
		ch := ' '
		if cursorVisCol < len(fullLinePadded) {
			ch = rune(fullLinePadded[cursorVisCol])
		}
		f.Set(cursorX, cursorY, ch, cursorStyle)
	}

	// statusY := ctx.Height - 1
	if statusRect.H > 0 {
		var names strings.Builder
		for i, b := range e.views {
			if i == e.active && len(e.views) > 1 {
				names.WriteString("[")
			}
			name := "untitled"
			if b.Buf.Path != "" {
				name = filepath.Base(b.Buf.Path)
			}
			names.WriteString(name)
			if b.Buf.Dirty {
				names.WriteString("*")
			}
			if i == e.active && len(e.views) > 1 {
				names.WriteString("]")
			}
			names.WriteString(" ")
		}
		status := fmt.Sprintf(" %s| Line %d, Col %d", names.String(), v.Cursor.Row+1, cursorVisCol+1)
		if v.Selecting {
			status = status + " | Selecting"
		}
		f.Fill(statusRect, ' ', statusStyle)
		f.Write(statusRect.X, statusRect.Y, trimToWidth(status, ctx.Width), statusStyle)
		if s := e.LastEvent(); s != "" {
			f.Write(fSize.Width-runewidth.StringWidth(s), statusRect.Y, s, statusStyle)
		}
		if len(e.diags) > 0 {
			warn := fmt.Sprintf("%d diagnostic", len(e.diags))
			statusWidth := runewidth.StringWidth(status)
			eventWidth := runewidth.StringWidth(e.LastEvent())
			remainWidth := fSize.Width - statusWidth - eventWidth
			if remainWidth > 0 {
				f.Write(statusRect.X+statusWidth+1, statusRect.Y, trimToWidth("| "+warn, remainWidth), statusStyle.Background(kero.ColorRed))
			}
		}
	}

	if msgRect.H > 0 {
		switch {
		case e.saveAs:
			e.drawSaveAs(f, msgRect)
		case e.finding:
			e.drawFind(f, msgRect)
		case e.renaming:
			e.drawRename(f, msgRect)
		case e.ref.Active:
			e.drawReferences(f, LayoutRefPanel(fSize.Width, fSize.Height))
		default:
			if e.message == "" {
				e.message = "^S save | ^W close | ^Q quit | ^F find | ^P palette"
			}
			if strings.HasPrefix(e.message, "error:") || strings.HasPrefix(e.message, "warn:") {
				messageStyle = messageStyle.Foreground(kero.ColorRed)
			}
			f.Write(msgRect.X, msgRect.Y, trimToWidth(" "+e.message, ctx.Width), messageStyle)
		}
	}

	if e.completion.Active {
		e.drawCompletion(f)
	}

	if e.palette.Active {
		e.drawPalette(f, LayoutPalatte(fSize.Width, fSize.Height, e.palette.VisibleRows()+1))
	}
}

func (e *Editor) selectLine() {
	if !e.View().Selecting {
		e.View().Selecting = true
		e.View().SelAnchor = Position{Row: e.View().Cursor.Row, Col: 0}
	}
	if e.View().Cursor.Row < len(e.Buf().Lines)-1 {
		e.View().Cursor.Row++
		e.View().Cursor.Col = 0
	} else {
		e.View().Cursor = e.Buf().LineEnd(e.View().Cursor)
	}
}

func isWordChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

func (e *Editor) hasSelect() bool {
	return e.View().Selecting && e.View().SelAnchor != e.View().Cursor
}

func (e *Editor) clearSelect() {
	e.View().Selecting = false
}

func (e *Editor) copy() {
	if e.hasSelect() {
		e.clipboard = e.Buf().TextRange(e.View().SelAnchor, e.View().Cursor)
		e.clipIsLine = false
		return
	}
	// copy entire current line, remember it's a line copy
	e.clipboard = string(e.Buf().Lines[e.View().Cursor.Row])
	e.clipIsLine = true
}

func (e *Editor) indentSelect() {
	if !e.hasSelect() {
		return
	}

	start, end := orderPos(e.View().SelAnchor, e.View().Cursor)
	if start.Row == end.Row {
		return
	}
	lastRow := end.Row
	if end.Col == 0 && end.Row > start.Row {
		lastRow = end.Row - 1
	}
	for r := start.Row; r <= lastRow; r++ {
		e.Buf().Insert(Position{Row: r, Col: 0}, "\t")
	}
	if start.Row <= e.View().SelAnchor.Row && e.View().SelAnchor.Row <= lastRow {
		e.View().SelAnchor.Col++
	}
	if start.Row <= e.View().Cursor.Row && e.View().Cursor.Row <= lastRow {
		e.View().Cursor.Col++
	}
	e.markDirty()
}

// unindent the line, return the removed bytes number
func unindent(line string) (string, int) {
	if strings.HasPrefix(line, "\t") {
		return line[1:], 1
	}
	spaces := 0
	for spaces < 4 && spaces < len(line) && line[spaces] == ' ' {
		spaces++
	}
	if spaces > 0 {
		return line[spaces:], spaces
	}
	return line, 0
}

func (e *Editor) unindentSelectOrLine() {
	if !e.hasSelect() {
		newLine, removed := unindent(string(e.Buf().Lines[e.View().Cursor.Row]))
		if removed > 0 {
			e.Buf().Lines[e.View().Cursor.Row] = []byte(newLine)
			e.View().Cursor.Col = max(0, e.View().Cursor.Col-removed)
			e.markDirty()
		}
		return
	}

	start, end := orderPos(e.View().SelAnchor, e.View().Cursor)
	lastRow := end.Row
	if end.Col == 0 && end.Row > start.Row {
		lastRow = end.Row - 1
	}
	var startRemoved, endRemoved int
	for r := start.Row; r <= lastRow; r++ {
		newLine, removed := unindent(string(e.Buf().Lines[r]))
		if r == e.View().SelAnchor.Row {
			startRemoved = removed
		}
		if r == e.View().Cursor.Row {
			endRemoved = removed
		}
		e.Buf().Lines[r] = []byte(newLine)
	}

	if e.View().SelAnchor.Row <= lastRow {
		e.View().SelAnchor.Col = max(0, e.View().SelAnchor.Col-startRemoved)
	}
	if e.View().Cursor.Row <= lastRow {
		e.View().Cursor.Col = max(0, e.View().Cursor.Col-endRemoved)
	}
	e.markDirty()
}

func (e *Editor) deleteSelect() {
	if !e.hasSelect() {
		return
	}
	e.View().Cursor = e.Buf().Delete(e.View().SelAnchor, e.View().Cursor)
	e.View().Selecting = false
	e.markDirty()
}

func (e *Editor) cut() {
	if e.hasSelect() {
		e.copy()
		e.deleteSelect()
		e.clipIsLine = false
		return
	}
	// cut current line
	p1 := Position{Row: e.View().Cursor.Row}
	e.clipboard = e.Buf().TextRange(p1, e.Buf().LineEnd(e.View().Cursor))
	e.clipIsLine = true
	e.View().Cursor = e.Buf().Delete(p1, Position{Row: e.View().Cursor.Row + 1})
	e.markDirty()
}

func (e *Editor) paste() {
	if e.clipboard == "" {
		return
	}
	// if selection present, replace it
	if e.hasSelect() {
		e.deleteSelect()
	}

	// whole-line copy: paste a fresh copy of that line ABOVE the current line,
	// pushing the current line downward.
	if e.clipIsLine && e.clipboard != "" {
		e.Buf().Insert(Position{Row: e.View().Cursor.Row, Col: 0}, e.clipboard+"\n")
		e.View().Cursor.Row++
		e.markDirty()
		return
	}

	e.View().Cursor = e.Buf().Insert(e.View().Cursor, e.clipboard)
	e.markDirty()
}

func padTab(s string, tabSize int) string {
	var result strings.Builder
	col := 0
	for _, r := range s {
		if r == '\t' {
			spaces := tabSize - (col % tabSize)
			result.WriteString(strings.Repeat(" ", spaces))
			col += spaces
		} else {
			result.WriteRune(r)
			col++
		}
	}
	return result.String()
}

func (e *Editor) save() error {
	if e.Buf() == nil {
		return nil
	}
	if e.Buf().Path == "" {
		e.startSaveAs()
		return nil
	}

	if _, err := e.Buf().Format(); err != nil {
		log.Print(err)
	}

	if err := e.Buf().SaveFile(); err != nil {
		e.message = "error: " + err.Error()
		return nil
	}

	e.message = fmt.Sprintf("saved %s", filepath.Base(e.Buf().Path))
	return nil
}

func (e *Editor) startSaveAs() {
	e.saveAs = true
	e.saveInput.SetText(e.Buf().Path)
	e.message = "enter a filename"
}

func (e *Editor) updateSaveAs(key kero.KeyEvent) error {
	switch key.Key {
	case kero.KeyEnter:
		return e.finishSaveAs()
	case kero.KeyEsc:
		e.saveAs = false
		e.message = "save canceled"
		return nil
	}

	e.saveInput.Update(key)
	return nil
}

func (e *Editor) finishSaveAs() error {
	path := strings.TrimSpace(e.saveInput.String())
	if path == "" {
		e.message = "filename required"
		return nil
	}

	p, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	e.Buf().Path = p
	e.saveAs = false
	if err := e.save(); err != nil {
		return err
	}
	e.diagnose()
	return nil
}

func (e *Editor) drawSaveAs(f *kero.Frame, rect kero.Rect) {
	normal := kero.NewStyle().Foreground(kero.ColorYellow)
	errorStyle := kero.NewStyle().Foreground(kero.ColorRed)
	style := normal
	if e.message == "filename required" {
		style = errorStyle
	}

	prompt := " Save as: "
	f.Write(rect.X, rect.Y, trimToWidth(prompt, rect.W), style)
	inputX := len([]rune(prompt))
	if inputX >= rect.W {
		return
	}
	e.saveInput.Draw(f, kero.Rect{X: inputX, Y: rect.Y, W: rect.W - inputX, H: 1}, style)
}

// startFind starts a Find prompt, and pre-fill with selection or last query, if any.
func (e *Editor) startFind() {
	e.finding = true
	e.findBlur = false
	e.replacing = false
	if e.hasSelect() {
		e.findInput.SetTextAndSelectAll(e.Buf().TextRange(e.View().SelAnchor, e.View().Cursor))
		return
	}
	if e.findInput.String() != "" {
		e.findInput.SetTextAndSelectAll(e.findInput.String())
	}
}

func findQueryIgnoreCase(query string) bool {
	for _, r := range query {
		if unicode.IsUpper(r) {
			return false
		}
	}
	return true
}

func (e *Editor) updateFind(ev kero.KeyEvent) error {
	if ev.String() == "ctrl+r" {
		e.replacing = !e.replacing
		if e.replacing {
			e.replaceInput.SetText("")
			e.replaceInput.Placeholder = "replacement"
		}
		return nil
	}
	if e.replacing {
		switch ev.Key {
		case kero.KeyEsc:
			e.finding = false
			e.replacing = false
			return nil
		case kero.KeyTab:
			e.skipFindMatch()
			return nil
		case kero.KeyEnter:
			if ev.Mod&kero.ModCtrl != 0 {
				e.replaceAll()
				e.finding = false
				e.replacing = false
				return nil
			}
			return e.replaceCurrent()
		}
		e.replaceInput.Update(ev)
		return nil
	}

	switch ev.Key {
	case kero.KeyEsc:
		e.finding = false
		return nil
	case kero.KeyEnter:
		query := e.findInput.String()
		if query == "" {
			return nil
		}
		ignoreCase := findQueryIgnoreCase(query)

		if ev.Mod&kero.ModShift != 0 {
			if ignoreCase {
				if start, end, ok := e.Buf().FindPrevIgnoreCase(query, e.View().Cursor); ok {
					e.findMatch = true
					e.findMatchStart = start
					e.findMatchEnd = end
					e.View().Cursor = start
					e.clearSelect()
				}
			} else {
				if start, end, ok := e.Buf().FindPrev(query, e.View().Cursor); ok {
					e.findMatch = true
					e.findMatchStart = start
					e.findMatchEnd = end
					e.View().Cursor = start
					e.clearSelect()
				}
			}
			return nil
		}

		if ignoreCase {
			start, end, ok := e.Buf().FindNextIgnoreCase(query, e.View().Cursor)
			if ok {
				e.findMatch = true
				e.findMatchStart = start
				e.findMatchEnd = end
				e.View().Cursor = end
				e.clearSelect()
			}
		} else {
			start, end, ok := e.Buf().FindNext(query, e.View().Cursor)
			if ok {
				e.findMatch = true
				e.findMatchStart = start
				e.findMatchEnd = end
				e.View().Cursor = end
				e.clearSelect()
			}
		}
		return nil
	}

	e.findMatch = false
	e.findInput.Update(ev)
	// this tidy editor hasn't implemented Go Back/Forward,
	// don't jump to the first match on typing
	return nil
}

func (e *Editor) drawFind(f *kero.Frame, rect kero.Rect) {
	normal := kero.NewStyle()
	prompt := " Find: "
	f.Write(rect.X, rect.Y, trimToWidth(prompt, rect.W), normal)
	inputX := len([]rune(prompt))
	if inputX >= rect.W {
		return
	}
	if !e.replacing {
		e.findInput.Draw(f, kero.Rect{X: inputX, Y: rect.Y, W: rect.W - inputX, H: 1}, normal)
		return
	}
	e.findInput.Draw(f, kero.Rect{X: inputX, Y: rect.Y, W: rect.W - inputX, H: 1}, normal)
	replaceX := inputX + len([]rune(e.findInput.String())) + 4
	if replaceX < rect.W {
		f.Write(replaceX-4, rect.Y, " -> ", normal.Foreground(kero.ColorYellow))
		e.replaceInput.Draw(f, kero.Rect{X: replaceX, Y: rect.Y, W: rect.W - replaceX, H: 1}, normal)
	}
}

func (e *Editor) skipFindMatch() {
	query := e.findInput.String()
	if query == "" {
		return
	}
	from := e.View().Cursor
	if e.findMatch {
		from = e.findMatchEnd
	}
	ignoreCase := findQueryIgnoreCase(query)
	var start, end Position
	var ok bool
	if ignoreCase {
		start, end, ok = e.Buf().FindNextIgnoreCase(query, from)
	} else {
		start, end, ok = e.Buf().FindNext(query, from)
	}
	if !ok {
		e.findMatch = false
		return
	}
	e.findMatch = true
	e.findMatchStart = start
	e.findMatchEnd = end
	e.View().Cursor = end
	e.clearSelect()
}

func (e *Editor) replaceCurrent() error {
	query := e.findInput.String()
	if query == "" {
		return nil
	}
	if !e.findMatch {
		e.skipFindMatch()
	}
	if !e.findMatch {
		return nil
	}

	replacedEnd := e.Buf().ReplaceRange(e.findMatchStart, e.findMatchEnd, e.replaceInput.String())
	e.markDirty()
	e.View().Cursor = replacedEnd
	e.findMatch = false
	if e.replaceInput.String() == query && replacedEnd.Col < e.Buf().LineEnd(replacedEnd).Col {
		replacedEnd.Col++
		e.View().Cursor = replacedEnd
	}
	e.skipFindMatch()
	return nil
}

func (e *Editor) replaceAll() error {
	query := e.findInput.String()
	if query == "" {
		return nil
	}
	ignoreCase := findQueryIgnoreCase(query)
	var count int
	if ignoreCase {
		count = e.Buf().ReplaceAllIgnoreCase(query, e.replaceInput.String())
	} else {
		count = e.Buf().ReplaceAll(query, e.replaceInput.String())
	}
	if count > 0 {
		e.markDirty()
	}
	e.findMatch = false
	e.message = fmt.Sprintf("replaced %d matches", count)
	return nil
}

func (v *View) moveLeft() {
	v.Cursor = v.Buf.PrevRunePos(v.Cursor)
}

func (v *View) moveRight() {
	v.Cursor = v.Buf.NextRunePos(v.Cursor)
}

func (v *View) moveUp() {
	if v.Cursor.Row == 0 {
		return
	}
	vCol := v.Buf.ByteToVisualCol(v.Cursor, 4)
	col := v.Buf.VisualToByteCol(v.Cursor.Row-1, vCol, 4)
	v.Cursor = Position{Row: v.Cursor.Row - 1, Col: col}
}

func (v *View) moveDown() {
	if v.Cursor.Row >= len(v.Buf.Lines)-1 {
		return
	}
	vCol := v.Buf.ByteToVisualCol(v.Cursor, 4)
	col := v.Buf.VisualToByteCol(v.Cursor.Row+1, vCol, 4)
	v.Cursor = Position{Row: v.Cursor.Row + 1, Col: col}
}

func (e *Editor) markDirty() {
	e.Buf().Dirty = true
	e.message = ""
}

type diagResult struct {
	version uint64
	errs    scanner.ErrorList
}

// diagnose starts a goroutine to check file.
func (e *Editor) diagnose() {
	if e.Buf() == nil {
		return
	}

	go func() {
		diags, err := CheckFile(e.Buf().Path)
		if err != nil {
			log.Print(err)
			e.message = err.Error()
			return
		}
		select {
		case e.diagChan <- diags:
		default:
		}
	}()
}

func (e *Editor) applyDiagnostic() {
	if e.diagChan == nil {
		return
	}
	select {
	case result := <-e.diagChan:
		e.diags = result
	default:
		return
	}
}

func (e *Editor) diagnosticForLine(row int) *scanner.Error {
	for _, d := range e.diags {
		if d.Pos.Line-1 == row {
			return d
		}
	}
	return nil
}

func (e *Editor) gotoPrevDiag() {
	if len(e.diags) == 0 {
		return
	}

	v := e.View()
	var se *scanner.Error
	for i := len(e.diags) - 1; i >= 0; i-- {
		d := e.diags[i]
		dRow := d.Pos.Line - 1
		dCol := d.Pos.Column - 1
		if dRow < v.Cursor.Row || (dRow == v.Cursor.Row && dCol < v.Cursor.Col) {
			se = d
			break
		}
	}
	if se == nil {
		se = e.diags[len(e.diags)-1]
	}
	v.Cursor = v.Buf.Clamp(Position{
		Row: se.Pos.Line - 1,
		Col: se.Pos.Column - 1,
	})
}

func (e *Editor) gotoNextDiag() {
	if len(e.diags) == 0 {
		return
	}

	v := e.View()
	// find next diagnostic
	var err *scanner.Error
	for _, d := range e.diags {
		dRow := d.Pos.Line - 1
		dCol := d.Pos.Column - 1
		if dRow > v.Cursor.Row ||
			(dRow == v.Cursor.Row && dCol > v.Cursor.Col) {
			err = d
			break
		}
	}
	if err == nil {
		err = e.diags[0]
	}

	dRow := err.Pos.Line - 1
	dCol := err.Pos.Column - 1
	v.Cursor = v.Buf.Clamp(Position{Row: dRow, Col: dCol})
}

func isGoFile(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".go")
}

// showCursor adjusts vertical and horizontal scrolling to ensure the cursor is within
// the visible viewport.
func (v *View) showCursor() {
	if v.Height <= 0 {
		return
	}

	// 1. Cursor is above the viewport -> scroll UP
	if v.Cursor.Row < v.ScrollRow {
		v.ScrollRow = v.Cursor.Row
	}

	// 2. Cursor is below the viewport -> scroll DOWN
	// Bottom-most visible row index is (ScrollRow + Height - 1)
	if v.Cursor.Row >= v.ScrollRow+v.Height {
		v.ScrollRow = v.Cursor.Row - v.Height + 1
	}

	// Clamp to top boundary
	if v.ScrollRow < 0 {
		v.ScrollRow = 0
	}

	if v.Width <= 0 {
		return
	}

	vCol := v.Buf.ByteToVisualCol(v.Cursor, 4)

	// 1. Cursor is to the left of the viewport -> scroll LEFT
	if vCol < v.ScrollCol {
		v.ScrollCol = vCol
	}

	// 2. Cursor is to the right of the viewport -> scroll RIGHT
	// Right-most visible visual column index is (ScrollCol + Width - 1)
	if vCol >= v.ScrollCol+v.Width {
		v.ScrollCol = vCol - v.Width + 1
	}

	// Clamp to left boundary
	if v.ScrollCol < 0 {
		v.ScrollCol = 0
	}
}

// showCursorCenter centers the cursor in the viewport both vertically and horizontally.
func (v *View) showCursorCenter() {
	tabWidth := 4

	// Center vertically
	v.ScrollRow = max(v.Cursor.Row-(v.Height/2), 0)

	// Center horizontally
	vCol := v.Buf.ByteToVisualCol(v.Cursor, tabWidth)
	v.ScrollCol = max(vCol-(v.Width/2), 0)
}

// isCursorVisible returns true if the cursor is currently inside the visible viewport.
func (v *View) isCursorVisible() bool {
	if v.Width <= 0 || v.Height <= 0 {
		return false
	}

	// Vertical check
	if v.Cursor.Row < v.ScrollRow || v.Cursor.Row >= v.ScrollRow+v.Height {
		return false
	}

	// Horizontal check
	vCol := v.Buf.ByteToVisualCol(v.Cursor, 4)
	if vCol < v.ScrollCol || vCol >= v.ScrollCol+v.Width {
		return false
	}

	return true
}

// ShowCursorSmart minimal-scrolls for nearby moves, but centers for long-distance jumps.
func (v *View) ShowCursorSmart() {
	if v.isCursorVisible() {
		return
	}

	// Check vertical jump distance
	dist := v.Cursor.Row - v.ScrollRow
	if dist < 0 {
		dist = -dist
	}

	// If the jump is far outside the viewport (e.g. > 1 full viewport height), center it.
	// Otherwise, just do standard minimal scrolling.
	if dist > v.Height {
		v.showCursorCenter()
	} else {
		v.showCursor()
	}
}

func trimToWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width])
}

// CheckSemantics checks syntax and type error.
// filename is used for position resolution (e.g., "main.go").
// src can be a string, []byte, or io.Reader.
//
// It responds instantly(~5-20ms), as a tradeoff,
// external module imports are not resolved and are filtered out.
// func CheckSemantics(filename string, src any) scanner.ErrorList {
// 	fset := token.NewFileSet()
// 	// 1. Parse AST with comments and full error reporting
// 	file, err := parser.ParseFile(fset, filename, src, parser.AllErrors)

// 	var errs scanner.ErrorList

// 	// 2. Collect syntax errors first
// 	if err != nil {
// 		if scannerErrs, ok := err.(scanner.ErrorList); ok {
// 			return scannerErrs
// 		}
// 		// If syntax is broken, return early (type checking invalid AST causes redundant noise)
// 		return errs
// 	}

// 	// 3. Configure type checker for semantic validation
// 	pkgName := file.Name.Name
// 	if pkgName == "" {
// 		pkgName = "main"
// 	}

// 	conf := types.Config{
// 		// only resolve standard library imports, ignores third-party or local module
// 		Importer: importer.Default(),

// 		// Custom error handler to collect semantic diagnostics
// 		Error: func(err error) {
// 			if typeErr, ok := err.(types.Error); ok {
// 				// Suppress import resolution errors for external modules during real-time typing
// 				if strings.Contains(typeErr.Msg, "could not import") ||
// 					strings.Contains(typeErr.Msg, "cannot find package") {
// 					return
// 				}

// 				pos := fset.Position(typeErr.Pos)
// 				errs.Add(pos, typeErr.Msg)
// 			}
// 		},
// 	}

// 	// Optional: Pass an empty Info struct to trigger full type resolution
// 	info := &types.Info{
// 		Types:      make(map[ast.Expr]types.TypeAndValue),
// 		Defs:       make(map[*ast.Ident]types.Object),
// 		Uses:       make(map[*ast.Ident]types.Object),
// 		Implicits:  make(map[ast.Node]types.Object),
// 		Selections: make(map[*ast.SelectorExpr]*types.Selection),
// 	}

// 	// 4. Run type checker on the AST
// 	_, _ = conf.Check(pkgName, fset, []*ast.File{file}, info)
// 	// Optional: Multi-file package type checking
// 	/*
// 		If your file references types or functions declared in another file in the same package,
// 		go/types will report them as undefined if only a single file AST is passed.
// 		To handle multi-file packages in your editor,
// 		simply pass all parsed AST files in the same directory to conf.Check:
// 		_, _ = conf.Check(pkgName, fset, []*ast.File{currentFileAST, otherFileAST1, otherFileAST2}, info)
// 	*/

// 	return errs
// }

type SymbolPosition struct {
	Name     string
	Receiver string
	Kind     string // "func", "method", "type", "struct", "var", "const"
	token.Position
}

// combines receiver and symbol name
func (s SymbolPosition) String() string {
	if s.Receiver == "" {
		return s.Name
	}
	return fmt.Sprintf("(%s).%s", s.Receiver, s.Name)
}

// ExtractSymbols collects all top-level symbols in the file.
// It is in-memory and fast.
func ExtractSymbols(filename string, src any) []SymbolPosition {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil && file == nil {
		return nil
	}

	var results []SymbolPosition

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			pos := fset.Position(d.Name.Pos())
			kind := "func"

			// Extract receiver type if this function is a method
			var recv string
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
				recv = formatReceiver(d.Recv.List[0].Type)
			}

			results = append(results, SymbolPosition{
				Name:     d.Name.Name,
				Receiver: recv,
				Kind:     kind,
				Position: pos,
			})

		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					pos := fset.Position(s.Name.Pos())
					kind := "type"
					if _, ok := s.Type.(*ast.StructType); ok {
						kind = "struct"
					} else if _, ok := s.Type.(*ast.InterfaceType); ok {
						kind = "interface"
					}
					results = append(results, SymbolPosition{
						Name:     s.Name.Name,
						Kind:     kind,
						Position: pos,
					})

				case *ast.ValueSpec:
					kind := "var"
					if d.Tok == token.CONST {
						kind = "const"
					}
					for _, name := range s.Names {
						pos := fset.Position(name.Pos())
						results = append(results, SymbolPosition{
							Name:     name.Name,
							Kind:     kind,
							Position: pos,
						})
					}
				}
			}
		}
	}

	return results
}

// formatReceiver recursively extracts the receiver string representation
// handling pointer receivers (*Buffer) and value receivers (Buffer).
func formatReceiver(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + formatReceiver(t.X)
	case *ast.IndexExpr: // Generic receiver: Buffer[T]
		return fmt.Sprintf("%s[%s]", formatReceiver(t.X), formatReceiver(t.Index))
	default:
		return ""
	}
}

// OpenFile loads a file into memory or focuses it if already loaded.
func (e *Editor) OpenFile(path string) error {
	if path != "" {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		path = absPath
	}

	// switch to existing buffer
	for i, v := range e.views {
		if v.Buf.Path == path {
			e.active = i
			e.diags = nil
			return nil
		}
	}

	buf, err := BufferFromFile(path)
	if err != nil {
		return err
	}

	e.views = append(e.views, &View{Buf: buf})
	e.active = len(e.views) - 1
	e.diags = nil
	return nil
}

// CloseBuffer closes the active buffer.
func (e *Editor) CloseBuffer() {
	if len(e.views) <= 1 {
		// Either exit or leave an empty scratch buffer
		return
	}
	// Remove from slice and adjust active index
	e.views = slices.Delete(e.views, e.active, e.active+1)
	if e.active >= len(e.views) {
		e.active = len(e.views) - 1
	}
	e.diags = nil
	e.message = ""
}

func (e *Editor) NextBuffer() {
	if len(e.views) > 1 {
		e.active = (e.active + 1) % len(e.views)
		e.diags = nil
	}
}

func (e *Editor) PrevBuffer() {
	if len(e.views) > 1 {
		e.active = (e.active - 1 + len(e.views)) % len(e.views)
		e.diags = nil
	}
}

func readLines(r io.Reader) ([][]byte, error) {
	var lines [][]byte
	reader := bufio.NewReader(r)

	for {
		line, err := reader.ReadBytes('\n')

		// Strip '\n' and carriage return '\r' (Windows normalization)
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})

		if line != nil || err == nil {
			lines = append(lines, line)
		}

		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
	}

	return lines, nil
}

// JumpList manages navigation history for long jumps (Go To Def, Find Symbol, etc.).
type JumpList struct {
	items []Location
	index int // Points to current position in history
}

const maxJumps = 100

// Push adds a new jump location to the stack.
// If the new position is identical or right next to the current jump, it is ignored.
func (j *JumpList) Push(path string, pos Position) {
	if len(j.items) > 0 && j.index >= 0 && j.index < len(j.items) {
		curr := j.items[j.index]
		// Avoid pushing duplicate positions in the same file
		if curr.Filename == path && curr.Pos == pos {
			return
		}
	}

	// Truncate forward history if we jump from somewhere in the middle
	if j.index < len(j.items)-1 {
		j.items = j.items[:j.index+1]
	}

	j.items = append(j.items, Location{Filename: path, Pos: pos})
	if len(j.items) > maxJumps {
		j.items = j.items[1:]
	}
	j.index = len(j.items) - 1
}

// Back steps back in history and returns the target jump position.
func (j *JumpList) Back(currentPath string, currentPos Position) (Location, bool) {
	if len(j.items) == 0 {
		return Location{}, false
	}

	// If we are at the head of the jump list, record current position first
	// so we can jump forward back to it later.
	if j.index == len(j.items)-1 {
		j.Push(currentPath, currentPos)
	}

	if j.index <= 0 {
		j.index = 0
		return j.items[0], true
	}

	j.index--
	return j.items[j.index], true
}

// Forward steps forward in history and returns the target jump position.
func (j *JumpList) Forward() (Location, bool) {
	if len(j.items) == 0 || j.index >= len(j.items)-1 {
		return Location{}, false
	}

	j.index++
	return j.items[j.index], true
}

// recordJump saves the current editor position into the jump history.
func (e *Editor) recordJump() {
	if e.Buf() == nil {
		return
	}
	e.jumps.Push(e.Buf().Path, e.View().Cursor)
}

// jumpTo restores a recorded location, switching buffers if necessary.
func (e *Editor) jumpTo(target Location) {
	// 1. Switch buffer if the target is in a different file
	if target.Filename != "" && target.Filename != e.Buf().Path {
		err := e.OpenFile(target.Filename)
		if err != nil {
			log.Print(err)
			return
		}
		e.diagnose()
	}

	// 2. Set cursor position
	if target.Pos.Row >= 0 && target.Pos.Row < len(e.Buf().Lines) {
		e.View().Cursor = e.Buf().Clamp(target.Pos)
	}
}

// JumpBack moves to the previous position in jump history.
func (e *Editor) JumpBack() {
	if e.Buf() == nil {
		return
	}
	if target, ok := e.jumps.Back(e.Buf().Path, e.View().Cursor); ok {
		e.jumpTo(target)
	}
}

// JumpForward moves to the next position in jump history.
func (e *Editor) JumpForward() {
	if e.Buf() == nil {
		return
	}
	if target, ok := e.jumps.Forward(); ok {
		e.jumpTo(target)
	}
}

// PaletteItem represents a single selectable row in the overlay.
type PaletteItem struct {
	Label  string // Main display text (e.g., "(b *Buffer) Format" or "fmt")
	Detail string // Secondary detail (e.g., "Line 42" or "Ctrl+Shift+I")
	// Kind   string          // Visual badge/tag (e.g., "sym", "cmd", "line", "file")
	Action func(e *Editor) // Execution logic when user presses Enter
}

// Palette manages state, input handling, and item rendering for the overlay.
type Palette struct {
	Active  bool
	Input   TextInput
	Items   []PaletteItem
	Index   int // selected item
	Offset  int // scrolling offset
	MaxRows int // UI render cap (e.g., 10 items)
	symbols []SymbolPosition
}

// Open initializes the palette with a starting prefix ("@", ":", "/", or "").
func (p *Palette) Open(e *Editor, prefix string) {
	p.Active = true
	p.Input.Reset()
	p.Input.SetText(prefix)
	p.Input.Placeholder = "search file (@symbol, /command or :line)"
	p.Index = 0
	p.MaxRows = 10
	p.symbols = nil
	p.Offset = 0
	p.Refresh(e)
}

func (p *Palette) Close() {
	p.Active = false
	p.Input.Reset()
	p.Items = nil
	p.Index = 0
}

// Refresh updates p.Items based on the current input value.
func (p *Palette) Refresh(e *Editor) {
	input := p.Input.String()

	switch {
	case strings.HasPrefix(input, "@"):
		p.Items = p.symbolItems(e, strings.TrimPrefix(input, "@"))
	case strings.HasPrefix(input, ":"):
		p.Items = p.lineItems(e, strings.TrimPrefix(input, ":"))
	case strings.HasPrefix(input, "/"):
		p.Items = p.commandItems(e, strings.TrimPrefix(input, "/"))
	default:
		p.Items = p.fileItems(e, input)
	}

	// Reset index bounds
	if p.Index >= len(p.Items) {
		p.Index = max(0, len(p.Items)-1)
	}
}

// Symbol Provider (@)
func (p *Palette) symbolItems(e *Editor, query string) []PaletteItem {
	if e.Buf() == nil {
		return nil
	}

	lowerQuery := strings.ToLower(query)
	queries := strings.Split(lowerQuery, ".")
	if len(queries) == 1 {
		queries = strings.Split(lowerQuery, " ")
	}

	if p.symbols == nil {
		p.symbols = ExtractSymbols(e.Buf().Path, e.Buf().NewReader())
	}
	var items []PaletteItem

	for _, sym := range p.symbols {

		if query != "" {
			match := true
			for _, q := range queries {
				if !strings.Contains(strings.ToLower(sym.String()), q) {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}

		items = append(items, PaletteItem{
			Label:  sym.String(),
			Detail: sym.Kind,
			// Kind: sym.Kind,
			Action: func(ed *Editor) {
				ed.recordJump()
				ed.View().Cursor = Position{
					Row: sym.Line - 1,
					Col: sym.Column - 1,
				}
			},
		})
	}
	return items
}

// Command Provider (/)
func (p *Palette) commandItems(_ *Editor, query string) []PaletteItem {
	commands := []struct {
		cmd    string
		detail string
		action func(e *Editor)
	}{
		// use readable name for cmd, easy to search
		{"jump back", "ctrl+-", func(e *Editor) {
			e.JumpBack()
		}},
		{"jump forward", "ctrl+shift+-", func(e *Editor) {
			e.JumpForward()
		}},
		{"next buffer", "", func(e *Editor) {
			e.NextBuffer()
		}},
		{"prev buffer", "", func(e *Editor) {
			e.PrevBuffer()
		}},
		{"LSP: check", "", func(e *Editor) {
			e.diagnose()
		}},
		{"LSP: find references", "", findReferences},
		{"LSP: format file", "", func(e *Editor) { e.Buf().Format() }},
		{"LSP: goto definition", "ctrl+g", GotoDefinition},
		{"LSP: next diagnostic", "ctrl+]", func(e *Editor) {
			e.gotoNextDiag()
		}},
		{"LSP: prev diagnostic", "ctrl+]", func(e *Editor) {
			e.gotoPrevDiag()
		}},
		{"LSP: goto symbol", "ctrl+r", func(e *Editor) {
			e.palette.Open(e, "@")
		}},
		{"LSP: rename symbol", "", func(e *Editor) {
			if !isGoFile(e.Buf().Path) {
				return
			}
			e.startRename()
		}},
		{"toggle reference", "", func(e *Editor) { e.ref.Active = !e.ref.Active }},
		{"next reference", "ctrl+shift+]", func(e *Editor) {
			if len(e.ref.Items) <= 1 {
				return
			}
			e.gotoLocation(e.ref.Next())
		}},
		{"prev reference", "ctrl+shift+[", func(e *Editor) {
			if len(e.ref.Items) <= 1 {
				return
			}
			e.gotoLocation(e.ref.Prev())
		}},
	}

	var items []PaletteItem
	for _, c := range commands {
		match := true
		parts := strings.Fields(query)
		for _, q := range parts {
			if !strings.Contains(strings.ToLower(c.cmd), strings.ToLower(q)) {
				match = false
				break
			}
		}
		if !match {
			continue
		}

		action := c.action
		items = append(items, PaletteItem{
			Label:  c.cmd,
			Detail: c.detail,
			// Kind:   "cmd",
			Action: func(ed *Editor) {
				action(ed)
			},
		})
	}
	return items
}

// Goto Line Provider (:)
func (p *Palette) lineItems(e *Editor, query string) []PaletteItem {
	if query == "" || e.Buf() == nil {
		return nil
	}

	lineNum, err := strconv.Atoi(query)
	if err != nil || lineNum <= 0 || lineNum > len(e.Buf().Lines) {
		return nil
	}

	targetRow := lineNum - 1
	return []PaletteItem{
		{
			Label: fmt.Sprintf("Go to line %d", lineNum),
			Action: func(ed *Editor) {
				ed.recordJump()
				ed.View().Cursor = Position{Row: targetRow, Col: 0}
			},
		},
	}
}

func (p *Palette) VisibleRows() int {
	return min(len(p.Items), p.MaxRows)
}

func (e *Editor) updatePalette(ev kero.KeyEvent) {
	switch ev.Key {
	case kero.KeyEsc:
		e.palette.Close()
	case kero.KeyDown:
		total := len(e.palette.Items)
		if total == 0 {
			return
		}
		e.palette.Index = (e.palette.Index + 1) % total
		// Calculate scrolling offset to keep selected item inside dropdown viewport
		visibleRows := e.palette.VisibleRows()
		offset := 0
		if e.palette.Index >= visibleRows {
			offset = e.palette.Index - visibleRows + 1
		}
		e.palette.Offset = offset
	case kero.KeyUp:
		total := len(e.palette.Items)
		if total == 0 {
			return
		}
		e.palette.Index = (e.palette.Index - 1 + total) % total
		// Calculate scrolling offset to keep selected item inside dropdown viewport
		visibleRows := e.palette.VisibleRows()
		offset := 0
		if e.palette.Index >= visibleRows {
			offset = e.palette.Index - visibleRows + 1
		}
		e.palette.Offset = offset
	case kero.KeyEnter:
		if len(e.palette.Items) > 0 && e.palette.Index < len(e.palette.Items) {
			action := e.palette.Items[e.palette.Index].Action
			e.palette.Close()
			action(e) // Run selected action
		}
	default:
		// Pass key to TextInput (typing query text)
		e.palette.Input.Update(ev)
		e.palette.Refresh(e)
	}
}

// File Provider (Default mode when no prefix like '@', ':', or '/' is typed)
func (p *Palette) fileItems(e *Editor, query string) []PaletteItem {
	var items []PaletteItem
	lowerQuery := strings.ToLower(query)

	// 1. Include open buffers first for quick switching
	openPaths := make(map[string]bool)
	for i, v := range e.views {
		if v.Buf.Path == "" {
			continue
		}
		openPaths[v.Buf.Path] = true

		if query != "" && !strings.Contains(strings.ToLower(v.Buf.Path), lowerQuery) {
			continue
		}

		bufIdx := i
		bufPath := v.Buf.Path
		items = append(items, PaletteItem{
			Label:  filepath.Base(bufPath),
			Detail: "active",
			Action: func(ed *Editor) {
				ed.recordJump()
				ed.active = bufIdx
				e.diags = nil
			},
		})
	}

	// 2. Scan workspace files on disk (skipping hidden folders & vendor)
	const maxDiskItems = 50
	_ = filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		name := d.Name()
		// Skip hidden files and directories (.git, .env, .build, etc.)
		if strings.HasPrefix(name, ".") && name != "." {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip heavy dependency directories
		if d.IsDir() {
			if name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}

		// Don't re-add files already listed as open buffers
		absPath, _ := filepath.Abs(path)
		if openPaths[absPath] {
			return nil
		}

		if query != "" && !strings.Contains(strings.ToLower(absPath), lowerQuery) {
			return nil
		}

		items = append(items, PaletteItem{
			Label: filepath.Base(absPath),
			Action: func(ed *Editor) {
				ed.recordJump()
				if err := ed.OpenFile(absPath); err != nil {
					log.Print(err)
					return
				}
				ed.diagnose()
			},
		})

		// Cap results to preserve TUI render responsiveness
		if len(items) >= maxDiskItems {
			return filepath.SkipAll
		}
		return nil
	})

	return items
}

// drawPalette renders the input field and popup overlay menu above row y.
func (e *Editor) drawPalette(f *kero.Frame, rect kero.Rect) {
	if !e.palette.Active {
		return
	}

	normal := kero.NewStyle()
	p := &e.palette

	// Fill background for dropdown overlay
	f.Fill(rect, ' ', normal.Reverse())
	p.Input.Draw(f, kero.Rect{X: rect.X + 1, Y: rect.Y, W: rect.W, H: 1}, normal.Reverse())

	// Calculate visible window bounds
	total := len(p.Items)
	if total == 0 {
		return
	}
	visibleRows := min(total, rect.H-1)

	// Render item rows
	for i := range visibleRows {
		idx := i + p.Offset
		if idx >= total {
			break
		}

		item := p.Items[idx]
		lineY := rect.Y + i + 1 // plus 1 because of Input

		// Selection cursor indicator
		prefix := "  "
		style := normal.Reverse()
		if idx == p.Index {
			prefix = " >"
			style = normal.Reverse().Bold()
		}

		// Left side text: Prefix + Label
		leftText := fmt.Sprintf("%s %s", prefix, item.Label)
		leftRunes := []rune(leftText)

		if len(leftRunes) > rect.W {
			leftText = string(leftRunes[:rect.W])
		}
		f.Write(rect.X, lineY, leftText, style)

		// Right side text: Detail (e.g. line number or keybinding hint)
		if item.Detail != "" {
			detailRunes := []rune(item.Detail)
			detailX := rect.X + (rect.W - len(detailRunes) - 1)

			// Only render detail if it doesn't overlap left text
			if detailX > len(leftRunes)+2 {
				f.Write(detailX, lineY, item.Detail, style)
			}
		}
	}
}

type Completion struct {
	Active bool
	Index  int
	Items  []SymbolPosition
}

func (c *Completion) Refresh(src any, query string) {
	results := ExtractSymbols("", src)
	if len(results) == 0 {
		return
	}

	b := findQueryIgnoreCase(query)
	items := make([]SymbolPosition, 0, len(results))
	for _, s := range results {
		if !b {
			if strings.Contains(s.Name, query) {
				items = append(items, s)
			}
		} else {
			if strings.Contains(strings.ToLower(s.Name), strings.ToLower(query)) {
				items = append(items, s)
			}
		}
	}
	c.Items = items
	c.Index = 0
}

func (c *Completion) Next() {
	if len(c.Items) == 0 {
		return
	}
	c.Index = (c.Index + 1) % len(c.Items)
}

func (c *Completion) Prev() {
	if len(c.Items) == 0 {
		return
	}
	c.Index = (c.Index - 1 + len(c.Items)) % len(c.Items)
}

func (e *Editor) drawCompletion(f *kero.Frame) {
	if len(e.completion.Items) == 0 {
		return
	}

	_, textRect, _, _ := LayoutWindow(f.Size().Width, f.Size().Height, len(e.Buf().Lines))

	c := e.completion
	visibleRows := min(len(c.Items), 10)
	var maxWidth int
	for i := range c.Items {
		if width := len([]rune(c.Items[i].String())); width > maxWidth {
			maxWidth = width
		}
	}

	// Calculate scrolling offset to keep selected item inside dropdown viewport
	offset := 0
	if c.Index >= visibleRows {
		offset = c.Index - visibleRows + 1
	}

	var normal kero.Style
	x := textRect.X + e.Buf().ByteToVisualCol(e.View().Cursor, 4) - e.View().ScrollCol
	y := e.View().Cursor.Row - e.View().ScrollRow
	w := maxWidth + 3 // 2 for indicator, 1 for right padding
	rect := kero.Rect{X: x, Y: y - visibleRows, W: w, H: visibleRows}
	f.Fill(rect, ' ', normal.Reverse())
	for i := range visibleRows {
		y := rect.Y + i
		if i+offset == c.Index {
			f.Write(rect.X, y, " >"+c.Items[i+offset].String(), normal.Reverse().Bold())
		} else {
			f.Write(rect.X, y, "  "+c.Items[i+offset].String(), normal.Reverse())
		}
	}
}

func (e *Editor) startRename() {
	e.renaming = true
	start, end := e.Buf().WordBounds(e.View().Cursor)
	symbol := e.Buf().TextRange(start, end)
	e.renameInput.SetTextAndSelectAll(symbol)
}

func (e *Editor) updateRename(ev kero.KeyEvent) {
	switch ev.Key {
	case kero.KeyEsc:
		e.renaming = false
	case kero.KeyEnter:
		newName := e.renameInput.String()
		// try save
		if e.Buf().Dirty {
			if err := e.Buf().SaveFile(); err != nil {
				log.Print(err)
				e.message = err.Error()
				return
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		/*
			For example run:
				gopls rename -w -l main.go:1639:6 NewBufferX
			Output:
				/Users/aha/code/ke/editor_test.go
				/Users/aha/code/ke/buffer_test.go
				/Users/aha/code/ke/main.go
		*/
		now := time.Now()
		cmd := exec.CommandContext(ctx, "gopls", "rename", "-w", "-l", fmt.Sprintf("%s:%d:%d",
			e.Buf().Path, e.View().Cursor.Row+1, e.View().Cursor.Col+1), newName)
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Print(err)
			e.message = err.Error()
			return
		}
		log.Printf("run %q in %.1fs:\n%s", cmd, time.Since(now).Seconds(), string(out))

		// reload buffer
		editedFiles := strings.Split(string(out), "\n")
		for i := range editedFiles {
			for j := range e.views {
				v := e.views[j]
				if v.Buf.Path != editedFiles[i] {
					continue
				}
				newBuf, err := BufferFromFile(editedFiles[i])
				if err != nil {
					log.Print(err)
					continue
				}
				newView := &View{Buf: newBuf}
				newView.Cursor = newBuf.Clamp(v.Cursor)
				newView.ScrollRow = v.ScrollRow
				newView.ScrollCol = v.ScrollCol
				e.views[j] = newView
				break
			}
		}
		e.diags = nil
		e.renaming = false
	default:
		e.renameInput.Update(ev)
	}
}

func (e *Editor) drawRename(f *kero.Frame, rect kero.Rect) {
	normal := kero.NewStyle()
	prompt := " Rename: "
	f.Write(rect.X, rect.Y, trimToWidth(prompt, rect.W), normal)
	inputX := len([]rune(prompt))
	if inputX >= rect.W {
		return
	}
	e.renameInput.Draw(f, kero.Rect{X: inputX, Y: rect.Y, W: rect.W - inputX, H: rect.H}, normal)
}

func GotoDefinition(e *Editor) {
	if !isGoFile(e.Buf().Path) {
		return
	}
	// flush buffer to disk before running gopls
	if e.Buf().Dirty {
		if err := e.Buf().SaveFile(); err != nil {
			log.Print(err)
			e.message = err.Error()
			return
		}
	}

	// For example, run:
	//   gopls definition main.go:33:2
	// ouput:
	//   /Users/cse/code/ke/main.go:33:2-7: defined here as var parts []string
	now := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gopls", "definition", fmt.Sprintf("%s:%d:%d",
		e.Buf().Path, e.View().Cursor.Row+1, e.View().Cursor.Col+1))
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Print(err)
		e.message = err.Error()
		return
	}
	log.Printf("run %q in %.1fs:\n%s", cmd, time.Since(now).Seconds(), string(out))
	l, err := ParseLocation(string(out))
	if err != nil {
		log.Print(err)
		return
	}
	e.gotoLocation(l)
}

type ReferencesPanel struct {
	Active    bool
	Index     int // active item
	Items     []Location
	ScrollRow int // vertical scrolling
	Height    int // UI render cap
	Header    string
}

func NewReferencesPanel(header string, items []Location) ReferencesPanel {
	return ReferencesPanel{Active: true, Header: header, Items: items, Height: 10}
}

func (r *ReferencesPanel) VisibleRows() int {
	h := r.Height
	if h == 0 {
		h = 10
	}
	return min(len(r.Items), h)
}

func (r *ReferencesPanel) Next() Location {
	total := len(r.Items)
	if total == 0 {
		return Location{}
	}
	if total == 1 {
		return r.Items[0]
	}
	r.Index = (r.Index + 1) % total

	// Calculate scrolling offset to keep selected item inside dropdown viewport
	visibleRows := r.VisibleRows()
	scroll := 0
	if r.Index >= visibleRows {
		scroll = r.Index - visibleRows + 1
	}
	r.ScrollRow = scroll
	return r.Items[r.Index]
}

func (r *ReferencesPanel) Prev() Location {
	total := len(r.Items)
	if total == 0 {
		return Location{}
	}
	if total == 1 {
		return r.Items[0]
	}
	r.Index = (r.Index - 1 + total) % total

	// Calculate scrolling offset to keep selected item inside dropdown viewport
	visibleRows := r.VisibleRows()
	scroll := 0
	if r.Index >= visibleRows {
		scroll = r.Index - visibleRows + 1
	}
	r.ScrollRow = scroll
	return r.Items[r.Index]
}

// Location represents coordinate within a specified file.
type Location struct {
	Filename string
	Pos      Position
}

func ParseLocation(s string) (Location, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 3 {
		return Location{}, errors.New("unknown position: " + s)
	}
	filename := parts[0]
	lineNo, err := strconv.Atoi(parts[1])
	if err != nil {
		return Location{}, err
	}
	segments := strings.Split(parts[2], "-")
	if len(segments) != 2 {
		return Location{}, errors.New("unknown position: " + s)
	}
	startCol, err := strconv.Atoi(segments[0])
	if err != nil {
		return Location{}, err
	}
	endCol, err := strconv.Atoi(segments[1])
	if err != nil {
		return Location{}, err
	}
	_ = endCol
	loc := Location{
		Filename: filename,
		Pos: Position{
			Row: lineNo - 1,
			Col: startCol - 1,
		},
	}
	return loc, nil
}

func (e *Editor) gotoLocation(l Location) {
	e.recordJump()
	if e.Buf().Path != l.Filename {
		err := e.OpenFile(l.Filename)
		if err != nil {
			log.Print(err)
			e.message = err.Error()
			return
		}
		e.diagnose()
	}
	e.View().Cursor = l.Pos
}

// For example, run:
//
//	gopls references main.go:34:2
//
// ouput:
//
//	/Users/cse/code/ke/main.go:35:9-14
//	/Users/cse/code/ke/main.go:39:9-14
//
// Positions within files are specified as file.go:line:column triples,
// where the line and column start at 1, and columns are measured in bytes of the UTF-8 encoding.
// More details see https://go.dev/gopls/command-line
func findReferences(e *Editor) {
	if !isGoFile(e.Buf().Path) {
		return
	}
	// flush buffer to disk before running gopls
	if e.Buf().Dirty {
		if err := e.Buf().SaveFile(); err != nil {
			log.Print(err)
			e.message = err.Error()
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Now()
	cmd := exec.CommandContext(ctx, "gopls", "references", fmt.Sprintf("%s:%d:%d",
		e.Buf().Path, e.View().Cursor.Row+1, e.View().Cursor.Col+1))
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Print(err)
		e.message = err.Error()
		return
	}
	log.Printf("run %q in %.1fs:\n%s", cmd, time.Since(now).Seconds(), string(out))
	rawLines := strings.Split(string(out), "\n")
	if len(rawLines) == 0 {
		return
	}
	locations := make([]Location, 0, len(rawLines))
	for _, line := range rawLines {
		p, err := ParseLocation(line)
		if err != nil {
			continue
		}
		locations = append(locations, p)
	}
	start, end := e.Buf().WordBounds(e.View().Cursor)
	header := fmt.Sprintf("%d references for %q", len(locations), e.Buf().TextRange(start, end))
	e.ref = NewReferencesPanel(header, locations)
}

// drawReferences renders a bottom overlay panel for LSP References.
func (e *Editor) drawReferences(f *kero.Frame, rect kero.Rect) {
	if !e.ref.Active || len(e.ref.Items) == 0 {
		return
	}

	style := kero.NewStyle().Reverse()
	f.Fill(rect, ' ', style)
	f.Write(rect.X+1, rect.Y, e.ref.Header, style)

	buffers := make([]*Buffer, 0, len(e.views))
	for _, v := range e.views {
		buffers = append(buffers, v.Buf)
	}
	getBuffer := func(path string) (*Buffer, error) {
		for _, b := range buffers {
			if b.Path == path {
				return b, nil
			}
		}
		b, err := BufferFromFile(path)
		if err != nil {
			return nil, err
		}
		buffers = append(buffers, b)
		return b, nil
	}

	// Render items
	for i := range rect.H - 1 {
		idx := i + e.ref.ScrollRow
		if idx >= len(e.ref.Items) {
			break
		}

		item := e.ref.Items[idx]
		lineY := rect.Y + i + 1 // 1 is the header row

		indicator := " "
		itemStyle := style
		if idx == e.ref.Index {
			indicator = ">"
			itemStyle = itemStyle.Bold()
		}

		buf, err := getBuffer(item.Filename)
		if err != nil {
			log.Print(err)
			continue
		}

		line := string(buf.Lines[item.Pos.Row])
		text := fmt.Sprintf("%s %s:%d:%d: %s", indicator, filepath.Base(item.Filename), item.Pos.Row+1,
			item.Pos.Col+1, line)
		runes := []rune(text)

		if len(runes) > rect.W {
			text = string(runes[:rect.W])
		}
		f.Write(rect.X+1, lineY, text, itemStyle)
	}
}

// CheckFile checks file on disk, make accurate diagnoses.
func CheckFile(path string) ([]*scanner.Error, error) {
	if !isGoFile(path) {
		return nil, errors.New("Go file only")
	}

	/*
	   gopls check main.go
	   /Users/cse/code/ke/main.go:35:2-6: declared and not used: part
	   /Users/cse/code/ke/main.go:36:9-14: undefined: parts
	*/
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Now()
	cmd := exec.CommandContext(ctx, "gopls", "check", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	log.Printf("run %q in %.1fs", cmd, time.Since(now).Seconds())
	rawLines := strings.Split(string(out), "\n")
	if len(rawLines) == 0 {
		return nil, nil
	}
	errs := make([]*scanner.Error, 0, len(rawLines))
	for _, line := range rawLines {
		before, after, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		loc, err := ParseLocation(before)
		if err != nil {
			log.Print(err)
			continue
		}
		errs = append(errs, &scanner.Error{
			Pos: token.Position{Line: loc.Pos.Row + 1, Column: loc.Pos.Col + 1},
			Msg: after,
		})
	}
	return errs, nil
}

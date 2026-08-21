package main

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cansyan/kero"
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
	e.Buffer.Cursor = e.Buffer.ClampPos(Position{Row: row, Col: col})

	f, err := os.OpenFile("/tmp/ke.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	log.SetOutput(f)

	// set FPS for refreshing diagnostic
	p := kero.New(e, kero.WithAltScreen(true), kero.WithKitty(true), kero.WithFPS(3), kero.WithMouse(true))
	if err := p.Run(); err != nil {
		panic(err)
	}
}

// Editor implements kero.App interface
type Editor struct {
	*Buffer // alias to the buffers[active], make refactor easier
	buffers []*Buffer
	active  int // index of currently active buffer
	Width   int
	Height  int

	message string

	// prompt for saving
	saveAs    bool
	saveInput TextInput

	// find mode opens a find line at the message area
	finding        bool
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

	diags       []Diagnostic
	diagTimer   *time.Timer
	diagChan    chan diagResult
	diagVersion atomic.Uint64

	// reports whether the key comes from a paste action,
	// to distinguish the manual KeyEnter or a pasted \n
	pasting bool

	gutterW int

	jumps JumpList

	completion Completion
}

type Completion struct {
	Active bool
	Index  int
	Items  []SymbolLocation
}

func (c *Completion) Refresh(src any, query string) {
	results := ExtractAllSymbols(src)
	if len(results) == 0 {
		return
	}

	b := findQueryIgnoreCase(query)
	items := make([]SymbolLocation, 0, len(results))
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

	visibleRows := min(len(e.completion.Items), 10)
	var maxWidth int
	for i := range e.completion.Items {
		if width := len([]rune(e.completion.Items[i].String())); width > maxWidth {
			maxWidth = width
		}
	}

	// Calculate scrolling offset to keep selected item inside dropdown viewport
	offset := 0
	if e.completion.Index >= visibleRows {
		offset = e.completion.Index - visibleRows + 1
	}

	var normal kero.Style
	x := e.gutterW + e.VisualPos(e.Cursor).Col - e.LeftCol
	y := e.Cursor.Row - e.TopRow
	indicator := " >"
	rect := kero.Rect{X: x, Y: y - visibleRows, W: len(indicator) + maxWidth + 1, H: visibleRows}
	f.Fill(rect, ' ', normal.Reverse())
	for i := range visibleRows {
		y := rect.Y + i
		prefix := indicator
		if i+offset != e.completion.Index {
			prefix = "  "
		}
		label := prefix + e.completion.Items[i+offset].String()
		f.Write(rect.X, y, label, normal.Reverse())
	}
}

type diagResult struct {
	version     uint64
	diagnostics []Diagnostic
}

func (e *Editor) Init(ctx *kero.Context) error {
	e.Width, e.Height = ctx.Width, ctx.Height
	e.showCursor()
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

	switch ev := ev.(type) {
	case kero.TickEvent:
		return nil
	case kero.ResizeEvent:
		e.Width, e.Height = ev.Width, ev.Height
	case kero.PasteStartEvent:
		e.pasting = true
	case kero.PasteEndEvent:
		e.pasting = false
	case kero.MouseEvent:
		e.handleMouse(ev)
	case kero.KeyEvent:
		e.handleKey(ctx, ev)
	}
	e.lastEvent = ev
	return nil
}

func (e *Editor) cursorFromMouse(m kero.MouseEvent) Position {
	row := e.TopRow + m.Y
	if row >= len(e.Lines) {
		// out of viewport, return current cursor
		return e.Cursor
	}

	displayCol := m.X - e.gutterW + e.LeftCol
	return e.PosFromVisual(Position{Row: row, Col: displayCol})
}

func (e *Editor) handleMouse(m kero.MouseEvent) error {
	switch m.Button {
	case kero.MouseWheelUp:
		e.TopRow = max(0, e.TopRow-1)
	case kero.MouseWheelDown:
		e.TopRow = min(e.TopRow+1, len(e.Lines)-e.bufferH())
	case kero.MouseLeft:
		switch m.Action {
		case kero.MousePress:
			if m.Y >= e.bufferH() {
				// out of viewport
				return nil
			}
			e.Cursor = e.cursorFromMouse(m)
			if e.hasSelect() {
				e.clearSelect()
			}
		case kero.MouseRelease:
			if m.Y >= e.bufferH() {
				// out of viewport
				return nil
			}
			// ctrl+mouse_left_release goto definition
			if m.Mod == kero.ModCtrl {
				e.recordJump()
				e.GotoDefinition()
				e.showCursorCenter()
			}
		case kero.MouseDrag:
			if last, ok := e.lastEvent.(kero.MouseEvent); ok &&
				last.Button == kero.MouseLeft && last.Action == kero.MousePress {
				// first drag sets selection anchor
				e.Selecting = true
				e.SelAnchor = e.cursorFromMouse(last)
			}
			// later drag expands selection
			e.Cursor = e.cursorFromMouse(m)
			e.showCursor()
		}
	}
	return nil
}

func (e *Editor) handleKey(ctx *kero.Context, key kero.KeyEvent) error {
	// can quit at anytime, first priority
	if key.String() == "ctrl+q" {
		quitAgain := e.LastEvent() == "ctrl+q"
		if e.Dirty && !quitAgain {
			e.message = "warn: unsaved changes, press ctrl+s to save or ctrl+q again to quit"
			return nil
		}
		ctx.Quit()
		return nil
	}

	if e.saveAs {
		return e.updateSaveAs(key)
	}
	if e.finding {
		defer e.showCursor()
		return e.updateFind(key)
	}
	if e.palette.Active {
		e.updatePalette(key)
		return nil
	}

	var centerCursor bool
	var completing bool // mark whether showing completion on keystroke
	defer func() {
		if centerCursor {
			e.showCursorCenter()
		} else {
			e.showCursor()
		}

		e.completion.Active = completing
	}()

	switch key.Key {
	case kero.KeyRune:
		switch key.String() {
		case "ctrl+n":
			start, end := e.WordBounds(e.PrevPos(e.Cursor))
			word := e.GetRange(start, end)
			e.completion.Refresh(e.Buffer.NewReader(), word)
			if len(e.completion.Items) == 1 {
				// only 1 alternative, apply it early
				cursor := e.DeleteRange(start, end)
				e.Cursor = e.Insert(cursor, e.completion.Items[e.completion.Index].Name)
				e.markDirty()
				return nil
			}
			completing = len(e.completion.Items) > 0
		case "ctrl+-":
			e.JumpBack()
			centerCursor = true
		case "ctrl+shift+-", "ctrl+_":
			e.JumpForward()
			centerCursor = true
		case "ctrl+w":
			if e.Dirty && !(e.LastEvent() == "ctrl+w") {
				e.message = "warn: unsaved changes, press ctrl+s to save or ctrl+w again to close"
				return nil
			}
			if len(e.buffers) <= 1 {
				ctx.Quit()
				return nil
			}
			e.CloseBuffer()
		case "ctrl+r":
			e.palette.Open(ctx, e, "@")
		case "ctrl+g":
			e.recordJump()
			e.GotoDefinition()
			centerCursor = true
			return nil
		case "ctrl+p":
			e.palette.Open(ctx, e, "")
			return nil
		case "ctrl+[":
			e.PrevBuffer()
			return nil
		case "ctrl+]":
			e.NextBuffer()
			return nil
		case "ctrl+s":
			return e.save()
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
			if start, end := e.Buffer.WordBounds(e.Cursor); start != end {
				e.Selecting = true
				e.SelAnchor = start
				e.Cursor = end
			}
			return nil
		case "ctrl+l":
			// select the line under cursor
			e.selectLine()
			return nil
		case "ctrl+k":
			e.deleteToLineEnd()
			return nil
		case "ctrl+a":
			p := e.Buffer.LineStartNonSpace(e.Cursor)
			if e.Cursor == p {
				e.Cursor.Col = 0
				return nil
			}
			e.Cursor = p
		case "ctrl+e":
			e.Cursor = e.Buffer.LineEnd(e.Cursor)
		}
		if key.Mod != 0 {
			break
		}
		e.insertRune(key.Rune)
		if e.completion.Active {
			start, end := e.WordBounds(e.PrevPos(e.Cursor))
			word := e.GetRange(start, end)
			e.completion.Refresh(e.Buffer.NewReader(), word)
			completing = len(e.completion.Items) > 0
		}
	case kero.KeyEnter:
		if e.completion.Active {
			start, end := e.WordBounds(e.PrevPos(e.Cursor))
			cursor := e.DeleteRange(start, end)
			e.Cursor = e.Insert(cursor, e.completion.Items[e.completion.Index].Name)
			e.markDirty()
			return nil
		}

		if e.hasSelect() {
			e.deleteSelect()
		}

		if e.pasting {
			e.Cursor = e.Buffer.Insert(e.Cursor, "\n")
			return nil
		}

		// compute indentation
		autoIndent := func(line []rune, col int) string {
			if len(line) == 0 || col == 0 {
				return ""
			}
			// get indentation before the column
			var n int
			for _, b := range line[:col] {
				if !unicode.IsSpace(b) {
					break
				}
				n++
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
			line := e.Buffer.Line(e.Cursor.Row)
			indent := autoIndent(line, len(line))
			p := Position{Row: e.Cursor.Row, Col: len(line)}
			e.Cursor = e.Buffer.Insert(p, "\n"+indent)
		case "shift+enter":
			// insert newline above
			var prevIndent string
			if e.Cursor.Row > 0 {
				prevLine := e.Buffer.Line(e.Cursor.Row - 1)
				prevIndent = autoIndent(prevLine, len(prevLine))
			}
			e.Buffer.Insert(Position{Row: e.Cursor.Row, Col: 0}, "\n")
			e.Cursor = e.Buffer.Insert(Position{Row: e.Cursor.Row, Col: 0}, prevIndent)
		default:
			// insert newline under cursor
			line := e.Buffer.Line(e.Cursor.Row)
			indent := autoIndent(line, e.Cursor.Col)
			e.Cursor = e.Buffer.Insert(e.Cursor, "\n"+string(indent))
		}
		e.markDirty()
	case kero.KeyTab:
		if e.completion.Active {
			start, end := e.WordBounds(e.PrevPos(e.Cursor))
			cursor := e.DeleteRange(start, end)
			e.Cursor = e.Insert(cursor, e.completion.Items[e.completion.Index].Name)
			e.markDirty()
			break
		}

		if key.Mod&kero.ModShift != 0 {
			e.unindentSelectOrLine()
			break
		}
		if e.hasSelect() {
			start, end := orderPos(e.SelAnchor, e.Cursor)
			if start.Row != end.Row {
				e.indentSelect()
				break
			}
		}
		e.insertRune('\t')
	case kero.KeyBackspace:
		if e.hasSelect() {
			e.deleteSelect()
			break
		}
		switch key.String() {
		case "ctrl+backspace", "cmd+backspace":
			// delete to line start
			e.Cursor = e.Buffer.DeleteRange(Position{Row: e.Cursor.Row, Col: 0}, e.Cursor)
		case "alt+backspace":
			// delete word backwards
			prev := e.Buffer.MoveWordLeft(e.Cursor)
			e.Cursor = e.Buffer.DeleteRange(prev, e.Cursor)
		case "cmd+shift+backspace":
			// delete whole line
			e.Cursor = e.Buffer.DeleteRange(Position{Row: e.Cursor.Row}, Position{Row: e.Cursor.Row + 1})
		default:
			e.Cursor = e.Buffer.DeleteRange(e.Buffer.PrevPos(e.Cursor), e.Cursor)
		}
		e.markDirty()
	case kero.KeyDelete:
		e.Cursor = e.Buffer.DeleteRange(e.Cursor, e.Buffer.NextPos(e.Cursor))
		e.markDirty()
	case kero.KeyLeft:
		switch key.String() {
		case "alt+left":
			e.Cursor = e.Buffer.MoveWordLeft(e.Cursor)
		case "cmd+left":
			p := e.Buffer.LineStartNonSpace(e.Cursor)
			if e.Cursor == p {
				e.Cursor.Col = 0
			} else {
				e.Cursor = p
			}
		case "shift+left":
			// start selection
			if !e.Selecting {
				e.Selecting = true
				e.SelAnchor = e.Cursor
			}
			e.moveLeft()
		default:
			e.moveLeft()
		}
	case kero.KeyRight:
		switch key.String() {
		case "alt+right":
			e.Cursor = e.Buffer.MoveWordRight(e.Cursor)
		case "cmd+right":
			e.Cursor = e.Buffer.LineEnd(e.Cursor)
		case "shift+right":
			// start selection
			if !e.Selecting {
				e.Selecting = true
				e.SelAnchor = e.Cursor
			}
			e.moveRight()
		default:
			e.moveRight()
		}
	case kero.KeyUp:
		switch key.String() {
		case "cmd+up":
			e.recordJump()
			// file start
			e.Cursor.Row = 0
			e.Cursor.Col = 0
		case "shift+up":
			// start selection
			if !e.Selecting {
				e.Selecting = true
				e.SelAnchor = e.Cursor
			}
			e.moveUp()
		default:
			if e.completion.Active {
				e.completion.Prev()
				completing = true
				return nil
			}
			e.moveUp()
		}
	case kero.KeyDown:
		switch key.String() {
		case "cmd+down":
			e.recordJump()
			// file end
			e.Cursor.Row = len(e.Buffer.Lines) - 1
			e.Cursor = e.Buffer.LineEnd(e.Cursor)
		case "shift+down":
			// start selection
			if !e.Selecting {
				e.Selecting = true
				e.SelAnchor = e.Cursor
			}
			e.moveDown()
		default:
			if e.completion.Active {
				e.completion.Next()
				completing = true
				return nil
			}
			e.moveDown()
		}
	case kero.KeyHome:
		p := e.Buffer.LineStartNonSpace(e.Cursor)
		if e.Cursor == p {
			e.Cursor.Col = 0
			return nil
		}
		e.Cursor = p
	case kero.KeyEnd:
		e.Cursor = e.Buffer.LineEnd(e.Cursor)
	case kero.KeyPgUp:
		e.recordJump()
		e.Cursor.Row -= e.bufferH()
		e.Cursor = e.Buffer.ClampPos(e.Cursor)
	case kero.KeyPgDown:
		e.recordJump()
		e.Cursor.Row += e.bufferH()
		e.Cursor = e.Buffer.ClampPos(e.Cursor)
	case kero.KeyEsc:
		if e.completion.Active {
			e.completion.Active = false
			return nil
		}
		e.clearSelect()
	}
	return nil
}

func (e *Editor) View(ctx *kero.Context, f *kero.Frame) {
	statusStyle := kero.NewStyle().Reverse()
	lineNoStyle := kero.NewStyle().Foreground(kero.ColorBlue).Dim()
	textStyle := kero.NewStyle()
	cursorStyle := textStyle.Reverse().Foreground(kero.ColorRed)
	selectStyle := textStyle.Reverse()
	messageStyle := kero.NewStyle()

	bufferH := e.bufferH()
	lineNoW := lineNumberW(len(e.Lines))
	gutterW := lineNoW + 2 // marker, line number, and separator
	e.gutterW = gutterW
	for y := range bufferH {
		lineIndex := e.TopRow + y
		if lineIndex >= len(e.Lines) {
			f.Write(0, y, "~", lineNoStyle)
			continue
		}

		lnStyle := lineNoStyle
		if lineIndex == e.Cursor.Row {
			lnStyle = kero.NewStyle().Foreground(kero.ColorBlue)
		}
		f.Write(1, y, fmt.Sprintf("%*d ", lineNoW, lineIndex+1), lnStyle)

		srcLine := e.Buffer.Line(lineIndex)
		fullPadded := padTab(srcLine, 4)
		limit := max(0, ctx.Width-gutterW)

		// visual part of the padded line
		var visPadded string
		if e.LeftCol < len(fullPadded) {
			visPadded = fullPadded[e.LeftCol:]
		}
		if len(visPadded) > limit {
			visPadded = visPadded[:limit]
		}

		// draw the line
		f.Write(gutterW, y, visPadded, textStyle)

		if v, ok := e.diagnosticForLine(lineIndex); ok {
			red := kero.NewStyle().Foreground(kero.ColorRed)
			f.Write(0, y, "x", red)
			//f.Write(gutterW+len(visPadded)+2, y, v.Message, red)
			dx := max(gutterW+len(visPadded), ctx.Width-len(v.Message))
			f.Write(dx, y, v.Message, red)
		}

		// highlight selection if any
		if e.Selecting {
			start, end := orderPos(e.SelAnchor, e.Cursor)
			if lineIndex >= start.Row && lineIndex <= end.Row {
				var selStartCol, selEndCol int
				if start.Row == end.Row {
					selStartCol = start.Col
					selEndCol = end.Col
				} else if lineIndex == start.Row {
					selStartCol = start.Col
					selEndCol = len(srcLine)
				} else if lineIndex == end.Row {
					selStartCol = 0
					selEndCol = end.Col
				} else {
					selStartCol = 0
					selEndCol = len(srcLine)
				}

				visStart := e.VisualPos(Position{Row: lineIndex, Col: selStartCol})
				visEnd := e.VisualPos(Position{Row: lineIndex, Col: selEndCol})

				startDisplay := max(0, min(visStart.Col-e.LeftCol, len(visPadded)))
				endDisplay := max(0, min(visEnd.Col-e.LeftCol, len(visPadded)))

				for x := startDisplay; x < endDisplay; x++ {
					ch := rune(visPadded[x])
					f.Set(gutterW+x, y, ch, selectStyle)
				}
			}
		}

		if e.finding && e.findMatch && lineIndex == e.findMatchStart.Row && lineIndex == e.findMatchEnd.Row {
			visStart := e.VisualPos(Position{Row: lineIndex, Col: e.findMatchStart.Col})
			visEnd := e.VisualPos(Position{Row: lineIndex, Col: e.findMatchEnd.Col})

			startDisplay := max(0, min(visStart.Col-e.LeftCol, len(visPadded)))
			endDisplay := max(0, min(visEnd.Col-e.LeftCol, len(visPadded)))

			for x := startDisplay; x < endDisplay; x++ {
				ch := rune(visPadded[x])
				f.Set(gutterW+x, y, ch, selectStyle)
			}
		}
	}

	cursorVisPos := e.VisualPos(e.Cursor)
	cursorX := gutterW + cursorVisPos.Col - e.LeftCol
	cursorY := 0 + e.Cursor.Row - e.TopRow
	if cursorY >= 0 && cursorY < bufferH && cursorX >= gutterW && cursorX < ctx.Width {
		fullLinePadded := padTab(e.Line(e.Cursor.Row), 4)
		ch := ' '
		if cursorVisPos.Col < len(fullLinePadded) {
			ch = rune(fullLinePadded[cursorVisPos.Col])
		}
		f.Set(cursorX, cursorY, ch, cursorStyle)
	}

	statusY := ctx.Height - 1
	if statusY >= 0 {
		var names strings.Builder
		for i, b := range e.buffers {
			if i == e.active && len(e.buffers) > 1 {
				names.WriteString("[")
			}
			name := "untitled"
			if b.Path != "" {
				name = filepath.Base(b.Path)
			}
			names.WriteString(name)
			if b.Dirty {
				names.WriteString("*")
			}
			if i == e.active && len(e.buffers) > 1 {
				names.WriteString("]")
			}
			names.WriteString(" ")
		}
		status := fmt.Sprintf(" %s| Line %d, Col %d", names.String(), e.Cursor.Row+1, cursorVisPos.Col+1)
		if e.Selecting {
			status = status + " | Selecting"
		}
		f.Fill(kero.Rect{X: 0, Y: statusY, W: ctx.Width, H: 1}, ' ', statusStyle)
		f.Write(0, statusY, trimToWidth(status, ctx.Width), statusStyle)
		if len(e.diags) > 0 {
			warn := fmt.Sprintf("%d diagnostic", len(e.diags))
			statusWidth := len([]rune(status))
			f.Write(statusWidth+1, statusY, "| ", statusStyle)
			eventWidth := len(e.LastEvent())
			remainWidth := ctx.Width - statusWidth - eventWidth
			if remainWidth > 0 {
				f.Write(statusWidth+3, statusY, trimToWidth(warn, remainWidth), statusStyle.Background(kero.ColorRed))
			}
		}
		if s := e.LastEvent(); s != "" {
			f.Write(ctx.Width-len(s), statusY, s, statusStyle)
		}
	}

	messageY := ctx.Height - 2
	if messageY >= 0 {
		if e.saveAs {
			e.drawSaveAs(f, messageY, ctx.Width)
			return
		}
		if e.finding {
			e.drawFind(f, messageY, ctx.Width)
			return
		}
		if e.palette.Active {
			e.drawPalette(f, messageY, ctx.Width)
			return
		}
		if e.message == "" {
			e.message = "^S save | ^W close | ^Q quit | ^F find | ^P palette | ^G definition | ^R symbols"
		}
		if strings.HasPrefix(e.message, "error:") || strings.HasPrefix(e.message, "warn:") {
			messageStyle = messageStyle.Foreground(kero.ColorRed)
		}
		f.Write(0, messageY, trimToWidth(" "+e.message, ctx.Width), messageStyle)
	}

	if e.completion.Active {
		e.drawCompletion(f)
	}
}

func (e *Editor) selectLine() {
	if !e.Selecting {
		e.Selecting = true
		e.SelAnchor = Position{Row: e.Cursor.Row, Col: 0}
	}
	if e.Cursor.Row < len(e.Buffer.Lines)-1 {
		e.Cursor.Row++
		e.Cursor.Col = 0
	} else {
		e.Cursor = e.Buffer.LineEnd(e.Cursor)
	}
}

func isWordChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

func (e *Editor) hasSelect() bool {
	return e.Selecting && e.SelAnchor != e.Cursor
}

func (e *Editor) clearSelect() {
	e.Selecting = false
}

func (e *Editor) copy() {
	if e.hasSelect() {
		e.clipboard = e.Buffer.GetRange(e.SelAnchor, e.Cursor)
		e.clipIsLine = false
		return
	}
	// copy entire current line, remember it's a line copy
	e.clipboard = string(e.Buffer.Line(e.Cursor.Row))
	e.clipIsLine = true
}

func (e *Editor) indentSelect() {
	if !e.hasSelect() {
		return
	}

	start, end := orderPos(e.SelAnchor, e.Cursor)
	if start.Row == end.Row {
		return
	}
	lastRow := end.Row
	if end.Col == 0 && end.Row > start.Row {
		lastRow = end.Row - 1
	}
	for r := start.Row; r <= lastRow; r++ {
		e.Buffer.Insert(Position{Row: r, Col: 0}, "\t")
	}
	if start.Row <= e.SelAnchor.Row && e.SelAnchor.Row <= lastRow {
		e.SelAnchor.Col++
	}
	if start.Row <= e.Cursor.Row && e.Cursor.Row <= lastRow {
		e.Cursor.Col++
	}
	e.markDirty()
}

func unindentLine(s string) (string, int) {
	if strings.HasPrefix(s, "\t") {
		return s[1:], 1
	}
	spaces := 0
	for spaces < 4 && spaces < len(s) && s[spaces] == ' ' {
		spaces++
	}
	if spaces > 0 {
		return s[spaces:], spaces
	}
	return s, 0
}

func (e *Editor) unindentSelectOrLine() {
	if !e.hasSelect() {
		newLine, removed := unindentLine(string(e.Buffer.Line(e.Cursor.Row)))
		if removed > 0 {
			e.Buffer.SetLine(e.Cursor.Row, []rune(newLine))
			e.Cursor.Col = max(0, e.Cursor.Col-removed)
			e.markDirty()
		}
		return
	}

	start, end := orderPos(e.SelAnchor, e.Cursor)
	lastRow := end.Row
	if end.Col == 0 && end.Row > start.Row {
		lastRow = end.Row - 1
	}
	var startRemoved, endRemoved int
	for r := start.Row; r <= lastRow; r++ {
		newLine, removed := unindentLine(string(e.Buffer.Line(r)))
		if r == e.SelAnchor.Row {
			startRemoved = removed
		}
		if r == e.Cursor.Row {
			endRemoved = removed
		}
		e.Buffer.SetLine(r, []rune(newLine))
	}

	if e.SelAnchor.Row <= lastRow {
		e.SelAnchor.Col = max(0, e.SelAnchor.Col-startRemoved)
	}
	if e.Cursor.Row <= lastRow {
		e.Cursor.Col = max(0, e.Cursor.Col-endRemoved)
	}
	e.markDirty()
}

func (e *Editor) deleteSelect() {
	if !e.hasSelect() {
		return
	}
	start, end := orderPos(e.SelAnchor, e.Cursor)
	e.Cursor = e.Buffer.DeleteRange(start, end)
	e.Selecting = false
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
	p1 := Position{Row: e.Cursor.Row}
	e.clipboard = e.Buffer.GetRange(p1, e.Buffer.LineEnd(e.Cursor))
	e.clipIsLine = true
	e.Cursor = e.Buffer.DeleteRange(p1, Position{Row: e.Cursor.Row + 1})
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
		e.Buffer.Insert(Position{Row: e.Cursor.Row, Col: 0}, e.clipboard+"\n")
		e.Cursor.Row++
		e.markDirty()
		return
	}

	e.Cursor = e.Buffer.Insert(e.Cursor, e.clipboard)
	e.markDirty()
}

func padTab(s []rune, tabSize int) string {
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
	if e.Buffer == nil {
		return nil
	}
	if e.Path == "" {
		e.startSaveAs()
		return nil
	}

	if _, err := e.Buffer.Format(); err != nil {
		log.Print(err)
	}

	if err := e.Buffer.Save(); err != nil {
		e.message = "error: " + err.Error()
		return nil
	}

	e.message = fmt.Sprintf("saved %s", filepath.Base(e.Path))
	return nil
}

func (e *Editor) startSaveAs() {
	e.saveAs = true
	e.saveInput.SetText(e.Path)
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

	e.Path = path
	e.saveAs = false
	return e.save()
}

func (e *Editor) drawSaveAs(f *kero.Frame, y int, width int) {
	normal := kero.NewStyle().Foreground(kero.ColorYellow)
	errorStyle := kero.NewStyle().Foreground(kero.ColorRed)
	style := normal
	if e.message == "filename required" {
		style = errorStyle
	}

	prompt := " Save as: "
	f.Write(0, y, trimToWidth(prompt, width), style)
	inputX := len([]rune(prompt))
	if inputX >= width {
		return
	}
	e.saveInput.Draw(f, kero.Rect{X: inputX, Y: y, W: width - inputX, H: 1}, style)
}

// startFind starts a Find prompt, and pre-fill with selection or last query, if any.
func (e *Editor) startFind() {
	e.finding = true
	e.replacing = false
	if e.hasSelect() {
		e.findInput.SetTextAndSelectAll(e.Buffer.GetRange(e.SelAnchor, e.Cursor))
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
				return e.replaceAll()
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
				if start, end, ok := e.Buffer.FindPrevIgnoreCase(query, e.Cursor); ok {
					e.findMatch = true
					e.findMatchStart = start
					e.findMatchEnd = end
					e.Cursor = start
					e.clearSelect()
				}
			} else {
				if start, end, ok := e.Buffer.FindPrev(query, e.Cursor); ok {
					e.findMatch = true
					e.findMatchStart = start
					e.findMatchEnd = end
					e.Cursor = start
					e.clearSelect()
				}
			}
			return nil
		}

		if ignoreCase {
			start, end, ok := e.Buffer.FindNextIgnoreCase(query, e.Cursor)
			if ok {
				e.findMatch = true
				e.findMatchStart = start
				e.findMatchEnd = end
				e.Cursor = end
				e.clearSelect()
			}
		} else {
			start, end, ok := e.Buffer.FindNext(query, e.Cursor)
			if ok {
				e.findMatch = true
				e.findMatchStart = start
				e.findMatchEnd = end
				e.Cursor = end
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

func (e *Editor) drawFind(f *kero.Frame, y int, width int) {
	normal := kero.NewStyle()
	prompt := " Find: "
	f.Write(0, y, trimToWidth(prompt, width), normal)
	inputX := len([]rune(prompt))
	if inputX >= width {
		return
	}
	if !e.replacing {
		e.findInput.Draw(f, kero.Rect{X: inputX, Y: y, W: width - inputX, H: 1}, normal)
		return
	}
	e.findInput.Draw(f, kero.Rect{X: inputX, Y: y, W: width - inputX, H: 1}, normal)
	replaceX := inputX + len([]rune(e.findInput.String())) + 4
	if replaceX < width {
		f.Write(replaceX-4, y, " -> ", normal.Foreground(kero.ColorYellow))
		e.replaceInput.Draw(f, kero.Rect{X: replaceX, Y: y, W: width - replaceX, H: 1}, normal)
	}
}

func (e *Editor) skipFindMatch() {
	query := e.findInput.String()
	if query == "" {
		return
	}
	from := e.Cursor
	if e.findMatch {
		from = e.findMatchEnd
	}
	ignoreCase := findQueryIgnoreCase(query)
	var start, end Position
	var ok bool
	if ignoreCase {
		start, end, ok = e.Buffer.FindNextIgnoreCase(query, from)
	} else {
		start, end, ok = e.Buffer.FindNext(query, from)
	}
	if !ok {
		e.findMatch = false
		return
	}
	e.findMatch = true
	e.findMatchStart = start
	e.findMatchEnd = end
	e.Cursor = end
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

	start := e.findMatchStart
	replacedEnd := e.Buffer.DeleteRange(start, e.findMatchEnd)
	replacedEnd = e.Buffer.Insert(replacedEnd, e.replaceInput.String())
	e.markDirty()
	e.Cursor = replacedEnd
	e.findMatch = false
	if e.replaceInput.String() == query && replacedEnd.Col < e.Buffer.LineEnd(replacedEnd).Col {
		replacedEnd.Col++
		e.Cursor = replacedEnd
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
		count = e.Buffer.ReplaceAllIgnoreCase(query, e.replaceInput.String())
	} else {
		count = e.Buffer.ReplaceAll(query, e.replaceInput.String())
	}
	if count > 0 {
		e.markDirty()
	}
	e.findMatch = false
	e.message = fmt.Sprintf("replaced %d matches", count)
	return nil
}

func (e *Editor) insertRune(r rune) {
	if e.hasSelect() {
		e.deleteSelect()
	}
	e.Cursor = e.Buffer.Insert(e.Cursor, string([]rune{r}))
	e.markDirty()
}

func (e *Editor) deleteToLineEnd() {
	if e.hasSelect() {
		e.deleteSelect()
		return
	}
	e.Cursor = e.Buffer.DeleteRange(e.Cursor, e.Buffer.LineEnd(e.Cursor))
	e.markDirty()
}

func (e *Editor) moveLeft() {
	e.Cursor = e.Buffer.PrevPos(e.Cursor)
}

func (e *Editor) moveRight() {
	e.Cursor = e.Buffer.NextPos(e.Cursor)
}

func (e *Editor) moveUp() {
	if e.Cursor.Row == 0 {
		return
	}
	vp := e.VisualPos(e.Cursor)
	e.Cursor = e.PosFromVisual(Position{Row: vp.Row - 1, Col: vp.Col})
}

func (e *Editor) moveDown() {
	if e.Cursor.Row == len(e.Buffer.Lines)-1 {
		return
	}
	vp := e.VisualPos(e.Cursor)
	e.Cursor = e.PosFromVisual(Position{Row: vp.Row + 1, Col: vp.Col})
}

func (e *Editor) markDirty() {
	e.Dirty = true
	e.message = ""
	if isGoFile(e.Path) {
		e.debounceDiagnose()
	}
}

// debounceDiagnose is In-memory, fast syntax and type checking
// on current buffer, debounced 300ms on keypress
func (e *Editor) debounceDiagnose() {
	if e.Buffer == nil {
		return
	}

	version := e.diagVersion.Add(1)
	// cancel previous check
	if e.diagTimer != nil {
		e.diagTimer.Stop()
	}
	if e.diagChan == nil {
		e.diagChan = make(chan diagResult, 4)
	}
	filename := e.Path
	src := e.Buffer.NewReader()
	e.diagTimer = time.AfterFunc(300*time.Millisecond, func() {
		diags := CheckSemantics(filename, src)
		result := diagResult{version: version, diagnostics: diags}
		select {
		case <-e.diagChan:
		default:
		}
		select {
		case e.diagChan <- result:
		default:
		}
	})
}

func (e *Editor) applyDiagnostic() {
	if e.diagChan == nil {
		return
	}
	for {
		select {
		case result := <-e.diagChan:
			if result.version == e.diagVersion.Load() {
				e.diags = result.diagnostics
			}
		default:
			return
		}
	}
}

func (e *Editor) diagnosticForLine(row int) (Diagnostic, bool) {
	for _, d := range e.diags {
		if d.Row == row {
			return d, true
		}
	}
	return Diagnostic{}, false
}

func (e *Editor) nextDiagnostic() Diagnostic {
	if len(e.diags) == 0 {
		return Diagnostic{}
	}

	for _, d := range e.diags {
		if d.Row > e.Cursor.Row ||
			(d.Row == e.Cursor.Row && d.Col > e.Cursor.Col) {
			return d
		}
	}

	return e.diags[0]
}

func isGoFile(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".go")
}

// showCursor adjusts TopRow and LeftCol to ensure the cursor is within
// the visible viewport.
func (e *Editor) showCursor() {
	bufH := e.bufferH()
	if bufH <= 0 {
		return
	}

	// 1. Vertical Scrolling (Row)
	maxTopRow := e.Cursor.Row - (bufH - 1)
	if e.TopRow < maxTopRow {
		e.TopRow = maxTopRow
	}

	// Ensure cursor is below the top margin
	minTopRow := e.Cursor.Row
	if e.TopRow > minTopRow {
		e.TopRow = minTopRow
	}

	// Clamp to top boundary
	if e.TopRow < 0 {
		e.TopRow = 0
	}
	// 2. Horizontal Scrolling (Column)
	textW := e.Width - lineNumberW(len(e.Lines)) - 2
	textW = max(1, textW)

	visualCursor := e.VisualPos(e.Cursor)
	if visualCursor.Col < e.LeftCol {
		e.LeftCol = visualCursor.Col
	} else if visualCursor.Col >= e.LeftCol+textW {
		e.LeftCol = visualCursor.Col - textW + 1
	}

	if e.LeftCol < 0 {
		e.LeftCol = 0
	}
}

// showCursor adjusts TopRow and LeftCol to ensure the cursor is at
// the center of visible viewport.
func (e *Editor) showCursorCenter() {
	bufH := e.bufferH()
	if bufH <= 0 {
		return
	}

	margin := bufH / 2

	// 1. Vertical Scrolling (Row)
	// Ensure cursor is above the bottom margin
	maxTopRow := e.Cursor.Row - (bufH - 1 - margin)
	if e.TopRow < maxTopRow {
		e.TopRow = maxTopRow
	}

	// Ensure cursor is below the top margin
	minTopRow := e.Cursor.Row - margin
	if e.TopRow > minTopRow {
		e.TopRow = minTopRow
	}

	// Clamp to top boundary
	if e.TopRow < 0 {
		e.TopRow = 0
	}

	// 2. Horizontal Scrolling (Column)
	textW := e.Width - lineNumberW(len(e.Lines)) - 2
	textW = max(1, textW)

	visCursor := e.VisualPos(e.Cursor)

	if visCursor.Col < e.LeftCol {
		e.LeftCol = visCursor.Col
	} else if visCursor.Col >= e.LeftCol+textW {
		e.LeftCol = visCursor.Col - textW + 1
	}

	if e.LeftCol < 0 {
		e.LeftCol = 0
	}
}

// buffer height = app height - 2
func (e *Editor) bufferH() int {
	h := e.Height - 2
	if h < 0 {
		return 0
	}
	return h
}

// return the width of line number
func lineNumberW(lines int) int {
	width := 1
	for lines >= 10 {
		width++
		lines /= 10
	}
	return width
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

func (e *Editor) GotoDefinition() {
	reader := e.Buffer.NewReader()

	// Convert internal 0-based cursor to 1-based for go/token
	res := FindDefinitionLoc(reader, e.Cursor.Row+1, e.Cursor.Col+1)
	if !res.Found {
		return // Symbol definition not found in current file
	}

	// Jump editor cursor (converting back to 0-based)
	e.Cursor.Row = res.Line - 1
	e.Cursor.Col = res.Column - 1
}

// TextInput is a small editable single-line text widget.
type TextInput struct {
	runes       []rune
	Cursor      int // Rune index
	Placeholder string
	SelectAll   bool
}

// SetText populates the input and sets cursor to the end.
func (t *TextInput) SetText(s string) {
	t.runes = []rune(s)
	t.Cursor = len(t.runes)
}

func (t *TextInput) SetTextAndSelectAll(s string) {
	t.runes = []rune(s)
	t.Cursor = len(t.runes)
	t.SelectAll = true
}

func (t *TextInput) Reset() {
	t.runes = t.runes[:0] // Reuse underlying array memory
	t.Cursor = 0
	t.Placeholder = ""
	t.SelectAll = false
}

func (t *TextInput) String() string {
	return string(t.runes)
}

// Len returns the current rune count.
func (t *TextInput) Len() int {
	return len(t.runes)
}

func (t *TextInput) Update(ev kero.Event) {
	e, ok := ev.(kero.KeyEvent)
	if !ok {
		return
	}

	// Handle SelectAll replacement on first keystroke
	if t.SelectAll {
		if e.Key == kero.KeyRune || e.Key == kero.KeyBackspace {
			t.SelectAll = false
			t.runes = t.runes[:0]
			t.Cursor = 0
			if e.Key == kero.KeyBackspace {
				return
			}
		} else {
			// Arrow keys or Esc just clear selection state
			t.SelectAll = false
		}
	}

	switch e.Key {
	case kero.KeyRune:
		if e.Mod&kero.ModCtrl != 0 {
			return
		}
		t.runes = slices.Insert(t.runes, t.Cursor, e.Rune)
		t.Cursor++

	case kero.KeyBackspace:
		if t.Cursor > 0 {
			t.runes = slices.Delete(t.runes, t.Cursor-1, t.Cursor)
			t.Cursor--
		}

	case kero.KeyDelete:
		if t.Cursor < len(t.runes) {
			t.runes = slices.Delete(t.runes, t.Cursor, t.Cursor+1)
		}

	case kero.KeyLeft:
		if t.Cursor > 0 {
			t.Cursor--
		}

	case kero.KeyRight:
		if t.Cursor < len(t.runes) {
			t.Cursor++
		}

	case kero.KeyHome:
		t.Cursor = 0

	case kero.KeyEnd:
		t.Cursor = len(t.runes)
	}
}

// Draw renders the text input and its cursor.
func (t TextInput) Draw(f *kero.Frame, r kero.Rect, s kero.Style) {
	cursorStyle := s.Reverse().Foreground(kero.ColorRed)

	if t.SelectAll && len(t.runes) > 0 {
		for i, ch := range t.runes {
			x := r.X + i
			if x < r.Right() {
				f.Set(x, r.Y, ch, s.Reverse())
			}
		}
		return
	}

	if t.Cursor < 0 {
		t.Cursor = 0
	}
	if t.Cursor > len(t.runes) {
		t.Cursor = len(t.runes)
	}

	// Render placeholder when value is empty
	if len(t.runes) == 0 && t.Placeholder != "" {
		for i, ch := range []rune(t.Placeholder) {
			if i == 0 {
				f.Set(r.X+i, r.Y, ch, cursorStyle)
			} else {
				f.Set(r.X+i, r.Y, ch, s.Dim())
			}
		}
		return
	}

	// Render characters
	for i, ch := range t.runes {
		x := r.X + i
		if x >= r.Right() {
			break
		}

		if i == t.Cursor {
			f.Set(x, r.Y, ch, cursorStyle)
		} else {
			f.Set(x, r.Y, ch, s)
		}
	}

	// Render trailing cursor when at end of line
	if t.Cursor == len(t.runes) {
		x := r.X + t.Cursor
		if x < r.Right() {
			f.Set(x, r.Y, ' ', cursorStyle)
		}
	}
}

// Position represents the buffer character index
type Position struct {
	Row int // line index, starting at 0
	Col int // column index, starting at 0
}

type Buffer struct {
	Path   string
	Lines  [][]rune // Using [][]rune handles multi-byte UTF-8 correctly
	Cursor Position
	Dirty  bool

	// viewport
	TopRow  int // vertical scroll offsets, starts from 0
	LeftCol int // horizontal scroll offsets, starts from display column offset (0-based)

	Selecting bool
	SelAnchor Position // selection at [e.selAnchor, e.pos)
}

func NewBuffer(content string) *Buffer {
	rawLines := strings.Split(content, "\n")
	lines := make([][]rune, len(rawLines))
	for i, l := range rawLines {
		lines[i] = []rune(l)
	}
	return &Buffer{Lines: lines}
}

// Line returns a copy of the line at the specified row.
// Returns nil if the row index is out of bounds.
func (b *Buffer) Line(row int) []rune {
	if row < 0 || row >= len(b.Lines) {
		return nil
	}
	src := b.Lines[row]
	if src == nil {
		return nil
	}

	dst := make([]rune, len(src))
	copy(dst, src)
	return dst
}

func (b *Buffer) SetLine(row int, line []rune) {
	if row < 0 || row >= len(b.Lines) {
		return
	}

	// Copy input slice to prevent external modification
	dst := make([]rune, len(line))
	copy(dst, line)
	b.Lines[row] = dst
}

// String returns the full buffer text as a string, joined by newlines.
func (b *Buffer) String() string {
	linesCount := len(b.Lines)
	if linesCount == 0 {
		return ""
	}

	// 1. Calculate approximate total byte capacity to minimize allocations
	var totalBytes int
	for _, line := range b.Lines {
		for _, r := range line {
			totalBytes += utf8.RuneLen(r)
		}
	}
	// Add space for newline characters ('\n')
	totalBytes += (linesCount - 1)

	// 2. Pre-allocate strings.Builder buffer
	var sb strings.Builder
	sb.Grow(totalBytes)

	// 3. Write lines joined by newlines
	for i, line := range b.Lines {
		if i > 0 {
			sb.WriteByte('\n')
		}
		for _, r := range line {
			sb.WriteRune(r)
		}
	}

	return sb.String()
}

// InsertAt inserts text at the given position and returns the new cursor position.
func (b *Buffer) Insert(p Position, text string) Position {
	if p.Row < 0 || p.Row >= len(b.Lines) {
		return p
	}

	line := b.Lines[p.Row]
	if p.Col < 0 {
		p.Col = 0
	}
	if p.Col > len(line) {
		p.Col = len(line)
	}

	insertLines := strings.Split(text, "\n")

	// Single-line insertion fast path
	if len(insertLines) == 1 {
		runesToInsert := []rune(insertLines[0])
		newLine := make([]rune, 0, len(line)+len(runesToInsert))
		newLine = append(newLine, line[:p.Col]...)
		newLine = append(newLine, runesToInsert...)
		newLine = append(newLine, line[p.Col:]...)

		b.Lines[p.Row] = newLine

		return Position{
			Row: p.Row,
			Col: p.Col + len(runesToInsert),
		}
	}

	// Multi-line insertion path
	prefix := line[:p.Col]
	suffix := line[p.Col:]

	firstInsert := []rune(insertLines[0])
	lastInsert := []rune(insertLines[len(insertLines)-1])

	// First line gets prefix + first line of inserted text
	firstLine := append([]rune{}, prefix...)
	firstLine = append(firstLine, firstInsert...)

	// Last line gets last line of inserted text + suffix
	lastLine := append([]rune{}, lastInsert...)
	lastLine = append(lastLine, suffix...)

	// Prepare middle lines (if any)
	newSegment := make([][]rune, 0, len(insertLines))
	newSegment = append(newSegment, firstLine)

	for i := 1; i < len(insertLines)-1; i++ {
		newSegment = append(newSegment, []rune(insertLines[i]))
	}
	newSegment = append(newSegment, lastLine)

	// Replace target line with the expanded multi-line segment
	finalLines := make([][]rune, 0, len(b.Lines)+len(insertLines)-1)
	finalLines = append(finalLines, b.Lines[:p.Row]...)
	finalLines = append(finalLines, newSegment...)
	finalLines = append(finalLines, b.Lines[p.Row+1:]...)

	b.Lines = finalLines

	return Position{
		Row: p.Row + len(insertLines) - 1,
		Col: len(lastInsert),
	}
}

// orderPos guarantees start <= end (top-to-bottom, left-to-right)
func orderPos(p1, p2 Position) (start, end Position) {
	if p1.Row < p2.Row || (p1.Row == p2.Row && p1.Col <= p2.Col) {
		return p1, p2
	}
	return p2, p1
}

// ClampPos ensures p falls within valid buffer bounds.
func (b *Buffer) ClampPos(p Position) Position {
	if len(b.Lines) == 0 {
		return Position{Row: 0, Col: 0}
	}

	// Clamp Row
	row := p.Row
	if row < 0 {
		row = 0
	} else if row >= len(b.Lines) {
		row = len(b.Lines) - 1
	}

	// Clamp Col within the valid row
	lineLen := len(b.Lines[row])
	col := p.Col
	if col < 0 {
		col = 0
	} else if col > lineLen {
		col = lineLen
	}

	return Position{Row: row, Col: col}
}

// GetRange extracts the text between two positions (inclusive start, exclusive end).
func (b *Buffer) GetRange(p1, p2 Position) string {
	start, end := orderPos(b.ClampPos(p1), b.ClampPos(p2))

	if start == end {
		return ""
	}

	// Single-line range fast path
	if start.Row == end.Row {
		return string(b.Lines[start.Row][start.Col:end.Col])
	}

	// Multi-line range path
	var sb strings.Builder

	// First line fragment
	sb.WriteString(string(b.Lines[start.Row][start.Col:]))
	sb.WriteRune('\n')

	// Intermediate full lines
	for l := start.Row + 1; l < end.Row; l++ {
		sb.WriteString(string(b.Lines[l]))
		sb.WriteRune('\n')
	}

	// Final line fragment
	sb.WriteString(string(b.Lines[end.Row][:end.Col]))

	return sb.String()
}

// DeleteRange removes text between two positions [p1, p2) and returns the new cursor position.
func (b *Buffer) DeleteRange(p1, p2 Position) Position {
	start, end := orderPos(b.ClampPos(p1), b.ClampPos(p2))

	if start == end {
		return start
	}

	// Single-line deletion fast path
	if start.Row == end.Row {
		line := b.Lines[start.Row]
		newLine := make([]rune, 0, len(line)-(end.Col-start.Col))
		newLine = append(newLine, line[:start.Col]...)
		newLine = append(newLine, line[end.Col:]...)

		b.Lines[start.Row] = newLine
		return start
	}

	// Multi-line deletion path
	startLinePrefix := b.Lines[start.Row][:start.Col]
	endLineSuffix := b.Lines[end.Row][end.Col:]

	// Stitch start prefix and end suffix into one merged line
	mergedLine := make([]rune, 0, len(startLinePrefix)+len(endLineSuffix))
	mergedLine = append(mergedLine, startLinePrefix...)
	mergedLine = append(mergedLine, endLineSuffix...)

	// Rebuild line slice removing deleted lines
	newLines := make([][]rune, 0, len(b.Lines)-(end.Row-start.Row))
	newLines = append(newLines, b.Lines[:start.Row]...)
	newLines = append(newLines, mergedLine)
	newLines = append(newLines, b.Lines[end.Row+1:]...)

	b.Lines = newLines

	return start
}

// WordBounds finds the start and end of the word surrounding pos on its line.
func (b *Buffer) WordBounds(p Position) (start, end Position) {
	if p.Row < 0 || p.Row >= len(b.Lines) {
		return
	}

	line := b.Lines[p.Row]
	if len(line) == 0 {
		return
	}

	col := p.Col
	if col >= len(line) {
		col = len(line) - 1
	}

	// Determine matching mode based on target character under cursor
	targetIsWord := isWordChar(line[col])

	// Scan left for start
	startCol := col
	for startCol > 0 && isWordChar(line[startCol-1]) == targetIsWord {
		startCol--
	}

	// Scan right for end
	endCol := col
	for endCol < len(line) && isWordChar(line[endCol]) == targetIsWord {
		endCol++
	}

	start = Position{Row: p.Row, Col: startCol}
	end = Position{Row: p.Row, Col: endCol}
	return start, end
}

// FindNext searches forward starting after "from" for an exact query.
// Returns the position matching query, and true if found.
func (b *Buffer) FindNext(query string, from Position) (start, end Position, ok bool) {
	return b.findNext(query, from, false)
}

// FindNextIgnoreCase searches forward starting after "from" for query text,
// ignoring case.
func (b *Buffer) FindNextIgnoreCase(query string, from Position) (start, end Position, ok bool) {
	return b.findNext(query, from, true)
}

func (b *Buffer) findNext(query string, from Position, ignoreCase bool) (start, end Position, ok bool) {
	if query == "" || len(b.Lines) == 0 {
		return Position{}, Position{}, false
	}

	queryRunes := []rune(query)
	if len(queryRunes) == 0 {
		return Position{}, Position{}, false
	}

	row := from.Row
	col := from.Col
	for rowsSearched := 0; rowsSearched <= len(b.Lines); rowsSearched++ {
		line := b.Lines[row]
		for i := col; i <= len(line)-len(queryRunes); i++ {
			match := true
			for j := range queryRunes {
				if !runeMatches(line[i+j], queryRunes[j], ignoreCase) {
					match = false
					break
				}
			}
			if match {
				start = Position{Row: row, Col: i}
				end = Position{Row: row, Col: i + len(queryRunes)}
				return start, end, true
			}
		}

		if row+1 < len(b.Lines) {
			row++
		} else {
			row = 0
		}
		col = 0
	}

	return Position{}, Position{}, false
}

// FindPrev searches backward starting before fromPos for an exact query.
// Returns the start and end positions matching query, and true if found.
func (b *Buffer) FindPrev(query string, from Position) (start, end Position, ok bool) {
	return b.findPrev(query, from, false)
}

// FindPrevIgnoreCase searches backward starting before fromPos for query text,
// ignoring case.
func (b *Buffer) FindPrevIgnoreCase(query string, from Position) (start, end Position, ok bool) {
	return b.findPrev(query, from, true)
}

func (b *Buffer) findPrev(query string, from Position, ignoreCase bool) (start, end Position, ok bool) {
	if query == "" || len(b.Lines) == 0 {
		return Position{}, Position{}, false
	}

	queryRunes := []rune(query)
	if len(queryRunes) == 0 {
		return Position{}, Position{}, false
	}
	qLen := len(queryRunes)

	row := from.Row
	for rowsSearched := 0; rowsSearched <= len(b.Lines); rowsSearched++ {
		line := b.Lines[row]
		// Determine the rightmost starting column index for search on this row
		maxCol := len(line) - qLen
		if row == from.Row {
			maxCol = from.Col - qLen
		}

		// Search backward within current line
		if maxCol >= 0 {
			for c := maxCol; c >= 0; c-- {
				matched := true
				for i := range queryRunes {
					if !runeMatches(line[c+i], queryRunes[i], ignoreCase) {
						matched = false
						break
					}
				}
				if matched {
					start = Position{Row: row, Col: c}
					end = Position{Row: row, Col: c + qLen}
					return start, end, true
				}
			}
		}

		if row > 0 {
			row--
		} else {
			row = len(b.Lines) - 1
		}
	}

	return Position{}, Position{}, false
}

// ReplaceAll replaces every exact occurrence of query in the buffer.
func (b *Buffer) ReplaceAll(query, replacement string) (count int) {
	return b.replaceAll(query, replacement, false)
}

// ReplaceAllIgnoreCase replaces every case-insensitive occurrence of query.
func (b *Buffer) ReplaceAllIgnoreCase(query, replacement string) (count int) {
	return b.replaceAll(query, replacement, true)
}

func (b *Buffer) replaceAll(query, replacement string, ignoreCase bool) (count int) {
	queryRunes := []rune(query)
	if len(queryRunes) == 0 {
		return 0
	}

	for row, line := range b.Lines {
		result := make([]rune, 0, len(line))
		for col := 0; col < len(line); {
			if col+len(queryRunes) <= len(line) {
				matched := true
				for i, queryRune := range queryRunes {
					if !runeMatches(line[col+i], queryRune, ignoreCase) {
						matched = false
						break
					}
				}
				if matched {
					result = append(result, []rune(replacement)...)
					count++
					col += len(queryRunes)
					continue
				}
			}
			result = append(result, line[col])
			col++
		}
		if count > 0 {
			b.Lines[row] = result
		}
	}
	return count
}

func runeMatches(left, right rune, ignoreCase bool) bool {
	if ignoreCase {
		return unicode.ToLower(left) == unicode.ToLower(right)
	}
	return left == right
}

// MoveWordRight moves the cursor to the end of the current word,
// or across whitespace/punctuation to the end of the next word.
func (b *Buffer) MoveWordRight(p Position) Position {
	if p.Row >= len(b.Lines) {
		return p
	}

	line := b.Lines[p.Row]

	// If at or past line end, wrap to the start of the next line
	if p.Col >= len(line) {
		if p.Row+1 < len(b.Lines) {
			return Position{Row: p.Row + 1, Col: 0}
		}
		return p // End of document
	}

	col := p.Col

	// Skip leading non-word characters (whitespace, punctuation)
	for col < len(line) && !isWordChar(line[col]) {
		col++
	}

	// Consume the word characters until the end of word
	for col < len(line) && isWordChar(line[col]) {
		col++
	}

	return Position{Row: p.Row, Col: col}
}

// MoveWordLeft moves the cursor to the start of the current word,
// or across whitespace/punctuation to the start of the previous word.
func (b *Buffer) MoveWordLeft(p Position) Position {
	if p.Row < 0 || p.Row >= len(b.Lines) {
		return p
	}

	line := b.Lines[p.Row]
	col := p.Col
	if col > len(line) {
		col = len(line)
	}

	// If at start of line, move to end of previous line
	if col == 0 {
		if p.Row == 0 {
			return Position{Row: 0, Col: 0}
		}
		prevLine := b.Lines[p.Row-1]
		return Position{Row: p.Row - 1, Col: len(prevLine)}
	}

	i := col
	// skip non-word characters (whitespace/punctuation)
	for i > 0 && !isWordChar(line[i-1]) {
		i--
	}
	// skip word characters to the start of the word
	for i > 0 && isWordChar(line[i-1]) {
		i--
	}
	return Position{Row: p.Row, Col: i}
}

// NextPos returns the Position after stepping one rune right, wrapping lines if needed.
func (b *Buffer) NextPos(p Position) Position {
	lineLen := len(b.Line(p.Row))
	if p.Col < lineLen {
		return Position{Row: p.Row, Col: p.Col + 1}
	}
	if p.Row < len(b.Lines)-1 {
		return Position{Row: p.Row + 1, Col: 0}
	}
	return p
}

// PrevPos returns the Position after stepping one rune left, wrapping lines if needed.
func (b *Buffer) PrevPos(p Position) Position {
	if p.Col > 0 {
		return Position{Row: p.Row, Col: p.Col - 1}
	}
	if p.Row > 0 {
		prevRow := p.Row - 1
		return Position{Row: prevRow, Col: len(b.Line(prevRow))}
	}
	return p
}

func (b *Buffer) LineStartNonSpace(p Position) Position {
	if p.Row < 0 || p.Row >= len(b.Lines) {
		return p
	}
	for i, char := range b.Lines[p.Row] {
		if !unicode.IsSpace(char) {
			return Position{Row: p.Row, Col: i}
		}
	}
	return Position{Row: p.Row, Col: 0}
}

func (b *Buffer) LineEnd(p Position) Position {
	if p.Row < 0 || p.Row >= len(b.Lines) {
		return p
	}
	return Position{Row: p.Row, Col: len(b.Lines[p.Row])}
}

// BufReader implements io.Reader over Buffer lines.
type BufReader struct {
	buf       *Buffer
	lineIdx   int
	colIdx    int
	encoded   [utf8.UTFMax]byte
	encLen    int
	encOffset int
}

func (b *Buffer) NewReader() *BufReader {
	return &BufReader{buf: b}
}

func (r *BufReader) Read(p []byte) (n int, err error) {
	if r.lineIdx >= len(r.buf.Lines) {
		return 0, io.EOF
	}

	for n < len(p) {
		// Flush remaining encoded bytes from current rune
		if r.encOffset < r.encLen {
			p[n] = r.encoded[r.encOffset]
			n++
			r.encOffset++
			continue
		}

		line := r.buf.Lines[r.lineIdx]

		// At end of line, output newline character
		if r.colIdx >= len(line) {
			p[n] = '\n'
			n++
			r.lineIdx++
			r.colIdx = 0
			r.encLen = 0
			r.encOffset = 0
			if r.lineIdx >= len(r.buf.Lines) {
				break
			}
			continue
		}

		// Encode next rune
		runeVal := line[r.colIdx]
		r.colIdx++
		r.encLen = utf8.EncodeRune(r.encoded[:], runeVal)
		r.encOffset = 0
	}

	return n, nil
}

// VisualPos converts a buffer position to a visual display position, accounting tab stop.
func (b *Buffer) VisualPos(p Position) Position {
	if p.Row < 0 || p.Row >= len(b.Lines) {
		return p
	}

	line := b.Lines[p.Row]
	if p.Col < 0 {
		p.Col = 0
	} else if p.Col > len(line) {
		p.Col = len(line)
	}

	col := 0
	for i := range p.Col {
		if line[i] == '\t' {
			col += 4 - (col % 4)
			continue
		}
		col++
	}
	return Position{Row: p.Row, Col: col}
}

// PosFromVisual converts a visual display Position back to a buffer Position.
func (b *Buffer) PosFromVisual(p Position) Position {
	if p.Row < 0 || p.Row >= len(b.Lines) {
		return p
	}

	if p.Col <= 0 {
		return Position{Row: p.Row, Col: 0}
	}

	col := 0
	line := b.Lines[p.Row]
	for i, r := range line {
		advance := 1
		if r == '\t' {
			advance = 4 - (col % 4)
		}
		if col+advance > p.Col {
			return Position{Row: p.Row, Col: i}
		}
		col += advance
	}
	return Position{Row: p.Row, Col: len(line)}
}

// Format runs go/format on the buffer's content if it is a Go source file.
// It returns true if the buffer was modified, and an error if formatting fails.
func (b *Buffer) Format() (bool, error) {
	// Only format Go files
	if filepath.Ext(b.Path) != ".go" {
		return false, nil
	}

	// 1. Join [][]rune lines into a single byte slice for go/format
	var buf bytes.Buffer
	for i, line := range b.Lines {
		buf.WriteString(string(line))
		if i < len(b.Lines)-1 {
			buf.WriteByte('\n')
		}
	}

	// 2. Format the source code using go/format
	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return false, err // Returns syntax/parser errors from the Go compiler
	}

	// 3. Convert formatted bytes back to [][]rune
	formattedStr := string(formatted)
	// Handle trailing newline splitting gracefully
	rawLines := strings.Split(strings.TrimSuffix(formattedStr, "\n"), "\n")

	newLines := make([][]rune, len(rawLines))
	for i, l := range rawLines {
		newLines[i] = []rune(l)
	}

	// 4. Update lines and preserve cursor sanity
	b.Lines = newLines
	b.Dirty = true

	// Clamp cursor to valid row/col bounds after formatting changes line lengths
	if b.Cursor.Row >= len(b.Lines) {
		b.Cursor.Row = max(0, len(b.Lines)-1)
	}
	if len(b.Lines) > 0 {
		b.Cursor.Col = min(b.Cursor.Col, len(b.Lines[b.Cursor.Row]))
	} else {
		b.Cursor.Col = 0
	}

	return true, nil
}

type Diagnostic struct {
	Row     int // start from 0
	Col     int // start from 0
	Message string
}

// CheckSemantics checks syntax and type error.
// filename is used for position resolution (e.g., "main.go").
// src can be a string, []byte, or io.Reader.
//
// It responds instantly (~5–20ms), as a tradeoff,
// external module imports are not resolved and are filtered out.
func CheckSemantics(filename string, src any) []Diagnostic {
	fset := token.NewFileSet()
	// 1. Parse AST with comments and full error reporting
	file, err := parser.ParseFile(fset, filename, src, parser.AllErrors)

	var diags []Diagnostic

	// 2. Collect syntax errors first
	if err != nil {
		if scannerErrs, ok := err.(scanner.ErrorList); ok {
			for _, e := range scannerErrs {
				diags = append(diags, Diagnostic{
					Row:     e.Pos.Line - 1,
					Col:     e.Pos.Column - 1,
					Message: e.Msg,
				})
			}
		}
		// If syntax is broken, return early (type checking invalid AST causes redundant noise)
		return diags
	}

	// 3. Configure type checker for semantic validation
	pkgName := file.Name.Name
	if pkgName == "" {
		pkgName = "main"
	}

	conf := types.Config{
		// only resolve standard library imports, ignores third-party or local module
		Importer: importer.Default(),

		// Custom error handler to collect semantic diagnostics
		Error: func(err error) {
			if typeErr, ok := err.(types.Error); ok {
				// Suppress import resolution errors for external modules during real-time typing
				if strings.Contains(typeErr.Msg, "could not import") ||
					strings.Contains(typeErr.Msg, "cannot find package") {
					return
				}

				pos := fset.Position(typeErr.Pos)
				diags = append(diags, Diagnostic{
					Row:     pos.Line - 1,
					Col:     pos.Column - 1,
					Message: typeErr.Msg,
				})
			}
		},
	}

	// Optional: Pass an empty Info struct to trigger full type resolution
	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Implicits:  make(map[ast.Node]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}

	// 4. Run type checker on the AST
	_, _ = conf.Check(pkgName, fset, []*ast.File{file}, info)
	// Optional: Multi-file package type checking
	/*
		If your file references types or functions declared in another file in the same package,
		go/types will report them as undefined if only a single file AST is passed.
		To handle multi-file packages in your editor,
		simply pass all parsed AST files in the same directory to conf.Check:
		_, _ = conf.Check(pkgName, fset, []*ast.File{currentFileAST, otherFileAST1, otherFileAST2}, info)
	*/

	return diags
}

// DefinitionResult holds the target location for a definition jump.
type DefinitionResult struct {
	Found  bool
	Line   int // 1-based
	Column int // 1-based
}

// FindDefinitionLoc returns the target definition line/col for the identifier
// at the given cursor position (1-based line, 1-based col).
func FindDefinitionLoc(src any, cursorLine, cursorCol int) DefinitionResult {
	fset := token.NewFileSet()
	// src can be string, []byte, or io.Reader
	file, err := parser.ParseFile(fset, "buffer.go", src, parser.SkipObjectResolution)
	if err != nil && file == nil {
		return DefinitionResult{}
	}

	targetPos := positionToPos(fset, file, cursorLine, cursorCol)
	if !targetPos.IsValid() {
		return DefinitionResult{}
	}

	// 1. Find the *ast.Ident under the cursor
	var targetIdent *ast.Ident
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return true
		}
		if n.Pos() <= targetPos && targetPos <= n.End() {
			if ident, ok := n.(*ast.Ident); ok {
				targetIdent = ident
			}
			return true
		}
		return false
	})

	if targetIdent == nil {
		return DefinitionResult{}
	}

	// 2. Find declaration by inspecting the AST scopes explicitly
	declPos := findDeclarationPos(file, targetIdent)
	if !declPos.IsValid() {
		return DefinitionResult{}
	}

	pos := fset.Position(declPos)
	return DefinitionResult{
		Found:  true,
		Line:   pos.Line,
		Column: pos.Column,
	}
}

// findDeclarationPos replaces the deprecated Ident.Obj lookup by walking
// local function scopes and top-level declarations in the file.
func findDeclarationPos(file *ast.File, target *ast.Ident) token.Pos {
	name := target.Name
	var match token.Pos

	// Step A: Search for local variables / parameters inside the enclosing function
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}

		// Check if target is inside this function body
		if fn.Pos() <= target.Pos() && target.Pos() <= fn.End() {
			// Check function parameters and return values
			if fn.Type != nil && fn.Type.Params != nil {
				for _, param := range fn.Type.Params.List {
					for _, id := range param.Names {
						if id.Name == name {
							match = id.Pos()
							return false
						}
					}
				}
			}

			// Check local variable assignments inside function body
			ast.Inspect(fn.Body, func(bodyNode ast.Node) bool {
				switch stmt := bodyNode.(type) {
				case *ast.AssignStmt: // e.g. x := 10 or x, y := 1, 2
					for _, lh := range stmt.Lhs {
						if id, ok := lh.(*ast.Ident); ok && id.Name == name {
							if id.Pos() <= target.Pos() { // Defined before or at target
								match = id.Pos()
								return false
							}
						}
					}
				case *ast.ValueSpec: // e.g. var x int
					for _, id := range stmt.Names {
						if id.Name == name {
							match = id.Pos()
							return false
						}
					}
				}
				return match == token.NoPos
			})

			return false
		}
		return true
	})

	if match.IsValid() {
		return match
	}

	// Step B: Search top-level file declarations (funcs, structs, types, consts, vars)
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name.Name == name {
				return d.Name.Pos()
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name.Name == name {
						return s.Name.Pos()
					}
					// If searching for a struct field name
					if st, ok := s.Type.(*ast.StructType); ok {
						for _, field := range st.Fields.List {
							for _, id := range field.Names {
								if id.Name == name {
									return id.Pos()
								}
							}
						}
					}
				case *ast.ValueSpec:
					for _, id := range s.Names {
						if id.Name == name {
							return id.Pos()
						}
					}
				}
			}
		}
	}

	return token.NoPos
}

func positionToPos(fset *token.FileSet, file *ast.File, line, col int) token.Pos {
	tf := fset.File(file.Pos())
	if tf == nil || line < 1 || line > tf.LineCount() {
		return token.NoPos
	}
	return tf.LineStart(line) + token.Pos(col-1)
}

type SymbolLocation struct {
	Name     string
	Receiver string
	Kind     string // "func", "method", "type", "struct", "var", "const"
	Line     int    // 1-based
	Column   int    // 1-based
}

// combines receiver and symbol name
func (s SymbolLocation) String() string {
	if s.Receiver == "" {
		return s.Name
	}
	return fmt.Sprintf("(%s).%s", s.Receiver, s.Name)
}

// FindSymbolLoc finds the location of a top-level symbol matching exactName.
// src can be string, []byte, or io.Reader.
func FindSymbolLoc(src any, exactName string) (SymbolLocation, bool) { // TODO
	symbols := ExtractAllSymbols(src)
	for _, sym := range symbols {
		if sym.Name == exactName {
			return sym, true
		}
	}
	return SymbolLocation{}, false
}

// ExtractAllSymbols collects all top-level symbols in the file.
// Useful for fuzzy finding or symbol pickers.
func ExtractAllSymbols(src any) []SymbolLocation {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "buffer.go", src, 0)
	if err != nil && file == nil {
		return nil
	}

	var results []SymbolLocation

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

			results = append(results, SymbolLocation{
				Name:     d.Name.Name,
				Receiver: recv,
				Kind:     kind,
				Line:     pos.Line,
				Column:   pos.Column,
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
					results = append(results, SymbolLocation{
						Name:   s.Name.Name,
						Kind:   kind,
						Line:   pos.Line,
						Column: pos.Column,
					})

				case *ast.ValueSpec:
					kind := "var"
					if d.Tok == token.CONST {
						kind = "const"
					}
					for _, name := range s.Names {
						pos := fset.Position(name.Pos())
						results = append(results, SymbolLocation{
							Name:   name.Name,
							Kind:   kind,
							Line:   pos.Line,
							Column: pos.Column,
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
	var buf *Buffer

	if path != "" {
		path = filepath.Clean(path)
	}
	// switch to existing buffer
	for i, buf := range e.buffers {
		if buf.Path == path {
			e.active = i
			e.Buffer = buf
			e.diags = nil
			if isGoFile(e.Path) {
				e.debounceDiagnose()
			}
			return nil
		}
	}

	var err error
	buf, err = loadBuffer(path)
	if err != nil {
		return err
	}

	e.buffers = append(e.buffers, buf)
	e.active = len(e.buffers) - 1
	e.Buffer = buf
	e.diags = nil
	if isGoFile(e.Path) {
		e.debounceDiagnose()
	}
	return nil
}

// CloseBuffer closes the active buffer.
func (e *Editor) CloseBuffer() {
	if len(e.buffers) <= 1 {
		// Either exit or leave an empty scratch buffer
		return
	}
	// Remove from slice and adjust active index
	e.buffers = append(e.buffers[:e.active], e.buffers[e.active+1:]...)
	if e.active >= len(e.buffers) {
		e.active = len(e.buffers) - 1
	}
	e.Buffer = e.buffers[e.active]
	e.diags = nil
	e.message = ""
	if isGoFile(e.Path) {
		e.debounceDiagnose()
	}
}

func (e *Editor) NextBuffer() {
	if len(e.buffers) > 1 {
		e.active = (e.active + 1) % len(e.buffers)
		e.Buffer = e.buffers[e.active]
		e.diags = nil
		if isGoFile(e.Path) {
			e.debounceDiagnose()
		}
	}
}

func (e *Editor) PrevBuffer() {
	if len(e.buffers) > 1 {
		e.active = (e.active - 1 + len(e.buffers)) % len(e.buffers)
		e.Buffer = e.buffers[e.active]
		e.diags = nil
		if isGoFile(e.Path) {
			e.debounceDiagnose()
		}
	}
}

// loadBuffer opens a file and prepares a Buffer struct.
// If the file does not exist, it creates a new empty buffer associated with that path.
func loadBuffer(path string) (*Buffer, error) {
	if path == "" {
		return &Buffer{
			Path:  "",
			Lines: [][]rune{[]rune("")},
		}, nil
	}

	cleanPath := filepath.Clean(path)
	file, err := os.Open(cleanPath)
	if os.IsNotExist(err) {
		// New/unsaved file: initialize with one empty line
		return &Buffer{
			Path:  cleanPath,
			Lines: [][]rune{[]rune("")},
		}, nil
	} else if err != nil {
		return nil, err
	}
	defer file.Close()

	lines, err := readLines(file)
	if err != nil {
		return nil, err
	}

	// Guarantee at least one line exists in memory
	if len(lines) == 0 {
		lines = [][]rune{}
	}

	return &Buffer{
		Path:  cleanPath,
		Lines: lines,
	}, nil
}

func readLines(r io.Reader) ([][]rune, error) {
	var lines [][]rune
	reader := bufio.NewReader(r)

	for {
		line, err := reader.ReadString('\n')

		// Strip '\n' and carriage return '\r' (Windows normalization)
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")

		if line != "" || err == nil {
			lines = append(lines, []rune(line))
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

func (b *Buffer) Save() error {
	file, err := os.OpenFile(b.Path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	for i, line := range b.Lines {
		if _, err := writer.WriteString(string(line)); err != nil {
			return err
		}
		if i < len(b.Lines)-1 {
			if _, err := writer.WriteString("\n"); err != nil {
				return err
			}
		}
	}

	if err := writer.Flush(); err != nil {
		return err
	}

	b.Dirty = false
	return nil
}

// Jump represents a recorded location in a buffer.
type Jump struct {
	Path string   // File path (used to match across buffer switches/reopens)
	Pos  Position // Cursor position (Row, Col)
}

// JumpList manages navigation history for long jumps (Go To Def, Find Symbol, etc.).
type JumpList struct {
	items []Jump
	index int // Points to current position in history
}

const maxJumps = 100

// Push adds a new jump location to the stack.
// If the new position is identical or right next to the current jump, it is ignored.
func (j *JumpList) Push(path string, pos Position) {
	if len(j.items) > 0 && j.index >= 0 && j.index < len(j.items) {
		curr := j.items[j.index]
		// Avoid pushing duplicate positions in the same file
		if curr.Path == path && curr.Pos == pos {
			return
		}
	}

	// Truncate forward history if we jump from somewhere in the middle
	if j.index < len(j.items)-1 {
		j.items = j.items[:j.index+1]
	}

	j.items = append(j.items, Jump{Path: path, Pos: pos})
	if len(j.items) > maxJumps {
		j.items = j.items[1:]
	}
	j.index = len(j.items) - 1
}

// Back steps back in history and returns the target jump position.
func (j *JumpList) Back(currentPath string, currentPos Position) (Jump, bool) {
	if len(j.items) == 0 {
		return Jump{}, false
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
func (j *JumpList) Forward() (Jump, bool) {
	if len(j.items) == 0 || j.index >= len(j.items)-1 {
		return Jump{}, false
	}

	j.index++
	return j.items[j.index], true
}

// recordJump saves the current editor position into the jump history.
func (e *Editor) recordJump() {
	if e.Buffer == nil {
		return
	}
	e.jumps.Push(e.Path, e.Cursor)
}

// jumpTo restores a recorded location, switching buffers if necessary.
func (e *Editor) jumpTo(target Jump) {
	// 1. Switch buffer if the target is in a different file
	if target.Path != "" && target.Path != e.Path {
		err := e.OpenFile(target.Path)
		if err != nil {
			log.Print(err)
			return
		}
	}

	// 2. Set cursor position
	if target.Pos.Row >= 0 && target.Pos.Row < len(e.Lines) {
		e.Cursor = e.ClampPos(target.Pos)
	}
}

// JumpBack moves to the previous position in jump history.
func (e *Editor) JumpBack() {
	if e.Buffer == nil {
		return
	}
	if target, ok := e.jumps.Back(e.Path, e.Cursor); ok {
		e.jumpTo(target)
	}
}

// JumpForward moves to the next position in jump history.
func (e *Editor) JumpForward() {
	if e.Buffer == nil {
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
	Active   bool
	Input    TextInput
	Items    []PaletteItem
	Selected int // Highlighted item index
	MaxRows  int // UI render cap (e.g., 10 items)
}

// Open initializes the palette with a starting prefix ("@", ":", "/", or "").
func (p *Palette) Open(ctx *kero.Context, e *Editor, prefix string) {
	p.Active = true
	p.Input.Reset()
	p.Input.SetText(prefix)
	p.Input.Placeholder = "search file (@symbol, /command or :line)"
	p.Selected = 0
	p.MaxRows = 10
	p.Refresh(e)
}

func (p *Palette) Close() {
	p.Active = false
	p.Input.Reset()
	p.Items = nil
	p.Selected = 0
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
	if p.Selected >= len(p.Items) {
		p.Selected = max(0, len(p.Items)-1)
	}
}

// Symbol Provider (@)
func (p *Palette) symbolItems(e *Editor, query string) []PaletteItem {
	if e.Buffer == nil {
		return nil
	}

	lowerQuery := strings.ToLower(query)
	queries := strings.Split(lowerQuery, ".")
	if len(queries) == 1 {
		queries = strings.Split(lowerQuery, " ")
	}

	symbols := ExtractAllSymbols(e.Buffer.NewReader())
	var items []PaletteItem

	for _, sym := range symbols {

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

		symPos := Position{Row: sym.Line - 1, Col: sym.Column - 1}
		items = append(items, PaletteItem{
			Label: sym.String(),
			// Detail: fmt.Sprintf("Line %d", sym.Line),
			// Kind: sym.Kind,
			Action: func(ed *Editor) {
				ed.recordJump()
				ed.Cursor = symPos
				e.showCursorCenter()
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
		{"format", "", func(e *Editor) { e.Buffer.Format() }},
		{"goto definition", "ctrl+g", func(e *Editor) {
			e.recordJump()
			e.GotoDefinition()
			e.showCursorCenter()
		}},
		{"goto diagnostic", "", func(e *Editor) {
			if d := e.nextDiagnostic(); d.Message != "" {
				e.Cursor = e.Buffer.ClampPos(Position{Row: d.Row, Col: d.Col})
				e.showCursorCenter()
			}
		}},
		{"jump back", "ctrl+-", func(e *Editor) {
			e.JumpBack()
			e.showCursorCenter()
		}},
		{"jump forward", "ctrl+shift+-", func(e *Editor) {
			e.JumpForward()
			e.showCursorCenter()
		}},
		{"next buffer", "ctrl+]", func(e *Editor) {
			e.NextBuffer()
		}},
		{"prev buffer", "ctrl+[", func(e *Editor) {
			e.PrevBuffer()
		}},
	}

	var items []PaletteItem
	for _, c := range commands {
		if query != "" && !strings.Contains(c.cmd, query) {
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
	if query == "" || e.Buffer == nil {
		return nil
	}

	lineNum, err := strconv.Atoi(query)
	if err != nil || lineNum <= 0 || lineNum > len(e.Lines) {
		return nil
	}

	targetRow := lineNum - 1
	return []PaletteItem{
		{
			Label: fmt.Sprintf("Go to line %d", lineNum),
			Action: func(ed *Editor) {
				ed.recordJump()
				ed.Cursor = Position{Row: targetRow, Col: 0}
				ed.showCursorCenter()
			},
		},
	}
}

func (e *Editor) updatePalette(ev kero.KeyEvent) {
	switch ev.Key {
	case kero.KeyEsc:
		e.palette.Close()
	case kero.KeyDown:
		if len(e.palette.Items) > 0 {
			e.palette.Selected = (e.palette.Selected + 1) % len(e.palette.Items)
		}
	case kero.KeyUp:
		if len(e.palette.Items) > 0 {
			e.palette.Selected = (e.palette.Selected - 1 + len(e.palette.Items)) % len(e.palette.Items)
		}
	case kero.KeyEnter:
		if len(e.palette.Items) > 0 && e.palette.Selected < len(e.palette.Items) {
			action := e.palette.Items[e.palette.Selected].Action
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
	for i, buf := range e.buffers {
		if buf.Path == "" {
			continue
		}
		openPaths[buf.Path] = true

		if query != "" && !strings.Contains(strings.ToLower(buf.Path), lowerQuery) {
			continue
		}

		bufIdx := i
		bufPath := buf.Path
		items = append(items, PaletteItem{
			Label:  filepath.Base(bufPath),
			Detail: "active",
			Action: func(ed *Editor) {
				ed.recordJump()
				ed.active = bufIdx
				ed.Buffer = ed.buffers[bufIdx]
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
		if openPaths[path] {
			return nil
		}

		cleanPath := filepath.Clean(path)
		if query != "" && !strings.Contains(strings.ToLower(cleanPath), lowerQuery) {
			return nil
		}

		filePath := cleanPath
		items = append(items, PaletteItem{
			Label: filepath.Base(filePath),
			Action: func(ed *Editor) {
				ed.recordJump()
				ed.OpenFile(filePath)
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
func (e *Editor) drawPalette(f *kero.Frame, y, width int) {
	if !e.palette.Active {
		return
	}

	normal := kero.NewStyle()
	p := &e.palette

	// 1. Render input line at row y
	f.Fill(kero.Rect{X: 0, Y: y, W: width, H: 1}, ' ', normal.Reverse())
	p.Input.Draw(f, kero.Rect{X: 1, Y: y, W: width, H: 1}, normal.Reverse())

	// 2. Calculate visible window bounds
	total := len(p.Items)
	if total == 0 {
		return
	}

	visibleRows := min(total, p.MaxRows)

	// Calculate scrolling offset to keep selected item inside dropdown viewport
	offset := 0
	if p.Selected >= visibleRows {
		offset = p.Selected - visibleRows + 1
	}

	// 3. Fill background for dropdown overlay rendered directly above row y
	rect := kero.Rect{X: 0, Y: y - visibleRows, W: width, H: visibleRows}
	f.Fill(rect, ' ', normal.Reverse())

	// 4. Render item rows
	for i := range visibleRows {
		idx := i + offset
		if idx >= total {
			break
		}

		item := p.Items[idx]
		lineY := rect.Y + i

		// Selection cursor indicator
		prefix := "  "
		style := normal.Reverse()
		if idx == p.Selected {
			prefix = " >"
			style = normal.Reverse().Bold()
		}

		// Left side text: Prefix + Label
		leftText := fmt.Sprintf("%s %s", prefix, item.Label)
		leftRunes := []rune(leftText)

		if len(leftRunes) > width {
			leftText = string(leftRunes[:width])
		}
		f.Write(rect.X, lineY, leftText, style)

		// Right side text: Detail (e.g. line number or keybinding hint)
		if item.Detail != "" {
			detailRunes := []rune(item.Detail)
			detailX := width - len(detailRunes) - 1

			// Only render detail if it doesn't overlap left text
			if detailX > len(leftRunes)+2 {
				f.Write(detailX, lineY, item.Detail, style)
			}
		}
	}
}
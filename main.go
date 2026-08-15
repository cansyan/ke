package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"log"
	"os"
	"path/filepath"
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
	f, err := os.OpenFile("/tmp/ke.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	log.SetOutput(f)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	app := &Editor{}
	if len(os.Args) > 1 {
		app.path, app.cursor.Row, app.cursor.Col = parsePathArg(os.Args[1])
	}

	// set FPS for refreshing diagnostic
	p := kero.New(app, kero.WithAltScreen(true), kero.WithKitty(true), kero.WithFPS(3))
	if err := p.Run(); err != nil {
		panic(err)
	}
}

// Editor implements kero.App interface
type Editor struct {
	path string

	buf    *Buffer
	cursor Position

	rowOffset int // Scroll offsets for the view viewport
	colOffset int // visual display column offset (0-based horizontal scroll position)

	dirty   bool
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

	// cmd mode opens a input line at the message area
	cmdMode  bool
	cmdInput TextInput

	// selection state
	selecting bool
	selAnchor Position // selection at [e.selAnchor, e.pos)

	clipboard  string
	clipIsLine bool

	lastKey kero.KeyEvent

	diags       []Diagnostic
	diagTimer   *time.Timer
	diagChan    chan diagResult
	diagVersion atomic.Uint64

	symbolPicker SymbolPicker
	symbolInput  TextInput

	// reports whether the key comes from a paste action,
	// to distinguish the manual KeyEnter or a pasted \n
	pasting bool
}

type diagResult struct {
	version     uint64
	diagnostics []Diagnostic
}

func (e *Editor) Init(ctx *kero.Context) error {
	if e.path == "" {
		e.buf = NewBuffer("")
		e.cursor.Row, e.cursor.Col = 0, 0
		e.message = "new buffer"
		return nil
	}

	data, err := os.ReadFile(e.path)
	if err != nil {
		if os.IsNotExist(err) {
			e.buf = NewBuffer("")
			e.cursor.Row, e.cursor.Col = 0, 0
			e.message = "new file"
			return nil
		}
		return err
	}

	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	e.buf = NewBuffer(text)
	e.cursor = e.buf.ClampPos(e.cursor)
	e.ensureCursorVisible(ctx)
	if isGoFile(e.path) {
		e.debounceCheckSemantic()
	}
	return nil
}

func (e *Editor) Update(ctx *kero.Context, ev kero.Event) error {
	switch ev.(type) {
	case kero.PasteStartEvent:
		e.pasting = true
	case kero.PasteEndEvent:
		e.pasting = false
	}

	// refresh diagnostic as soon as possible, no matter what event is
	e.applyDiagnosticResults()
	key, ok := ev.(kero.KeyEvent)
	if !ok {
		return nil
	}
	defer func() {
		e.lastKey = key
		e.ensureCursorVisible(ctx)
	}()

	// can quit at anytime, first priority
	if key.String() == "ctrl+q" {
		quitAgain := e.lastKey.String() == "ctrl+q"
		if e.dirty && !quitAgain {
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
		return e.updateFind(key)
	}
	if e.cmdMode {
		return e.updateCmdPalette(key)
	}
	if e.symbolPicker.Active {
		e.updateSymbolPicker(key)
		return nil
	}

	switch key.Key {
	case kero.KeyRune:
		switch key.String() {
		case "ctrl+r":
			e.OpenSymbolPicker()
		case "ctrl+g":
			e.GotoDefinition()
			return nil
		case "ctrl+p":
			e.startCmdPalette("")
			return nil
		case "ctrl+]":
			if v := e.nextDiagnostic(); v.Message != "" {
				e.cursor = e.buf.ClampPos(Position{Row: v.Row, Col: v.Col})
			}
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
			if start, end := e.buf.WordBounds(e.cursor); start != end {
				e.selecting = true
				e.selAnchor = start
				e.cursor = end
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
			p := e.buf.LineStartNonSpace(e.cursor)
			if e.cursor == p {
				e.cursor.Col = 0
				return nil
			}
			e.cursor = p
		case "ctrl+e":
			e.cursor = e.buf.LineEnd(e.cursor)
		}
		if key.Mod != 0 {
			break
		}
		e.insertRune(key.Rune)
	case kero.KeyEnter:
		if e.hasSelect() {
			e.deleteSelect()
		}
		// insert raw newline
		if e.pasting {
			e.cursor = e.buf.Insert(e.cursor, "\n")
			return nil
		}

		// insert newline with auto-indent
		var n int
		line := e.buf.Line(e.cursor.Row)
		for _, b := range line {
			if !unicode.IsSpace(b) {
				break
			}
			n++
		}
		indent := line[:n]
		if key.Mod&kero.ModCtrl != 0 {
			p := Position{Row: e.cursor.Row, Col: len(line)}
			e.cursor = e.buf.Insert(p, "\n"+string(indent))
		} else {
			e.cursor = e.buf.Insert(e.cursor, "\n"+string(indent))
		}
		e.markDirty()
	case kero.KeyTab:
		if key.Mod&kero.ModShift != 0 {
			e.unindentSelectOrLine()
			break
		}
		if e.hasSelect() {
			start, end := orderPos(e.selAnchor, e.cursor)
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
			e.cursor = e.buf.DeleteRange(Position{Row: e.cursor.Row, Col: 0}, e.cursor)
		case "alt+backspace":
			// delete word backwards
			prev := e.buf.MoveWordLeft(e.cursor)
			e.cursor = e.buf.DeleteRange(prev, e.cursor)
		case "cmd+shift+backspace":
			// delete whole line
			e.cursor = e.buf.DeleteRange(Position{Row: e.cursor.Row}, Position{Row: e.cursor.Row + 1})
		default:
			e.cursor = e.buf.DeleteRange(e.buf.PrevPos(e.cursor), e.cursor)
		}
		e.markDirty()
	case kero.KeyDelete:
		e.cursor = e.buf.DeleteRange(e.cursor, e.buf.NextPos(e.cursor))
		e.markDirty()
	case kero.KeyLeft:
		switch key.String() {
		case "alt+left":
			e.cursor = e.buf.MoveWordLeft(e.cursor)
		case "cmd+left":
			p := e.buf.LineStartNonSpace(e.cursor)
			if e.cursor == p {
				e.cursor.Col = 0
			} else {
				e.cursor = p
			}
		case "shift+left":
			// start selection
			if !e.selecting {
				e.selecting = true
				e.selAnchor = e.cursor
			}
			e.moveLeft()
		default:
			e.moveLeft()
		}
	case kero.KeyRight:
		switch key.String() {
		case "alt+right":
			e.cursor = e.buf.MoveWordRight(e.cursor)
		case "cmd+right":
			e.cursor = e.buf.LineEnd(e.cursor)
		case "shift+right":
			// start selection
			if !e.selecting {
				e.selecting = true
				e.selAnchor = e.cursor
			}
			e.moveRight()
		default:
			e.moveRight()
		}
	case kero.KeyUp:
		switch key.String() {
		case "cmd+up":
			// file start
			e.cursor.Row = 0
			e.cursor.Col = 0
		case "shift+up":
			// start selection
			if !e.selecting {
				e.selecting = true
				e.selAnchor = e.cursor
			}
			e.moveUp()
		default:
			e.moveUp()
		}
	case kero.KeyDown:
		switch key.String() {
		case "cmd+down":
			// file end
			e.cursor.Row = e.buf.LenLines() - 1
			e.cursor = e.buf.LineEnd(e.cursor)
		case "shift+down":
			// start selection
			if !e.selecting {
				e.selecting = true
				e.selAnchor = e.cursor
			}
			e.moveDown()
		default:
			e.moveDown()
		}
	case kero.KeyHome:
		p := e.buf.LineStartNonSpace(e.cursor)
		if e.cursor == p {
			e.cursor.Col = 0
			return nil
		}
		e.cursor = p
	case kero.KeyEnd:
		e.cursor = e.buf.LineEnd(e.cursor)
	case kero.KeyPgUp:
		e.cursor.Row -= editorHeight(ctx)
		e.cursor = e.buf.ClampPos(e.cursor)
	case kero.KeyPgDown:
		e.cursor.Row += editorHeight(ctx)
		e.cursor = e.buf.ClampPos(e.cursor)
	case kero.KeyEsc:
		e.clearSelect()
	}

	return nil
}

func (e *Editor) View(ctx *kero.Context, f *kero.Frame) {
	statusStyle := kero.NewStyle().Reverse()
	lineNoStyle := kero.NewStyle().Foreground(kero.ColorBlue).Dim()
	textStyle := kero.NewStyle()
	cursorStyle := textStyle.Reverse()
	selectStyle := kero.NewStyle().Foreground(kero.ColorBlack).Background(kero.ColorYellow)
	messageStyle := kero.NewStyle()

	name := "[No Name]"
	if e.path != "" {
		name = filepath.Base(e.path)
	}
	modified := ""
	if e.dirty {
		modified = " *"
	}

	editorH := editorHeight(ctx)
	lineNoW := lineNumberWidth(e.buf.LenLines())
	gutterW := lineNoW + 2 // marker, line number, and separator
	for y := range editorH {
		lineIndex := e.rowOffset + y
		if lineIndex >= e.buf.LenLines() {
			f.Write(0, y, "~", lineNoStyle)
			continue
		}

		f.Write(1, y, fmt.Sprintf("%*d ", lineNoW, lineIndex+1), lineNoStyle)

		srcLine := e.buf.Line(lineIndex)
		fullPadded := padTab(srcLine, 4)
		limit := max(0, ctx.Width-gutterW)

		// visual part of the padded line
		var visPadded string
		if e.colOffset < len(fullPadded) {
			visPadded = fullPadded[e.colOffset:]
		}
		if len(visPadded) > limit {
			visPadded = visPadded[:limit]
		}

		// draw the line
		style := textStyle
		if lineIndex == e.cursor.Row {
			style = style.Underline()
		}
		f.Write(gutterW, y, visPadded, style)

		if v, ok := e.diagnosticForLine(lineIndex); ok {
			red := kero.NewStyle().Foreground(kero.ColorRed)
			f.Write(0, y, "x", red)
			f.Write(gutterW+len(visPadded)+2, y, v.Message, red)
		}

		// highlight selection if any
		if e.selecting {
			start, end := orderPos(e.selAnchor, e.cursor)
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

				startVisCol := runeIndexToDisplayColumn(srcLine, selStartCol)
				endVisCol := runeIndexToDisplayColumn(srcLine, selEndCol)

				startDisplay := max(0, min(startVisCol-e.colOffset, len(visPadded)))
				endDisplay := max(0, min(endVisCol-e.colOffset, len(visPadded)))

				for x := startDisplay; x < endDisplay; x++ {
					ch := rune(visPadded[x])
					f.Set(gutterW+x, y, ch, selectStyle)
				}
			}
		}

		if e.finding && e.findMatch && lineIndex == e.findMatchStart.Row && lineIndex == e.findMatchEnd.Row {
			startVisCol := runeIndexToDisplayColumn(srcLine, e.findMatchStart.Col)
			endVisCol := runeIndexToDisplayColumn(srcLine, e.findMatchEnd.Col)

			startDisplay := max(0, min(startVisCol-e.colOffset, len(visPadded)))
			endDisplay := max(0, min(endVisCol-e.colOffset, len(visPadded)))

			for x := startDisplay; x < endDisplay; x++ {
				ch := rune(visPadded[x])
				f.Set(gutterW+x, y, ch, selectStyle)
			}
		}
	}

	line := e.buf.Line(e.cursor.Row)
	cursorDisplayCol := runeIndexToDisplayColumn(line, e.cursor.Col)
	cursorX := gutterW + cursorDisplayCol - e.colOffset
	cursorY := 0 + e.cursor.Row - e.rowOffset
	if cursorY >= 0 && cursorY < editorH && cursorX >= gutterW && cursorX < ctx.Width {
		fullLinePadded := padTab(line, 4)
		ch := ' '
		if cursorDisplayCol < len(fullLinePadded) {
			ch = rune(fullLinePadded[cursorDisplayCol])
		}
		f.Set(cursorX, cursorY, ch, cursorStyle)
	}

	statusY := ctx.Height - 1
	if statusY >= 0 {
		status := fmt.Sprintf(" %s | %d lines | Ln %d, Col %d",
			name+modified, e.buf.LenLines(), e.cursor.Row+1, cursorDisplayCol+1)
		if e.selecting {
			status = status + " | Selecting"
		}
		f.Fill(kero.Rect{X: 0, Y: statusY, W: ctx.Width, H: 1}, ' ', statusStyle)
		f.Write(0, statusY, trimToWidth(status, ctx.Width), statusStyle)
		if len(e.diags) > 0 {
			warn := fmt.Sprintf("ctrl+] goto diagnostic")
			statusWidth := len([]rune(status))
			f.Write(statusWidth+1, statusY, "| ", statusStyle)
			keyWidth := len([]rune(e.lastKey.String()))
			remainWidth := ctx.Width - statusWidth - keyWidth
			if remainWidth > 0 {
				f.Write(statusWidth+3, statusY, trimToWidth(warn, remainWidth), statusStyle.Background(kero.ColorRed))
			}
		}
		if e.lastKey.Key != kero.KeyUnknown {
			ks := e.lastKey.String()
			f.Write(ctx.Width-len(ks), statusY, ks, statusStyle)
		}
	}

	messageY := ctx.Height - 2
	if messageY >= 0 {
		if e.cmdMode {
			e.drawCmdPalette(f, messageY, ctx.Width)
			return
		}
		if e.saveAs {
			e.drawSaveAs(f, messageY, ctx.Width)
			return
		}
		if e.finding {
			e.drawFind(f, messageY, ctx.Width)
			return
		}
		if e.symbolPicker.Active {
			e.drawSymbolPicker(f, messageY, ctx.Width)
			return
		}
		if e.message == "" {
			e.message = "^S save | ^Q quit | ^F find | ^G definition | ^R symbols | ^] diagnostic"
		}
		if strings.HasPrefix(e.message, "error:") || strings.HasPrefix(e.message, "warn:") {
			messageStyle = messageStyle.Foreground(kero.ColorRed)
		}
		f.Write(0, messageY, trimToWidth(" "+e.message, ctx.Width), messageStyle)
	}
}

func (e *Editor) selectLine() {
	if !e.selecting {
		e.selecting = true
		e.selAnchor = Position{Row: e.cursor.Row, Col: 0}
	}
	if e.cursor.Row < e.buf.LenLines()-1 {
		e.cursor.Row++
		e.cursor.Col = 0
	} else {
		e.cursor = e.buf.LineEnd(e.cursor)
	}
}

func isWordChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

func (e *Editor) hasSelect() bool {
	return e.selecting && e.selAnchor != e.cursor
}

func (e *Editor) clearSelect() {
	e.selecting = false
}

func (e *Editor) copy() {
	if e.hasSelect() {
		e.clipboard = e.buf.GetRange(e.selAnchor, e.cursor)
		e.clipIsLine = false
		return
	}
	// copy entire current line, remember it's a line copy
	e.clipboard = string(e.buf.Line(e.cursor.Row))
	e.clipIsLine = true
}

func (e *Editor) indentSelect() {
	if !e.hasSelect() {
		return
	}

	start, end := orderPos(e.selAnchor, e.cursor)
	if start.Row == end.Row {
		return
	}
	lastRow := end.Row
	if end.Col == 0 && end.Row > start.Row {
		lastRow = end.Row - 1
	}
	for r := start.Row; r <= lastRow; r++ {
		e.buf.Insert(Position{Row: r, Col: 0}, "\t")
	}
	if start.Row <= e.selAnchor.Row && e.selAnchor.Row <= lastRow {
		e.selAnchor.Col++
	}
	if start.Row <= e.cursor.Row && e.cursor.Row <= lastRow {
		e.cursor.Col++
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
		newLine, removed := unindentLine(string(e.buf.Line(e.cursor.Row)))
		if removed > 0 {
			e.buf.SetLine(e.cursor.Row, []rune(newLine))
			e.cursor.Col = max(0, e.cursor.Col-removed)
			e.markDirty()
		}
		return
	}

	start, end := orderPos(e.selAnchor, e.cursor)
	lastRow := end.Row
	if end.Col == 0 && end.Row > start.Row {
		lastRow = end.Row - 1
	}
	var startRemoved, endRemoved int
	for r := start.Row; r <= lastRow; r++ {
		newLine, removed := unindentLine(string(e.buf.Line(r)))
		if r == e.selAnchor.Row {
			startRemoved = removed
		}
		if r == e.cursor.Row {
			endRemoved = removed
		}
		e.buf.SetLine(r, []rune(newLine))
	}

	if e.selAnchor.Row <= lastRow {
		e.selAnchor.Col = max(0, e.selAnchor.Col-startRemoved)
	}
	if e.cursor.Row <= lastRow {
		e.cursor.Col = max(0, e.cursor.Col-endRemoved)
	}
	e.markDirty()
}

func (e *Editor) deleteSelect() {
	if !e.hasSelect() {
		return
	}
	start, end := orderPos(e.selAnchor, e.cursor)
	e.cursor = e.buf.DeleteRange(start, end)
	e.selecting = false
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
	p1 := Position{Row: e.cursor.Row}
	e.clipboard = e.buf.GetRange(p1, e.buf.LineEnd(e.cursor))
	e.clipIsLine = true
	e.cursor = e.buf.DeleteRange(p1, Position{Row: e.cursor.Row + 1})
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
		e.buf.Insert(Position{Row: e.cursor.Row, Col: 0}, e.clipboard+"\n")
		e.cursor.Row++
		e.markDirty()
		return
	}

	e.cursor = e.buf.Insert(e.cursor, e.clipboard)
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
	if e.path == "" {
		e.startSaveAs()
		return nil
	}

	data := append(e.buf.Bytes(), '\n')
	if err := os.WriteFile(e.path, data, 0644); err != nil {
		e.message = "error: " + err.Error()
		return nil
	}

	e.dirty = false
	e.message = fmt.Sprintf("saved %s", filepath.Base(e.path))
	return nil
}

func (e *Editor) startSaveAs() {
	e.saveAs = true
	e.saveInput.Value = e.path
	e.saveInput.Cursor = len([]rune(e.saveInput.Value))
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
	path := strings.TrimSpace(e.saveInput.Value)
	if path == "" {
		e.message = "filename required"
		return nil
	}

	e.path = path
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

// startCmdPalette opens the command palette.
func (e *Editor) startCmdPalette(prefix string) {
	e.cmdMode = true
	e.cmdInput = TextInput{Placeholder: "/command or :line"}
	e.cmdInput.Value = prefix
	e.cmdInput.Cursor = len([]rune(prefix))
	e.message = ""
}

func (e *Editor) updateCmdPalette(key kero.KeyEvent) error {
	switch key.Key {
	case kero.KeyEnter:
		err := e.finishCmdPalette()
		if err != nil {
			e.message = "warn: " + err.Error()
		}
	case kero.KeyEsc:
		e.cmdMode = false
	default:
		e.cmdInput.Update(key)
	}
	return nil
}

func (e *Editor) drawCmdPalette(f *kero.Frame, y int, width int) {
	style := kero.NewStyle()
	prompt := " Command "
	f.Write(0, y, trimToWidth(prompt, width), style)
	inputX := len([]rune(prompt))
	if inputX >= width {
		return
	}
	e.cmdInput.Draw(f, kero.Rect{X: inputX, Y: y, W: width - inputX, H: 1}, style)
}

func (e *Editor) finishCmdPalette() error {
	input := strings.TrimSpace(e.cmdInput.Value)
	e.cmdMode = false
	if input == "" {
		return nil
	}

	switch input[0] {
	case '/':
		// run commands
	case ':':
		// goto line, for example :123
		if len(input) < 2 {
			return nil
		}
		lineStr := strings.TrimSpace(input[1:])
		lineNum, err := strconv.Atoi(lineStr)
		if err != nil {
			return errors.New("invalid line number: " + lineStr)
		}
		if lineNum < 1 || lineNum > e.buf.LenLines() {
			return errors.New("line number out of range")
		}
		e.cursor.Row = lineNum - 1
		e.cursor.Col = 0
		if e.hasSelect() {
			e.clearSelect()
		}
		return nil
	}
	return errors.New("command must start with / or :")
}

func (e *Editor) startFind() {
	e.finding = true
	e.replacing = false
	if e.hasSelect() {
		e.findInput.Value = e.buf.GetRange(e.selAnchor, e.cursor)
		e.findInput.Cursor = len([]rune(e.findInput.Value))
		e.findInput.SelStart = 0
		e.findInput.SelEnd = e.findInput.Cursor
		return
	}
	// if no selection, pre-fill with last query
	if e.findInput.Value != "" {
		e.findInput.Cursor = len([]rune(e.findInput.Value))
		e.findInput.SelStart = 0
		e.findInput.SelEnd = e.findInput.Cursor
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
		query := e.findInput.Value
		if query == "" {
			return nil
		}
		ignoreCase := findQueryIgnoreCase(query)

		if ev.Mod&kero.ModShift != 0 {
			if ignoreCase {
				if start, end, ok := e.buf.FindPrevIgnoreCase(query, e.cursor); ok {
					e.findMatch = true
					e.findMatchStart = start
					e.findMatchEnd = end
					e.cursor = start
					e.clearSelect()
				}
			} else {
				if start, end, ok := e.buf.FindPrev(query, e.cursor); ok {
					e.findMatch = true
					e.findMatchStart = start
					e.findMatchEnd = end
					e.cursor = start
					e.clearSelect()
				}
			}
			return nil
		}

		if ignoreCase {
			start, end, ok := e.buf.FindNextIgnoreCase(query, e.cursor)
			if ok {
				e.findMatch = true
				e.findMatchStart = start
				e.findMatchEnd = end
				e.cursor = end
				e.clearSelect()
			}
		} else {
			start, end, ok := e.buf.FindNext(query, e.cursor)
			if ok {
				e.findMatch = true
				e.findMatchStart = start
				e.findMatchEnd = end
				e.cursor = end
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
	f.Write(0, y, trimToWidth(prompt, width), normal.Foreground(kero.ColorYellow))
	inputX := len([]rune(prompt))
	if inputX >= width {
		return
	}
	if !e.replacing {
		e.findInput.Draw(f, kero.Rect{X: inputX, Y: y, W: width - inputX, H: 1}, normal)
		return
	}
	e.findInput.Draw(f, kero.Rect{X: inputX, Y: y, W: width - inputX, H: 1}, normal)
	replaceX := inputX + len([]rune(e.findInput.Value)) + 4
	if replaceX < width {
		f.Write(replaceX-4, y, " -> ", normal.Foreground(kero.ColorYellow))
		e.replaceInput.Draw(f, kero.Rect{X: replaceX, Y: y, W: width - replaceX, H: 1}, normal)
	}
}

func (e *Editor) skipFindMatch() {
	query := e.findInput.Value
	if query == "" {
		return
	}
	from := e.cursor
	if e.findMatch {
		from = e.findMatchEnd
	}
	ignoreCase := findQueryIgnoreCase(query)
	var start, end Position
	var ok bool
	if ignoreCase {
		start, end, ok = e.buf.FindNextIgnoreCase(query, from)
	} else {
		start, end, ok = e.buf.FindNext(query, from)
	}
	if !ok {
		e.findMatch = false
		return
	}
	e.findMatch = true
	e.findMatchStart = start
	e.findMatchEnd = end
	e.cursor = end
	e.clearSelect()
}

func (e *Editor) replaceCurrent() error {
	query := e.findInput.Value
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
	replacedEnd := e.buf.DeleteRange(start, e.findMatchEnd)
	replacedEnd = e.buf.Insert(replacedEnd, e.replaceInput.Value)
	e.markDirty()
	e.cursor = replacedEnd
	e.findMatch = false
	// if e.replaceInput.Value == query && replacedEnd.Col < len(e.buf.Line(replacedEnd.Row)) {
	if e.replaceInput.Value == query && replacedEnd.Col < e.buf.LineEnd(replacedEnd).Col {
		replacedEnd.Col++
		e.cursor = replacedEnd
	}
	e.skipFindMatch()
	return nil
}

func (e *Editor) replaceAll() error {
	query := e.findInput.Value
	if query == "" {
		return nil
	}
	ignoreCase := findQueryIgnoreCase(query)
	var count int
	if ignoreCase {
		count = e.buf.ReplaceAllIgnoreCase(query, e.replaceInput.Value)
	} else {
		count = e.buf.ReplaceAll(query, e.replaceInput.Value)
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
	e.cursor = e.buf.Insert(e.cursor, string([]rune{r}))
	e.markDirty()
}

func (e *Editor) deleteToLineEnd() {
	if e.hasSelect() {
		e.deleteSelect()
		return
	}
	e.cursor = e.buf.DeleteRange(e.cursor, e.buf.LineEnd(e.cursor))
	e.markDirty()
}

func (e *Editor) moveLeft() {
	e.cursor = e.buf.PrevPos(e.cursor)
}

func (e *Editor) moveRight() {
	e.cursor = e.buf.NextPos(e.cursor)
}

func (e *Editor) moveUp() {
	if e.cursor.Row == 0 {
		return
	}
	displayCol := runeIndexToDisplayColumn(e.buf.Line(e.cursor.Row), e.cursor.Col)
	i := displayColumnToRuneIndex(e.buf.Line(e.cursor.Row-1), displayCol)
	e.cursor = Position{Row: e.cursor.Row - 1, Col: i}
}

func (e *Editor) moveDown() {
	if e.cursor.Row == e.buf.LenLines()-1 {
		return
	}
	displayCol := runeIndexToDisplayColumn(e.buf.Line(e.cursor.Row), e.cursor.Col)
	i := displayColumnToRuneIndex(e.buf.Line(e.cursor.Row+1), displayCol)
	e.cursor = Position{Row: e.cursor.Row + 1, Col: i}
}

func (e *Editor) markDirty() {
	e.dirty = true
	e.message = ""
	if isGoFile(e.path) {
		e.debounceCheckSemantic()
	}
}

// debounceCheckSemantic is In-memory, fast syntax and type checking
// on current buffer, debounced 300ms on keypress
func (e *Editor) debounceCheckSemantic() {
	if e.buf == nil {
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
	filename := e.path
	src := e.buf.NewReader()
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

func (e *Editor) applyDiagnosticResults() {
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
		if d.Row > e.cursor.Row ||
			(d.Row == e.cursor.Row && d.Col > e.cursor.Col) {
			return d
		}
	}

	return e.diags[0]
}

func isGoFile(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".go")
}

func runeIndexToDisplayColumn(runes []rune, pos int) int {
	if pos < 0 {
		pos = 0
	}
	if pos > len(runes) {
		pos = len(runes)
	}

	col := 0
	for i := range pos {
		if runes[i] == '\t' {
			col += 4 - (col % 4)
			continue
		}
		col++
	}
	return col
}

func displayColumnToRuneIndex(runes []rune, targetCol int) int {
	if targetCol <= 0 {
		return 0
	}

	col := 0
	for i, r := range runes {
		advance := 1
		if r == '\t' {
			advance = 4 - (col % 4)
		}
		if col+advance > targetCol {
			return i
		}
		col += advance
	}
	return len(runes)
}

func (e *Editor) ensureCursorVisible(ctx *kero.Context) {
	editorH := editorHeight(ctx)
	if editorH <= 0 {
		return
	}

	// Dynamic margin: keep 5 lines padding, but never exceed half the viewport height
	margin := min(5, max(0, (editorH-1)/2))

	// 1. Vertical Scrolling (Row)
	// Ensure cursor is above the bottom margin
	maxRowOffset := e.cursor.Row - (editorH - 1 - margin)
	if e.rowOffset < maxRowOffset {
		e.rowOffset = maxRowOffset
	}

	// Ensure cursor is below the top margin
	minRowOffset := e.cursor.Row - margin
	if e.rowOffset > minRowOffset {
		e.rowOffset = minRowOffset
	}

	// Clamp to top boundary
	if e.rowOffset < 0 {
		e.rowOffset = 0
	}

	// 2. Horizontal Scrolling (Column)
	textW := ctx.Width - lineNumberWidth(e.buf.LenLines()) - 2
	textW = max(1, textW)

	line := e.buf.Line(e.cursor.Row)
	cursorDisplay := runeIndexToDisplayColumn(line, e.cursor.Col)

	if cursorDisplay < e.colOffset {
		e.colOffset = cursorDisplay
	} else if cursorDisplay >= e.colOffset+textW {
		e.colOffset = cursorDisplay - textW + 1
	}

	if e.colOffset < 0 {
		e.colOffset = 0
	}
}

func editorHeight(ctx *kero.Context) int {
	h := ctx.Height - 2
	if h < 0 {
		return 0
	}
	return h
}

func lineNumberWidth(lines int) int {
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
	reader := e.buf.NewReader()

	// Convert internal 0-based cursor to 1-based for go/token
	res := FindDefinitionLoc(reader, e.cursor.Row+1, e.cursor.Col+1)
	if !res.Found {
		return // Symbol definition not found in current file
	}

	// Jump editor cursor (converting back to 0-based)
	e.cursor.Row = res.Line - 1
	e.cursor.Col = res.Column - 1
}

func (e *Editor) GotoSymbol(name string) {
	loc, found := FindSymbolLoc(e.buf.String(), name)
	if !found {
		return
	}

	// Move cursor (converting 1-based line/col to 0-based)
	e.cursor.Row = loc.Line - 1
	e.cursor.Col = loc.Column - 1
}

type SymbolPicker struct {
	Active   bool
	All      []SymbolLocation
	Filtered []SymbolLocation
	Index    int
	Offset   int // scrolling offset
	Limit    int // max number of symbols to display
}

func (e *Editor) updateSymbolPicker(ev kero.KeyEvent) {
	prevQuery := e.symbolInput.Value
	switch ev.String() {
	case "esc":
		// Close overlay
		e.symbolPicker.Active = false
	case "enter":
		if len(e.symbolPicker.Filtered) == 0 {
			return
		}
		picked := e.symbolPicker.Filtered[e.symbolPicker.Index]
		e.cursor.Row = picked.Line - 1
		e.cursor.Col = picked.Column - 1
		e.symbolPicker.Active = false
	case "up", "ctrl+p":
		e.symbolPicker.Index = (e.symbolPicker.Index - 1 + len(e.symbolPicker.Filtered)) % len(e.symbolPicker.Filtered)
	case "down", "ctrl+n":
		e.symbolPicker.Index = (e.symbolPicker.Index + 1) % len(e.symbolPicker.Filtered)
	default:
		e.symbolInput.Update(ev)
	}
	if q := e.symbolInput.Value; q != prevQuery {
		e.symbolPicker.Filtered = FilterSymbols(e.symbolPicker.All, q)
		e.symbolPicker.Index = 0
	}
	if e.symbolPicker.Index < e.symbolPicker.Offset {
		e.symbolPicker.Offset = e.symbolPicker.Index
	}
	if e.symbolPicker.Index > e.symbolPicker.Offset+e.symbolPicker.Limit-1 {
		e.symbolPicker.Offset = e.symbolPicker.Index - (e.symbolPicker.Limit - 1)
	}
}

// draws symbol list and TextInput
func (e *Editor) drawSymbolPicker(f *kero.Frame, y, width int) {
	normal := kero.NewStyle()
	prompt := "Symbol: "
	f.Write(0, y, prompt, normal)

	p := e.symbolPicker
	inputX := len([]rune(prompt))
	e.symbolInput.Draw(f, kero.Rect{X: inputX, Y: y, W: width, H: 1}, normal)

	// render the symbol list above the editor buffer
	n := min(len(p.Filtered), p.Limit)
	rect := kero.Rect{X: inputX, Y: y - n, W: width, H: n}
	f.Fill(rect, ' ', normal.Reverse())
	for i := range n {
		j := i + p.Offset
		if j == p.Index {
			f.Write(rect.X, rect.Y+i, " > "+p.Filtered[j].Name, normal.Reverse().Bold())
		} else {
			f.Write(rect.X, rect.Y+i, "   "+p.Filtered[j].Name, normal.Reverse())
		}
	}
}

func (e *Editor) OpenSymbolPicker() {
	symbols := ExtractAllSymbols(e.buf.String())
	e.symbolPicker = SymbolPicker{
		Active:   true,
		All:      symbols,
		Filtered: FilterSymbols(symbols, ""),
		Limit:    8,
	}
	e.symbolInput = TextInput{}
}

// TextInput is a small editable single-line text widget.
type TextInput struct {
	Value       string
	Cursor      int
	SelStart    int // selection start
	SelEnd      int
	Placeholder string // displayed when Value is empty
}

func (t *TextInput) adjustSelect() (int, int) {
	runes := []rune(t.Value)
	start, end := t.SelStart, t.SelEnd
	if start > end {
		start, end = end, start
	}
	if start < 0 {
		start = 0
	}
	if end < 0 {
		end = 0
	}
	if start > len(runes) {
		start = len(runes)
	}
	if end > len(runes) {
		end = len(runes)
	}
	return start, end
}

func (t *TextInput) clearSelect() {
	t.SelStart = t.Cursor
	t.SelEnd = t.Cursor
}

// Update applies keyboard input to the text input.
func (t *TextInput) Update(ev kero.Event) {
	e, ok := ev.(kero.KeyEvent)
	if !ok {
		return
	}

	runes := []rune(t.Value)
	if t.Cursor < 0 {
		t.Cursor = 0
	}
	if t.Cursor > len(runes) {
		t.Cursor = len(runes)
	}

	switch e.Key {
	case kero.KeyRune:
		if e.Mod&kero.ModCtrl != 0 {
			return
		}
		start, end := t.adjustSelect()
		if start != end {
			runes = append(runes[:start], runes[end:]...)
			t.Cursor = start
			t.SelStart = start
			t.SelEnd = start
		}

		runes = append(runes, 0)
		copy(runes[t.Cursor+1:], runes[t.Cursor:])
		runes[t.Cursor] = e.Rune
		t.Cursor++
		t.clearSelect()
	case kero.KeyBackspace:
		start, end := t.adjustSelect()
		if start != end {
			runes = append(runes[:start], runes[end:]...)
			t.Cursor = start
			t.clearSelect()
			break
		}
		if t.Cursor > 0 {
			runes = append(runes[:t.Cursor-1], runes[t.Cursor:]...)
			t.Cursor--
		}
	case kero.KeyDelete:
		start, end := t.adjustSelect()
		if start != end {
			runes = append(runes[:start], runes[end:]...)
			t.Cursor = start
			t.clearSelect()
			break
		}
		if t.Cursor < len(runes) {
			runes = append(runes[:t.Cursor], runes[t.Cursor+1:]...)
		}
	case kero.KeyLeft:
		if t.Cursor > 0 {
			t.Cursor--
		}
	case kero.KeyRight:
		if t.Cursor < len(runes) {
			t.Cursor++
		}
	case kero.KeyHome:
		t.Cursor = 0
	case kero.KeyEnd:
		t.Cursor = len(runes)
	}

	t.Value = string(runes)
}

// Draw renders the text input and its cursor.
func (t TextInput) Draw(f *kero.Frame, r kero.Rect, s kero.Style) {
	selectStyle := s.Foreground(kero.ColorBlack).Background(kero.ColorYellow)
	cursorStyle := s.Reverse()
	runes := []rune(t.Value)

	if t.Cursor < 0 {
		t.Cursor = 0
	}
	if t.Cursor > len(runes) {
		t.Cursor = len(runes)
	}

	start, end := t.adjustSelect()

	for i, ch := range runes {
		x := r.X + i
		y := r.Y

		if x >= r.Right() {
			break
		}

		if i >= start && i < end {
			f.Set(x, y, ch, selectStyle)
			continue
		}

		if i == t.Cursor {
			f.Set(x, y, ch, cursorStyle)
			continue
		}

		f.Set(x, y, ch, s)
	}

	if t.Cursor == len(runes) {
		x := r.X + t.Cursor
		if x < r.Right() {
			f.Set(x, r.Y, ' ', cursorStyle)
		}
	}

	if t.Value == "" && t.Placeholder != "" {
		for i, ch := range []rune(t.Placeholder) {
			if i == 0 {
				f.Set(r.X+i, r.Y, ch, cursorStyle)
			} else {
				f.Set(r.X+i, r.Y, ch, s.Dim())
			}
		}
	}
}

type Position struct {
	Row int // line index, starting at 0
	Col int // column index, starting at 0
}

type Buffer struct {
	lines [][]rune // Using [][]rune handles multi-byte UTF-8 correctly
}

func NewBuffer(content string) *Buffer {
	rawLines := strings.Split(content, "\n")
	lines := make([][]rune, len(rawLines))
	for i, l := range rawLines {
		lines[i] = []rune(l)
	}
	return &Buffer{lines: lines}
}

// Line returns a copy of the line at the specified row.
// Returns nil if the row index is out of bounds.
func (b *Buffer) Line(row int) []rune {
	if row < 0 || row >= len(b.lines) {
		return nil
	}
	src := b.lines[row]
	if src == nil {
		return nil
	}

	dst := make([]rune, len(src))
	copy(dst, src)
	return dst
}

func (b *Buffer) SetLine(row int, line []rune) {
	if row < 0 || row >= len(b.lines) {
		return
	}

	// Copy input slice to prevent external modification
	dst := make([]rune, len(line))
	copy(dst, line)
	b.lines[row] = dst
}

func (b *Buffer) LenLines() int {
	return len(b.lines)
}

// Bytes returns the entire buffer content as a UTF-8 encoded byte slice.
func (b *Buffer) Bytes() []byte {
	linesCount := len(b.lines)
	if linesCount == 0 {
		return []byte{}
	}

	// 1. Calculate total byte capacity upfront to do a single allocation
	var totalBytes int
	for _, line := range b.lines {
		for _, r := range line {
			totalBytes += utf8.RuneLen(r)
		}
	}
	// Add room for newline characters ('\n' is 1 byte per line break)
	totalBytes += (linesCount - 1)

	// 2. Pre-allocate slice buffer
	buf := make([]byte, 0, totalBytes)

	// 3. Append UTF-8 encoded bytes line by line
	var runeBuf [utf8.UTFMax]byte
	for i, line := range b.lines {
		if i > 0 {
			buf = append(buf, '\n')
		}
		for _, r := range line {
			n := utf8.EncodeRune(runeBuf[:], r)
			buf = append(buf, runeBuf[:n]...)
		}
	}

	return buf
}

// String returns the full buffer text as a string, joined by newlines.
func (b *Buffer) String() string {
	linesCount := len(b.lines)
	if linesCount == 0 {
		return ""
	}

	// 1. Calculate approximate total byte capacity to minimize allocations
	var totalBytes int
	for _, line := range b.lines {
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
	for i, line := range b.lines {
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
	if p.Row < 0 || p.Row >= len(b.lines) {
		return p
	}

	line := b.lines[p.Row]
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

		b.lines[p.Row] = newLine

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
	finalLines := make([][]rune, 0, len(b.lines)+len(insertLines)-1)
	finalLines = append(finalLines, b.lines[:p.Row]...)
	finalLines = append(finalLines, newSegment...)
	finalLines = append(finalLines, b.lines[p.Row+1:]...)

	b.lines = finalLines

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
	if len(b.lines) == 0 {
		return Position{Row: 0, Col: 0}
	}

	// Clamp Row
	row := p.Row
	if row < 0 {
		row = 0
	} else if row >= len(b.lines) {
		row = len(b.lines) - 1
	}

	// Clamp Col within the valid row
	lineLen := len(b.lines[row])
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
		return string(b.lines[start.Row][start.Col:end.Col])
	}

	// Multi-line range path
	var sb strings.Builder

	// First line fragment
	sb.WriteString(string(b.lines[start.Row][start.Col:]))
	sb.WriteRune('\n')

	// Intermediate full lines
	for l := start.Row + 1; l < end.Row; l++ {
		sb.WriteString(string(b.lines[l]))
		sb.WriteRune('\n')
	}

	// Final line fragment
	sb.WriteString(string(b.lines[end.Row][:end.Col]))

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
		line := b.lines[start.Row]
		newLine := make([]rune, 0, len(line)-(end.Col-start.Col))
		newLine = append(newLine, line[:start.Col]...)
		newLine = append(newLine, line[end.Col:]...)

		b.lines[start.Row] = newLine
		return start
	}

	// Multi-line deletion path
	startLinePrefix := b.lines[start.Row][:start.Col]
	endLineSuffix := b.lines[end.Row][end.Col:]

	// Stitch start prefix and end suffix into one merged line
	mergedLine := make([]rune, 0, len(startLinePrefix)+len(endLineSuffix))
	mergedLine = append(mergedLine, startLinePrefix...)
	mergedLine = append(mergedLine, endLineSuffix...)

	// Rebuild line slice removing deleted lines
	newLines := make([][]rune, 0, len(b.lines)-(end.Row-start.Row))
	newLines = append(newLines, b.lines[:start.Row]...)
	newLines = append(newLines, mergedLine)
	newLines = append(newLines, b.lines[end.Row+1:]...)

	b.lines = newLines

	return start
}

// WordBounds finds the start and end of the word surrounding pos on its line.
func (b *Buffer) WordBounds(p Position) (start, end Position) {
	if p.Row < 0 || p.Row >= len(b.lines) {
		return
	}

	line := b.lines[p.Row]
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
	if query == "" || len(b.lines) == 0 {
		return Position{}, Position{}, false
	}

	queryRunes := []rune(query)
	if len(queryRunes) == 0 {
		return Position{}, Position{}, false
	}

	row := from.Row
	col := from.Col
	for rowsSearched := 0; rowsSearched <= len(b.lines); rowsSearched++ {
		line := b.lines[row]
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

		if row+1 < len(b.lines) {
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
	if query == "" || len(b.lines) == 0 {
		return Position{}, Position{}, false
	}

	queryRunes := []rune(query)
	if len(queryRunes) == 0 {
		return Position{}, Position{}, false
	}
	qLen := len(queryRunes)

	row := from.Row
	for rowsSearched := 0; rowsSearched <= len(b.lines); rowsSearched++ {
		line := b.lines[row]
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
			row = len(b.lines) - 1
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

	for row, line := range b.lines {
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
			b.lines[row] = result
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
	if p.Row >= len(b.lines) {
		return p
	}

	line := b.lines[p.Row]

	// If at or past line end, wrap to the start of the next line
	if p.Col >= len(line) {
		if p.Row+1 < len(b.lines) {
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
	if p.Row < 0 || p.Row >= len(b.lines) {
		return p
	}

	line := b.lines[p.Row]
	col := p.Col
	if col > len(line) {
		col = len(line)
	}

	// If at start of line, move to end of previous line
	if col == 0 {
		if p.Row == 0 {
			return Position{Row: 0, Col: 0}
		}
		prevLine := b.lines[p.Row-1]
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
	if p.Row < b.LenLines()-1 {
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
	if p.Row < 0 || p.Row >= len(b.lines) {
		return p
	}
	for i, char := range b.lines[p.Row] {
		if !unicode.IsSpace(char) {
			return Position{Row: p.Row, Col: i}
		}
	}
	return Position{Row: p.Row, Col: 0}
}

func (b *Buffer) LineEnd(p Position) Position {
	if p.Row < 0 || p.Row >= len(b.lines) {
		return p
	}
	return Position{Row: p.Row, Col: len(b.lines[p.Row])}
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
	if r.lineIdx >= len(r.buf.lines) {
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

		line := r.buf.lines[r.lineIdx]

		// At end of line, output newline character
		if r.colIdx >= len(line) {
			p[n] = '\n'
			n++
			r.lineIdx++
			r.colIdx = 0
			r.encLen = 0
			r.encOffset = 0
			if r.lineIdx >= len(r.buf.lines) {
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
	Name   string
	Kind   string // "func", "type", "struct", "var", "const"
	Line   int    // 1-based
	Column int    // 1-based
}

// FindSymbolLoc finds the location of a top-level symbol matching exactName.
// src can be string, []byte, or io.Reader.
func FindSymbolLoc(src any, exactName string) (SymbolLocation, bool) {
	symbols := ExtractAllSymbols(src)
	for _, sym := range symbols {
		if sym.Name == exactName {
			return sym, true
		}
	}
	return SymbolLocation{}, false
}

// ExtractAllSymbols collects all top-level symbols in the file.
// Useful for fuzzy finding or symbol pickers (e.g. Ctrl+P / Cmd+Shift+O).
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
			if d.Recv != nil {
				kind = "method"
			}
			results = append(results, SymbolLocation{
				Name:   d.Name.Name,
				Kind:   kind,
				Line:   pos.Line,
				Column: pos.Column,
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

// FilterSymbols returns top-level symbols whose names contain query (case-insensitive).
func FilterSymbols(src []SymbolLocation, query string) []SymbolLocation {
	if query == "" {
		return src
	}

	queryLower := strings.ToLower(query)
	var matches []SymbolLocation

	for _, sym := range src {
		if strings.Contains(strings.ToLower(sym.Name), queryLower) {
			matches = append(matches, sym)
		}
	}

	return matches
}

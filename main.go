package main

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"kero"
)

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

	vets       []vet
	vetTimer   *time.Timer
	vetResult  chan vetResult
	vetVersion atomic.Uint64

	symbolPicker SymbolPicker
	symbolInput  TextInput
}

type vetResult struct {
	version uint64
	vets    []vet
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
	e.goVet()
	return nil
}

func (e *Editor) Update(ctx *kero.Context, ev kero.Event) error {
	e.applyVetResults()
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
			if v := e.nextVet(); v.Message != "" {
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
		// FIXME: must distinguish the Enter from manual keypress and a pasted block of text, otherwise pasting mess up indentation
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

		if v, ok := e.vetForLine(lineIndex); ok {
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
		if len(e.vets) > 0 {
			warn := fmt.Sprintf("ctrl+] goto diagnostic")
			vetStyle := kero.NewStyle().Foreground(kero.ColorRed).Reverse()
			statusWidth := len([]rune(status))
			f.Write(statusWidth+1, statusY, "| ", statusStyle)
			keyWidth := len([]rune(e.lastKey.String()))
			remainWidth := ctx.Width - statusWidth - keyWidth
			if remainWidth > 0 {
				f.Write(statusWidth+3, statusY, trimToWidth(warn, remainWidth), vetStyle)
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
	e.goVet()
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
	e.debounceSyntaxCheck()
}

// debounceSyntaxCheck is In-memory, fast syntax check on current buffer, debounced on keypress
// To keep it predictable, align the names by Execution Phase
func (e *Editor) debounceSyntaxCheck() {
	if e.buf == nil {
		return
	}
	e.vets = nil
	version := e.vetVersion.Add(1)
	// cancel previous check
	if e.vetTimer != nil {
		e.vetTimer.Stop()
	}
	if !isGoFile(e.path) {
		return
	}
	if e.vetResult == nil {
		e.vetResult = make(chan vetResult, 4)
	}
	filename := e.path
	src := e.buf.NewReader()
	e.vetTimer = time.AfterFunc(300*time.Millisecond, func() {
		vets := CheckGoSyntax(filename, src)
		result := vetResult{version: version, vets: vets}
		select {
		case <-e.vetResult:
		default:
		}
		select {
		case e.vetResult <- result:
		default:
		}
	})
}

// goVet run external go vet process, triggered on app launch or save.
func (e *Editor) goVet() {
	if e.buf == nil || !isGoFile(e.path) {
		return
	}
	if e.vetTimer != nil {
		e.vetTimer.Stop()
	}
	if e.vetResult == nil {
		e.vetResult = make(chan vetResult, 4)
	}
	version := e.vetVersion.Add(1)
	filename := e.path
	go func() {
		vets := CheckGoVet(filename)
		result := vetResult{version: version, vets: vets}
		select {
		case <-e.vetResult:
		default:
		}
		select {
		case e.vetResult <- result:
		default:
		}
	}()
}

func (e *Editor) applyVetResults() {
	if e.vetResult == nil {
		return
	}
	for {
		select {
		case result := <-e.vetResult:
			if result.version == e.vetVersion.Load() {
				e.vets = result.vets
			}
		default:
			return
		}
	}
}

func (e *Editor) vetForLine(row int) (vet, bool) {
	for _, vet := range e.vets {
		if vet.Row == row {
			return vet, true
		}
	}
	return vet{}, false
}

func (e *Editor) nextVet() vet {
	if len(e.vets) == 0 {
		return vet{}
	}

	for _, vet := range e.vets {
		if vet.Row > e.cursor.Row ||
			(vet.Row == e.cursor.Row && vet.Col > e.cursor.Col) {
			return vet
		}
	}

	return e.vets[0]
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

type vet struct {
	File    string
	Row     int // start from 0
	Col     int // start from 0
	Message string
}

// CheckGoSyntax checks syntax for Go file.
// The src parameter must be string, []byte, or [io.Reader].
func CheckGoSyntax(filename string, src any) []vet {
	if !isGoFile(filename) {
		return nil
	}
	fset := token.NewFileSet()

	// ParseHeader or ParseComments keeps it fast
	_, err := parser.ParseFile(fset, filename, src, parser.AllErrors)
	if err == nil {
		return nil
	}

	var vets []vet
	if scannerErr, ok := err.(scanner.ErrorList); ok {
		for _, e := range scannerErr {
			vets = append(vets, vet{
				Row:     e.Pos.Line - 1,
				Col:     e.Pos.Column - 1,
				Message: e.Msg,
			})
		}
	}

	return vets
}

// vetPattern matches: <optional_prefix><filename>:<line>:<col>: <message>
// Uses non-greedy matching to swallow leading package header info.
var vetPattern = regexp.MustCompile(`^(?:.*?\s)?([^\s:]+\.go):([0-9]+):([0-9]+):\s*(.*)$`)

func CheckGoVet(filename string) []vet {
	if !isGoFile(filename) {
		return nil
	}

	dir := filepath.Dir(filename)
	if dir == "" {
		dir = "."
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "vet")
	cmd.Dir = dir
	output, _ := cmd.CombinedOutput()
	//log.Printf("vet output:\n%s", string(output))

	// filter out noise from other files
	vets := parseVetOutput(string(output))
	filtered := make([]vet, 0, len(vets))
	for _, v := range vets {
		if v.File == filename {
			filtered = append(filtered, v)
		}
	}
	return filtered
}

func parseVetOutput(output string) []vet {
	var vets []vet
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		matches := vetPattern.FindStringSubmatch(line)
		if len(matches) != 5 {
			continue
		}

		// Clean up path (e.g. "./main.go" -> "main.go")
		parsedFile := filepath.Base(filepath.Clean(matches[1]))
		lineNumber, errLine := strconv.Atoi(matches[2])
		column, errColumn := strconv.Atoi(matches[3])

		if errLine != nil || errColumn != nil || parsedFile == "" {
			continue
		}

		vets = append(vets, vet{
			File:    parsedFile,
			Row:     lineNumber - 1,
			Col:     column - 1,
			Message: matches[4],
		})
	}
	return vets
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

	// set FPS for refreshing vet result
	p := kero.New(app, kero.WithAltScreen(true), kero.WithKitty(true), kero.WithFPS(3))
	if err := p.Run(); err != nil {
		panic(err)
	}
}

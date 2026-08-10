package main

import (
	"context"
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
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

	// goto mode opens a input line at the message area
	gotoMode  bool
	gotoInput TextInput

	// selection state
	selecting bool
	selAnchor Position // selection at [e.selAnchor, e.pos)

	clipboard  string
	clipIsLine bool

	lastKey kero.KeyEvent

	diagnostics       []Diagnostic
	diagnosticTimer   *time.Timer
	diagnosticResult  chan diagnosticResult
	diagnosticVersion uint64
}

type diagnosticResult struct {
	version     uint64
	diagnostics []Diagnostic
}

func (e *Editor) Init(ctx *kero.Context) error {
	if e.path == "" {
		e.buf = NewBuffer("")
		e.cursor.Row, e.cursor.Col = 0, 0
		e.message = "new buffer"
		e.debounceSyntaxCheck()
		return nil
	}

	data, err := os.ReadFile(e.path)
	if err != nil {
		if os.IsNotExist(err) {
			e.buf = NewBuffer("")
			e.cursor.Row, e.cursor.Col = 0, 0
			e.message = "new file"
			e.debounceSyntaxCheck()
			return nil
		}
		return err
	}

	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	e.buf = NewBuffer(text)
	e.cursor = e.buf.ClampPos(e.cursor)
	e.ensureCursorVisible(ctx)
	e.debounceSyntaxCheck()
	return nil
}

func (e *Editor) Update(ctx *kero.Context, ev kero.Event) error {
	e.applyDiagnosticResults()
	key, ok := ev.(kero.KeyEvent)
	if !ok {
		return nil
	}
	defer func() {
		e.lastKey = key
		e.ensureCursorVisible(ctx)
	}()

	// anytime can quit
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
	if e.gotoMode {
		return e.updateCommandPalette(key)
	}

	switch key.Key {
	case kero.KeyRune:
		switch key.String() {
		case "ctrl+]":
			// similar to vim key "ctrl+]"
			e.smartGoto()
			return nil
		case "ctrl+p":
			// goto anything
			e.startCommandPalette("")
			return nil
		case "ctrl+n":
			e.nextDiagnostic()
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
		case "ctrl+.":
			// toggle selection anchor at cursor (start selection mode)
			if e.selecting {
				e.clearSelect()
			} else {
				e.selecting = true
				e.selAnchor = e.cursor
			}
			return nil
		case "ctrl+k":
			e.deleteToLineEnd()
			return nil
		}
		if key.Mod != 0 {
			break
		}
		e.insertRune(key.Rune)
	case kero.KeyEnter:
		if e.hasSelect() {
			e.deleteSelect()
		}
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
		if key.Mod&kero.ModCtrl != 0 {
			e.deleteToLineStart()
			break
		}
		// Alt+Backspace: delete previous word
		if key.Mod&kero.ModAlt != 0 {
			prev := e.buf.MoveWordLeft(e.cursor)
			e.cursor = e.buf.DeleteRange(prev, e.cursor)
			e.markDirty()
			break
		}
		e.backspace()
	case kero.KeyDelete:
		e.delete()
	case kero.KeyLeft:
		// Alt+Left move cursor to start of current/previous word
		if key.Mod&kero.ModAlt != 0 {
			e.cursor = e.buf.MoveWordLeft(e.cursor)
			break
		}
		e.moveLeft()
	case kero.KeyRight:
		// Alt+Right move cursor to end of current/next word
		if key.Mod&kero.ModAlt != 0 {
			e.cursor = e.buf.MoveWordRight(e.cursor)
			break
		}
		e.moveRight()
	case kero.KeyUp:
		e.moveUp()
	case kero.KeyDown:
		e.moveDown()
	case kero.KeyHome:
		if e.lastKey.Key == kero.KeyHome {
			e.cursor.Col = 0
			return nil
		}
		for i, char := range e.buf.Line(e.cursor.Row) {
			if !unicode.IsSpace(char) {
				e.cursor.Col = i
				return nil
			}
		}
		e.cursor.Col = 0
	case kero.KeyEnd:
		e.cursor.Col = len(e.buf.Line(e.cursor.Row))
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
	if strings.HasPrefix(e.message, "error:") || strings.HasPrefix(e.message, "warn:") {
		messageStyle = kero.NewStyle().Foreground(kero.ColorRed)
	}

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
		if e.hasDiagnostic(lineIndex) {
			f.Write(0, y, "!", kero.NewStyle().Foreground(kero.ColorRed))
		}

		origLine := e.buf.Line(lineIndex)
		fullPadded := padTab(string(origLine), 4)
		limit := ctx.Width - gutterW
		limit = max(0, limit)

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

		// highlight selection if any
		if e.selecting {
			start, end := orderPos(e.selAnchor, e.cursor)
			if lineIndex >= start.Row && lineIndex <= end.Row {
				lineRunes := []rune(origLine)
				var selStartCol, selEndCol int
				if start.Row == end.Row {
					selStartCol = start.Col
					selEndCol = end.Col
				} else if lineIndex == start.Row {
					selStartCol = start.Col
					selEndCol = len(lineRunes)
				} else if lineIndex == end.Row {
					selStartCol = 0
					selEndCol = end.Col
				} else {
					selStartCol = 0
					selEndCol = len(lineRunes)
				}

				startVisCol := runeIndexToDisplayColumn(origLine, selStartCol)
				endVisCol := runeIndexToDisplayColumn(origLine, selEndCol)

				startDisplay := max(0, min(startVisCol-e.colOffset, len(visPadded)))
				endDisplay := max(0, min(endVisCol-e.colOffset, len(visPadded)))

				for x := startDisplay; x < endDisplay; x++ {
					ch := rune(visPadded[x])
					f.Set(gutterW+x, y, ch, selectStyle)
				}
			}
		}
	}

	line := e.buf.Line(e.cursor.Row)
	cursorDisplayCol := runeIndexToDisplayColumn(line, e.cursor.Col)
	cursorX := gutterW + cursorDisplayCol - e.colOffset
	cursorY := 0 + e.cursor.Row - e.rowOffset
	if cursorY >= 0 && cursorY < editorH && cursorX >= gutterW && cursorX < ctx.Width {
		fullLinePadded := padTab(string(line), 4)
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
		var dmsg string
		if len(e.diagnostics) > 0 {
			dmsg = fmt.Sprintf("found %d error, press ctrl+n to jump", len(e.diagnostics))
			if dd, ok := e.diagnosticForLine(e.cursor.Row); ok {
				dmsg = dd.Message
			}
		}
		if dmsg != "" {
			dmsg = "| " + dmsg
			diagnosticStyle := kero.NewStyle().Foreground(kero.ColorRed).Reverse()
			statusWidth := len([]rune(status))
			keyWidth := len([]rune(e.lastKey.String()))
			remainWidth := ctx.Width - statusWidth - keyWidth
			if remainWidth > 0 {
				f.Write(statusWidth+1, statusY, trimToWidth(dmsg, remainWidth), diagnosticStyle)
			}
		}
		if e.lastKey.Key != kero.KeyUnknown {
			ks := e.lastKey.String()
			f.Write(ctx.Width-len(ks), statusY, ks, statusStyle)
		}
	}

	messageY := ctx.Height - 2
	if messageY >= 0 {
		if e.gotoMode {
			e.drawCommandPalette(f, messageY, ctx.Width)
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
		if e.message == "" {
			e.message = "^S save | ^Q quit | ^F find | ^. select| ^C copy | ^X Cut | ^V paste | ^P goto | ^N next diagnostic"
		}
		f.Write(0, messageY, trimToWidth(" "+e.message, ctx.Width), messageStyle)
	}
}

func (e *Editor) selectLine() {
	if !e.selecting {
		e.selecting = true
		e.selAnchor = Position{Row: e.cursor.Row, Col: 0}
		if e.cursor.Row < e.buf.LenLines()-1 {
			e.cursor = Position{Row: e.cursor.Row + 1, Col: 0}
		} else {
			e.cursor = Position{Row: e.cursor.Row, Col: len(e.buf.Line(e.cursor.Row))}
		}
		return
	}

	// Already selecting: expand line selection to next line
	if e.cursor.Row < e.buf.LenLines()-1 {
		e.cursor.Row++
		e.cursor.Col = 0
	} else {
		e.cursor.Col = len(e.buf.Line(e.cursor.Row))
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
	e.clipboard = e.buf.GetRange(p1, Position{Row: e.cursor.Row, Col: len(e.buf.Line(e.cursor.Row))})
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
	if e.path == "" {
		e.startSaveAs()
		return nil
	}

	if err := os.WriteFile(e.path, []byte(e.buf.String()+"\n"), 0644); err != nil {
		e.message = "error: " + err.Error()
		return nil
	}

	e.dirty = false
	e.message = fmt.Sprintf("saved %s", filepath.Base(e.path))
	e.scheduleVetOnSave()
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

// startCommandPalette opens the command palette.
func (e *Editor) startCommandPalette(prefix string) {
	e.gotoMode = true
	e.gotoInput = TextInput{Placeholder: " :line, @symbol, or >nexterror"}
	e.gotoInput.Value = prefix
	e.gotoInput.Cursor = len([]rune(prefix))
	e.message = ""
}

func (e *Editor) updateCommandPalette(key kero.KeyEvent) error {
	switch key.Key {
	case kero.KeyEnter:
		return e.finishCommandPalette()
	case kero.KeyEsc:
		e.gotoMode = false
		return nil
	}

	e.gotoInput.Update(key)
	return nil
}

func (e *Editor) drawCommandPalette(f *kero.Frame, y int, width int) {
	style := kero.NewStyle()
	prompt := " Command "
	f.Write(0, y, trimToWidth(prompt, width), style.Foreground(kero.ColorYellow))
	inputX := len([]rune(prompt))
	if inputX >= width {
		return
	}
	e.gotoInput.Draw(f, kero.Rect{X: inputX, Y: y, W: width - inputX, H: 1}, style)
}

// gotoQueries makes cursor jump to the first matching line, with loose string matching
func (e *Editor) gotoQueries(queries []string) bool {
	if len(queries) == 0 {
		return false
	}
	type qSpec struct {
		q          string
		ignoreCase bool
	}
	var specs []qSpec
	for _, q := range queries {
		q = strings.TrimSpace(q)
		if q == "" {
			continue
		}
		ignore := true
		for _, r := range q {
			if unicode.IsUpper(r) {
				ignore = false
				break
			}
		}
		specs = append(specs, qSpec{q: q, ignoreCase: ignore})
	}
	if len(specs) == 0 {
		return false
	}
	for row, line := range e.buf.Lines() {
		lineStr := string(line)
		// prepare lowercased line only if needed
		needLower := false
		for _, s := range specs {
			if s.ignoreCase {
				needLower = true
				break
			}
		}
		var lineLower string
		if needLower {
			lineLower = strings.ToLower(lineStr)
		}

		match := true
		for _, s := range specs {
			if s.ignoreCase {
				if !strings.Contains(lineLower, strings.ToLower(s.q)) {
					match = false
					break
				}
			} else {
				if !strings.Contains(lineStr, s.q) {
					match = false
					break
				}
			}
		}
		if match {
			e.cursor = Position{Row: row, Col: 0}
			if e.hasSelect() {
				e.clearSelect()
			}
			return true
		}
	}
	return false
}

func (e *Editor) smartGoto() error {
	var target string
	if e.hasSelect() {
		target = strings.TrimSpace(e.buf.GetRange(e.selAnchor, e.cursor))
	} else if start, end := e.buf.WordBounds(e.cursor); start != end {
		target = strings.TrimSpace(e.buf.GetRange(start, end))
	}

	if target == "" {
		e.message = "no word under cursor to goto"
		return nil
	}

	defPrefixes := []string{"type", "func", "struct", "var", "const"}
	for _, prefix := range defPrefixes {
		if e.gotoQueries([]string{prefix, target}) {
			// e.message = "goto: " + prefix + " " + target
			return nil
		}
	}

	if e.gotoQueries([]string{target}) {
		// e.message = "goto: " + target
		return nil
	}

	e.message = "no match found for: " + target
	return nil
}

func (e *Editor) finishCommandPalette() error {
	input := strings.TrimSpace(e.gotoInput.Value)
	e.gotoMode = false
	if input == "" {
		e.message = ""
		return nil
	}

	switch input[0] {
	case '>':
		// run commands
		switch input {
		case ">nexterror":
			e.nextDiagnostic()
			return nil
		case ">preverror":
			e.previousDiagnostic()
			return nil
		default:
			e.message = "warn: unknown diagnostic command: " + input
		}
	case ':':
		// goto line, for example :123
		if len(input) < 2 {
			return nil
		}
		lineStr := strings.TrimSpace(input[1:])
		lineNum, err := strconv.Atoi(lineStr)
		if err != nil {
			e.message = "invalid line number: " + lineStr
			return nil
		}
		if lineNum < 1 || lineNum > e.buf.LenLines() {
			e.message = "line number out of range"
			return nil
		}
		e.cursor.Row = lineNum - 1
		e.cursor.Col = 0
		if e.hasSelect() {
			e.clearSelect()
		}
	case '@':
		// "@filter symbol" goto symbol,
		// filter can be any keyword like Go's type/func/var, or Python's def.
		if len(input) < 2 {
			return nil
		}
		parts := strings.Fields(input[1:])
		if len(parts) == 0 {
			e.message = "symbol required after @"
			return nil
		}
		if e.gotoQueries(parts) {
			return nil
		}
		e.message = "no match found for symbol: " + input[1:]
	default:
		e.message = "warn: goto-anything must start with : or @"
	}
	return nil
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

func (e *Editor) findQueryIgnoreCase(query string) bool {
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
		ignoreCase := e.findQueryIgnoreCase(query)

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
	ignoreCase := e.findQueryIgnoreCase(query)
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
	if e.replaceInput.Value == query && replacedEnd.Col < len(e.buf.Line(replacedEnd.Row)) {
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
	ignoreCase := e.findQueryIgnoreCase(query)
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

func (e *Editor) backspace() {
	if e.hasSelect() {
		e.deleteSelect()
		return
	}
	var p Position
	if e.cursor.Col > 0 {
		p = Position{Row: e.cursor.Row, Col: e.cursor.Col - 1}
	} else if e.cursor.Row > 0 {
		p = Position{Row: e.cursor.Row - 1, Col: len(e.buf.Line(e.cursor.Row - 1))}
	}
	e.cursor = e.buf.DeleteRange(p, e.cursor)
	e.markDirty()
}

func (e *Editor) delete() {
	if e.hasSelect() {
		e.deleteSelect()
		return
	}
	e.cursor = e.buf.DeleteRange(e.cursor, Position{Row: e.cursor.Row, Col: e.cursor.Col + 1})
	e.markDirty()
}

func (e *Editor) deleteToLineStart() {
	if e.hasSelect() {
		e.deleteSelect()
		return
	}
	e.cursor = e.buf.DeleteRange(Position{Row: e.cursor.Row, Col: 0}, e.cursor)
	e.markDirty()
}

func (e *Editor) deleteToLineEnd() {
	if e.hasSelect() {
		e.deleteSelect()
		return
	}
	lineEnd := Position{Row: e.cursor.Row, Col: len(e.buf.Line(e.cursor.Row))}
	e.cursor = e.buf.DeleteRange(e.cursor, lineEnd)
	e.markDirty()
}

func (e *Editor) moveLeft() {
	if e.cursor.Col > 0 {
		e.cursor.Col--
		return
	}
	if e.cursor.Row > 0 {
		e.cursor.Row--
		e.cursor.Col = len(e.buf.Line(e.cursor.Row))
	}
}

func (e *Editor) moveRight() {
	if e.cursor.Col < len(e.buf.Line(e.cursor.Row)) {
		e.cursor.Col++
		return
	}
	if e.cursor.Row < e.buf.LenLines()-1 {
		e.cursor.Row++
		e.cursor.Col = 0
	}
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
	e.diagnostics = nil
	version := atomic.AddUint64(&e.diagnosticVersion, 1)
	if e.diagnosticTimer != nil {
		e.diagnosticTimer.Stop()
	}
	if !isGoFile(e.path) {
		return
	}
	if e.diagnosticResult == nil {
		e.diagnosticResult = make(chan diagnosticResult, 4)
	}
	filename := e.path
	content := []byte(e.buf.String())
	e.diagnosticTimer = time.AfterFunc(300*time.Millisecond, func() {
		diagnostics := CheckGoSyntax(filename, content)
		result := diagnosticResult{version: version, diagnostics: diagnostics}
		select {
		case <-e.diagnosticResult:
		default:
		}
		select {
		case e.diagnosticResult <- result:
		default:
		}
	})
}

// scheduleVetOnSave run external go vet process, triggered on save.
// To keep it predictable, align the names by Execution Phase
func (e *Editor) scheduleVetOnSave() {
	if e.buf == nil || !isGoFile(e.path) {
		return
	}
	if e.diagnosticTimer != nil {
		e.diagnosticTimer.Stop()
	}
	if e.diagnosticResult == nil {
		e.diagnosticResult = make(chan diagnosticResult, 4)
	}
	version := atomic.AddUint64(&e.diagnosticVersion, 1)
	filename := e.path
	go func() {
		diagnostics := CheckGoVet(filename)
		result := diagnosticResult{version: version, diagnostics: diagnostics}
		select {
		case <-e.diagnosticResult:
		default:
		}
		select {
		case e.diagnosticResult <- result:
		default:
		}
	}()
}

func (e *Editor) applyDiagnosticResults() {
	if e.diagnosticResult == nil {
		return
	}
	for {
		select {
		case result := <-e.diagnosticResult:
			if result.version == atomic.LoadUint64(&e.diagnosticVersion) {
				e.diagnostics = result.diagnostics
			}
		default:
			return
		}
	}
}

func (e *Editor) hasDiagnostic(row int) bool {
	_, ok := e.diagnosticForLine(row)
	return ok
}

func (e *Editor) diagnosticForLine(row int) (Diagnostic, bool) {
	for _, diagnostic := range e.diagnostics {
		if diagnostic.Line == row {
			return diagnostic, true
		}
	}
	return Diagnostic{}, false
}

func (e *Editor) nextDiagnostic() {
	if len(e.diagnostics) == 0 {
		e.message = "no diagnostics"
		return
	}

	for _, diagnostic := range e.diagnostics {
		if diagnostic.Line > e.cursor.Row ||
			(diagnostic.Line == e.cursor.Row && diagnostic.Col > e.cursor.Col) {
			e.cursor = e.buf.ClampPos(Position{Row: diagnostic.Line, Col: diagnostic.Col})
			return
		}
	}

	diagnostic := e.diagnostics[0]
	e.cursor = e.buf.ClampPos(Position{Row: diagnostic.Line, Col: diagnostic.Col})
}

func (e *Editor) previousDiagnostic() {
	if len(e.diagnostics) == 0 {
		e.message = "no diagnostics"
		return
	}

	for i := len(e.diagnostics) - 1; i >= 0; i-- {
		diagnostic := e.diagnostics[i]
		if diagnostic.Line < e.cursor.Row ||
			(diagnostic.Line == e.cursor.Row && diagnostic.Col < e.cursor.Col) {
			e.cursor = e.buf.ClampPos(Position{Row: diagnostic.Line, Col: diagnostic.Col})
			return
		}
	}

	diagnostic := e.diagnostics[len(e.diagnostics)-1]
	e.cursor = e.buf.ClampPos(Position{Row: diagnostic.Line, Col: diagnostic.Col})
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
	for i := 0; i < pos; i++ {
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
	if e.cursor.Row < e.rowOffset {
		e.rowOffset = e.cursor.Row
	}
	// for easy reading, controls scrolling behavior and edge padding around the cursor.
	margin := 5
	if e.cursor.Row >= e.rowOffset+editorH-margin {
		e.rowOffset = e.cursor.Row - editorH + margin
	}
	if e.rowOffset < 0 {
		e.rowOffset = 0
	}

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

type Diagnostic struct {
	Line    int
	Col     int
	Message string
}

func CheckGoSyntax(filename string, content []byte) []Diagnostic {
	if !isGoFile(filename) {
		return nil
	}
	fset := token.NewFileSet()

	// ParseHeader or ParseComments keeps it fast
	_, err := parser.ParseFile(fset, filename, content, parser.AllErrors)
	if err == nil {
		return nil
	}

	var diagnostics []Diagnostic
	if scannerErr, ok := err.(scanner.ErrorList); ok {
		for _, e := range scannerErr {
			diagnostics = append(diagnostics, Diagnostic{
				Line:    e.Pos.Line - 1,
				Col:     e.Pos.Column - 1,
				Message: e.Msg,
			})
		}
	}

	return diagnostics
}

var vetDiagnosticPattern = regexp.MustCompile(`^.*:([0-9]+):([0-9]+): (.*)$`)

func CheckGoVet(filename string) []Diagnostic {
	if !isGoFile(filename) {
		return nil
	}

	dir := filepath.Dir(filename)
	if dir == "" {
		dir = "."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "vet", dir)
	cmd.Dir = dir
	output, _ := cmd.CombinedOutput()
	return parseVetDiagnostics(string(output))
}

func parseVetDiagnostics(output string) []Diagnostic {
	var diagnostics []Diagnostic
	for line := range strings.SplitSeq(output, "\n") {
		matches := vetDiagnosticPattern.FindStringSubmatch(line)
		if len(matches) != 4 {
			continue
		}
		lineNumber, errLine := strconv.Atoi(matches[1])
		column, errColumn := strconv.Atoi(matches[2])
		if errLine != nil || errColumn != nil {
			continue
		}
		diagnostics = append(diagnostics, Diagnostic{
			Line:    lineNumber - 1,
			Col:     column - 1,
			Message: matches[3],
		})
	}
	return diagnostics
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
	app := &Editor{}
	if len(os.Args) > 1 {
		app.path, app.cursor.Row, app.cursor.Col = parsePathArg(os.Args[1])
	}

	p := kero.New(app, kero.WithAltScreen(true), kero.WithKitty(true), kero.WithFPS(1))
	if err := p.Run(); err != nil {
		panic(err)
	}
}

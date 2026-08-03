package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"kero"
)

type Editor struct {
	path string

	lines []string
	row   int
	col   int

	rowOffset int
	colOffset int // visual display column offset (0-based horizontal scroll position)

	dirty   bool
	message string

	// prompt for saving
	saveAs    bool
	saveInput TextInput

	// find mode opens a find line at the message area
	finding   bool
	findInput TextInput

	// command mode opens a command line at the message area
	cmdMode  bool
	cmdInput TextInput

	// selection state
	selecting   bool
	selStartRow int
	selStartCol int
	selEndRow   int
	selEndCol   int
	clipboard   string

	// last key is a user input state, don't hurry to make it into program context
	lastKey kero.KeyEvent
}

func (e *Editor) Init(ctx *kero.Context) error {
	if e.path == "" {
		e.lines = []string{""}
		e.message = "new buffer"
		return nil
	}

	data, err := os.ReadFile(e.path)
	if err != nil {
		if os.IsNotExist(err) {
			e.lines = []string{""}
			e.message = "new file"
			return nil
		}
		return err
	}

	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	e.lines = strings.Split(text, "\n")
	if len(e.lines) == 0 {
		e.lines = []string{""}
	}
	return nil
}

func (e *Editor) Update(ctx *kero.Context, ev kero.Event) error {
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
	if e.cmdMode {
		return e.updateCommand(key)
	}

	if e.lastKey.String() == "ctrl+k" && (key.String() == "ctrl+d" || key.String() == "d") {
		return e.smartGoto()
	}

	switch key.Key {
	case kero.KeyRune:
		switch key.String() {
		case "ctrl+k":
			e.message = "(ctrl+k) was pressed. Waiting for second key..."
			return nil
		case "ctrl+;":
			// imitation of vim's shift+; (:)
			e.startCommand()
			return nil
		case "ctrl+s":
			return e.save()
		case "ctrl+f":
			e.startFind()
			return nil
		case "ctrl+c":
			e.copySelect()
			return nil
		case "ctrl+x":
			e.cutSelect()
			return nil
		case "ctrl+v":
			e.pasteClipboard()
			return nil
		case "ctrl+w":
			// select the word under cursor
			e.selectWord()
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
				e.selStartRow = e.row
				e.selStartCol = e.col
				e.selEndRow = e.row
				e.selEndCol = e.col
			}
			return nil
		}
		if key.Mod != 0 {
			break
		}
		e.insertRune(key.Rune)
	case kero.KeyEnter:
		var n int
		line := e.currentLine()
		for _, b := range line {
			if !unicode.IsSpace(b) {
				break
			}
			n++
		}
		indent := line[:n]
		if key.Mod&kero.ModCtrl != 0 {
			e.col = len([]rune(line))
		}
		e.insertNewline()
		e.insertString(indent)
	case kero.KeyTab:
		if key.Mod&kero.ModShift != 0 {
			e.unindentSelectOrLine()
			break
		}
		if e.hasSelect() {
			sr, _, er, _ := e.adjustSelect()
			if sr != er {
				e.indentSelect()
				break
			}
		}
		e.insertRune('\t')
	case kero.KeyBackspace:
		e.backspace()
	case kero.KeyDelete:
		e.delete()
	case kero.KeyLeft:
		e.moveLeft()
		if e.selecting {
			e.selEndRow, e.selEndCol = e.row, e.col
		}
	case kero.KeyRight:
		e.moveRight()
		if e.selecting {
			e.selEndRow, e.selEndCol = e.row, e.col
		}
	case kero.KeyUp:
		e.moveUp()
		if e.selecting {
			e.selEndRow, e.selEndCol = e.row, e.col
		}
	case kero.KeyDown:
		e.moveDown()
		if e.selecting {
			e.selEndRow, e.selEndCol = e.row, e.col
		}
	case kero.KeyHome:
		if e.lastKey.Key == kero.KeyHome {
			e.col = 0
			return nil
		}
		for i, char := range e.lines[e.row] {
			if !unicode.IsSpace(char) {
				e.col = i
				return nil
			}
		}
		e.col = 0
	case kero.KeyEnd:
		e.col = len([]rune(e.currentLine()))
	case kero.KeyPgUp:
		e.row -= editorHeight(ctx)
		if e.row < 0 {
			e.row = 0
		}
		e.clampCol()
	case kero.KeyPgDown:
		e.row += editorHeight(ctx)
		if e.row >= len(e.lines) {
			e.row = len(e.lines) - 1
		}
		e.clampCol()
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
	lineNoW := lineNumberWidth(len(e.lines))
	for y := range editorH {
		lineIndex := e.rowOffset + y
		if lineIndex >= len(e.lines) {
			f.Write(0, y, "~", lineNoStyle)
			continue
		}

		f.Write(0, y, fmt.Sprintf("%*d ", lineNoW, lineIndex+1), lineNoStyle)

		origLine := e.lines[lineIndex]
		fullPadded := padTab(origLine, 4)
		limit := ctx.Width - lineNoW - 1
		limit = max(0, limit)

		var visPadded string
		if e.colOffset < len(fullPadded) {
			visPadded = fullPadded[e.colOffset:]
		}
		if len(visPadded) > limit {
			visPadded = visPadded[:limit]
		}

		style := textStyle
		if lineIndex == e.row {
			style = style.Underline()
		}
		f.Write(lineNoW+1, y, visPadded, style)

		// overlay selection if present
		if e.selecting {
			sr, sc, er, ec := e.adjustSelect()
			if lineIndex >= sr && lineIndex <= er {
				lineRunes := []rune(origLine)
				var selStartRune, selEndRune int
				if sr == er {
					selStartRune = sc
					selEndRune = ec
				} else if lineIndex == sr {
					selStartRune = sc
					selEndRune = len(lineRunes)
				} else if lineIndex == er {
					selStartRune = 0
					selEndRune = ec
				} else {
					selStartRune = 0
					selEndRune = len(lineRunes)
				}

				startVisCol := runeIndexToDisplayColumn(origLine, selStartRune)
				endVisCol := runeIndexToDisplayColumn(origLine, selEndRune)

				startDisplay := max(0, min(startVisCol-e.colOffset, len(visPadded)))
				endDisplay := max(0, min(endVisCol-e.colOffset, len(visPadded)))

				for x := startDisplay; x < endDisplay; x++ {
					ch := rune(visPadded[x])
					f.Set(lineNoW+1+x, y, ch, selectStyle)
				}
			}
		}
	}

	line := e.currentLine()
	cursorDisplayCol := runeIndexToDisplayColumn(line, e.col)
	cursorX := lineNoW + 1 + cursorDisplayCol - e.colOffset
	cursorY := 0 + e.row - e.rowOffset
	if cursorY >= 0 && cursorY < editorH && cursorX >= lineNoW+1 && cursorX < ctx.Width {
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
			name+modified, len(e.lines), e.row+1, cursorDisplayCol+1)
		if e.selecting {
			status = status + " | Selecting"
		}
		f.Fill(kero.Rect{X: 0, Y: statusY, W: ctx.Width, H: 1}, ' ', statusStyle)
		f.Write(0, statusY, trimToWidth(status, ctx.Width), statusStyle)
		if e.lastKey.Key != kero.KeyUnknown {
			ks := e.lastKey.String()
			f.Write(ctx.Width-len(ks), statusY, ks, statusStyle)
		}
	}

	messageY := ctx.Height - 2
	if messageY >= 0 {
		if e.cmdMode {
			e.drawCommand(f, messageY, ctx.Width)
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
			e.message = "^S save | ^Q quit | ^F find | ^. select| ^C copy | ^X Cut | ^V paste"
		}
		f.Write(0, messageY, trimToWidth(" "+e.message, ctx.Width), messageStyle)
	}
}

func (e *Editor) clearSelect() {
	e.selecting = false
}

// adjustSelect returns the selection start and end positions in normalized order (start <= end).
func (e *Editor) adjustSelect() (int, int, int, int) {
	sr, sc, er, ec := e.selStartRow, e.selStartCol, e.selEndRow, e.selEndCol
	if sr > er || (sr == er && sc > ec) {
		sr, sc, er, ec = er, ec, sr, sc
	}
	return sr, sc, er, ec
}

func (e *Editor) selectLine() {
	if !e.selecting {
		e.selecting = true
		e.selStartRow = e.row
		e.selStartCol = 0
		if e.row < len(e.lines)-1 {
			e.selEndRow = e.row + 1
			e.selEndCol = 0
		} else {
			e.selEndRow = e.row
			e.selEndCol = len([]rune(e.currentLine()))
		}
		e.row = e.selEndRow
		e.col = e.selEndCol
		return
	}

	// Already selecting: extend line selection to next line
	if e.selEndRow < len(e.lines)-1 {
		e.selEndRow++
		e.selEndCol = 0
	} else {
		e.selEndCol = len([]rune(e.lines[e.selEndRow]))
	}
	e.row = e.selEndRow
	e.col = e.selEndCol
}

func isWordChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

func (e *Editor) selectWord() {
	e.selecting = true
	line := []rune(e.currentLine())
	if len(line) == 0 {
		e.selStartCol = 0
		e.selEndCol = 0
		e.selStartRow = e.row
		e.selEndRow = e.row
		return
	}
	// position normalized
	pos := e.col
	if pos > 0 && pos == len(line) {
		pos = pos - 1
	}
	// if current rune is not a word char, advance to the next word char to the right
	if pos < len(line) && !isWordChar(line[pos]) {
		i := pos
		for i < len(line) && !isWordChar(line[i]) {
			i++
		}
		pos = min(i, len(line))
		// if we didn't find a word to the right, try moving left
		if pos >= len(line) {
			j := e.col
			for j > 0 && !isWordChar(line[j-1]) {
				j--
			}
			pos = min(j, len(line))
		}
	}
	// find word boundaries around pos
	start := pos
	for start > 0 && isWordChar(line[start-1]) {
		start--
	}
	end := pos
	for end < len(line) && isWordChar(line[end]) {
		end++
	}
	if start == end {
		// nothing selectable, keep empty selection at cursor
		start = pos
		end = pos
	}
	e.selStartRow = e.row
	e.selEndRow = e.row
	e.selStartCol = start
	e.selEndCol = end
	// put cursor at end
	e.col = end
}

func (e *Editor) hasSelect() bool {
	return e.selecting && !(e.selStartRow == e.selEndRow && e.selStartCol == e.selEndCol)
}

func (e *Editor) getSelect() string {
	sr, sc, er, ec := e.adjustSelect()
	if sr == er {
		line := []rune(e.lines[sr])
		return string(line[sc:ec])
	}
	var sb strings.Builder
	sb.WriteString(string([]rune(e.lines[sr])[sc:]))
	sb.WriteRune('\n')
	for r := sr + 1; r < er; r++ {
		sb.WriteString(e.lines[r])
		sb.WriteRune('\n')
	}
	sb.WriteString(string([]rune(e.lines[er])[0:ec]))
	return sb.String()
}

func (e *Editor) copySelect() {
	if !e.hasSelect() {
		// copy entire current line
		e.clipboard = e.lines[e.row]
		return
	}
	e.clipboard = e.getSelect()
}

func (e *Editor) indentSelect() {
	if !e.hasSelect() {
		return
	}
	sr, _, er, ec := e.adjustSelect()
	if sr == er {
		return
	}
	lastRow := er
	if ec == 0 && er > sr {
		lastRow = er - 1
	}
	for r := sr; r <= lastRow; r++ {
		e.lines[r] = "\t" + e.lines[r]
	}
	if sr <= e.selStartRow && e.selStartRow <= lastRow {
		e.selStartCol++
	}
	if sr <= e.selEndRow && e.selEndRow <= lastRow {
		e.selEndCol++
		e.col++
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
		newLine, removed := unindentLine(e.lines[e.row])
		if removed > 0 {
			e.lines[e.row] = newLine
			e.col = max(0, e.col-removed)
			e.markDirty()
		}
		return
	}

	sr, _, er, ec := e.adjustSelect()
	lastRow := er
	if ec == 0 && er > sr {
		lastRow = er - 1
	}
	var startRemoved, endRemoved int
	for r := sr; r <= lastRow; r++ {
		newLine, removed := unindentLine(e.lines[r])
		if r == e.selStartRow {
			startRemoved = removed
		}
		if r == e.selEndRow {
			endRemoved = removed
		}
		e.lines[r] = newLine
	}

	if e.selStartRow <= lastRow {
		e.selStartCol = max(0, e.selStartCol-startRemoved)
	}
	if e.selEndRow <= lastRow {
		e.selEndCol = max(0, e.selEndCol-endRemoved)
		e.col = max(0, e.col-endRemoved)
	}
	e.markDirty()
}

func (e *Editor) deleteSelect() {
	if !e.hasSelect() {
		return
	}
	sr, sc, er, ec := e.adjustSelect()
	if sr == er {
		line := []rune(e.lines[sr])
		left := string(line[:sc])
		right := string(line[ec:])
		e.lines[sr] = left + right
		e.row = sr
		e.col = sc
		e.selecting = false
		e.markDirty()
		return
	}
	left := string([]rune(e.lines[sr])[:sc])
	right := string([]rune(e.lines[er])[ec:])
	e.lines[sr] = left + right
	// remove middle lines
	if er >= sr+1 {
		e.lines = append(e.lines[:sr+1], e.lines[er+1:]...)
	} else {
		e.lines = append(e.lines[:sr+1], e.lines[er+1:]...)
	}
	e.row = sr
	e.col = sc
	e.selecting = false
	e.markDirty()
}

func (e *Editor) cutSelect() {
	if !e.hasSelect() {
		// cut current line
		e.clipboard = e.lines[e.row]
		if len(e.lines) == 1 {
			e.lines[0] = ""
			e.row = 0
			e.col = 0
		} else {
			// remove line
			if e.row == len(e.lines)-1 {
				e.lines = e.lines[:len(e.lines)-1]
				e.row--
				e.col = 0
			} else {
				e.lines = append(e.lines[:e.row], e.lines[e.row+1:]...)
				e.col = 0
			}
		}
		e.markDirty()
		return
	}
	e.copySelect()
	e.deleteSelect()
}

func (e *Editor) pasteClipboard() {
	if e.clipboard == "" {
		return
	}
	// if selection present, replace it
	if e.hasSelect() {
		e.deleteSelect()
	}
	clip := e.clipboard
	if !strings.Contains(clip, "\n") {
		// simple insert
		line := []rune(e.currentLine())
		left := string(line[:e.col])
		right := string(line[e.col:])
		e.lines[e.row] = left + clip + right
		e.col += len([]rune(clip))
		e.markDirty()
		return
	}
	// multi-line paste
	line := []rune(e.currentLine())
	left := string(line[:e.col])
	right := string(line[e.col:])
	parts := strings.Split(clip, "\n")
	e.lines[e.row] = left + parts[0]
	insert := make([]string, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		insert = append(insert, parts[i])
	}
	// append right to the last inserted line
	last := insert[len(insert)-1]
	insert[len(insert)-1] = last + right
	// splice into lines
	e.lines = append(e.lines[:e.row+1], append(insert, e.lines[e.row+1:]...)...)
	e.row = e.row + len(parts) - 1
	e.col = len([]rune(parts[len(parts)-1]))
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

	if err := os.WriteFile(e.path, []byte(strings.Join(e.lines, "\n")+"\n"), 0644); err != nil {
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

// startCommand puts the editor into command-line mode (like vim's :)
func (e *Editor) startCommand() {
	e.cmdMode = true
	e.cmdInput = TextInput{}
	e.cmdInput.Value = ""
	e.cmdInput.Cursor = 0
	e.message = ""
}

func (e *Editor) updateCommand(key kero.KeyEvent) error {
	switch key.Key {
	case kero.KeyEnter:
		return e.finishCommand()
	case kero.KeyEsc:
		e.cmdMode = false
		return nil
	}

	e.cmdInput.Update(key)
	return nil
}

func (e *Editor) drawCommand(f *kero.Frame, y int, width int) {
	style := kero.NewStyle()
	prompt := ">"
	f.Write(0, y, trimToWidth(prompt, width), style)
	inputX := len([]rune(prompt))
	if inputX >= width {
		return
	}
	e.cmdInput.Draw(f, kero.Rect{X: inputX, Y: y, W: width - inputX, H: 1}, style)
}

func (e *Editor) gotoQueries(queries []string) bool {
	if len(queries) == 0 {
		return false
	}
	var lowerQueries []string
	for _, q := range queries {
		q = strings.TrimSpace(strings.ToLower(q))
		if q != "" {
			lowerQueries = append(lowerQueries, q)
		}
	}
	if len(lowerQueries) == 0 {
		return false
	}
	for row, line := range e.lines {
		lineLower := strings.ToLower(line)
		match := true
		for _, query := range lowerQueries {
			if !strings.Contains(lineLower, query) {
				match = false
				break
			}
		}
		if match {
			e.row = row
			e.col = 0
			if e.hasSelect() {
				e.clearSelect()
			}
			return true
		}
	}
	return false
}

func (e *Editor) wordUnderCursor() string {
	line := []rune(e.currentLine())
	if len(line) == 0 {
		return ""
	}
	pos := e.col
	if pos >= len(line) {
		pos = len(line) - 1
	}
	if pos < 0 {
		pos = 0
	}
	if !isWordChar(line[pos]) {
		i := pos
		for i < len(line) && !isWordChar(line[i]) {
			i++
		}
		if i < len(line) {
			pos = i
		} else {
			j := pos
			for j > 0 && !isWordChar(line[j-1]) {
				j--
			}
			if j > 0 && isWordChar(line[j-1]) {
				pos = j - 1
			}
		}
	}
	if pos < 0 || pos >= len(line) || !isWordChar(line[pos]) {
		return ""
	}
	start := pos
	for start > 0 && isWordChar(line[start-1]) {
		start--
	}
	end := pos
	for end < len(line) && isWordChar(line[end]) {
		end++
	}
	return string(line[start:end])
}

func (e *Editor) smartGoto() error {
	var target string
	if e.hasSelect() {
		target = strings.TrimSpace(e.getSelect())
	} else {
		target = strings.TrimSpace(e.wordUnderCursor())
	}

	if target == "" {
		e.message = "no word under cursor to goto"
		return nil
	}

	defPrefixes := []string{"type", "func", "struct", "var", "const"}
	for _, prefix := range defPrefixes {
		if e.gotoQueries([]string{prefix, target}) {
			e.message = "goto: " + prefix + " " + target
			return nil
		}
	}

	if e.gotoQueries([]string{target}) {
		e.message = "goto: " + target
		return nil
	}

	e.message = "no match found for: " + target
	return nil
}

func (e *Editor) finishCommand() error {
	cmd := strings.TrimSpace(e.cmdInput.Value)
	e.cmdMode = false
	if cmd == "" {
		e.message = ""
		return nil
	}
	parts := strings.Fields(cmd)
	switch parts[0] {
	case "goto":
		// go to a line containing the query, case-insensitive
		// can acts like Go To Definition, for example "goto func xxx" or "goto type xxx"
		if len(parts) < 2 {
			e.message = "usage: goto <query> [query2 ...]"
			return nil
		}
		if !e.gotoQueries(parts[1:]) {
			e.message = "no match found for: " + strings.Join(parts[1:], " ")
		} else {
			e.message = ""
		}
		return nil
	default:
		e.message = "unknown command: " + cmd
	}
	return nil
}

func (e *Editor) startFind() {
	e.finding = true
	if e.hasSelect() {
		e.findInput.Value = e.getSelect()
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

func (e *Editor) updateFind(ev kero.KeyEvent) error {
	switch ev.Key {
	case kero.KeyEnter:
		if ev.Mod&kero.ModShift != 0 {
			e.findPrev()
		} else {
			e.findNext()
		}
		return nil
	case kero.KeyEsc:
		e.finding = false
		return nil
	}

	e.findInput.Update(ev)
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
	e.findInput.Draw(f, kero.Rect{X: inputX, Y: y, W: width - inputX, H: 1}, normal)
}

func (e *Editor) findNext() {
	query := e.findInput.Value
	if strings.TrimSpace(query) == "" {
		return
	}

	row := e.row
	col := e.col
	for {
		line := e.lines[row][col:]
		i := strings.Index(strings.ToLower(line), strings.ToLower(query))
		if i >= 0 {
			e.row = row
			e.col = col + i + len(query)
			if e.hasSelect() {
				e.clearSelect()
			}
			return
		}
		if row < len(e.lines)-1 {
			row++
		} else {
			row = 0
		}
		if row == e.row {
			// loop back
			break
		}
		col = 0
	}
}

func (e *Editor) findPrev() {
	query := e.findInput.Value
	if strings.TrimSpace(query) == "" {
		return
	}

	row := e.row
	col := e.col
	for {
		line := e.lines[row][:col]
		i := strings.Index(strings.ToLower(line), strings.ToLower(query))
		if i >= 0 {
			e.row = row
			e.col = i
			if e.hasSelect() {
				e.clearSelect()
			}
			return
		}
		if row > 0 {
			row--
		} else {
			row = len(e.lines) - 1
		}
		if row == e.row {
			break
		}
		col = max(0, len(e.lines[row])-1)
	}
}

func (e *Editor) insertRune(r rune) {
	if e.hasSelect() {
		e.deleteSelect()
	}
	line := []rune(e.currentLine())
	line = append(line, 0)
	copy(line[e.col+1:], line[e.col:])
	line[e.col] = r
	e.lines[e.row] = string(line)
	e.col++
	e.markDirty()
}

func (e *Editor) insertString(s string) {
	for _, r := range s {
		e.insertRune(r)
	}
}

func (e *Editor) insertNewline() {
	line := []rune(e.currentLine())
	left := string(line[:e.col])
	right := string(line[e.col:])

	e.lines[e.row] = left
	e.lines = append(e.lines, "")
	copy(e.lines[e.row+2:], e.lines[e.row+1:])
	e.lines[e.row+1] = right
	e.row++
	e.col = 0
	e.markDirty()
}

func (e *Editor) backspace() {
	if e.hasSelect() {
		e.deleteSelect()
		return
	}
	if e.col > 0 {
		line := []rune(e.currentLine())
		line = append(line[:e.col-1], line[e.col:]...)
		e.lines[e.row] = string(line)
		e.col--
		e.markDirty()
		return
	}

	if e.row == 0 {
		return
	}

	prev := []rune(e.lines[e.row-1])
	e.col = len(prev)
	e.lines[e.row-1] += e.lines[e.row]
	e.lines = append(e.lines[:e.row], e.lines[e.row+1:]...)
	e.row--
	e.markDirty()
}

func (e *Editor) delete() {
	if e.hasSelect() {
		e.deleteSelect()
		return
	}
	line := []rune(e.currentLine())
	if e.col < len(line) {
		line = append(line[:e.col], line[e.col+1:]...)
		e.lines[e.row] = string(line)
		e.markDirty()
		return
	}

	if e.row >= len(e.lines)-1 {
		return
	}

	e.lines[e.row] += e.lines[e.row+1]
	e.lines = append(e.lines[:e.row+1], e.lines[e.row+2:]...)
	e.markDirty()
}

func (e *Editor) moveLeft() {
	if e.col > 0 {
		e.col--
		return
	}
	if e.row > 0 {
		e.row--
		e.col = len([]rune(e.currentLine()))
	}
}

func (e *Editor) moveRight() {
	if e.col < len([]rune(e.currentLine())) {
		e.col++
		return
	}
	if e.row < len(e.lines)-1 {
		e.row++
		e.col = 0
	}
}

func (e *Editor) moveUp() {
	if e.row > 0 {
		dstCol := runeIndexToDisplayColumn(e.currentLine(), e.col)
		e.row--
		e.col = displayColumnToRuneIndex(e.currentLine(), dstCol)
		e.clampCol()
	}
}

func (e *Editor) moveDown() {
	if e.row < len(e.lines)-1 {
		dstCol := runeIndexToDisplayColumn(e.currentLine(), e.col)
		e.row++
		e.col = displayColumnToRuneIndex(e.currentLine(), dstCol)
		e.clampCol()
	}
}

func (e *Editor) clampCol() {
	lineLen := len([]rune(e.currentLine()))
	if e.col > lineLen {
		e.col = lineLen
	}
}

func (e *Editor) currentLine() string {
	if len(e.lines) == 0 {
		e.lines = []string{""}
	}
	if e.row < 0 {
		e.row = 0
	}
	if e.row >= len(e.lines) {
		e.row = len(e.lines) - 1
	}
	return e.lines[e.row]
}

func (e *Editor) markDirty() {
	e.dirty = true
	e.message = ""
}

func runeIndexToDisplayColumn(s string, pos int) int {
	runes := []rune(s)
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

func displayColumnToRuneIndex(s string, targetCol int) int {
	runes := []rune(s)
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
	if e.row < e.rowOffset {
		e.rowOffset = e.row
	}
	// for easy reading, controls scrolling behavior and edge padding around the cursor.
	margin := 5
	if e.row >= e.rowOffset+editorH-margin {
		e.rowOffset = e.row - editorH + margin
	}
	if e.rowOffset < 0 {
		e.rowOffset = 0
	}

	textW := ctx.Width - lineNumberWidth(len(e.lines)) - 1
	textW = max(1, textW)
	line := e.currentLine()
	cursorDisplay := runeIndexToDisplayColumn(line, e.col)
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

func main() {
	/*
		f, err := os.OpenFile("/tmp/keroedit.log", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			panic(err)
		}
		defer f.Close()
		log.SetOutput(f)
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	*/
	app := &Editor{}
	if len(os.Args) > 1 {
		app.path = os.Args[1]
	}

	p := kero.New(app, kero.WithAltScreen(true), kero.WithKitty(true))
	if err := p.Run(); err != nil {
		panic(err)
	}
}

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"kero"
)

type Editor struct {
	path string

	lines []string
	row   int
	col   int

	rowOffset int
	colOffset int

	dirty   bool
	message string

	// prompt for saving
	saveAs    bool
	saveInput kero.TextInput

	// find mode opens a find line at the message area
	finding   bool
	findInput kero.TextInput

	// command mode opens a command line at the message area
	cmdMode  bool
	cmdInput kero.TextInput
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

func isKeyCombo(k kero.KeyEvent, s string) bool {
	s = strings.ToLower(s)
	parts := strings.Split(s, "-")
	if len(parts) < 2 {
		return false
	}
	if parts[0] == "ctrl" {
		return k.Key == kero.KeyRune && k.Mod&kero.ModCtrl != 0 && k.Rune == []rune(parts[1])[0]
	}
	// todo: support other modifier
	return false
}

func (e *Editor) Update(ctx *kero.Context, ev kero.Event) error {
	key, ok := ev.(kero.KeyEvent)
	if !ok {
		return nil
	}
	defer e.ensureCursorVisible(ctx)

	if e.saveAs {
		return e.updateSaveAs(key)
	}
	if e.finding {
		return e.updateFind(key)
	}
	if e.cmdMode {
		return e.updateCommand(key)
	}

	switch key.Key {
	case kero.KeyRune:
		// Open command line on Ctrl+\
		if isKeyCombo(key, "ctrl-\\") {
			e.startCommand()
			return nil
		}
		if isKeyCombo(key, "ctrl-q") {
			last := ctx.LastKeyEvent
			quitAgain := isKeyCombo(last, "ctrl-q")
			if e.dirty && !quitAgain {
				e.message = "warn: unsaved changes - press Ctrl-S to save or Ctrl-Q again to quit"
				return nil
			}
			ctx.Quit()
			return nil
		}
		if isKeyCombo(key, "ctrl-s") {
			return e.save()
		}
		if isKeyCombo(key, "ctrl-f") {
			e.startFind()
		}
		// find next
		if isKeyCombo(key, "ctrl-n") {
			e.findNext()
			return nil
		}
		// find previous
		if isKeyCombo(key, "ctrl-p") {
			e.findPrev()
			return nil
		}
		if key.Mod != 0 {
			break
		}
		e.insertRune(key.Rune)
	case kero.KeyEnter:
		e.insertNewline()
	case kero.KeyTab:
		e.insertRune('\t')
	case kero.KeyBackspace:
		e.backspace()
	case kero.KeyDelete:
		e.delete()
	case kero.KeyLeft:
		e.moveLeft()
	case kero.KeyRight:
		e.moveRight()
	case kero.KeyUp:
		e.moveUp()
	case kero.KeyDown:
		e.moveDown()
	case kero.KeyHome:
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
		if e.finding {
			e.finding = false
			break
		}
	}

	return nil
}

func (e *Editor) View(ctx *kero.Context, f *kero.Frame) {
	statusStyle := kero.NewStyle().Reverse()
	lineNoStyle := kero.NewStyle().Foreground(kero.ColorBlue).Dim()
	textStyle := kero.NewStyle()
	cursorStyle := textStyle.Reverse()
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
		line := []rune(e.lines[lineIndex])
		if e.colOffset < len(line) {
			line = line[e.colOffset:]
		} else {
			line = nil
		}
		limit := ctx.Width - lineNoW - 1
		if limit < 0 {
			limit = 0
		}
		if len(line) > limit {
			line = line[:limit]
		}
		f.Write(lineNoW+1, y, padTab(string(line), 4), textStyle)
	}

	cursorX := lineNoW + 1 + e.col - e.colOffset
	cursorY := 0 + e.row - e.rowOffset
	if cursorY >= 0 && cursorY < editorH && cursorX >= lineNoW+1 && cursorX < ctx.Width {
		ch := ' '
		line := []rune(e.currentLine())
		if e.col < len(line) {
			ch = line[e.col]
		}
		pad := len(padTab(string(line[:e.col]), 4)) - len(line[:e.col])
		f.Set(cursorX+pad, cursorY, ch, cursorStyle)
	}

	statusY := ctx.Height - 2
	if statusY >= 0 {
		status := fmt.Sprintf(" %s | %d lines | Ln %d, Col %d",
			name+modified, len(e.lines), e.row+1, e.col+1)
		f.Fill(kero.Rect{X: 0, Y: statusY, W: ctx.Width, H: 1}, ' ', statusStyle)
		f.Write(0, statusY, trimToWidth(status, ctx.Width), statusStyle)
	}

	messageY := ctx.Height - 1
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
			e.message = "Ctrl-F find | Ctrl-S save | Ctrl-Q quit"
		}
		f.Write(0, messageY, trimToWidth(" "+e.message, ctx.Width), messageStyle)
	}
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
	e.cmdInput = kero.TextInput{}
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
	style := kero.NewStyle().Foreground(kero.ColorCyan)
	prompt := ">"
	f.Write(0, y, trimToWidth(prompt, width), style)
	inputX := len([]rune(prompt))
	if inputX >= width {
		return
	}
	e.cmdInput.Draw(f, kero.Rect{X: inputX, Y: y, W: width - inputX, H: 1}, style)
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
		// acts like Go To Definition, for example,
		// "goto func myFunction"
		// "goto type myType"
		if len(parts) < 2 {
			e.message = "usage: goto <query> [query2 ...]"
			return nil
		}
		for row, line := range e.lines {
			allMatch := true
			for _, query := range parts[1:] {
				if !strings.Contains(line, query) {
					allMatch = false
					break
				}
			}
			if allMatch {
				e.row = row
				e.col = 0
				return nil
			}
		}
	default:
		e.message = "unknown command: " + cmd
	}
	return nil
}

func (e *Editor) startFind() {
	e.finding = true
	e.findInput = kero.TextInput{}
}

func (e *Editor) updateFind(ev kero.KeyEvent) error {
	switch ev.Key {
	case kero.KeyEnter:
		e.findNext()
		return nil
	case kero.KeyEsc:
		e.finding = false
		// don't clear the query, let ctrl-n use it
		return nil
	}

	e.findInput.Update(ev)
	return nil
}

func (e *Editor) drawFind(f *kero.Frame, y int, width int) {
	normal := kero.NewStyle().Foreground(kero.ColorYellow)
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
	col := e.col
	for row := e.row; ; {
		line := e.lines[row]
		if i := strings.Index(line[col:], query); i >= 0 {
			e.row = row
			e.col = col + i + len(query)
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
	col := e.col
	for row := e.row; ; {
		line := e.lines[row]
		if i := strings.Index(line[:col], query); i >= 0 {
			e.row = row
			e.col = i
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

func (e *Editor) search(query string) [][2]int {
	var matches [][2]int
	if query == "" {
		return matches
	}
	qRunes := []rune(query)
	for row, line := range e.lines {
		lineRunes := []rune(line)
		for i := 0; i+len(qRunes) <= len(lineRunes); i++ {
			match := true
			for j := 0; j < len(qRunes); j++ {
				if lineRunes[i+j] != qRunes[j] {
					match = false
					break
				}
			}
			if match {
				matches = append(matches, [2]int{row, i + len(query)})
			}
		}
	}
	return matches
}

func (e *Editor) insertRune(r rune) {
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
		dstCol := len(padTab(string(e.currentLine()[:e.col]), 4))
		e.row--
		var width int
		for i, char := range e.currentLine() {
			if width >= dstCol {
				e.col = i
				break
			}
			if char == '\t' {
				width += 4 - width%4
			} else {
				width++
			}
		}
		e.clampCol()
	}
}

func (e *Editor) moveDown() {
	if e.row < len(e.lines)-1 {
		dstCol := len(padTab(string(e.currentLine()[:e.col]), 4))
		e.row++
		var width int
		for i, char := range e.currentLine() {
			if width >= dstCol {
				e.col = i
				break
			}
			if char == '\t' {
				width += 4 - width%4
			} else {
				width++
			}
		}
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

func (e *Editor) ensureCursorVisible(ctx *kero.Context) {
	editorH := editorHeight(ctx)
	if e.row < e.rowOffset {
		e.rowOffset = e.row
	}
	if e.row >= e.rowOffset+editorH {
		e.rowOffset = e.row - editorH + 1
	}
	if e.rowOffset < 0 {
		e.rowOffset = 0
	}

	textW := ctx.Width - lineNumberWidth(len(e.lines)) - 1
	if textW < 1 {
		textW = 1
	}
	if e.col < e.colOffset {
		e.colOffset = e.col
	}
	if e.col >= e.colOffset+textW {
		e.colOffset = e.col - textW + 1
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
	f, err := os.OpenFile("/tmp/keroedit.log", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	log.SetOutput(f)
	app := &Editor{}
	if len(os.Args) > 1 {
		app.path = os.Args[1]
	}

	p := kero.New(app, kero.WithAltScreen(true))
	if err := p.Run(); err != nil {
		panic(err)
	}
}

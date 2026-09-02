package main

import (
	"bufio"
	"bytes"
	"fmt"
	"go/format"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cansyan/ke/lsp"
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

// FindWorkspaceDir traverses parent directories looking for project markers,
// returns absolute root path.
func FindWorkspaceDir(filePath string) string {
	dir := filepath.Dir(filePath)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}

	markers := []string{"go.mod", ".git", "go.work"}

	curr := absDir
	for {
		for _, marker := range markers {
			markerPath := filepath.Join(curr, marker)
			if _, err := os.Stat(markerPath); err == nil {
				return curr // Found workspace root
			}
		}

		parent := filepath.Dir(curr)
		if parent == curr {
			break // Reached filesystem root
		}
		curr = parent
	}

	// Fall back to the directory containing the file
	return absDir
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	f, err := os.OpenFile("/tmp/ke.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	log.SetOutput(f)

	var path string
	var row, col int
	if len(os.Args) > 1 {
		path, row, col = parsePathArg(os.Args[1])
	}

	e, err := NewEditor(path, row, col)
	if err != nil {
		fmt.Print(err)
		return
	}
	defer e.Close()

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

	// diags    []*scanner.Error
	// diagChan chan []*scanner.Error

	// reports whether the key comes from a paste action,
	// to distinguish the manual KeyEnter or a pasted \n
	pasting bool

	jumps JumpList

	completion Completion

	renaming    bool
	renameInput TextInput

	ref ReferencesPanel

	lspClient *lsp.Client
	docVers   map[string]int // file path -> version sequence

	// Diagnostics storage for UI rendering: filePath -> diagnostics list
	diagnostics map[string][]lsp.Diagnostic
	mu          sync.RWMutex

	// Debouncer fields
	changeChan chan *Buffer
	stopChan   chan struct{}
}

// NewEditor return a new editor.
// Caller should Close the editor before program exit.
func NewEditor(filePath string, row, col int) (*Editor, error) {
	e := &Editor{
		docVers:     make(map[string]int),
		diagnostics: make(map[string][]lsp.Diagnostic),
	}

	if err := e.OpenFile(filePath); err != nil {
		return nil, err
	}
	e.View().Cursor = e.Buf().Clamp(Position{Row: row, Col: col})
	return e, nil
}

func (e *Editor) startLSP(path string) error {
	if e.lspClient != nil || !isGoFile(path) {
		return nil
	}

	client, err := lsp.StartClient("gopls", e.handleDiagnostics)
	if err != nil {
		return fmt.Errorf("failed to start gopls: %w", err)
	}
	e.lspClient = client

	workspace := FindWorkspaceDir(path)
	client.SendRequest("initialize", lsp.InitializeParams{
		ProcessID: os.Getpid(),
		RootURI:   "file://" + workspace,
	})
	client.SendNotification("initialized", struct{}{})
	e.changeChan = make(chan *Buffer, 100)
	e.stopChan = make(chan struct{})
	go e.StartDebouncer()
	return nil
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
	e.View().SetSize(ctx.Width, ctx.Height)
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
	switch ev := ev.(type) {
	case kero.TickEvent:
		return nil
	case kero.ResizeEvent:
		_, textRect, _, _, _ := LayoutWindow(ev.Width, ev.Height, len(e.Buf().Lines), e.ref.Active)
		for _, v := range e.views {
			v.SetSize(textRect.W, textRect.H)
		}
		return nil
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
	_, textRect, refRect, _, statusBar := LayoutWindow(ctx.Width, ctx.Height, len(e.Buf().Lines), e.ref.Active)
	paletteRect := LayoutPalatte(ctx.Width, ctx.Height, e.palette.VisibleRows()+1)
	point := kero.Point{X: m.X, Y: m.Y}
	switch m.Button {
	case kero.MouseWheelUp:
		p := e.palette
		if p.Active && paletteRect.Contains(point) {
			e.palette.Offset = max(0, p.Offset-1)
			return nil
		}

		if e.ref.Active && refRect.Contains(point) {
			e.ref.ScrollRow = max(0, e.ref.ScrollRow-1)
			return nil
		}

		if textRect.Contains(point) {
			e.View().ScrollRow = max(0, e.View().ScrollRow-1)
			return nil
		}
	case kero.MouseWheelDown:
		p := e.palette
		if p.Active && paletteRect.Contains(point) {
			e.palette.Offset = min(p.Offset+1, len(p.Items)-p.VisibleRows())
			return nil
		}

		if e.ref.Active && refRect.Contains(point) {
			e.ref.ScrollRow = min(e.ref.ScrollRow+1, len(e.ref.Items)-e.ref.VisibleRows())
			return nil
		}

		if textRect.Contains(point) {
			e.View().ScrollRow = min(e.View().ScrollRow+1, len(e.Buf().Lines)-textRect.H)
			return nil
		}
	case kero.MouseLeft:
		switch m.Action {
		case kero.MousePress:
			if e.palette.Active {
				if paletteRect.Contains(point) {
					index := m.Y - paletteRect.Y - 1 + e.palette.Offset
					if index < 0 || index >= len(e.palette.Items) {
						return nil
					}
					action := e.palette.Items[index].Action
					e.palette.Close()
					action(e)
					return nil
				}
				// close palette overlay when lost focus
				e.palette.Close()
			}

			if e.ref.Active {
				if refRect.Contains(point) {
					index := m.Y - refRect.Y - 1 + e.ref.ScrollRow // minus 1 for header
					if index < 0 || index >= len(e.ref.Items) {
						return nil
					}
					e.ref.Index = index
					loc := e.ref.Items[index]
					e.Goto(loc.Path, loc.Row, loc.Col)
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
				if e.completion.Active {
					e.completion.Active = false
				}
				return nil
			}

			// click file name to switch buffer
			if statusBar.Contains(point) {
				var offset int
				for _, v := range e.views {
					name := filenameStatus(e, v)
					width := runewidth.StringWidth(name)
					if offset <= point.X && point.X < offset+width {
						e.OpenFile(v.Buf.Path)
						return nil
					}
					offset += width
				}
				return nil
			}

		case kero.MouseRelease:
			if textRect.Contains(point) {
				switch m.Mod {
				case kero.ModCtrl:
					// ctrl+mouse_left_release goto definition
					e.GotoDefinition()
				case kero.ModAlt:
					// alt+mouse_left_release find references
					e.FindReferences()
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

	defer func() {
		_, textRect, _, _, _ := LayoutWindow(ctx.Width, ctx.Height, len(e.Buf().Lines), e.ref.Active)
		e.View().SetSize(textRect.W, textRect.H)
		e.View().ShowCursorSmart()
	}()

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

	// var completing bool // mark whether showing completion on keystroke
	// defer func() {
	// 	e.completion.Active = completing
	// }()

	v := e.View()
	buf := e.Buf()
	switch key.Key {
	case kero.KeyRune:
		switch key.String() {
		case "ctrl+n":
			e.requestCompletion()
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
		case "ctrl+shift+r":
			e.palette.Open(e, "#")
		case "ctrl+g":
			e.GotoDefinition()
			return nil
		case "ctrl+p":
			e.palette.Open(e, "")
			return nil
		case "ctrl+shift+p":
			e.palette.Open(e, "/")
			return nil
		case "ctrl+[":
			e.GotoPrevDiag()
			return nil
		case "ctrl+]":
			e.GotoNextDiag()
			return nil
		case "ctrl+shift+]", "ctrl+}":
			if len(e.ref.Items) <= 1 {
				return nil
			}
			loc := e.ref.Next()
			e.Goto(loc.Path, loc.Row, loc.Col)
		case "ctrl+shift+[", "ctrl+{":
			if len(e.ref.Items) <= 1 {
				return nil
			}
			loc := e.ref.Prev()
			e.Goto(loc.Path, loc.Row, loc.Col)
		case "ctrl+s":
			if err := e.SaveFile(); err != nil {
				e.message = err.Error()
				return err
			}
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

		// LSP Completion requires latest buffer,
		// so NotifyBufferChanged will be called and copys the whole buffer(not incremental update yet).
		// For efficiency, do not trigger completion on every keystroke.
		if e.completion.Active {
			e.requestCompletion()
		}
	case kero.KeyEnter:
		if e.completion.Active {
			e.applyCompletion()
			return nil
		}

		if e.hasSelect() {
			e.deleteSelect()
		}

		if e.pasting {
			v.Cursor = buf.Insert(v.Cursor, "\n")
			return nil
		}

		// calculate indentation
		indent := func(line []byte, col int) string {
			if len(line) == 0 || col == 0 {
				return ""
			}
			if col > len(line) {
				col = len(line)
			}
			// get indentation before the column, advancing through the prefix byte by byte.
			prefix := line[:col]
			var n int
			for n < len(prefix) {
				r, size := utf8.DecodeRune(prefix[n:])
				if !unicode.IsSpace(r) {
					break
				}
				n += size
			}
			indent := string(prefix[:n])
			// indent on block start
			if col == len(line) && len(line) > 0 && line[len(line)-1] == '{' {
				indent += "\t"
			}
			return indent
		}
		// newline retain the previous line's indentation
		switch key.String() {
		case "ctrl+enter":
			// insert newline below
			line := buf.Lines[v.Cursor.Row]
			indent := indent(line, len(line))
			p := Position{Row: v.Cursor.Row, Col: len(line)}
			v.Cursor = buf.Insert(p, "\n"+indent)
		case "shift+enter":
			// insert newline above
			var prevIndent string
			if v.Cursor.Row > 0 {
				prevLine := buf.Lines[v.Cursor.Row-1]
				prevIndent = indent(prevLine, len(prevLine))
			}
			buf.Insert(Position{Row: v.Cursor.Row, Col: 0}, "\n")
			v.Cursor = buf.Insert(Position{Row: v.Cursor.Row, Col: 0}, prevIndent)
		default:
			// insert newline under cursor
			line := buf.Lines[v.Cursor.Row]
			indent := indent(line, v.Cursor.Col)
			v.Cursor = buf.Insert(v.Cursor, "\n"+string(indent))
		}
		e.markDirty()
	case kero.KeyTab:
		if e.completion.Active {
			e.applyCompletion()
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
			if e.completion.Active {
				e.requestCompletion()
			}
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
		if e.completion.Active {
			e.requestCompletion()
		}
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
func LayoutWindow(totalWidth, totalHeight, lineCount int, showBottomPanel bool) (gutterRect, textRect, bottomPanelRect, msgRect, statusRect kero.Rect) {
	remaining := kero.Rect{W: totalWidth, H: totalHeight}

	remaining, statusRect = kero.SplitHorizontal(remaining, remaining.H-1)
	if !showBottomPanel {
		remaining, msgRect = kero.SplitHorizontal(remaining, remaining.H-1)
	} else {
		remaining, bottomPanelRect = kero.SplitHorizontal(remaining, remaining.H-10)
	}

	gutterWidth := gutterWidth(lineCount)
	gutterRect, textRect = kero.SplitVertical(remaining, gutterWidth)

	return gutterRect, textRect, bottomPanelRect, msgRect, statusRect
}

func LayoutPalatte(totalWidth, totalHeight, paletteHeight int) kero.Rect {
	rect := kero.Rect{X: (totalWidth - 60) / 2, Y: 2, W: 60, H: paletteHeight}
	if rect.X <= 0 {
		rect.X = totalWidth / 4
		rect.W = totalWidth / 2
	}
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
	gutterRect, textRect, bottomPanelRect, msgRect, statusRect := LayoutWindow(fSize.Width, fSize.Height, len(v.Buf.Lines), e.ref.Active)

	tabWidth := 4
	gutterMap, lineDiags := v.GetVisibleDiagnostics(e.diagnostics[e.Buf().Path], tabWidth)

	// 1. Draw Gutter Area
	for i := range gutterRect.H {
		lineIdx := v.ScrollRow + i
		y := gutterRect.Y + i

		if lineIdx >= len(v.Buf.Lines) {
			f.Write(gutterRect.X, y, "~", gutterStyle)
			continue
		}

		if _, ok := gutterMap[i]; ok {
			red := kero.NewStyle().Foreground(kero.ColorRed)
			f.Set(gutterRect.X, y, 'x', red)
		}

		gutterText := fmt.Sprintf("%*d ", gutterRect.W-2, lineIdx+1)
		if lineIdx == v.Cursor.Row {
			f.Write(gutterRect.X+1, y, gutterText, gutterActiveStyle)
		} else {
			f.Write(gutterRect.X+1, y, gutterText, gutterStyle)
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

		var diag *LineDiagnostic
		if diags, ok := lineDiags[i]; ok && len(diags) > 0 {
			diag = &diags[0]
		}

		// draw the line
		var width int
		for j, r := range visPadded {
			charStyle := textStyle
			if diag != nil && diag.StartCol <= j && j < diag.EndCol {
				charStyle = textStyle.Underline()
			}
			f.Set(textRect.X+width, y, r, charStyle)
			width += runewidth.RuneWidth(r)
		}

		// draw inline diagnostic message
		if diag != nil {
			red := kero.NewStyle().Foreground(kero.ColorRed)
			dx := max(textRect.X+runewidth.StringWidth(visPadded), fSize.Width-len(diag.Message))
			f.Write(dx, y, diag.Message, red)
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

	// Draw status bar
	if statusRect.H > 0 {
		f.Fill(statusRect, ' ', statusStyle)

		var offset int
		for i, v := range e.views {
			name := filenameStatus(e, v)
			style := statusStyle
			if i == e.active && len(e.views) > 1 {
				style = statusStyle.Bold()
			}
			f.Write(statusRect.X+offset, statusRect.Y, name, style)
			offset += runewidth.StringWidth(name)
		}

		status := fmt.Sprintf("| Line %d, Col %d", v.Cursor.Row+1, cursorVisCol+1)
		f.Write(statusRect.X+offset, statusRect.Y, trimToWidth(status, ctx.Width), statusStyle)
		offset += runewidth.StringWidth(status)
		if s := e.LastEvent(); s != "" {
			f.Write(fSize.Width-runewidth.StringWidth(s), statusRect.Y, s, statusStyle)
		}
		// show diagnostic count, ignored warning
		if diags, ok := e.diagnostics[e.Buf().Path]; ok {
			var n int
			for _, d := range diags {
				if d.Severity == lsp.DiagnosticSeverityError {
					n++
				}
			}
			if n > 0 {
				msg := fmt.Sprintf("%d error", n)
				eventWidth := runewidth.StringWidth(e.LastEvent())
				remainWidth := fSize.Width - offset - eventWidth
				if remainWidth > 0 {
					// the style is identical to status bar, avoid distracting
					f.Write(statusRect.X+offset+1, statusRect.Y, trimToWidth("| "+msg, remainWidth), statusStyle)
				}
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

	if e.ref.Active {
		e.drawReferences(f, bottomPanelRect)
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

func (e *Editor) SaveFile() error {
	buf := e.Buf()
	if buf == nil {
		return nil
	}
	if buf.Path == "" {
		e.startSaveAs()
		return nil
	}

	// 1. Format in-memory buffer if it's a Go file
	if isGoFile(buf.Path) {
		changed, err := buf.Format()
		if err != nil {
			log.Printf("failed to format: %s", err)
		} else if changed {
			e.View().Cursor = buf.Clamp(e.View().Cursor)
		}
	}

	// 2. Stream buffer content to disk
	f, err := os.Create(buf.Path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	if _, err := io.Copy(w, buf.NewReader()); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}

	buf.Dirty = false
	e.message = fmt.Sprintf("saved %s", buf.Path)

	// 3. Notify LSP server if active
	if e.lspClient != nil {
		e.lspClient.SendNotification("textDocument/didSave", lsp.DidSaveTextDocumentParams{
			TextDocument: lsp.TextDocumentIdentifier{
				URI: pathToURI(buf.Path),
			},
		})
	}

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
	if err := e.SaveFile(); err != nil {
		return err
	}
	// e.diagnose()
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

	// Non-blocking send: pushes buffer state to debouncer
	select {
	case e.changeChan <- e.Buf():
	default:
		// Queue full; safe to drop because the latest pointer will be picked up
	}
}

// Goto records jump history before navigating.
func (e *Editor) Goto(path string, row, col int) error {
	e.recordJump()
	return e.jumpTo(Location{Path: path, Row: row, Col: col})
}

func (e *Editor) GotoLSPLocation(l lsp.Location) error {
	e.recordJump()

	v := e.View()
	if v == nil {
		return nil
	}

	path := uriToPath(l.URI)
	// 1. Switch buffer if needed (normalize paths in production if necessary)
	if path != "" && path != e.Buf().Path {
		if err := e.OpenFile(path); err != nil {
			return err
		}
	}

	buf := e.Buf()
	if buf == nil {
		return nil
	}

	row := l.Range.Start.Line
	col := lsp.CharToByteOffset(buf.Lines[row], l.Range.Start.Character)
	return e.jumpTo(Location{Path: path, Row: row, Col: col})
}

func (e *Editor) GotoPrevDiag() {
	diags, ok := e.diagnostics[e.Buf().Path]
	if !ok {
		return
	}
	if len(diags) == 0 {
		return
	}

	v := e.View()
	var prev lsp.Diagnostic
	for _, d := range slices.Backward(diags) {
		dRow := d.Range.Start.Line
		dCol := lsp.CharToByteOffset(v.Buf.Lines[dRow], d.Range.Start.Character)
		if dRow < v.Cursor.Row || (dRow == v.Cursor.Row && dCol < v.Cursor.Col) {
			prev = d
			break
		}
	}
	if prev.Message == "" {
		prev = diags[len(diags)-1]
	}
	dRow := prev.Range.Start.Line
	dCol := lsp.CharToByteOffset(v.Buf.Lines[dRow], prev.Range.Start.Character)
	e.Goto(v.Buf.Path, dRow, dCol)
}

func (e *Editor) GotoNextDiag() {
	diags, ok := e.diagnostics[e.Buf().Path]
	if !ok {
		return
	}
	if len(diags) == 0 {
		return
	}

	v := e.View()
	var next lsp.Diagnostic
	for _, d := range diags {
		dRow := d.Range.Start.Line
		dCol := lsp.CharToByteOffset(v.Buf.Lines[dRow], d.Range.Start.Character)
		if dRow > v.Cursor.Row || (dRow == v.Cursor.Row && dCol > v.Cursor.Col) {
			next = d
			break
		}
	}
	if next.Message == "" {
		next = diags[0]
	}
	dRow := next.Range.Start.Line
	dCol := lsp.CharToByteOffset(v.Buf.Lines[dRow], next.Range.Start.Character)
	e.Goto(v.Buf.Path, dRow, dCol)
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
	// Center vertically
	v.ScrollRow = max(v.Cursor.Row-(v.Height/2), 0)

	// Center horizontally
	// vCol := v.Buf.ByteToVisualCol(v.Cursor, tabWidth)
	// v.ScrollCol = max(vCol-(v.Width/2), 0)

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

// cursorVisible returns true if the cursor is currently inside the visible viewport.
func (v *View) cursorVisible() bool {
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
	if v.cursorVisible() {
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

// OpenFile loads a file into memory or focuses it if already loaded.
// TODO: consider return a Buffer pointer for further operations
func (e *Editor) OpenFile(path string) error {
	if path != "" {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		path = absPath
	}
	if err := e.startLSP(path); err != nil {
		return err
	}

	// switch to existing buffer
	for i, v := range e.views {
		if v.Buf.Path == path {
			e.active = i
			return nil
		}
	}

	buf, err := BufferFromFile(path)
	if err != nil {
		return err
	}

	newView := &View{Buf: buf}
	if e.View() != nil {
		newView.Width = e.View().Width
		newView.Height = e.View().Height
	}
	e.views = append(e.views, newView)
	e.active = len(e.views) - 1
	e.NotifyBufferOpened(buf)
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
	e.message = ""
}

func (e *Editor) NextBuffer() {
	if len(e.views) > 1 {
		e.active = (e.active + 1) % len(e.views)
	}
}

func (e *Editor) PrevBuffer() {
	if len(e.views) > 1 {
		e.active = (e.active - 1 + len(e.views)) % len(e.views)
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
		if curr.Path == path && curr.Row == pos.Row && curr.Col == pos.Col {
			return
		}
	}

	// Truncate forward history if we jump from somewhere in the middle
	if j.index < len(j.items)-1 {
		j.items = j.items[:j.index+1]
	}

	j.items = append(j.items, Location{Path: path, Row: pos.Row, Col: pos.Col})
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

// jumpTo handles the core logic of switching buffers, updating cursor,
// clamping positions, and updating the viewport.
// It is used by JumpBack and JumpForward.
func (e *Editor) jumpTo(target Location) error {
	v := e.View()
	if v == nil {
		return nil
	}

	// 1. Switch buffer if needed (normalize paths in production if necessary)
	if target.Path != "" && target.Path != e.Buf().Path {
		if err := e.OpenFile(target.Path); err != nil {
			return err
		}
	}

	buf := e.Buf()
	if buf == nil {
		return nil
	}

	// 2. Safely clamp position to valid buffer bounds
	v.Cursor = buf.Clamp(Position{Row: target.Row, Col: target.Col})

	// 3. Clear active selection on jump to prevent state leakage
	v.Selecting = false

	// 4. Ensure view updates to show new cursor position
	v.ShowCursorSmart()

	return nil
}

// JumpBack moves to the previous position in jump history.
func (e *Editor) JumpBack() {
	if e.Buf() == nil {
		return
	}
	if target, ok := e.jumps.Back(e.Buf().Path, e.View().Cursor); ok {
		if err := e.jumpTo(target); err != nil {
			log.Print(err)
			e.message = err.Error()
		}
	}
}

// JumpForward moves to the next position in jump history.
func (e *Editor) JumpForward() {
	if e.Buf() == nil {
		return
	}
	if target, ok := e.jumps.Forward(); ok {
		if err := e.jumpTo(target); err != nil {
			log.Print(err)
			e.message = err.Error()
		}
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
	symbols []lsp.DocumentSymbol
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
	case strings.HasPrefix(input, "#"):
		p.Items = p.workspaceSymbolItems(e, strings.TrimPrefix(input, "#"))
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
	if e.Buf() == nil || e.lspClient == nil {
		return nil
	}

	lowerQuery := strings.ToLower(query)
	queries := strings.Split(lowerQuery, ".")
	if len(queries) == 1 {
		queries = strings.Split(lowerQuery, " ")
	}

	if p.symbols == nil {
		symbols, err := e.lspClient.DocumentSymbols(pathToURI(e.Buf().Path))
		if err != nil {
			log.Printf("failed to fetch document symbols: %v", err)
			return nil
		}
		p.symbols = symbols
	}
	var items []PaletteItem

	for _, sym := range p.symbols {
		if query != "" {
			match := true
			for _, q := range queries {
				if !strings.Contains(strings.ToLower(sym.Name), q) {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}

		path := e.Buf().Path
		row := sym.Range.Start.Line
		col := lsp.CharToByteOffset(e.Buf().Lines[row], sym.Range.Start.Character)
		items = append(items, PaletteItem{
			Label:  sym.Name,
			Detail: sym.Kind.String(),
			Action: func(ed *Editor) {
				ed.Goto(path, row, col)
			},
		})
	}
	return items
}

func (p *Palette) workspaceSymbolItems(e *Editor, query string) []PaletteItem {
	if e.Buf() == nil || e.lspClient == nil {
		return nil
	}

	symbols, err := e.lspClient.WorkspaceSymbols(query)
	if err != nil {
		log.Printf("failed to fetch workspace symbols: %v", err)
		return nil
	}

	var items []PaletteItem
	for _, sym := range symbols {
		items = append(items, PaletteItem{
			Label:  sym.Name,
			Detail: sym.Kind.String(),
			Action: func(ed *Editor) {
				ed.GotoLSPLocation(sym.Location)
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
			// e.diagnose()
		}},
		{"LSP: find references", "", func(e *Editor) {
			e.FindReferences()
		}},
		{"LSP: goto definition", "ctrl+g", func(e *Editor) {
			e.GotoDefinition()
		}},
		{"LSP: next diagnostic", "ctrl+]", func(e *Editor) {
			e.GotoNextDiag()
		}},
		{"LSP: prev diagnostic", "ctrl+]", func(e *Editor) {
			e.GotoPrevDiag()
		}},
		{"LSP: goto symbol", "ctrl+r", func(e *Editor) {
			e.palette.Open(e, "@")
		}},
		{"LSP: goto symbol in workspace", "ctrl+shift+r", func(e *Editor) {
			e.palette.Open(e, "#")
		}},
		{"LSP: rename symbol", "", func(e *Editor) {
			if !isGoFile(e.Buf().Path) {
				return
			}
			e.startRename()
		}},
		{"toggle reference", "", func(e *Editor) {
			e.ref.Active = !e.ref.Active
		}},
		{"next reference", "ctrl+shift+]", func(e *Editor) {
			if len(e.ref.Items) <= 1 {
				return
			}
			loc := e.ref.Next()
			e.Goto(loc.Path, loc.Row, loc.Col)
		}},
		{"prev reference", "ctrl+shift+[", func(e *Editor) {
			if len(e.ref.Items) <= 1 {
				return
			}
			loc := e.ref.Prev()
			e.Goto(loc.Path, loc.Row, loc.Col)
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
				// ed.diagnose()
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

func (e *Editor) requestCompletion() {
	v := e.View()
	if v == nil || v.Buf == nil || e.lspClient == nil || !isGoFile(v.Buf.Path) {
		return
	}
	if v.Buf.Path != "" && v.Buf.Dirty {
		// gopls completion must operate on the latest document content. The per-key
		// didChange notifications are debounced, so trigger the completion request
		// after syncing the current buffer text to the server.
		e.NotifyBufferChanged(v.Buf)
	}

	charOffset := lsp.CharFromByteOffset(v.Buf.Lines[v.Cursor.Row], v.Cursor.Col)
	items, err := e.lspClient.Completion(pathToURI(v.Buf.Path), v.Cursor.Row, charOffset)
	if err != nil {
		e.completion.Items = nil
		e.completion.Active = false
		log.Print(err)
		return
	}
	e.completion.Items = items
	e.completion.Index = 0
	e.completion.Active = len(items) > 0
}

func (e *Editor) applyCompletion() {
	c := e.completion
	if len(c.Items) == 0 || c.Index < 0 || c.Index >= len(c.Items) {
		return
	}

	v := e.View()
	if v == nil || v.Buf == nil {
		return
	}

	item := c.Items[c.Index]
	switch {
	case item.TextEdit != nil:
		v.Cursor = v.Buf.ApplyTextEdit(*item.TextEdit)
	case item.InsertText != "":
		v.Cursor = v.Buf.Insert(v.Cursor, item.InsertText)
	case item.Label != "":
		v.Cursor = v.Buf.Insert(v.Cursor, item.Label)
	default:
		e.completion.Active = false
		return
	}

	e.markDirty()
	e.completion.Active = false
}

type Completion struct {
	Active bool
	Index  int
	Items  []lsp.CompletionItem
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

	_, textRect, _, _, _ := LayoutWindow(f.Size().Width, f.Size().Height, len(e.Buf().Lines), e.ref.Active)

	c := e.completion
	visibleRows := min(len(c.Items), 10)
	maxWidth := 45

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
	if rect.Y < 0 {
		rect.Y = 0
	}
	f.Fill(rect, ' ', normal.Reverse())
	for i := range visibleRows {
		x := rect.X
		y := rect.Y + i
		item := c.Items[i+offset]
		style := normal.Reverse()
		prefix := " "
		// it is unnecessary to show indicator for only 1 item
		if i+offset == c.Index && len(c.Items) > 1 {
			prefix = ">"
			style = normal.Reverse().Bold()
		}
		label := prefix + c.Items[i+offset].Label
		f.Write(rect.X, y, label, style)

		// right side text
		if item.Detail != "" && i+offset == c.Index {
			x += runewidth.StringWidth(label)
			remaining := rect.W - runewidth.StringWidth(label) - 3
			dx := max(rect.Right()-runewidth.StringWidth(item.Detail), x+3)
			f.Write(dx, y, runewidth.Truncate(item.Detail, remaining, ""), style)
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
		err := e.Rename(newName)
		if err != nil {
			log.Print(err)
			e.message = err.Error()
		}
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

func (e *Editor) GotoDefinition() error {
	v := e.View()
	if v == nil || v.Buf == nil || !isGoFile(v.Buf.Path) || e.lspClient == nil {
		return nil
	}

	buf := v.Buf
	cursor := v.Cursor

	if cursor.Row >= len(buf.Lines) {
		return nil
	}

	lineBytes := buf.Lines[cursor.Row]
	lspChar := lsp.CharFromByteOffset(lineBytes, cursor.Col)
	uri := pathToURI(buf.Path)

	// Call gopls
	locs, err := e.lspClient.GotoDefinition(uri, cursor.Row, lspChar)
	if err != nil {
		e.message = "Goto definition failed: " + err.Error()
		return err
	}

	if len(locs) == 0 {
		e.message = "No definition found"
		return nil
	}

	// Jump to the first resolved location
	target := locs[0]
	e.GotoLSPLocation(target)
	return nil
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
// It is similar to lsp.Location, but converted to a more editor-friendly
// format with byte offsets instead of LSP character positions.
// It is used for jump history, references, and other navigation features.
type Location struct {
	Path string
	Row  int // line index (0-based)
	Col  int // byte offset within line (0-based)
}

func (e *Editor) FindReferences() error {
	v := e.View()
	if v == nil || v.Buf == nil || e.lspClient == nil {
		return nil
	}

	buf := v.Buf
	cursor := v.Cursor

	if cursor.Row >= len(buf.Lines) {
		return nil
	}

	lineBytes := buf.Lines[cursor.Row]
	lspChar := lsp.CharFromByteOffset(lineBytes, cursor.Col)
	uri := pathToURI(buf.Path)

	// Query gopls (include declaration = true)
	locs, err := e.lspClient.FindReferences(uri, cursor.Row, lspChar, true)
	if err != nil {
		e.message = "Find references failed: " + err.Error()
		return err
	}

	if len(locs) == 0 {
		e.message = "No references found"
		return nil
	}

	// Multiple references
	locations := make([]Location, 0, len(locs))
	for _, l := range locs {
		locations = append(locations, Location{
			Path: uriToPath(l.URI),
			Row:  l.Range.Start.Line,
			Col:  lsp.CharToByteOffset(buf.Lines[l.Range.Start.Line], l.Range.Start.Character),
		})
	}
	start, end := buf.WordBounds(e.View().Cursor)
	header := fmt.Sprintf("%d references for %q", len(locations), buf.TextRange(start, end))
	e.ref = NewReferencesPanel(header, locations)
	return nil
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

		buf, err := getBuffer(item.Path)
		if err != nil {
			log.Print(err)
			continue
		}

		line := string(buf.Lines[item.Row])
		text := fmt.Sprintf("%s %s:%d:%d: %s", indicator, filepath.Base(item.Path), item.Row+1,
			item.Col+1, line)
		runes := []rune(text)

		if len(runes) > rect.W {
			text = string(runes[:rect.W])
		}
		f.Write(rect.X+1, lineY, text, itemStyle)
	}
}

// Convert absolute filepath to file:// URI
func pathToURI(path string) string {
	abs, _ := filepath.Abs(path)
	return "file://" + abs
}

// Notify LSP when a buffer is opened
func (e *Editor) NotifyBufferOpened(buf *Buffer) {
	if e.lspClient == nil || buf == nil || !isGoFile(buf.Path) {
		return
	}
	uri := pathToURI(buf.Path)
	e.docVers[buf.Path] = 1

	content := string(bytes.Join(buf.Lines, []byte{'\n'}))
	e.lspClient.SendNotification("textDocument/didOpen", lsp.DidOpenTextDocumentParams{
		TextDocument: lsp.TextDocumentItem{
			URI:        uri,
			LanguageID: "go",
			Version:    1,
			Text:       content,
		},
	})
}

// Notify LSP as the user edits (Call from your debouncer or OnContentChanged).
// Sending full document updates on didChange is simple, fast, and robust
// for files under a few thousand lines.
// gopls handles full updates rapidly without needing complex incremental edit diffing
func (e *Editor) NotifyBufferChanged(buf *Buffer) {
	if e.lspClient == nil || buf == nil || !isGoFile(buf.Path) {
		return
	}
	uri := pathToURI(buf.Path)
	e.docVers[buf.Path]++
	version := e.docVers[buf.Path]

	// Create a safe snapshot of current buffer content
	content := string(bytes.Join(buf.Lines, []byte{'\n'}))

	e.lspClient.SendNotification("textDocument/didChange", lsp.DidChangeTextDocumentParams{
		TextDocument: lsp.VersionedTextDocumentIdentifier{
			URI:     uri,
			Version: version,
		},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{
			{Text: content},
		},
	})
}

// Async callback triggered by readLoop when gopls pushes diagnostics
func (e *Editor) handleDiagnostics(uri string, diags []lsp.Diagnostic) {
	// Convert URI back to file path if needed
	filePath := uriToPath(uri)

	// Post event to TUI thread or protect map with a RWMutex
	e.mu.Lock()
	e.diagnostics[filePath] = diags
	e.mu.Unlock()

	// for _, d := range diags {
	// 	log.Printf("%s:%d severity:%d %s", filePath, d.Range.Start.Line, d.Severity, d.Message)
	// }
}

// StartDebouncer launches the background worker goroutine.
func (e *Editor) StartDebouncer() {
	go func() {
		var (
			timer   *time.Timer
			timerCh <-chan time.Time
			lastBuf *Buffer
		)

		for {
			select {
			case buf := <-e.changeChan:
				lastBuf = buf

				// Stop active timer if user typed another character
				if timer != nil {
					timer.Stop()
				}
				// 150ms debounce delay (optimal for instant feel without flooding)
				timer = time.NewTimer(150 * time.Millisecond)
				timerCh = timer.C

			case <-timerCh:
				if lastBuf != nil {
					e.NotifyBufferChanged(lastBuf)
					lastBuf = nil
				}
				timerCh = nil

			case <-e.stopChan:
				if timer != nil {
					timer.Stop()
				}
				return
			}
		}
	}()
}

// Close shuts down the background worker cleanly.
func (e *Editor) Close() {
	if e.stopChan != nil {
		close(e.stopChan)
	}
	if e.lspClient != nil {
		e.lspClient.Close()
	}
}

// LineDiagnostic holds rendered positions bounded to a single line.
type LineDiagnostic struct {
	StartCol int // 0-based visual cell offset on line
	EndCol   int // 0-based visual cell offset on line
	Severity int // 1: Error, 2: Warning, 3: Info, 4: Hint
	Message  string
}

// ByteOffsetToVisualCol converts byte offset on a line to visual display cells (handling tabs).
func ByteOffsetToVisualCol(line []byte, byteOffset int, tabWidth int) int {
	col := 0
	currByte := 0

	for currByte < byteOffset && currByte < len(line) {
		r, size := utf8.DecodeRune(line[currByte:])
		if r == '\t' {
			col += tabWidth - (col % tabWidth)
		} else {
			col++ // Assuming standard 1-cell width (use wcwidth for full CJK/Emoji support)
		}
		currByte += size
	}
	return col
}

// GetVisibleDiagnostics maps buffer diagnostics into visible viewport line & column ranges.
func (v *View) GetVisibleDiagnostics(diags []lsp.Diagnostic, tabWidth int) (map[int]int, map[int][]LineDiagnostic) {
	// Gutter indicators: viewportRow -> highest severity (1 is Error, 2 is Warning)
	gutterMap := make(map[int]int)
	// Text underlines: viewportRow -> list of column ranges
	underlineMap := make(map[int][]LineDiagnostic)

	if v.Buf == nil || len(diags) == 0 {
		return gutterMap, underlineMap
	}

	viewStartRow := v.ScrollRow
	viewEndRow := v.ScrollRow + v.Height

	for _, d := range diags {
		diagStartRow := d.Range.Start.Line
		diagEndRow := d.Range.End.Line

		// Skip diagnostics completely outside visible viewport
		if diagEndRow < viewStartRow || diagStartRow >= viewEndRow {
			continue
		}

		// 1. Process Gutter Indicators for all affected lines in viewport
		for r := diagStartRow; r <= diagEndRow; r++ {
			if r >= viewStartRow && r < viewEndRow {
				vRow := r - viewStartRow
				existingSev, found := gutterMap[vRow]
				// Higher priority to lower severity numbers (1 = Error)
				if !found || d.Severity < existingSev {
					gutterMap[vRow] = d.Severity
				}
			}
		}

		// 2. Process Underline Highlights line by line
		for r := diagStartRow; r <= diagEndRow; r++ {
			if r < viewStartRow || r >= viewEndRow || r >= len(v.Buf.Lines) {
				continue
			}

			lineBytes := v.Buf.Lines[r]
			var startByte, endByte int

			if r == diagStartRow {
				startByte = lsp.CharToByteOffset(lineBytes, d.Range.Start.Character)
			} else {
				startByte = 0
			}

			if r == diagEndRow {
				endByte = lsp.CharToByteOffset(lineBytes, d.Range.End.Character)
			} else {
				endByte = len(lineBytes)
			}

			// Handle single-character zero-length ranges from LSP (e.g., missing semicolons)
			if startByte == endByte {
				if endByte < len(lineBytes) {
					_, size := utf8.DecodeRune(lineBytes[endByte:])
					endByte += size
				} else {
					endByte = startByte + 1
				}
			}

			startCol := ByteOffsetToVisualCol(lineBytes, startByte, tabWidth)
			endCol := ByteOffsetToVisualCol(lineBytes, endByte, tabWidth)

			// Map to viewport visual columns
			vStartCol := startCol - v.ScrollCol
			vEndCol := endCol - v.ScrollCol

			// Clip to viewport horizontal boundaries
			if vEndCol > 0 && vStartCol < v.Width {
				if vStartCol < 0 {
					vStartCol = 0
				}
				if vEndCol > v.Width {
					vEndCol = v.Width
				}

				vRow := r - viewStartRow
				underlineMap[vRow] = append(underlineMap[vRow], LineDiagnostic{
					StartCol: vStartCol,
					EndCol:   vEndCol,
					Severity: d.Severity,
					Message:  d.Message,
				})
			}
		}
	}

	return gutterMap, underlineMap
}

// uriToPath converts a file:// URI into a clean, platform-native file path.
func uriToPath(uri string) string {
	// If it's already a local filepath, clean and return it
	if !strings.HasPrefix(uri, "file://") {
		return filepath.Clean(uri)
	}

	u, err := url.Parse(uri)
	if err != nil {
		// Fallback for malformed URIs
		trimmed := strings.TrimPrefix(uri, "file://")
		decoded, err := url.PathUnescape(trimmed)
		if err != nil {
			return filepath.Clean(trimmed)
		}
		return filepath.Clean(decoded)
	}

	// Unescape percent-encoded sequences (e.g. %20 -> space)
	path := u.Path

	// Windows URI normalization:
	// "file:///C:/path/file.go" parsed by url.Parse yields Path "/C:/path/file.go"
	if runtime.GOOS == "windows" {
		if len(path) > 2 && path[0] == '/' && path[2] == ':' {
			path = path[1:] // Strip leading slash: "C:/path/file.go"
		}
		path = strings.ReplaceAll(path, "/", `\`)
	}

	return filepath.Clean(path)
}

// Format runs go/format on the buffer's content if it is a Go source file.
// It returns true if the buffer was modified, and an error if formatting fails.
func (b *Buffer) Format() (bool, error) {
	src := bytes.Join(b.Lines, []byte{'\n'})

	formatted, err := format.Source(src)
	if err != nil {
		return false, err // Return syntax/format error without modifying buffer
	}

	// if no changes, return early
	if bytes.Equal(src, formatted) {
		return false, nil
	}

	// standard gofmt always formats source with a trailing newline,
	// so we can safely replace the buffer content.
	newLines := bytes.Split(formatted, []byte{'\n'})

	b.Lines = newLines
	b.Dirty = true
	return true, nil
}

func (e *Editor) Rename(newName string) error {
	v := e.View()
	if v == nil || v.Buf == nil || e.lspClient == nil || newName == "" {
		return nil
	}

	buf := v.Buf
	cursor := v.Cursor

	if cursor.Row >= len(buf.Lines) {
		return nil
	}

	lspChar := lsp.CharFromByteOffset(buf.Lines[cursor.Row], cursor.Col)
	uri := pathToURI(buf.Path)

	e.message = "Renaming symbol..."

	// Request workspace edits from gopls
	workspaceEdit, err := e.lspClient.Rename(uri, cursor.Row, lspChar, newName)
	if err != nil {
		log.Print(err)
		e.message = "Rename failed: " + err.Error()
		return err
	}

	if workspaceEdit == nil || (len(workspaceEdit.Changes) == 0 && len(workspaceEdit.DocumentChanges) == 0) {
		e.message = "No symbol found to rename"
		return nil
	}

	// Normalizing changes from both WorkspaceEdit representations
	editsPerFile := make(map[string][]lsp.TextEdit)

	// 1. Direct changes map
	for uri, edits := range workspaceEdit.Changes {
		editsPerFile[uri] = append(editsPerFile[uri], edits...)
	}

	// 2. DocumentChanges array (preferred by modern gopls)
	for _, docEdit := range workspaceEdit.DocumentChanges {
		uri := docEdit.TextDocument.URI
		editsPerFile[uri] = append(editsPerFile[uri], docEdit.Edits...)
	}

	totalEdits := 0
	affectedFiles := 0

	// Apply edits across all files returned by gopls
	for fileURI, edits := range editsPerFile {
		filePath := uriToPath(fileURI)
		err := e.OpenFile(filePath)
		if err != nil {
			continue
		}
		targetBuf := e.Buf()

		// Apply edits in-memory
		targetBuf.ApplyTextEdits(edits)

		// Notify LSP server of changed buffer content
		e.NotifyBufferChanged(targetBuf)

		totalEdits += len(edits)
		affectedFiles++

		// TODO: consider to clamp the cursor, save the file after applying edits
		e.View().Cursor = e.Buf().Clamp(e.View().Cursor)
	}

	// Clamp current view cursor in case active line shrank
	v.Cursor = buf.Clamp(v.Cursor)

	// come back to the original buffer after renaming
	if err := e.OpenFile(buf.Path); err != nil {
		return err
	}

	e.message = fmt.Sprintf("Renamed symbol: applied %d edits across %d file(s)", totalEdits, affectedFiles)
	return nil
}

func filenameStatus(e *Editor, v *View) string {
	if e == nil || v == nil {
		return ""
	}

	var sb strings.Builder
	if len(e.views) > 1 && e.views[e.active] == v {
		sb.WriteByte('[')
	} else {
		sb.WriteByte(' ')
	}
	if v.Buf.Path != "" {
		sb.WriteString(filepath.Base(v.Buf.Path))
	} else {
		sb.WriteString("untitled")
	}
	if v.Buf.Dirty {
		sb.WriteByte('*')
	}
	if len(e.views) > 1 && e.views[e.active] == v {
		sb.WriteByte(']')
	} else {
		sb.WriteByte(' ')
	}
	return sb.String()
}

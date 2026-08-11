Key bindings:
- Ctrl+S save
- Ctrl+Q quit
- Ctrl+F find
- Ctrl+R toggle replace while finding
	- Enter replace current match and move to the next match
	- Ctrl+Enter replace all matches
	- Tab skip current match and move to the next match
- Shift+Up/Down/Left/Right start selection
	- Ctrl+D select word under cursor
	- Ctrl+L expand selection to line
- Ctrl+C copy
- Ctrl+X cut
- Ctrl+V paste
- Ctrl+G goto definition
- Ctrl+P command palette
	- `:123` goto line 123
	- `@filter symbol` goto symbol, for example `@func main`, `@type Editor`
    - `>dnext` goto next diagnostic error
    - `>dprev` goto previous diagnostic error
- Ctrl+Backspace delete to line start
- Ctrl+K delete to line end
- Ctrl+Enter insert line below
- Ctrl+A or Home move cursor to line start
- Ctrl+E or End move cursor to line end
- Ctrl+] goto next diagnostic
- Alt+Left move cursor to start of current/previous word
- Alt+Right move cursor to end of current/next word
- Alt+Backspace delete word backwards
- Esc cancel

With Kitty keyboard protocol enabled, Kitty terminal reports Command key:
- Cmd+Up move cursor to file start
- Cmd+Down move cursor to find end
- Cmd+Left move cursor to line start
- Cmd+Right move cursor to line end
- Cmd+Backspace delete to line start

Design Choices:
- ctrl+[ is identical to ESC in almost all terminal, can't use it for navigating diagnostic
- future key chords, Ctrl+K or Ctrl+X as leader key are good, but both taken. 
- Ctrl+Shift+K could a shortcut for deleting the whole line, but Ctrl+L then Delete do the same thing.
- keep things tiny, avoid duplicate selection, mutilple cursor, undo/redo, jump back/forward.

TODO:
[ ] CheckGoSyntax generates AST, can help Goto Definition
[ ] Since Buffer seperated, with a bit changes, Editor can deal with mutiple files.
	[ ] Ctrl+P open files, Ctrl+Shift+P open command palette
	[ ] jump to diagnostic error in other file

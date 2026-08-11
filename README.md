Key bindings:
- Ctrl+S save
- Ctrl+Q quit
- Ctrl+F find
- Ctrl+R toggle replace while finding
	- Enter replace current match and move to the next match
	- Ctrl+Enter replace all matches
	- Tab skip current match and move to the next match
- Ctrl+. select
	- Ctrl+D select word under cursor
	- Ctrl+L expand selection to line
- Ctrl+C copy
- Ctrl+X cut
- Ctrl+V paste
- Ctrl+] smart jump
- Ctrl+P command palette
	- `:123` goto line 123
	- `@filter symbol` goto symbol, for example `@func main`, `@type Editor`
    - `>nexterror` goto next diagnostic error
    - `>preverror` goto previous diagnostic error
- Ctrl+Backspace delete to line start
- Ctrl+K delete to line end
- Ctrl+Enter insert line below
- Ctrl+A or Home move cursor to line start
- Ctrl+E or End move cursor to line end
- Alt+Left move cursor to start of current/previous word
- Alt+Right move cursor to end of current/next word
- Alt+Backspace delete word backwards
- Esc cancel

Early versions should keep things tiny.
Later, may consider duplicate selection, mutilple cursor, undo/redo, jump back/forward.

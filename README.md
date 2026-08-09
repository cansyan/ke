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
	- Ctrl+L select line
- Ctrl+C copy
- Ctrl+X cut
- Ctrl+V paste
- Ctrl+] smart jump
- Ctrl+P goto anything
	- `:123` goto line 123
	- `@filter symbol` goto symbol, filter can be keyword like Go's type/func/var, or Python's def, or empty
- Ctrl+Backspace delete to line start
- Ctrl+K delete to line end
- Ctrl+Enter insert line below
- Esc cancel

Early versions should keep things tiny.
Later, may consider duplicate selection, mutilple cursor, undo/redo, jump back/forward.

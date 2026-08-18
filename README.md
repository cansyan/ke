Key bindings:
- Ctrl+S save
- Ctrl+Q quit
- Ctrl+F find
	- Enter next match
	- Shift+Enter previous match
	- Ctrl+R toggle replace while finding
		- Enter replace current match and move to the next match
		- Ctrl+Enter replace all matches
		- Tab skip current match and move to the next match
- Ctrl+R symbol picker
- Shift+Up/Down/Left/Right start selection
	- Ctrl+D select word under cursor
	- Ctrl+L expand selection to line
- Ctrl+C copy
- Ctrl+X cut
- Ctrl+V paste
- Ctrl+G goto definition
- Ctrl+P command palette
	- `:123` goto line 123
	- `/ls` list buffer
- Ctrl+Backspace delete to line start
- Ctrl+K delete to line end
- Ctrl+Enter insert newline below
- Shift+Enter insert newline above
- Ctrl+A or Home move cursor to line start
- Ctrl+E or End move cursor to line end
- Ctrl+] goto next diagnostic
- Ctrl+W close buffer
- Ctrl+B Ctrl+N next buffer
- Ctrl+B Ctrl+P previous buffer
- Ctrl+- go back (long jumps only)
- Ctrl+Shift+- go forward (long jumps only)
- Alt+Left move cursor to start of current/previous word
- Alt+Right move cursor to end of current/next word
- Alt+Backspace delete word backwards
- Esc cancel

Command keys are supported in Kitty terminal:
- Cmd+Up move cursor to file start
- Cmd+Down move cursor to find end
- Cmd+Left move cursor to line start
- Cmd+Right move cursor to line end
- Cmd+Backspace delete to line start
- Cmd+Shift+Backspace delete whole line

Mouse motions:
- mouse_left_press move cursor
- mouse_left_drag expand selection
- ctrl+mouse_left_release goto definition

Design Choices:
- future key chords, Ctrl+K or Ctrl+X as leader key are good, but both taken. 
- did consider Ctrl+Shift+P for command palette, but the key combo seems unreliable in practice
- early version should keep things tiny, avoid duplicate selection, mutilple cursor, undo/redo
- using / as command prefix is more convenient than > (shift+.)
- in-process semantics checking ignores external file/module, benefits instant feedback

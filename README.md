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
- Shift+Up/Down/Left/Right start selection
	- Ctrl+D select word under cursor
	- Ctrl+L expand selection to line
- Ctrl+C copy
- Ctrl+X cut
- Ctrl+V paste
- Ctrl+G goto definition
- Ctrl+P command palette
	- `/command` command picker
	- `@symbol` symbol picker
	- `:linenumber` goto line
- Ctrl+Shift+P command picker
- Ctrl+R symbol picker
- Ctrl+Backspace delete to line start
- Ctrl+K delete to line end
- Ctrl+Enter insert newline below
- Shift+Enter insert newline above
- Ctrl+A or Home move cursor to line start
- Ctrl+E or End move cursor to line end
- Ctrl+W close buffer
- Ctrl+] goto next diagnostic
- Ctrl+Shift+[ goto previous reference
- Ctrl+Shift+] goto next reference
- Ctrl+- jump back
- Ctrl+Shift+- jump forward
- Ctrl+N trigger completion
	- Up/Down navigate
	- Tab/Enter complete
- Alt+Left move cursor to start of current/previous word
- Alt+Right move cursor to end of current/next word
- Alt+Backspace delete word backwards
- Esc cancel

Command keys are supported in Kitty terminal:
- Cmd+Up move cursor to file start
- Cmd+Down move cursor to file end
- Cmd+Left move cursor to line start
- Cmd+Right move cursor to line end
- Cmd+Backspace delete to line start
- Cmd+Shift+Backspace delete whole line

Mouse motions:
- mouse_left_press move cursor
- mouse_left_drag expand selection
- ctrl+mouse_left_release goto definition

Technical decisions:
- using key ctrl+[, ctrl+d, ctrl+c is ok with Kitty protocol enabled
- future key chords, Ctrl+K or Ctrl+X as leader key are good, but both taken. 
- early version should keep things tiny, avoid duplicate selection, mutilple cursor, undo/redo
- using / as command prefix is more convenient than > (shift+.)
- in-process semantics checking ignores external file/module, benefits instant feedback
- using gopls cli commands, skipping massive details of LSP communications.

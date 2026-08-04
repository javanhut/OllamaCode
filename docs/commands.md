# Commands and keys

Type `/` in the input to get an autocomplete menu of every command.

## Keys

### Chat

| Key | Action |
|---|---|
| `Enter` | Send |
| `Shift+Enter` / `Ctrl+J` | Newline |
| `Shift+Tab` | Cycle mode: explore → plan → write → explore |
| `Esc` / `Ctrl+S` | Interrupt the running turn |
| `Ctrl+C` | Interrupt mid-turn; quit when idle |
| `↑` / `↓` | Recall previous messages (when the input is empty or unmodified) |
| `Ctrl+F` | Search the transcript |
| `n` / `N` | Next / previous match (with the search prompt dismissed) |
| `Ctrl+G` | Jump to the live end of the transcript |
| `Ctrl+T` | Expand or collapse tool call details |
| `Shift+↑/↓`, `PgUp/PgDn`, `Ctrl+U/D` | Scroll |
| drag with the mouse | Select transcript lines; release copies |

### Approval prompt

| Key | Action |
|---|---|
| `y` / `Enter` | Allow this call |
| `a` | Allow every pending call in this turn |
| `n` / `Esc` | Deny |

### Connection modal (`/settings`, `/provider`)

| Key | Action |
|---|---|
| `Tab` / `Shift+Tab` | Next / previous field |
| `↑` / `↓` | Switch endpoint (default host, each provider, + new provider) |
| `Space` / `←` / `→` | Cycle the wire format on the Wire row |
| `Enter` | Save and test the selected endpoint |
| `Ctrl+D` | Delete the selected provider |
| `Esc` | Cancel |

### Model picker (`/models`)

| Key | Action |
|---|---|
| `↑` / `↓` or `k` / `j` | Move |
| `Enter` | Select |
| `p` | Pull a new model, with live progress |
| `r` | Refresh the list |
| `Esc` | Close |

## Slash commands

### Models and routing

| Command | Description |
|---|---|
| `/models` | Interactive list — switch or pull |
| `/model` | Show the active model's settings |
| `/model use <name>` | Set the default model |
| `/model ctx <tokens>` | Override this model's context window |
| `/model temp <0.0–2.0>` | Override sampling temperature |
| `/route` | Show the mode→model table |
| `/route <mode> <spec>` | Bind a model to a mode |
| `/route <mode> off` | Unbind one mode |
| `/route off` | Disable routing entirely |
| `/provider` | List configured endpoints |
| `/provider new` | Add one (modal) |
| `/provider <name>` | Edit one (modal) |
| `/provider remove <name>` | Delete it and any routes bound to it |
| `/settings` | Edit the default host's URL and key |
| `/settings <provider>` | Jump straight to that provider |

### Modes

| Command | Description |
|---|---|
| `/mode <explore\|plan\|write\|auto>` | Switch directly |
| `/auto` | Shortcut for `/mode auto` |

### Session

| Command | Description |
|---|---|
| `/clear` | Reset the conversation |
| `/save [name]` | Save the session |
| `/load <name>` | Restore one |
| `/sessions` | List saved sessions |
| `/archive` | Retrieve compacted history from the KV archive |
| `/undo` | Revert the file changes from the last turn |
| `/diff` | View the last turn's diffs full-screen |
| `/copy` | Copy the last response to the clipboard |
| `/stats` | Timing and token totals |

### Notes and memory

| Command | Description |
|---|---|
| `/notes` | Toggle the session-notes sidebar |
| `/clearnotes` | Clear the notes scratchpad |
| `/notes restore` | Restore notes from the pre-dream backup |
| `/dreams` | What it thought about while idle |
| `/dream` | Toggle idle dream mode |

### Display and behavior

| Command | Description |
|---|---|
| `/help`, `/?` | Help screen |
| `/verbose` | Toggle detailed tool output |
| `/show_thinking`, `/thinking` | Replay the model's reasoning in the transcript |
| `/face` | Toggle the mascot overlay |
| `/welcome` | Toggle the startup panel |
| `/verify` | Toggle the auto compile-check after edits |
| `/companion` | Toggle the voice companion (speech in, speech out) |
| `/quit`, `/exit` | Exit |

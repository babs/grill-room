# S12: plugin options in the sandbox

**Verdict: works by writing two files.** A `--plugin-dir` plugin reads its option values from the sandbox home's `.claude/.credentials.json` and `.claude/settings.json`, which the daemon already rewrites every turn. No `claude plugin configure` call is needed. Claude Code 2.1.296, the sandbox image, `--network none`, no API call.

## Findings

- A plugin loaded with `--plugin-dir` has the id `<name>@inline`: `claude plugin details` prints it as the Source line. `claude plugin configure rich` fails; `claude plugin configure rich@inline` works.
- `claude --plugin-dir /p plugin configure <name>@inline --values-stdin` stores:
  - sensitive options in `~/.claude/.credentials.json`: `{"pluginSecrets": {"<name>@inline": {"KEY": "value"}}}`;
  - the others in `~/.claude/settings.json`: `{"pluginConfigs": {"<name>@inline": {"options": {"KEY": "value"}}}}`.
- Writing those two files directly does the same. A SessionStart hook run by `claude -p` got `CLAUDE_PLUGIN_OPTION_API_KEY=s3cret-value` and `CLAUDE_PLUGIN_OPTION_REGION=us`. Without the files, both variables are empty.
- Claude Code does not refuse a plugin whose `required` option has no value. grill-room enforces it: such a plugin is left out of the turn ("needs host setup").
- `${user_config.X}` in a shell-form hook command is refused ("use exec form"). In MCP server headers it is substituted, and the debug log redacts `Authorization`.
- SessionStart hooks run before any API call, so they run with no network too.

## Plan taken

The daemon merges `pluginSecrets` into the dummy `.credentials.json` it writes before each turn, and writes `settings.json` with `pluginConfigs`, both whole, both in the sandbox home. Only the plugins the turn loads get values; nothing carries over from an earlier turn. The fake claude used by the tests reads the same two files and runs SessionStart command hooks with `CLAUDE_PLUGIN_OPTION_<KEY>`, as 2.1.296 does.

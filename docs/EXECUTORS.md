# Executors

Waza runs evaluations through one of five product executors. Select one in `config.executor`, or override it for a single run with `waza run eval.yaml --executor <name>`.

| Executor | Default command | Output | Tool evidence | Usage | MCP | Skill invocation | Prompt tools | Sessions |
|---|---|---|---:|---:|---:|---:|---:|---:|
| `copilot-sdk` | bundled Copilot client | SDK events | Yes | Yes | Yes | Yes | Yes | Yes |
| `codex-cli` | `codex` | JSONL | Command and MCP calls | Yes | No | No | No | No |
| `claude-cli` | `claude` | stream JSON | No | Yes | No | No | No | No |
| `hermes-cli` | `hermes` | text plus usage file | No | Yes | No | No | No | No |
| `generic-cli` | required in YAML | `text` or `jsonl` | No | JSONL only | No | No | No | No |

Final output grading and workspace file grading work with every executor. Waza checks eval-level graders, task graders, checkpoint graders, MCP settings, `trigger_tests.yaml`, responders, and follow-up turns before execution. Unsupported evidence or session requirements fail preflight instead of being silently skipped.

## Configuration

```yaml
config:
  executor: codex-cli
  model: gpt-5.5
  executor_config:
    command: codex
    args: []
```

Built-in adapters provide their default command and native output protocol. `args` adds argv entries without invoking a shell. `command` can select a different executable path.

`generic-cli` requires an explicit command:

```yaml
config:
  executor: generic-cli
  model: my-model
  executor_config:
    command: my-agent
    args: ["run", "--format", "jsonl"]
    prompt_transport: stdin
    output_format: jsonl
    env_allowlist: [MY_AGENT_TOKEN]
```

The default prompt transport is stdin. Argument transport must be explicit; `{prompt}` is only expanded in that mode. Other supported placeholders are `{workspace}`, `{workdir}`, `{skill_dir}`, and `{model}`. Only `PATH`, `TMPDIR`, the listed environment variables, and Waza's `WAZA_*` runtime variables are exposed to `generic-cli`.

For `output_format: text`, stdout is the final response. For `jsonl`, emit one object per line:

```json
{"type":"assistant_message","text":"working"}
{"type":"usage","usage":{"turns":1,"input_tokens":20,"output_tokens":5}}
{"type":"result","text":"final answer","session_id":"run-1"}
```

Unknown JSONL event types are ignored. Malformed JSONL, explicit error events, non-zero exits, missing commands, timeouts, and cancellations fail the run.

## Native Skill Isolation

Each trial runs in an isolated temporary workspace:

- Codex receives `.agents/skills/<skill>/`.
- Claude receives `.claude/skills/<skill>/`.
- Hermes receives a run-local external skills directory through a temporary `HERMES_HOME`.
- Generic CLI receives the skill under `.waza/skills/<skill>/` and its path in `WAZA_SKILL_DIR`.

The skill and baseline runs start from identical fixture resources; only the skill run receives the target skill. Waza copies only that target skill, rejects symbolic links, and fails when a same-named globally visible skill could contaminate the baseline. `executor_config.allow_skill_shadowing: true` is an explicit escape hatch and should not be used for comparative measurements.

`inject_skill_body` remains a Copilot compatibility option. CLI executors never inject the skill body into the prompt and emit a migration warning when the field is present.

## Process Safety

CLI processes run with separate stdout and stderr capture, bounded buffers, context cancellation, and process-group cleanup. Waza captures the resulting workspace files for file graders and removes executor-specific skill/runtime directories from the artifact view. Use `--keep-workspace` when debugging a failed run.

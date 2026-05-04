# claude-ds installer test protocol

End-to-end testing for `install.sh` and `claude-ds --setup`. Driven through
a real `tmux` session so we exercise interactive prompts the same way a
human user would.

## Why tmux

`install.sh` is interactive: path selection, overwrite prompts, secret-ref
input, proxy opt-in. Piping canned input via `stdin` doesn't catch issues
that only show up under a real PTY (cursor handling, `read -e`, masked
input, terminal-width-dependent output). tmux gives us:

- a real PTY with the script's `tput cols` / readline behavior intact,
- programmatic `send-keys` / `capture-pane` for assertions,
- isolation: each scenario gets its own session.

## Sandbox model

Every scenario runs with:

- `HOME=$TESTROOT/home` — a fresh empty home directory under `/tmp`,
- `XDG_CONFIG_HOME` unset (so `claude-ds` falls back to `$HOME/.config`),
- the harness pre-pends `$TESTROOT/home/.local/bin` to `$PATH`,
- network: real (we hit GitHub raw URLs); a `--ref <branch>` flag selects
  which branch to pull from. Default `main`. CI/local testing uses
  `worktree-cds-2-installer` to exercise local changes.

This means scenarios never touch your real `~/.local/bin/claude-ds` or
`~/.config/claude-ds/config`. They DO hit GitHub (read-only) and may use
real `sudo` for `/usr/local/bin` tests — those are explicitly skipped by
default and require `--include-sudo` to enable.

## Test secret-ref strategy

The onboarding flow validates the API key against the configured
`base_url`. Since tests don't have a real DeepSeek key:

- supply a plaintext fake key (`test-key-123`),
- the live check returns `unauth` (401),
- the harness sends `n` to "Try a different reference?" so the wrapper
  proceeds to write config after the first attempt,
- proxy choice: `s` (skip).

This validates the full onboarding *path* without needing a real key.

## Scenarios

Each scenario is a function in `tests/harness.sh`. Status legend:

- `[A]`  acceptance-criterion-mapped — must pass before resolve
- `[R]`  regression — protects existing behavior
- `[S]`  sudo-required — skipped unless `--include-sudo`

| ID  | Name                                | Status |
|-----|-------------------------------------|--------|
| T01 | Fresh install, default path         | [A]    |
| T02 | Fresh install, custom path          | [A]    |
| T03 | Custom path with ~ expansion        | [A]    |
| T04 | Existing binary, decline overwrite  | [A]    |
| T05 | Existing binary, accept overwrite   | [A]    |
| T06 | Existing config, keep               | [A]    |
| T07 | Existing config, backup + re-run    | [A]    |
| T08 | Existing config, overwrite          | [A]    |
| T09 | --setup with config already present | [A]    |
| T10 | PATH-not-on-$PATH warning           | [A]    |
| T11 | Damaged config recovery             | [R]    |
| T12 | Branch-aware install                | [A]    |
| T13 | Install to /usr/local/bin via sudo  | [S]    |

## Running

```bash
# Run all (skips sudo tests):
./tests/harness.sh --ref worktree-cds-2-installer

# Run one scenario:
./tests/harness.sh --ref worktree-cds-2-installer T01

# Include sudo tests:
./tests/harness.sh --ref worktree-cds-2-installer --include-sudo
```

Output: each scenario prints `PASS` / `FAIL` with the assertion that
tripped. On failure, the captured tmux pane is preserved at
`$TESTROOT/<scenario>.pane` for post-mortem.

## What "passes" means

Scenarios assert on:

1. **Process exit code** — `bash` running `install.sh` must exit 0
   (or whatever the scenario expects).
2. **File system state** — binary at expected path, mode 0755, config
   at expected path with mode 0600, backups present where required.
3. **Output strings** — banner text, prompts, success markers,
   warnings. Matched as substrings, not regex (less brittle).

We do *not* assert the live API check passes — the test key is fake by
design. We DO assert the wrapper handled the rejection and proceeded.

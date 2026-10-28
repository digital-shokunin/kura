# Using kura with an Obsidian vault

The [Obsidian Git](https://github.com/Vinzent03/obsidian-git) plugin runs your system's `git`, so a `kura::` remote works once git can find `git-remote-kura`. Apps launched from the Dock get a minimal `PATH` (`/usr/bin:/bin:/usr/sbin:/sbin`), and that's the usual reason pushes fail.

## Plugin settings

**Settings → Community plugins → Git → Advanced**:

| Setting | Value |
|---|---|
| Custom Git binary path | the full path to a git binary, e.g. `/opt/homebrew/bin/git`, **not** a directory |
| Additional PATH environment variable paths | the directory holding `git-remote-kura`, e.g. `~/.local/bin` written out in full (`/Users/you/.local/bin`) |

Then click **Reload with new environment variables**. A full Obsidian restart isn't needed.

The plugin saves these two values per device, outside the vault's `data.json`. Set them on each machine.

## Before the first push

Cache the repo key once, in a terminal:

```sh
kura unlock 'kura::s3://...?fp=...'
```

The helper never prompts. If the key isn't cached, a push from Obsidian fails with a notice telling you to run `kura unlock`, so it never hangs waiting for Touch ID or a PIN.

After upgrading kura, run `kura unlock` once in a terminal as well. macOS asks you to approve the new build's access to the Keychain item; choose **Always Allow**. Doing it in the terminal keeps that dialog from interrupting a background push. See [macOS Keychain and code signing](../README.md#macos-keychain-and-code-signing).

## Checking that pushes land

Run **Git: Push** from the command palette (Cmd-P), then from a terminal:

```sh
git -C ~/path/to/vault fetch && git -C ~/path/to/vault status -sb   # expect: ## main...origin/main
kura info 'kura::...'                                                # the manifest seq goes up after each push
```

| Symptom | Cause |
|---|---|
| `git: 'remote-kura' is not a git command` | `git-remote-kura` isn't on the plugin's PATH |
| `spawn ... ENOENT` | the Custom Git binary path points at something that doesn't exist |
| `repo key not unlocked on this machine` | run `kura unlock` in a terminal |
| commits pile up locally, nothing reaches the remote | the auto-backup commit ran before the settings were fixed; run **Git: Push** once |

## Suggested `.gitignore`

```gitignore
.DS_Store
# Videos bloat history permanently; keep them outside the vault
*.mp4
*.m4v
*.mov
*.mkv
*.webm
# Plugin code can be reinstalled from the registry; manifest.json and data.json (settings) stay tracked
.obsidian/plugins/*/main.js
.obsidian/plugins/*/styles.css
```

## Phones and other sync plugins

Obsidian mobile can't run external git remote helpers. A separate file-sync plugin (for example remotely-save with its own encryption) can carry changes between the phone and a desktop. The desktop's Obsidian Git then commits them and pushes them through kura.

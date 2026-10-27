# kura

kura (蔵, a traditional fireproof storehouse) gives you end-to-end encrypted git remotes on storage you don't have to trust: Amazon S3, S3-compatible services (DigitalOcean Spaces, Cloudflare R2, MinIO, ...), or a local directory such as a NAS or USB disk.

There's no daemon or server component: git runs `git-remote-kura` when you push or fetch a `kura::` URL. The storage provider sees only ciphertext. File names, contents, commit messages, branch names and authors are all hidden, and the provider can't forge data, substitute keys, or roll the repository back without being detected.

> **Status:** young and unaudited. The cryptography is standard (age, XChaCha20-Poly1305, HKDF-SHA256), but the composition hasn't been independently reviewed. Keep another backup.

## How it works

- Each repository has a random 32-byte master key **K**.
- K is wrapped to your long-term keys, and **any one** of them can unlock it:
  - age X25519 keys
  - age plugin keys, such as Touch ID through [age-plugin-se] or a YubiKey PIV slot through [age-plugin-yubikey]
  - OpenPGP keys through `gpg`, including smartcard or YubiKey subkeys
- After one unlock per machine, K is cached locally: in the login Keychain on macOS, or a `0600` file elsewhere.
- Every stored object is an [age] file whose only stanza wraps the file key with XChaCha20-Poly1305 under a subkey of K. The repo id, format version and the object's storage key are bound in as associated data, so a ciphertext can't be replayed into another repository or slot. Only K holders can create or read objects.
- **A push:**
  1. uploads one encrypted, Padmé-padded packfile
  2. creates the next manifest with an atomic put-if-absent (S3 `If-None-Match: *`, or `link(2)` on a filesystem)
  - A manifest lists refs, packs and recipients, and links to the previous manifest by hash.
  - Concurrent pushes can't overwrite each other: the loser re-checks fast-forward and retries.
  - Once there are more than 24 packs, a push merges the incremental ones into a single pack. Only that small history is re-uploaded, never the base pack.
- **Clones** record a high-water mark and refuse a manifest chain that goes backwards or forks.
- **Key-substitution defense:** anyone can encrypt a key to your *public* key, so a fingerprint of K is pinned in three places: the clone URL (`?fp=`), `remote.<name>.kurafp`, and your recovery sheet. `unlock` refuses a key that doesn't match, or that can't authenticate the existing manifests.

Remote layout:

```
config.json                  plaintext: format, version, random repo id
keys/age.age                 K, wrapped to age recipients
keys/gpg.asc                 K, gpg-encrypted with hidden recipient key ids
manifests/000000000042       encrypted, hash-chained manifests: refs, packs, recipients (never deleted)
packs/<random>/0000          encrypted pack parts (64 MiB max)
```

**What the provider can see:** the URL prefix, pack count, padded pack sizes, and push times. It **can** delete objects, so enable bucket versioning and keep local clones.

## Compared to

- **[git-remote-gcrypt]** encrypts with GPG over any git or rsync remote. It rewrites a single manifest with force-push semantics, so two machines pushing at once can lose data, and it needs a GPG decrypt (often a smartcard touch) on every operation.
- **[git-crypt]** encrypts selected file contents only. File names, history, commit messages and branches stay visible.
- **[git-remote-sealed]** encrypts with age to each device's key and uses a git host as storage. Adding a device rewrites the vault.
- **Keybase** encrypted git needs the Keybase client and service running, and depends on Keybase's servers.

kura targets object storage with atomic conditional writes, wraps a single repo key (so adding a device re-wraps one small file), and supports hardware-backed keys through age plugins and gpg.

## Install

```sh
go install github.com/digital-shokunin/kura/cmd/kura@latest
ln -sf "$(go env GOPATH)/bin/kura" "$(go env GOPATH)/bin/git-remote-kura"
```

Or from a checkout, on macOS with code signing (recommended, see below):

```sh
make codesign-setup   # once: self-signed "kura-codesign" identity in the login keychain
make install          # build, sign, install to ~/.local/bin (PREFIX=... to change)
```

Optional tools: `brew install age age-plugin-se gnupg`.

### macOS Keychain and code signing

The macOS Keychain lets a program read an item based on its code signature. `make install` signs kura with a stable self-signed identity, and `git-remote-kura` is a symlink to the same binary, so both match the item's access list.

A self-signed binary has no Apple Team ID, though, so macOS also pins the item to the exact build (a `cdhash:` partition entry). After each upgrade, the first read shows a dialog asking to let **kura** use the item. Choose **Always Allow**; it may also ask for your login password.

Trigger that prompt yourself, in a terminal, right after upgrading, so it never interrupts a GUI git client:

```sh
kura unlock 'kura::s3://...?fp=...'
```

The first `make install` also asks to let **codesign** use the signing key. Choose **Always Allow** for that too.

## Keys

```sh
# Touch ID (Secure Enclave). The identity file is a handle, not the key.
age-plugin-se keygen --access-control=any-biometry -o ~/.config/kura/identities
age-plugin-se recipients -i ~/.config/kura/identities          # -> age1se1...

# Offline recovery key: store it in a password manager or on paper, then delete the file
age-keygen -o recovery.txt                                     # prints age1... public key

# OpenPGP smartcard subkey: append ! to force that exact subkey
gpg --card-status | grep 'Encryption key'
```

## Create a repository

```sh
kura init 's3://BUCKET/PREFIX?region=us-east-2&creds=mybucket' \
  --age age1se1...  \
  --age age1...     \
  --gpg 'FINGERPRINT!'
```

`init` prints the key fingerprint and the exact `git remote add` and `git config remote.origin.kurafp` commands to run. **Write the fingerprint down next to your recovery key.**

Choose a neutral `PREFIX`; it's the only plaintext name on the remote.

## S3 setup

1. Create a private bucket.
2. Enable versioning, with a lifecycle rule that expires noncurrent versions after 30 days. That's your safety net against deletion.
3. Create an access key scoped to that bucket, with `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` and `s3:ListBucket`. Without `ListBucket`, S3 reports missing objects as 403 instead of 404, which breaks kura. Store the key in the Keychain:
   ```sh
   kura s3-creds set mybucket       # then add &creds=mybucket to the URL
   ```
   An AWS shared-config profile (`&profile=NAME`) or the standard AWS environment variables also work. SSO profiles expire, which breaks unattended pushes until you log in again.
4. Check that the provider really honors conditional writes before trusting it:
   ```sh
   kura probe 's3://BUCKET/scratch?region=...'
   ```

| URL parameter     | Meaning                                                            |
|-------------------|--------------------------------------------------------------------|
| `region=`         | bucket region (required unless set in the AWS config)              |
| `endpoint=`       | S3-compatible endpoint, e.g. `https://nyc3.digitaloceanspaces.com` |
| `path_style=true` | path-style addressing (MinIO and similar)                          |
| `profile=`        | AWS shared-config profile                                          |
| `creds=`          | static key stored with `kura s3-creds set`                         |
| `fp=`             | pinned key fingerprint                                             |

Local or NAS storage: `kura::file:///Volumes/nas/repos/notes`.

## New machine

```sh
kura unlock 'kura::s3://...?fp=...' -i ~/.config/kura/identities   # age / plugin identities
kura unlock 'kura::s3://...?fp=...' --gpg                          # OpenPGP / smartcard
git clone 'kura::s3://...?fp=...' notes
```

`git-remote-kura` never prompts. If the key isn't cached, it exits and prints the `kura unlock` command, so GUI git clients fail cleanly instead of hanging.

GUI apps often run git with a minimal `PATH` (`/usr/bin:/bin:...`), where git can't find `git-remote-kura`. For example, with the Obsidian Git plugin:

- set its custom git binary path to your full git, e.g. `/opt/homebrew/bin/git`
- add kura's install directory (e.g. `~/.local/bin`) to "Additional PATH environment variable paths"

Alternatively, symlink `git-remote-kura` into `$(git --exec-path)`. Git always searches that directory, but package upgrades of git remove the link.

## Maintenance

```sh
kura info    'kura::...'                # fingerprint, object format, seq, refs, packs, recipients
kura verify  'kura::...' [--repo DIR]   # fresh clone into a temp dir, fsck --strict, compare refs (and HEAD tree)
kura compact 'kura::...'                # rewrite all packs into one
kura gc      'kura::...' [--dry-run]    # delete packs unreferenced for 24 h
kura recipients 'kura::...' list|add|remove [--age R] [--gpg FPR]
kura lock    'kura::...'                # forget the cached key on this machine
```

`gc` only deletes a pack when no manifest written during the grace period references it and the pack itself is older than the grace period. In-flight pushes and readers are therefore safe.

## Recovery drill

Do this after setup and periodically:

```sh
kura lock 'kura::...'
kura unlock 'kura::...' -i ~/.config/kura/identities && kura lock 'kura::...'
kura unlock 'kura::...' --gpg && kura lock 'kura::...'
kura unlock 'kura::...' -i recovery.txt
kura verify 'kura::...'
```

## Development

```sh
make test     # go vet + go test -race; integration tests drive real git against a file backend
make linux    # static linux/amd64 build
```

Tests set `KURA_KEYSTORE_DIR`, so they never touch the real Keychain.

Both git object formats are supported: SHA-1 (the default) and SHA-256 (`git init --object-format=sha256`). The first push records the format, clones are created to match, and pushes from a repository in the other format are refused. Git's object ids are only content addresses; integrity against the storage provider comes from kura's authenticated encryption either way.

## Limitations

- Removing a recipient doesn't revoke a key it already unwrapped. Re-keying (`rotate`) isn't implemented yet.
- The provider can see push timing and padded pack sizes.
- An uploaded pack whose push never committed (crash, conflict) stays in storage until `kura gc`.

## License

BSD 3-Clause. See [LICENSE](LICENSE).

[age]: https://age-encryption.org
[age-plugin-se]: https://github.com/remko/age-plugin-se
[age-plugin-yubikey]: https://github.com/str4d/age-plugin-yubikey
[git-remote-gcrypt]: https://github.com/spwhitton/git-remote-gcrypt
[git-crypt]: https://github.com/AGWA/git-crypt
[git-remote-sealed]: https://github.com/hibariya/git-remote-sealed

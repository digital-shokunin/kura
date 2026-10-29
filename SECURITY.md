# Security policy

kura is young and has not been independently audited. Reports are welcome, including design-level ones. Results of automated audits are in [docs/audits](docs/audits/).

## Reporting a vulnerability

Use GitHub's private reporting: **Security → Report a vulnerability** on this repository. Please don't open a public issue for anything that could affect confidentiality or integrity.

Include what you can:

- the kura version (`kura version`) and backend (S3, S3-compatible service, file)
- what an attacker needs: storage write access, a stolen device, a removed recipient's key, ...
- steps or a test that reproduces it; a failing Go test against the file backend is ideal

This is a one-person project. Expect an acknowledgement within a week. Coordinated disclosure defaults to 90 days, or sooner once a fix ships.

## Supported versions

Only the latest commit on `main` (and the latest release, once there are releases). The on-storage format version is bound into every ciphertext; a format change will ship with a migration path.

## Threat model

The storage provider, and anyone who can read or write the bucket or directory, is untrusted. kura aims to guarantee the following against them:

| Property | Mechanism |
|---|---|
| Confidentiality of contents, file names, commit messages, refs, authors | Every object is age-encrypted under keys derived from the repo key K |
| Integrity: no forged or modified objects | AEAD, with repo id, format version and storage key as associated data |
| No key substitution | K's fingerprint pinned in the clone URL, git config and your recovery sheet |
| No rollback or fork of history | Hash-chained manifests plus a per-clone high-water mark |
| No lost updates between concurrent pushers | Put-if-absent manifests (`If-None-Match: *`, `link(2)`) |
| A shared `file://` directory can't redirect kura's file operations | All reads, writes and deletes go through `os.Root`; planted symlinks can't point outside the directory |

Other key holders are trusted with the repository, not with your machine. Anyone with K can push anything, but kura validates every manifest field that reaches git or a storage key (ref names, object ids, pack ids), so they can't inject remote-helper protocol lines or git options into your fetches.

Out of scope, by design:

- **Deletion and availability.** The provider can delete objects or refuse service. Use bucket versioning and keep local clones.
- **Traffic analysis.** Push times, pack counts and padded sizes are visible.
- **First contact.** A clone with no high-water mark trusts the newest authenticated manifest it finds. It prints that manifest's sequence number, writer and time.
- **A compromised machine.** Anything that can read your cached key (the Keychain item or the `0600` key file) or your age/gpg identities has K.
- **Removed recipients.** `recipients remove` stops new wraps; a key that already unwrapped K keeps it. Re-keying isn't implemented yet.
- **Store timestamps for uncommitted uploads.** gc judges a pack that no manifest references yet by the store's modification time. A provider that falsifies it can get an in-flight upload deleted, which it could do directly anyway; the push then fails its pre-commit check instead of committing a manifest with missing data.
- **URLs from someone else.** A `kura::` URL chooses the storage endpoint and which stored credential signs requests, so treat one like any remote you'd add by hand. SigV4 never sends the secret key, and git doesn't run remote helpers for submodule URLs by default (`protocol.allow`).
- **Storage that ignores conditional writes.** Check with `kura probe` before trusting a provider; a backend that accepts a second put-if-absent can lose concurrent pushes.

Findings in any of these areas are still useful if kura claims more than it delivers, or if the documentation understates a risk.

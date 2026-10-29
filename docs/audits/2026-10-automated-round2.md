# Automated audit, round 2 (October 2026)

Four model configurations audited kura at `e1fab93`, its first commit, using an
autonomous multi-wave vulnerability-finding and validation pipeline. Each
finding was triaged against the source, and every confirmed bug was fixed with
a regression test. All of them are fixed on `main`.

Round 1 was an earlier partial GLM-5.3 run. It found two bugs, fixed in
`606cb37`.

This is an automated self-audit, not an independent one.

## Summary

- **12 distinct real bugs:** 2 rediscoveries of round-1 bugs and 10 new ones.
- The best single run (DeepSeek V4.1 Flash) found 9 of the 12, for $2.83.
- Every model except the self-hosted one found at least one bug no other model
  found.
- At first, the pipeline's own validation confirmed 0 of the 18 real hits,
  because of harness bugs. After those were fixed, re-validation confirmed 3 of
  the 4 bugs that were re-run.

## Results by model

| Run | Real hits (incl. dups) | Not known from round 1 | Unique to this model | Precision | Cost | Cost per real hit |
|---|---|---|---|---|---|---|
| DeepSeek V4.1 Flash (1017), cloud | 9 | 8 | 5 | 9/16 | $2.83 | $0.31 |
| GLM-5.3, cloud | 5 | 3 | 2 | 5/5 | $7.21 | $1.44 |
| DeepSeek V4.1 Pro, cloud | 3 | 3 | 1 | 3/5 | $4.88 | $1.63 |
| GLM-5.3-Flash, self-hosted | 1 (dup) | 0 | 0 | 1/10 | $0 | n/a |

The cloud runs took 5 to 8 hours each. The self-hosted run took about 19 hours.

## The 12 bugs

Severities are from triage, not from the models or the pipeline's judge.

Round-1 rediscoveries, both fixed in `606cb37`:

| Bug | Severity | Found by |
|---|---|---|
| `recipients add/remove` wrote the key files before the manifest. A change that failed after the write left `keys/` out of step with the manifest and could drop a concurrent writer's recipient. | LOW-MEDIUM | Flash, GLM-5.3, GLM-5.3-Flash |
| `compact` and the incremental merge committed without the freshness and presence checks that push had. A machine that slept between upload and commit could commit a pack `kura gc` had already deleted. | MEDIUM | GLM-5.3 only |

New in round 2:

| Bug | Severity | Found by | Fixed in |
|---|---|---|---|
| The `file://` store followed a symlinked `keys/` or `packs/` directory, and could write, create and delete files outside the store root. | MEDIUM | Pro, GLM-5.3, Flash | `84f3122` |
| `kura gc` stopped its manifest walk early when a writer's clock was slow. It deleted packs that readers of recent manifests still needed. | LOW | Pro, Flash | `84f3122` |
| gpg's decrypted output was uncapped, allowing a decompression bomb during interactive unlock. | LOW | Pro only | `84f3122` |
| The object format was checked only while refs existed. After every ref was deleted, a sha256 repository could add packs to a sha1 remote. | LOW | Flash, GLM-5.3 | `db8ac9d` |
| `recipients` could delete one key file before writing the other. It also skipped the restore after a failed write, locking recipients out until `recipients sync`. | LOW | GLM-5.3 only | `5cb4f94` |
| Ref names from the manifest reached git's remote-helper protocol unvalidated. A key holder could inject protocol lines and make another clone's `git fetch` delete a file. | MEDIUM | Flash only | `db8ac9d` |
| The incremental merge, run in a shallow or grafted clone, dropped older objects and left the remote impossible to clone. | MEDIUM | Flash only | `db8ac9d` |
| Option-like object ids from the manifest could reach git command lines. | LOW | Flash only | `db8ac9d` |
| `verify` passed manifest values to `git rev-parse` without `--verify --end-of-options`. | LOW | Flash only | `db8ac9d` |
| `kura lock` trusted the plaintext repo id. An altered id made it delete another repository's cached key and report success. | LOW | Flash only | `db8ac9d` |

## Severity calibration

How the models' claimed severities compared with triage:

- DeepSeek V4.1 Flash rated both of its MEDIUM findings correctly.
- GLM-5.3 rated one finding HIGH as a privilege escalation. It is a real LOW:
  the key-file write order.
- GLM-5.3-Flash rated one non-bug HIGH. That was the incremental-merge pack-set
  rewrite, a reasonable hypothesis that turned out wrong; a regression test was
  added anyway in `fbbd8ca`.

On re-validation, the pipeline's judge got all three severities wrong (see
below).

## Pipeline validation

**First pass:** 0 of 18 real hits confirmed. Only one, the `compact`/merge vs.
gc race, was rejected outright. The others either got no verdict, because the
harness failed before the judge ran, or were marked unconfirmed. The causes
were harness bugs:

- The builder ran out of turns while assembling the test harness, and its
  harness-writing tool dropped fields between calls.
- The judge returned "failed" even when the proof of concept never reached the
  code under test.
- A record check downgraded a reproduced proof of concept because the run did
  not record the build digest and run command.
- Dedup by file and bug class merged two different findings in the same file.

**Re-validation on the fixed pipeline** ($2.32 for all four):

| Bug | Before | After |
|---|---|---|
| Ref-name injection | builder ran out of turns | confirmed: the injected ref appears in the victim's `ls-remote` and clone, and the control shows only the real refs. Judge: HIGH, triage: MEDIUM (it needs the repo key). |
| Symlinked store directories | unconfirmed | confirmed: `kura init` writes the sealed manifest and wrapped key outside the store root, and the symlink-free control stays confined. Judge: LOW, triage: MEDIUM (it can also overwrite and delete). |
| Key files before manifest | unconfirmed | confirmed: a removed age identity can still unlock, and a never-added one is refused. Judge: HIGH, triage: LOW-MEDIUM. |
| `compact`/merge vs. gc race | rejected | unconfirmed: the proof of concept called `kura init` without a recipient and stopped during setup, so it never reached the race. |

On the key-file result: removing a recipient doesn't revoke a key it could
already unwrap, because re-keying isn't implemented (see
[SECURITY.md](../../SECURITY.md)). The bug is `keys/` disagreeing with the
manifest, not a new way to obtain the key.

The gc race is a real bug. It stays unconfirmed because of a remaining gap in
the pipeline: the check against nonexistent CLI flags covers the harness
builder but not the exploit author.

## Lessons for kura

- Running several models finds more than any one of them, mostly through
  single-model finds rather than a big combined total.
- Bugs at interface boundaries recur: the remote-helper protocol stream, git
  argument lists, and filesystem path confinement. Future hardening should
  prioritise those.
- Severity claims need checking from both directions. Models and the judge
  overrated some bugs and underrated others.
- A naive proof of concept for the ref-name injection looks harmless. The
  file-deletion impact only showed with a more careful payload.

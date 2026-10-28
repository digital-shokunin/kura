# Migrating a repository from Keybase git to kura

Keybase encrypted git (`keybase://private/<user>/<repo>`) needs the Keybase service running. If it stops, pushes fail, often without anyone noticing. This is the procedure used to move a vault off it, with checks at each step.

## 1. Make sure nothing is stranded

```sh
cd ~/path/to/repo
git fetch origin                  # needs Keybase running
git log --oneline origin/main..main
```

If local commits never reached Keybase, push them now (`git push origin main`) so the Keybase copy is complete while you migrate. Treat your local clone as the source of truth either way.

## 2. Snapshot

```sh
git clone --mirror ~/path/to/repo ~/backups/repo-pre-migration.git
git -C ~/backups/repo-pre-migration.git fsck
```

## 3. Stop everything that commits

Quit any app that auto-commits, such as Obsidian with the Obsidian Git plugin. Closing the window isn't enough on macOS: quit the app (Cmd-Q) and check with `pgrep -lx Obsidian`.

## 4. Optional: clean history

If history contains large or unwanted files, rewrite a **fresh copy**, never the original:

```sh
git clone --no-local ~/path/to/repo ~/backups/rewrite
cd ~/backups/rewrite
cat > /tmp/drop-paths.txt <<'EOF'
path/to/big-file.mp4
glob:*.DS_Store
EOF
git filter-repo --invert-paths --paths-from-file /tmp/drop-paths.txt
git fsck --full
```

Check the result: the commit count should be unchanged, and the only paths missing from `HEAD` should be the ones you dropped. Compare with `git ls-tree -r -z HEAD` in both repositories, keyed by path.

Rewriting changes every commit id and drops commit signatures. That doesn't matter for a single-clone repository; for anything shared, coordinate with every clone first.

## 5. Create the kura remote and push

```sh
kura probe 's3://BUCKET/scratch?region=...&creds=...'
kura init  's3://BUCKET/PREFIX?region=...&creds=...' --age age1se1... --age age1... --gpg 'FPR!'
git -C ~/backups/rewrite remote add kura 'kura::s3://...?fp=...'
git -C ~/backups/rewrite push -u kura main
```

Write the fingerprint that `init` prints next to your recovery key.

## 6. Verify with a fresh clone

```sh
git clone --no-checkout 'kura::s3://...?fp=...' ~/backups/newclone
git -C ~/backups/newclone fsck --full --strict
# HEAD and HEAD^{tree} must match the rewrite (or the original)
```

## 7. Swap the working repository's `.git`

This keeps every working file in place:

```sh
cd ~/path/to/repo
mv .git ~/backups/repo-keybase.git           # outside the working tree, never inside it
mv ~/backups/newclone/.git .git
git config remote.origin.kurafp FPR          # plus any repo-local settings from the old .git/config
cp ~/backups/repo-keybase.git/info/exclude .git/info/exclude
git reset                                    # mixed: rebuild the index; don't touch files
git checkout -- .gitignore                   # if the rewrite changed it
git status --porcelain --ignored
```

Expect a clean status, apart from anything you deliberately changed in the working tree.

Moving the old `.git` *outside* the working tree matters: an auto-committing app would otherwise commit the entire old repository into the new one.

## 8. Push once from the command line, then from the app

```sh
git commit -am "First commit through kura" && git push
kura verify 'kura::...' --repo .
```

Then reconfigure the app (see [obsidian.md](obsidian.md)) and confirm a push from inside it lands.

## 9. Retire Keybase

Once you're confident in the new remote:

```sh
git ls-remote keybase://private/<user>/<repo> refs/heads/main   # must match your snapshot
keybase git delete <repo>                                        # immediate and irreversible
```

Keep the snapshot from step 2 for as long as you want a rollback.

# Scoop bucket (template)

This directory holds the canonical Scoop manifest template for Nomi.
The actual bucket users install from lives in a separate repository,
`nomiai/scoop-bucket`. The release workflow opens an auto-bump PR
against that external repo on every published release.

## First-time setup (maintainer)

1. Create the public repo `nomiai/scoop-bucket` on GitHub.
2. Copy the contents of this directory into the new repo:
   ```bash
   cp -r scoop-bucket/* /path/to/nomiai-scoop-bucket/
   ```
3. Commit + push.
4. Create a fine-grained PAT with `Contents: write` scope on
   `nomiai/scoop-bucket` and add it as the `SCOOP_BUCKET_GITHUB_TOKEN`
   secret on `nomiai/nomi`. Set repo variable
   `SCOOP_BUCKET_REPO=nomiai/scoop-bucket` so the workflow knows where
   to push.

## User installation

```powershell
scoop bucket add nomi https://github.com/nomiai/scoop-bucket
scoop install nomi
```

Scoop downloads the signed MSI from the latest GitHub Release, verifies
the SHA256 against the manifest, and runs the installer silently.

## winget submission

Scoop covers power-users; `winget` reaches everyone. Submission to
`microsoft/winget-pkgs` is a separate manual flow on each release:

```powershell
# Install wingetcreate once
winget install --id Microsoft.WingetCreate

# After the GitHub Release is published with a signed MSI:
wingetcreate update Nomi.Nomi `
    --version <semver> `
    --urls https://github.com/klarlabs-studio/nomi/releases/download/v<semver>/Nomi_<semver>_x64_en-US.msi `
    --submit `
    --token <github-pat>
```

`wingetcreate` opens the PR against `microsoft/winget-pkgs` for the
Microsoft team to review. Automation of this flow is deferred to
Phase 4 — the manual command is reliable enough for a sub-monthly
release cadence and Microsoft's review SLA dominates the wall-clock
time anyway.

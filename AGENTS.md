# Project working agreements

## Test temporary directory

- All test runs must use `D:\Go\temp` as their fixed temporary-directory root.
- Do not choose a workspace-local, randomly generated, or otherwise different temporary root, and do not silently fall back to one. Framework-created child directories are acceptable only when they resolve below `D:\Go\temp`.
- Before running Go tests, set the temporary-directory variables explicitly:

  ```powershell
  $env:TMP='D:\Go\temp'
  $env:TEMP='D:\Go\temp'
  $env:GOTMPDIR='D:\Go\temp'
  go test ./...
  ```

- If `D:\Go\temp` is unavailable or lacks permission, stop and report the environment problem. Do not silently switch to another temporary directory.

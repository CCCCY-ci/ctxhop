# Testing, Coverage, and Git Ignore Rules (Testing Rules)

This document outlines the testing standards, test types, and the **untracked test code** setup for the ElsevierTrackor project. These rules keep our remote repository clean while maintaining high QA standards locally.

---

## 1. Test Standards & Coverage Targets

* **Coverage Requirement**: The code coverage for core business logic, API endpoints, and data layers must be **90% or higher** for all new features.
* **Edge Case Verification**: Test cases must cover all edge cases, input extremes, concurrency conditions, and external failures defined in the Spec document.
* **PR Prerequisite**: Meeting the 90%+ coverage standard is a strict requirement before submitting any Pull Request.

---

## 2. Testing Types & Tools

We select testing tools based on task type to maximize efficiency:

### 2.1 Unit Testing
* **Goal**: Test individual functions or components in isolation, mocking external dependencies.
* **Go Backend**: Use Go's native `testing` package along with mock frameworks (e.g., `mockgen` / `stretchr/testify`).
  * Run command: `go test -v -cover ./...`
* **Admin Dashboard**: Use `Vitest` and `React Testing Library`.
  * Run command: `npm run test`

### 2.2 Integration Testing
* **Goal**: Verify that multiple layers (e.g., Handler + Service + DB) function correctly together.
* **Go Backend**: Run tests against an isolated local test database (a MySQL test instance or an in-memory SQLite database) to verify GORM CRUD operations.
* **Admin Dashboard**: Use `msw` (Mock Service Worker) to intercept network requests, verifying how frontend components interact with simulated API responses.

### 2.3 Smoke and End-to-End (E2E) Testing
* **Goal**: Quickly verify that core user flows are functional during feature acceptance and release phases.
* **Tools**: Use `Playwright` to script light E2E flows or `k6` to run backend load/performance tests.

---

## 3. Excluding Test Code from Version Control (Git Ignore)

To keep the remote code repository clean and focused on production-ready code, all test files, test configurations, and mock generators **must remain local and never be committed to Git**.

### 3.1 Git Ignore Configuration

The root `.gitignore` and sub-folder `.gitignore` files must exclude the following patterns:

```text
# === Testing Rules: Ignore all test files & outputs ===

# Go test files and coverage reports
**/*_test.go
*.test
*.out
coverage.html

# React/TS test files and coverage folders
**/__tests__/
**/*.test.ts
**/*.test.tsx
**/*.spec.ts
**/*.spec.tsx
admin/coverage/
frontend/coverage/

# Automatically generated mocks
**/mocks/
mock_*.go

# Local test configurations and temp data
**/testdata/
test.env
*.network-response
```

### 3.2 Cleaning Up Tracked Test Files

If any test files (e.g., `admin_admins_test.go`, `SidebarNav.test.tsx`) are already tracked by Git, adding them to `.gitignore` will not remove them from tracking. You must run the following commands in your terminal to remove them from Git tracking (while preserving local files):

```bash
# 1. Untrack Go backend test files
git rm --cached $(git ls-files "*_test.go")

# 2. Untrack Admin dashboard test files
git rm --cached $(git ls-files "*test.ts" "*test.tsx")

# 3. Commit the untracking changes
git commit -m "chore(git): untrack all test files according to testing rules"
```

Once executed, check `git status`. You will see these files are untracked and automatically ignored by Git, ensuring they will not be added in subsequent commits.

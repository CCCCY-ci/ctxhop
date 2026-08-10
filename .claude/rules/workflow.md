# Standard Development and Spec Design Workflow (Workflow Rules)

This document details the "Spec-First" design process and the "Test-Directed Development" methodology for the ElsevierTrackor project. These rules ensure that all development is structured, pre-designed, and verified, enforcing that code is built to satisfy tests.

---

## 1. Core Mindset: Test-Directed Development

We adhere to the following development philosophy:
> [!IMPORTANT]
> **We build code to pass the tests, rather than writing tests to pass the code.**

* **Proactive Boundary Analysis**: Before writing the first line of implementation, you must explicitly design for all edge cases, error conditions, and successful execution paths.
* **Inherent Robustness**: Code should be designed from the start to handle unexpected inputs, network timeouts, database connection drops, and other errors, rather than patching code later simply to satisfy test coverage.

---

## 2. Core Process: Spec-First Development

For all non-trivial features or structural modifications, **never write code immediately**. You must first author a functional specification document `[feature-name]-spec.md` under the `/docs/specs/` directory (create this folder if it does not exist).

A standard Spec document must cover the following areas:

### 2.1 Requirements and Functional Scope
* **Functional Description**: What problem does this new feature solve? What is the user journey?
* **Interaction Flow**: Define UI interaction flows (e.g., page navigation in the WeChat Mini-Program, confirmation dialogs in the Admin panel).

### 2.2 Database and Data Flow Design
* **Schema Modifications**: Define new tables, columns, indexes, types, constraints, and default values.
* **Data Flow**: Detail data serialization and flow from the client API request through the Controller (Handler) -> Business logic (Usecase) -> Data persistence (Repository/DB).

### 2.3 Test Cases and Boundary Design
* **Happy Path**: List inputs and expected outputs for standard successful scenarios.
* **Edge Cases and Failure Scenarios**:
  * **Input Validation**: Minimum/maximum character lengths, empty inputs, null validations, special character injections, out-of-range numerical bounds.
  * **Concurrency & Timing**: Double-submit requests (idempotency checks), high-frequency request limits (rate limiting).
  * **External Dependency Failures**: What happens when Redis is unavailable? What happens if third-party APIs time out or return 500s (e.g., publisher page DOM changes, Aliyun Captcha validation failures, Resend email dispatch issues)?

---

## 3. Standard Development Lifecycle

The implementation of any feature must cycle through the following five standard phases:

```text
┌────────────────┐      ┌────────────────┐      ┌────────────────┐      ┌────────────────┐      ┌────────────────┐
│  1. Design     │ ──>  │  2. Coding     │ ──>  │  3. Testing    │ ──>  │  4. Acceptance │ ──>  │   5. PR &      │
│  (Write Spec)  │      │ (Implement)    │      │ (Local Tests)  │      │ (Smoke Tests)  │      │   Release      │
└────────────────┘      └────────────────┘      └────────────────┘      └────────────────┘      └────────────────┘
```

### 3.1 Phase 1: Design
* Clarify requirements and write the feature `spec` document in `docs/specs/`.
* Define test cases and map out how to achieve 90%+ code coverage.
* Review the architecture to ensure high cohesion and low coupling.

### 3.2 Phase 2: Coding (Implementation)
* Write the code according to the Spec, checking against the defined boundary conditions as you implement each component.
* Write local test code (note that test files are kept local and **not committed** to Git; see the [Testing guidelines](file:///d:/AntigravityProjects/ElsevierTrackor/.claude/rules/testing.md)).

### 3.3 Phase 3: Testing
* Run unit and integration tests locally, check test reports, and review code coverage.
* If coverage is below 90% or a boundary test fails, optimize the implementation until all tests pass.

### 3.4 Phase 4: Acceptance
* Execute smoke tests (Smoke Test) or perform manual end-to-end user flow verification.
* Verify extreme scenarios and boundary states to guarantee that the system behavior aligns perfectly with the Spec definition.

### 3.5 Phase 5: PR & Release

> [!WARNING]
> **Push 触发 CI/CD 自动部署**：任何 `git push` 到远端仓库都会触发 GitHub Actions，自动打包并重启生产服务器。**严禁在用户明确确认本地验收通过前执行 push。**

标准流程：
1. 开发完成后立即 `git commit`（本地提交，**不 push**）。
2. 用户在本地完成验收测试（Smoke Test / 真机调试）。
3. 用户明确说"可以提交" / "push" / "发 PR"后，执行 `git push` 并创建 Pull Request。
4. 在远端仓库完成 Code Review，通过后合并到 main 分支，CI/CD 自动部署。

---

## 4. Command Execution and Scripting Standard (Unified Shell)

To ensure cross-platform compatibility and maintain clean scripts across local development and CI/CD environments, all commands, documentation references, and shell scripts **must be unified on Bash**.

### 4.1 Shell Selection: Bash over PowerShell

| Aspect | Bash (Unified Standard) | PowerShell |
| :--- | :--- | :--- |
| **Cross-Platform** | Yes (Linux, macOS, Windows via Git Bash/WSL) | Primarily Windows-focused (requires installation on Linux/macOS) |
| **CI/CD Compatibility** | Native (Default runner shell on GitHub Actions/GitLab CI) | Requires explicit runner setup |
| **Syntax** | POSIX-compliant, industry standard for web/Go | Verbose, object-oriented syntax |

### 4.2 Guidelines for Commands

1. **Path Separators**: Always use forward slashes (`/`) for paths (e.g., `cd backend/cmd/server`), which Bash interprets correctly on all platforms. Avoid backslashes (`\`).
2. **Environment Variables**: Reference variables using the `$VARIABLE_NAME` syntax (e.g., `APP_ENV=prod go run main.go`) instead of `$env:VARIABLE_NAME`.
3. **Common Utilities**: Use standard GNU/Linux commands (e.g., `rm -rf`, `mkdir -p`, `grep`, `cat`) instead of PowerShell cmdlets (`Remove-Item`, `New-Item`, `Select-String`).
4. **Local Execution on Windows**: Developers working on Windows must run commands inside a Bash environment (e.g., Git Bash, VS Code integrated Git Bash terminal, or WSL) to ensure they match documented instructions.

---

## 5. Documentation Synchronization Rules

To prevent codebase implementation from diverging from reference materials, all code modifications must adhere to this rule:
* **Synchronous Updates**: Whenever any code is modified, you must synchronously update all affected or involved documentation in the same change/commit. This includes but is not limited to:
  * Key overview documents like [CLAUDE.md](file:///d:/AntigravityProjects/ElsevierTrackor/CLAUDE.md).
  * Specifications in the `docs/specs/` directory.
  * API documentation, configuration examples, and database schema definitions.
  * README files and code comments detailing changed logic.
* **Consistency Check**: Before committing or opening a PR, verify that all modified or new paths, settings, parameters, and behaviors are fully documented.



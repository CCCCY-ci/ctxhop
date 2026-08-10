# AI Agent Tooling and Integration Standards (Tooling Rules)

This document defines the rules for tool selection, browser automation, and AI workflows within the ElsevierTrackor project. These rules ensure maximum efficiency, prevent tool duplication, and protect the development flow.

---

## 1. Tool Prioritization

When performing file searches, repository analysis, or interacting with remote services, you must prioritize the tools already configured in the environment. Do not install alternatives or write redundant scripts.

* **Code Search**: Always use **Ripgrep** (`rg` / the native `grep_search` tool) for fast pattern matching across directories. Avoid writing custom Node/Python scripts for parsing files.
* **Code Navigation**: Utilize the **CodeGraph** index for semantic navigation and code relationship mapping.
* **GitHub Interoperability**: Use the **GitHub CLI** (`gh`) for git-host interactions (e.g., creating PRs, checking issue status, managing releases) instead of calling the GitHub API manually via scripts.
* **Pre-existing Skills**: Prioritize using the installed project skills (e.g., document parsing, automated art, data extraction tools) when executing related workflows.

---

## 2. Browser Automation Constraints (Strict Chrome MCP Only)

To prevent environment pollution and ensure execution stability, we restrict browser-based tasks to a single, verified toolset:

> [!WARNING]
> **When a web browser is required (for page navigation, screenshot capture, DOM parsing, or frontend testing), you are ONLY permitted to use the Chrome DevTools MCP server tools.**

* **Prohibited Tools**: Using **Playwright** (or any other standalone headless browser driver/library) is **strictly forbidden** for browsing operations in this repository.
* **Approved Workflow**: Interact with the locally running Chrome instance via the devtools MCP tools (e.g., `mcp__chrome-devtools__navigate_page`, `mcp__chrome-devtools__evaluate_script`, `mcp__chrome-devtools__take_screenshot`).

---

## 3. Code Review Guidelines

Code reviews must be used strategically to protect developer productivity and maintain a high-quality main branch.

* **Code Review Timing**: Code reviews must be performed **at the Pull Request (PR) stage, immediately before merging** into the main branch.
* **Prohibited Timing**: **Do not** run code review plugins, workflows, or trigger comprehensive code reviews during active coding (Phase 2 of the development lifecycle). Doing so disrupts implementation flow and generates noisy feedback.
* **Objective**: Focus reviews on structural design, conformance to code styles, error handling correctness, and test coverage verification before final integration.

---

## 4. UI/UX Design & Aesthetics (ui-ux-pro-max)

* **UI/UX Design Skill**: When designing frontend components, mockups, dashboards, or addressing layout aesthetics, you are encouraged to use the **ui-ux-pro-max** skill.
* **Aesthetic Standard**: Use it to enforce modern layout rules, responsive design systems, and visually striking components matching the project's premium design goals.

---

## 5. Multi-Agent Development and Progress Reporting Standards

To ensure systematic and controlled execution of complex coding tasks, development should follow a hierarchical multi-agent collaboration model:
* **Role Division**:
  * **Main Agent (Coordinator)**: Acts as the project manager and architect. Leads the planning, sets direction, designs specs, controls progress, and performs final verification. The Main Agent does not directly write heavy implementation code, leaving concrete execution to sub-agents.
  * **Sub-Agents (Executors)**: Dedicated to specific, scoped coding, research, or testing tasks as delegated by the Main Agent.
* **Coordination & Progress Monitoring**:
  * **Sub-Agent Delegation**: Delegate concrete implementation tasks to specialized sub-agents with clear, decoupled instructions and narrow scope.
  * **Periodic Progress Reporting**: The Main Agent must actively monitor sub-agents' status and periodically report execution progress and milestones back to the user, keeping the workflow transparent and under control.

---

## 6. Guidelines for User Consultation and Decision Making (Interactive Clarification)

To resolve design ambiguities, clarify underspecified requirements, or assist the user when they seek recommendations, you must leverage the `ask_question` tool:
* **Trigger Conditions**: Call the `ask_question` tool in the following scenarios:
  * When critical implementation details or design options require user feedback/selection.
  * When the user's requirements are ambiguous or key choices are open-ended.
  * When the user explicitly requests recommendations or lacks initial ideas for a solution.
* **Best Practices**:
  * Formulate concise, distinct options written as the user's direct response (rather than describing the agent's actions).
  * If the agent has a recommendation, list it first and prefix it with `(Recommended)`.
  * Avoid using the tool for trivial yes/no questions; use standard text communication instead.




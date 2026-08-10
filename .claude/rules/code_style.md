# Multi-Language Coding Style and Architecture Guidelines (Coding Standards)

This document defines the coding styles and architectural design conventions for the ElsevierTrackor project. These rules enforce **high cohesion, low coupling**, **readability**, **reusability**, and **maintainability**.

---

## 1. Core Architecture Design Principles

* **High Cohesion, Low Coupling**: Each module, package, or component must focus on a single responsibility. Reduce direct dependencies between components and interact using clean, well-defined interfaces or APIs.
* **Reusability and Readability**: Avoid duplicate code (DRY - Don't Repeat Yourself). Use self-explanatory naming conventions for variables, functions, and classes. Document complex algorithms or custom business logic with clear comments.
* **Maintainability**: Keep logic simple and easy to reason about. Favor composing small, single-purpose functions instead of writing long, monolithic routines.

---

## 2. Go Backend Coding Standards (Go Backend Guidelines)

The backend is built using **Go 1.22 + Gin + GORM** and follows Clean Architecture principles.

### 2.1 Directory Structure & Layering

Code is organized into separate layers, with dependency flow strictly one-way (outer to inner):

* **Controller / Handler Layer** (`internal/api/handler/`): Responsible for parsing incoming HTTP requests (input binding and validation) and writing HTTP responses. No business logic belongs in this layer.
* **Usecase / Service Layer** (`internal/usecase/` or `internal/service/`): Coordinates core business logic. Communicates with external subsystems (databases, caches, third-party APIs) through abstract interfaces.
* **Repository / Model Layer** (`internal/model/` or `internal/db/`): Manages data persistence operations and direct database queries. Database entity models are defined in this layer.

### 2.2 Coding Standards
1. **Error Handling**:
   * **Never ignore errors**: Writing statements like `_ = db.Save(&user)` is strictly forbidden.
   * **Explicit error propagation**: Always return errors to the calling function. Wrap errors using the `%w` verb to maintain the context of the error:
     ```go
     if err != nil {
         return fmt.Errorf("failed to fetch paper %d: %w", paperID, err)
     }
     ```
2. **Concurrency & Thread Safety**:
   * Any resource shared across multiple goroutines (e.g., in-memory maps, rate-limiting caches) must be protected by a `sync.Mutex` or `sync.RWMutex`, or synchronized using Channels.
   * All asynchronous goroutines must receive a `context.Context` to handle graceful timeouts and cascading cancellation.
3. **Database Operations (GORM)**:
   * Sensitive information (e.g., scraping Tracking Links) must be encrypted using GORM hooks (Encryption Hooks) before being written to the database.
   * Multi-statement database operations must be wrapped in transactions using `db.Transaction(func(tx *gorm.DB) error { ... })` to ensure ACID compliance.

---

## 3. React / TypeScript Coding Standards (Admin Dashboard Guidelines)

The admin panel is built using **React 18 + TypeScript + Vite + Vanilla CSS**.

### 3.1 Type Safety
* **Avoid `any`**: All data structures, API responses, and component Props must have explicit `interface` or `type` definitions. If a type is unknown, use `unknown` and perform runtime type checks.
* Enable TypeScript's `strict` mode to prevent common `null` and `undefined` runtime reference errors.

### 3.2 Component Design & State Management
* **Single Responsibility**: If a React component grows beyond 250 lines or manages too many unrelated states, decompose it into smaller sub-components.
* **Custom Hooks**: Extract complex stateful logic and side effects (e.g., API requests, polling timers) from components into custom Hooks (e.g., `usePaperStatus`) to keep rendering logic clean.
* **Style Isolation**: Structure Vanilla CSS using modular naming conventions or CSS Modules to prevent global class selector pollution.

---

## 4. WeChat Mini-Program Coding Standards (Frontend Guidelines)

The frontend mini-program uses native **JavaScript (Glass-easel component framework) + CSS**.

### 4.1 Core Guidelines
* **Global State Isolation**: Do not store temporary variables in the global `globalData` object in `app.js`.
* **Lifecycle Awareness**: Perform initialization and cleanup tasks in the appropriate page or component lifecycle callbacks (e.g., `onLoad`, `onUnload`, `attached`, `detached`) to prevent memory leaks.
* **API Wrapper**: All network requests must pass through a unified Request wrapper. Centralize logic for token expiration, network error handling, and error toast alerts.

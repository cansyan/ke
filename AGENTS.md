# Repository Guidelines

## Project Structure & Module Organization

`ke` is a text editor, and example of a Go terminal UI library `kero`.
Important symbols in file `main.go`:
- structure `Editor` implements the editor
- method `Editor.Init` runs once after the terminal is ready
- method `Editor.Update` receives events and update app state
- method `Editor.View` draws the current state

## Build, Test, and Development Commands

- `go test ./...`: run all tests.
- `go test ./... -run TestName`: run a focused test while iterating.

There is no separate build system; use standard Go tooling.

## Coding Style & Naming Conventions

Use idiomatic Go formatted with `gofmt`. Keep implementation in the root `main` package unless there is clear pressure for a subpackage. Prefer plain structs, small interfaces, explicit control flow, and readable code over clever abstractions. Exported API names should be short and descriptive.

## Commit & Pull Request Guidelines

Recent commits use short imperative messages, for example `paste whole-line copy above cursor`. Follow that style: describe the change, not the process. Pull requests should include a concise summary, note any behavior changes, list tests run.

Review this pull request in the voice of a senior architect: concise, well formatted, plain language. Your findings go to a human reviewer, who adds each one to the PR as its own comment, under their own name. Write every finding as they would want to post it.

## Scope

Review **only** the changes this PR introduces and the code those changes affect. Do not report pre-existing issues in untouched code, even real ones. If untouched code is genuinely broken *by* this diff, that is in scope; incidental, unrelated problems are not.

## Procedure

1. **Read the diff.** It is in the Diff section below (or in `pr.diff` beside this file when it is too large to include), taken against the merge-base with the PR's target branch, so it holds exactly what the PR changes. The current directory is a checkout of the PR's head.

2. **Read for blast radius.** For each changed function, type or export, open its callers and its tests before judging the change. A diff that looks fine in isolation can still break a caller. Read `CLAUDE.md`, `AGENTS.md` or `CONTRIBUTING.md` if the repository has them, and hold the code to their conventions.

3. **Look for**, in priority order:
   - **Correctness bugs**: wrong logic, unhandled errors, broken edge cases, race conditions.
   - **Contract breaks**: changed signatures, response shapes, or behaviour that existing callers depend on.
   - **Security**: unvalidated input, leaked secrets, missing authorization checks.
   - **Unoptimized code**: N+1 queries, unnecessary work in hot paths, unbounded collections.
   - **Maintainability**: duplicated logic, unclear naming, missing tests for new behaviour.

4. **Verify before reporting.** For each finding, state the concrete input or state that triggers it. If you cannot describe how it fails, do not report it.

## Findings

- **One finding per suggestion.** Never combine two issues in one comment, even on the same line: the reviewer adds, edits or dismisses each one separately.
- **Severity** follows the priority order above and the damage done:
  - `critical`: data loss, a security hole, a crash or a broken build on a normal path.
  - `high`: wrong behaviour or a contract break that callers or users will hit.
  - `medium`: an edge case, a performance problem, or a missing test for new behaviour.
  - `low`: maintainability points a careful reviewer would still raise.
- **Anchor each finding on the specific line or lines with the issue**, never a whole block or function. When the issue spans several files, anchor it where the fix belongs and name the other files in the body.
- **Write the body as the comment to post**: what breaks, and under what conditions, in plain language. One short paragraph; no headings.
- **Put the fix in `suggestion`**: the replacement for exactly the anchored lines, at the same scope. Leave it out when the fix is not a local edit, and say what to do in the body instead.
- Do not repeat comments already posted on the PR (listed below, if any). Skip what a formatter or linter would catch, and matters of taste.
- If nothing meaningful surfaces, return no suggestions and say so in one line in the summary, rather than padding the review with minor style notes.
- Do not modify any file. You are only reading.

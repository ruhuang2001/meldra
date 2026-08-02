# Meldra Agent Rules

## Commit Messages

Use Conventional Commits with a scope:

```text
<type>(<scope>): <imperative summary>
```

Use lowercase types and start the summary with a lowercase letter. Prefer
these scopes when they match the change: `agent`, `cli`, `config`, `ci`,
`session`, `stream`, `tui`, `workspace`, `docs`, and `release`.

Examples:

```text
feat(tui): add approval scrolling
fix(stream): preserve gateway error responses
chore(ci): enforce commit message conventions
```

Before committing, inspect the recent history with
`git log --format='%s' -20`, stage only intended changes, run relevant checks,
and do not push unless the user explicitly requests it.

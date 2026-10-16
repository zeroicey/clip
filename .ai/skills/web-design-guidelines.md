---
name: web-design-guidelines
description: 'Review UI code for Web Interface Guidelines compliance. Use when asked to "review my UI", "check accessibility", "audit design", "review UX", or "check my site against best practices".'
---

Review UI code against the Web Interface Guidelines and report findings as a terse
`file:line` list. A review that only says "looks good" is a failed review.

ARGUMENTS: `<ARGUMENTS>` is a file, directory or glob to review.

## Source of truth

The rules are **fetched fresh on every review**, never recalled from memory — they change:

```
https://raw.githubusercontent.com/vercel-labs/web-interface-guidelines/main/command.md
```

Canonical mirror of the skill: `.agents/skills/web-design-guidelines/SKILL.md`.
The fetched document also defines the output format; follow it.

## Procedure

1. Fetch the guidelines from the URL above.
2. Read every file in scope. Do not review a file from its name.
3. Check each rule against the code. Rules are grouped: Accessibility, Focus States, Forms,
   Animation, Typography, Content Handling, Images, Performance, Navigation & State,
   Touch & Interaction, Safe Areas & Layout, Dark Mode & Theming, Locale & i18n,
   Hydration Safety, Hover & Interactive States, Content & Copy.
4. Output `file:line - issue`, grouped by file. State the issue and location; skip
   explanation unless the fix is non-obvious. A file with no findings is `✓ pass`.
5. **Forbidden:** a bare "no issues found" with no rules actually checked. Report the rules
   you verified, especially the ones that passed only because someone remembered them.

## Interaction with this repository

- The project's own rules in `AGENTS.md` **outrank** this skill. A generic guideline that
  contradicts a hard constraint is reported as "not applicable, here is why", not as a finding.
- Rules that do not apply to a Vue 3 SPA with no framework meta-framework (Hydration Safety,
  `nuxt`/`next` specifics) should be skipped explicitly, not silently.
- Anything touching the share page, CSP, or the response envelope in
  `backend/internal/views/` must be cross-checked against `.ai/decisions/` first.

# `scripts/test-pg.sh` truncates `relay_test`, so a later UI probe silently tests empty pages

**Date:** 2026-10-16 · **Where:** `scripts/test-pg.sh` → `internal/clip/*_test.go` (`testStore`)

## Symptom

During the UI reskin I seeded `/tmp/ui-shots` fixtures by hand (`uilocked` = a clip **with a
password**, `uipretty` = a clip with content + an image) and then ran a DOM probe that navigated to
`/uilocked` to measure the **locked shell**. The probe reported the *unlocked* tab order
(`textarea#body > button#save` …) and `13 pairs, 0 failing` — a green result.

Nothing was red. The numbers were real. They were just about the wrong page: `/uilocked` no longer
existed, so the server rendered a **brand-new empty suffix** (`data-state="new"`,
`has_password=false`, `exists=false`), which is a perfectly valid page that passes every check.

The give-away was only the tab order: a locked page's first stop after the brand is `input#pw`, and
that stop was missing.

## Root cause

`scripts/test-pg.sh` runs the Go suite against the **same database** the manual probe uses
(`TEST_PG_DSN=…/relay_test`). The store tests clean up by deleting the rows they touched, so running
the suite **wipes the fixture data** a hand-seeded probe depends on. The two tools share one database
and neither knows about the other.

This is not a bug in either tool — it is an **ordering hazard**, and its failure direction is silent
because "suffix does not exist" is a legitimate 200 page in this service by design
(the "unknown suffix = empty editor" rule). Any probe that only asserts *"the page is fine"* will
pass on the empty page.

## Fix / guard

1. **Run order matters.** `test-pg.sh` **last**, after any seeded UI/DOM probe; or re-seed fixtures
   *after* it. Verified sequence that works:
   `restart server (clears the in-process write limiter) → seed → DOM probe → verify-clip-browser → test-pg.sh`.
2. **Probe must assert on state, not just on health.** A locked-state probe should assert the
   *first tab stop* is the secret input (or `document.body.dataset.state === "locked"`) — an
   assertion that cannot be satisfied by an empty page.
3. Re-seeding is idempotent-hostile: `PUT /api/clips/<suffix>` with a password returns **401** when
   the suffix already exists with a password. A `401` here means "fixture already correct", not
   "seed failed" — do not read it as an error.

## Guard worth writing if this is ever automated

Any future probe script should print the rendered `data-state` it measured and fail when the expected
state was not observed. The rule this repo already applies elsewhere — *assert the status explicitly,
because the body can look perfect* (`TestPublicPagesAreNot404`) — applies verbatim to fixtures.

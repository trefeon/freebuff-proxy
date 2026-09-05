# ADR-0015 — `<tool_calls>` plural dialect extracts like singular

Status: Accepted

Context: some models emit the plural wrapper `<tool_calls>…</tool_calls>` around the same `<function=…>/<parameter=…>` (or JSON) payload the singular `<tool_call>` carries. The extractor only knew singular/pipe/fence forms, so plural blocks leaked as literal `<tool_calls>` text to the harness and never became tool calls (observed live in the operator console 2026-09-06).

Decision: treat the plural wrapper as a first-class dialect — new `xmlShapeToolCalls` stream shape (opener, closer, split-opener withholding, dangling-tag scrub) plus the fifth block-regex group on the non-stream path. Payload parsing (function heads, params, JSON) is shape-agnostic and unchanged. A new dialect is added by extending the shape table + regex, never by forking the parser.

Reasoning: models will keep inventing wrapper spellings; the stable core is the payload grammar. Dialect handling belongs in exactly one table (openers) + one regex (blocks) + one scrub list (dangling tags) — three coordinated edits, all pinned by tests.

Alternatives considered: stripping unknown `<tool_*>` tags as text (destroys real tool calls); mapping plural→singular by string replace before parsing (fragile across split fragments; the shape table handles splits natively).

Consequences: any future wrapper dialect follows the same three-edit recipe with three tests (non-stream extract, split-opener stream, dangling flush). `danglingToolTagsRe` must gain each new tag or `Flush()` leaks it as literal text.

Invariants: every recognized opener has a closer, a split-opener entry, and a dangling-scrub entry; unparseable blocks flush as plain text, never dropped.

Affected packages: `internal/convert` (accumulator_xml.go).

Related tests: `TestExtractXMLToolCallsPluralDialect`, `TestXMLStreamExtractorPluralDialect`, `TestXMLStreamExtractorPluralDanglingFlush`.

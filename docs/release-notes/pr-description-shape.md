# Pull-request description compatibility

Restores the default Azure DevOps and GitHub description layout used by the
v1.84.0 Mosaiq pull requests: recorded Intent, What Changed, generator credit,
marked Risk Assessment, and the attributed, attested one-line-per-step Pipeline.
Recorded Testing evidence remains available under its own heading. Explicit
template and appendix-mode configuration retain their author-ownership rules.

Intent is never paraphrased by the drafting agent. When the forge description
limit is exceeded, the renderer reserves space for the other sections and
compacts Intent at a complete sentence, noting that the full intent is in the
first PR comment. If even one sentence does not fit, the note replaces the
preview. The full intent comment is posted immediately after creation, before
work-item linking; repeated publication does not duplicate an identical comment.
Publication privacy controls and home-path/secret redaction apply to comments
as well as descriptions. Large Testing embeds may still be shed to fit Azure's
4000 UTF-16-unit limit.

The primary `AB#` reference in the published intent or branch name becomes a
parenthesized title suffix; all extracted references are explicitly linked in
Azure DevOps. GitHub retains missing references as a What Changed bullet, since
Azure Boards links from descriptions, not titles or comments. Azure Boards must
be connected to that GitHub repository for the integration to create the link.

History comparison: `4822244` (v1.84.0) against `ddf8257` showed that the core
renderer already retained Risk Assessment, source attribution, and attestation;
the newer appendix modes did not change the default full layout. The reference
descriptions were read directly with `az repos pr show` because the browser
redirected to Microsoft sign-in. Deterministic generator attribution, compact
step rows, work-item suffixes, and overflow-comment publication now remove the
remaining reliance on external post-processing.

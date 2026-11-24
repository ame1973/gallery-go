# Shared search text profile

GalleryGo has a versioned text-processing primitive for future server and local
search. **It is not a search API, an index, or an offline-search feature.** A
normalized string or token is never an authorization grant. Future search queries
must filter candidate resources with the same fresh visibility rules as other reads.

## Profile `gallerygo-search-v2-u15`

The shared profile uses Unicode 15.0.0 data from the checked-in
`contracts/search-normalization-tables.json`. It applies NFKC, full default case
folding, a second NFKC, and Unicode 15 whitespace trimming, in that order. It is
not locale-specific lowercase. Unicode scalars unassigned in this profile remain
unassigned for classification even if a newer runtime recognizes them.

Both implementations use the pinned Unicode15 decomposition, combining and
composition tables, including arithmetic Hangul and stable canonical ordering. Go
uses the locked `golang.org/x/text` full-fold implementation and checks its
Unicode version; the browser uses the corresponding shared fold table. Neither
relies on native browser normalization or x/text's stream-safe CGJ insertion for
long combining runs. Categories and whitespace also use the pinned profile. An
incompatible Go profile fails closed.

Inputs must be valid UTF-8, contain no NUL, and be at most 16 KiB. Browser strings
with unpaired UTF-16 surrogates are rejected. Intermediate and normalized text is
limited to 64 KiB. Each normalization stage admits at most six decomposed scalars
per input byte, at most 393,216 scalars overall; the checked-in Unicode15 mappings
are validated against that bound. Query tokenization returns at most 2,048 unique
tokens. Document tokenization has a separate 131,072-token ceiling, twice the
normalized byte budget, so valid bounded source titles are not dropped merely
because they exceed the query budget. With these Unicode15 tables, a separate
expansion-invariant test bounds each normalization intermediate and output for a
4,096-byte source title to 45,056 bytes. This does not establish a complete search
index or an offline authorization grant. These bounds and semantics are versioned;
changing them requires an explicit index migration.

Tokenization retains the normalized text, deduplicated word tokens, individual Han
characters, and adjacent Han bigrams in first-seen order. Punctuation remains in
normalized text and breaks word/Han runs. A word-token match does not by itself
establish a literal punctuation match. These primitives do not yet define ranking,
query syntax, filtering, pagination, FTS candidate selection or the complete online
and offline search contract.

## Conformance and changes

`make search-conformance` checks table regeneration, Go fixtures, every valid
Unicode scalar, all five columns from the 19,074 rows of Unicode 15's official
normalization test, additional deterministic Go-generated multi-scalar cases, and
Chromium on desktop and mobile-sized projects. The compressed reference fixture is
`tests/fixtures/unicode15-normalization.txt.gz`; the SHA-256 of its uncompressed
contents is `fb9ac8cc154a80cad6caac9897af55a4e75176af6f4e2bb6edc2bf8b1d57f326`.
Its source is <https://www.unicode.org/Public/15.0.0/ucd/NormalizationTest.txt>.
The additional 537 long-segment and expansion-limit cases in
`tests/fixtures/search-long-unicode15.json.gz` were independently generated with
Python 3.12.3's Unicode 15.0.0 tables; their uncompressed SHA-256 is
`b04087381843c70ff22239bd71e92645318a39c9d2af76d0e6c89077303c04b5`.
`tests/searchconformance/generate_long_reference.py` regenerates those inputs when
run with Unicode15 Python; the Go reference reader checks the digest and result
before browser comparisons. These cases supplement, rather than replace, the
official Unicode conformance data.
The runner uses newly allocated temporary files,
keeps them on failure, and runs the locked local Go toolchain; install the pinned
Go/Node/pnpm toolchains and Playwright Chromium first. This does not test physical
devices or every possible multi-scalar sequence.

The category ranges were generated against Go's Unicode 15 tables. When that exact
runtime data is available, Go tests compare every valid scalar against those ranges;
otherwise that particular comparison is skipped rather than substituting newer
categories. The generator treats the checked-in category ranges as versioned input
and regenerates the remaining tables and digest from the pinned `x/text` release.
Updating Unicode or `x/text` requires a new profile and independently reviewed
reference data, fresh cross-runtime conformance, and an explicit rebuild/version
migration of all online and offline indexes before old and new data are mixed.

The generated profile is derived from Unicode data and the Go and `x/text` tables.
See the accompanying [Unicode license](licenses/UNICODE.txt) and
[Go authors' license](licenses/GO-TEXT.txt). The Unicode 15 release describes its
terms at <https://www.unicode.org/Public/15.0.0/ucd/ReadMe.txt>; the included
Unicode license is from <https://www.unicode.org/license.txt>.

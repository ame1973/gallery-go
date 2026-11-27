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
complete query syntax, filtering, pagination or the online/offline search contract.

## Bounded server literal verification

`internal/search.CompileLiteral` normalizes a nonempty literal and compiles a
reusable prefix table. `ContainsNormalized` then verifies the whole literal against
one already-normalized document in linear byte time, preserving punctuation. It
rejects invalid UTF-8, NUL and documents over the 64 KiB normalized budget; it
cannot verify that the caller used the correct profile or current document revision.
The table uses at most four bytes per normalized query byte, in addition to the
normalization buffers and query string. A compiled pattern may be shared across
concurrent calls. The caller must separately bound the number of candidates and
honor request cancellation between them.

This server-only helper is not an FTS index, endpoint, ranking rule, permission
check, or a complete online/offline query definition. Applying a page limit before
full verification can miss matches. Future integration must filter visibility in
SQL, verify index freshness and account for cancellation and bounded work; it must
not return partial results as complete.

## FTS candidates for one literal

The server-only `LiteralPostingsVersion` projection encodes every normalized Unicode
scalar and adjacent pair, including punctuation, as deduplicated ASCII hex terms.
`CompileLiteralCandidates` selects the single scalar or the first and last adjacent
pairs as **necessary** anchors. Terms are at most 17 ASCII bytes, avoiding SQLite's
long-token truncation; the generated expression must be bound as a MATCH parameter,
not inserted into SQL. The document postings string is bounded to 2,359,296 bytes,
besides normalization buffers and the deduplication map. This is an additional
candidate projection, not the shared word/Han token list used above.

An anchor hit is not a literal hit: candidates can be false positives and the
complete normalized literal must be checked with `ContainsNormalized`. One- and
two-character CJK literals do not rely on trigrams. This mechanism does not maintain
or migrate an index, enforce authorization or revision freshness, define the complete
query/ranking/filter contract, establish online/offline equivalence, or guarantee
performance for broad/adversarial queries. Do not limit before all necessary
verification or present a timed-out partial scan as a complete result.

`make search-candidates-load` runs a synthetic 100,000-document adversarial candidate
scan three times with Go's race detector and a 10-second query deadline, using only
temporary SQLite data. Setup creates about 400 MiB of normalized text outside that
query deadline. The fixture checks all coarse matches before finding one literal;
it does not exercise authorization, index maintenance, ranking, pagination, API,
network, NAS latency or the product P95 target.

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

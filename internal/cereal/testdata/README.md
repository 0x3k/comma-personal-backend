# Cereal log fixtures

The parser tests build their fixtures in-process rather than checking in a
binary rlog sample. This keeps the repo small, dodges any concern about
redistributing real driver data, and guarantees the fixture layout tracks
this package's schema generation.

## Rebuilding a fixture manually

If you want to poke at a fixture outside the test runner, something like
this produces the same bytes the unit tests use:

```go
package main

import (
    "compress/bzip2"  // read only; we use github.com/dsnet/compress/bzip2 for write
    "os"
    "time"

    capnp "capnproto.org/go/capnp/v3"
    "comma-personal-backend/internal/cereal/schema"
)

// See parser_test.go for the canonical builder -- this comment exists so a
// future engineer can follow the breadcrumbs.
```

The canonical builder is `buildFixture` in `parser_test.go`. It emits an
uncompressed Cap'n Proto stream of a handful of `Event`s spanning
`carState`, `controlsState`, and `selfdriveState`, which exercises every
branch of the signal extractor.

## Why no Python script?

openpilot ships a Python `capnp` API that can synthesize rlogs trivially;
we don't use it here because (a) our test infrastructure is pure Go and
(b) sharing a builder with production code (the generated schema bindings)
catches schema drift the moment it happens.

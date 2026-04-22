// Package cereal parses openpilot rlog / qlog files into Go values.
//
// An rlog (or qlog) is a stream of Cap'n Proto messages in the cereal schema.
// The raw on-disk form is a concatenation of framed messages; the segment
// files on a device are bz2-compressed. This package exposes a streaming
// Parser that decodes one Event at a time without buffering the whole file,
// plus a higher-level SignalExtractor (see signals.go) that yields the
// time-aligned driving signals downstream features need.
//
// # Schema bindings
//
// The Go bindings in internal/cereal/schema are generated from openpilot's
// cereal/log.capnp (and the imported car.capnp / legacy.capnp / custom.capnp).
// The exact regeneration command, from the schema directory, is:
//
//	capnp compile \
//	    -I $(go env GOMODCACHE)/capnproto.org/go/capnp/v3@v3.1.0-alpha.2/std \
//	    --src-prefix=. -ogo:. log.capnp car.capnp legacy.capnp custom.capnp
//
// This requires:
//   - capnp  (the Cap'n Proto compiler, v1.x)
//   - capnpc-go  (`go install capnproto.org/go/capnp/v3/capnpc-go@latest`)
//
// Upstream log.capnp lives at ../../openpilot/cereal/log.capnp. The
// openpilot tree symlinks car.capnp into opendbc_repo; this repo vendors the
// resolved file so schema generation doesn't require a working openpilot
// checkout.
package cereal

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"errors"
	"fmt"
	"io"

	capnp "capnproto.org/go/capnp/v3"

	"comma-personal-backend/internal/cereal/schema"
)

// Event is the top-level cereal union. It's an alias over the generated
// schema type so callers can import a single package.
type Event = schema.Event

// Parser decodes a cereal log stream (rlog or qlog, optionally bz2-wrapped)
// into individual Event values. It does not buffer the whole file; each
// message is decoded on demand and handed to the caller's handler.
type Parser struct {
	// MaxMessageSize, if non-zero, caps the per-message size in bytes. This
	// matches capnp.Decoder.MaxMessageSize and guards against truncated or
	// malicious inputs. Zero uses the go-capnp default.
	MaxMessageSize uint64

	// TraverseLimit caps the number of bytes a single Event can traverse
	// while reading (defends against amplification attacks per the Cap'n
	// Proto docs). Zero uses the go-capnp default (64 MiB).
	TraverseLimit uint64
}

// bzip2Magic is the prefix of any bz2 stream ("BZh" + block size digit).
var bzip2Magic = []byte{'B', 'Z', 'h'}

// zstdMagic is the prefix of any zstd stream. We detect it so we can return
// a clear error rather than feeding garbage into the Cap'n Proto decoder;
// openpilot has started emitting .zst logs in some versions.
var zstdMagic = []byte{0x28, 0xB5, 0x2F, 0xFD}

// ErrZstdUnsupported is returned when the input looks like a zstd stream.
// zstd support can be added by depending on a pure-Go zstd decoder; we keep
// this package bz2-only for now to avoid the extra dependency.
var ErrZstdUnsupported = errors.New("cereal: zstd-compressed logs are not supported yet")

// Parse reads Cap'n Proto Event messages from r and invokes handler for each
// one in order. If handler returns a non-nil error, parsing stops and that
// error is propagated. Parse transparently detects and decompresses bz2
// streams by inspecting the leading magic bytes.
//
// The underlying *capnp.Message is released after the handler returns -- do
// not retain references to the Event (or any pointer field of it) across
// handler invocations. Copy out the primitive fields and Text/Data payloads
// you care about inside the callback.
func (p *Parser) Parse(r io.Reader, handler func(Event) error) error {
	if handler == nil {
		return errors.New("cereal: Parse requires a non-nil handler")
	}

	// Peek just enough bytes to detect bz2/zstd framing without consuming
	// any data from the stream the decoder will see.
	br := bufio.NewReader(r)
	head, _ := br.Peek(4)
	switch {
	case bytes.HasPrefix(head, bzip2Magic):
		// bzip2.NewReader is streaming and decompresses incrementally.
		r = bzip2.NewReader(br)
	case bytes.HasPrefix(head, zstdMagic):
		return ErrZstdUnsupported
	default:
		r = br
	}

	dec := capnp.NewDecoder(r)
	if p.MaxMessageSize > 0 {
		dec.MaxMessageSize = p.MaxMessageSize
	}

	for {
		msg, err := dec.Decode()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("cereal: decode message: %w", err)
		}
		if p.TraverseLimit > 0 {
			msg.TraverseLimit = p.TraverseLimit
		}
		evt, err := schema.ReadRootEvent(msg)
		if err != nil {
			msg.Release()
			return fmt.Errorf("cereal: read root event: %w", err)
		}
		if err := handler(evt); err != nil {
			msg.Release()
			return err
		}
		msg.Release()
	}
}

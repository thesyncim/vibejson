// Package storeio implements the durable page/device engine behind
// store/durable: the superblock, the common page envelope, every directory
// and leaf page kind, checksums, and the copy-on-write commit protocol. The
// on-disk byte format this package reads and writes is specified in
// /docs/format.md; that document is the canonical reference and should stay
// in sync with this package's encode/decode functions.
package storeio

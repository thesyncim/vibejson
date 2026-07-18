//go:build go1.27

package simdjson

const useStableNumericMethods = false

// These entry points only type-check the compile-time-disabled stable branch
// in common code. The linker drops them from Go 1.27 binaries.
func decoderCursorInt[T signedInteger](c *decoderCursor, dst *T) error { return c.Int(dst) }
func decoderCursorUint[T unsignedInteger](c *decoderCursor, dst *T) error {
	return c.Uint(dst)
}
func decoderCursorFloat[T floatValue](c *decoderCursor, dst *T) error { return c.Float(dst) }

func (c *decoderCursor) IntNative(dst *int) error   { return c.Int(dst) }
func (c *decoderCursor) Int8(dst *int8) error       { return c.Int(dst) }
func (c *decoderCursor) Int16(dst *int16) error     { return c.Int(dst) }
func (c *decoderCursor) Int32(dst *int32) error     { return c.Int(dst) }
func (c *decoderCursor) Int64(dst *int64) error     { return c.Int(dst) }
func (c *decoderCursor) UintNative(dst *uint) error { return c.Uint(dst) }
func (c *decoderCursor) Uint8(dst *uint8) error     { return c.Uint(dst) }
func (c *decoderCursor) Uint16(dst *uint16) error   { return c.Uint(dst) }
func (c *decoderCursor) Uint32(dst *uint32) error   { return c.Uint(dst) }
func (c *decoderCursor) Uint64(dst *uint64) error   { return c.Uint(dst) }
func (c *decoderCursor) Uintptr(dst *uintptr) error { return c.Uint(dst) }
func (c *decoderCursor) Float32(dst *float32) error { return c.Float(dst) }
func (c *decoderCursor) Float64(dst *float64) error { return c.Float(dst) }

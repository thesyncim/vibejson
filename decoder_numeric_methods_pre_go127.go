//go:build !go1.27

package simdjson

const useStableNumericMethods = true

func (c *decoderCursor) IntNative(dst *int) error   { return decoderCursorInt(c, dst) }
func (c *decoderCursor) Int8(dst *int8) error       { return decoderCursorInt(c, dst) }
func (c *decoderCursor) Int16(dst *int16) error     { return decoderCursorInt(c, dst) }
func (c *decoderCursor) Int32(dst *int32) error     { return decoderCursorInt(c, dst) }
func (c *decoderCursor) Int64(dst *int64) error     { return decoderCursorInt(c, dst) }
func (c *decoderCursor) UintNative(dst *uint) error { return decoderCursorUint(c, dst) }
func (c *decoderCursor) Uint8(dst *uint8) error     { return decoderCursorUint(c, dst) }
func (c *decoderCursor) Uint16(dst *uint16) error   { return decoderCursorUint(c, dst) }
func (c *decoderCursor) Uint32(dst *uint32) error   { return decoderCursorUint(c, dst) }
func (c *decoderCursor) Uint64(dst *uint64) error   { return decoderCursorUint(c, dst) }
func (c *decoderCursor) Uintptr(dst *uintptr) error { return decoderCursorUint(c, dst) }
func (c *decoderCursor) Float32(dst *float32) error { return decoderCursorFloat(c, dst) }
func (c *decoderCursor) Float64(dst *float64) error { return decoderCursorFloat(c, dst) }

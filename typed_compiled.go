package simdjson

import (
	"unsafe"
)

func (cursor *decoderCursor) decodeCompiled(node *typedNode, dst unsafe.Pointer) error {
	var err error
	switch node.kind {
	case typedBool:
		err = cursor.Bool((*bool)(dst))
	case typedString:
		err = cursor.String((*string)(dst))
	case typedNumber:
		err = cursor.Number((*string)(dst))
	case typedInt:
		if useStableNumericMethods {
			switch node.bits {
			case 8:
				err = cursor.Int8((*int8)(dst))
			case 16:
				err = cursor.Int16((*int16)(dst))
			case 32:
				err = cursor.Int32((*int32)(dst))
			case 64:
				err = cursor.Int64((*int64)(dst))
			}
		} else {
			switch node.bits {
			case 8:
				err = cursor.Int((*int8)(dst))
			case 16:
				err = cursor.Int((*int16)(dst))
			case 32:
				err = cursor.Int((*int32)(dst))
			case 64:
				err = cursor.Int((*int64)(dst))
			}
		}
	case typedUint:
		if useStableNumericMethods {
			switch node.bits {
			case 8:
				err = cursor.Uint8((*uint8)(dst))
			case 16:
				err = cursor.Uint16((*uint16)(dst))
			case 32:
				err = cursor.Uint32((*uint32)(dst))
			case 64:
				err = cursor.Uint64((*uint64)(dst))
			}
		} else {
			switch node.bits {
			case 8:
				err = cursor.Uint((*uint8)(dst))
			case 16:
				err = cursor.Uint((*uint16)(dst))
			case 32:
				err = cursor.Uint((*uint32)(dst))
			case 64:
				err = cursor.Uint((*uint64)(dst))
			}
		}
	case typedFloat:
		if useStableNumericMethods {
			if node.bits == 32 {
				err = cursor.Float32((*float32)(dst))
			} else {
				err = cursor.Float64((*float64)(dst))
			}
		} else {
			if node.bits == 32 {
				err = cursor.Float((*float32)(dst))
			} else {
				err = cursor.Float((*float64)(dst))
			}
		}
	case typedStruct:
		return cursor.decodeCompiledStruct(node, dst)
	case typedSlice:
		return cursor.decodeCompiledSlice(node, dst)
	case typedArray:
		return cursor.decodeCompiledArray(node, dst)
	case typedPointer:
		return cursor.decodeCompiledPointer(node, dst)
	case typedMap:
		return cursor.decodeCompiledMap(node, dst)
	case typedAny:
		return cursor.decodeCompiledAny(dst)
	case typedBytes:
		return cursor.decodeCompiledBytes(node, dst)
	case typedUnmarshalerJSON:
		return cursor.decodeViaUnmarshaler(node, dst)
	case typedUnmarshalerText:
		return cursor.decodeViaTextUnmarshaler(node, dst)
	case typedUnmarshalerSimd:
		return cursor.decodeViaSimdHook(node, dst)
	case typedIface:
		return cursor.decodeCompiledIface(node, dst)
	default:
		return &DecodeError{Offset: cursor.i, Type: node.typ, Reason: "invalid compiled operation"}
	}
	if err == nil {
		return nil
	}
	return retagCompiledError(err, node.typ)
}

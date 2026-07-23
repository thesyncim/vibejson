package slopjson

import "reflect"

const (
	decoderReceiverArenaValues = 8
	decoderReceiverArenaBytes  = 64 << 10
	decoderReceiverArenaSlots  = 8
)

// decoderReceiverArena owns one current GC-scanned array of detached method
// receivers. Arrays are operation-local: a user method may retain any element,
// so neither an element nor its backing array is ever reused by another decode.
type decoderReceiverArena struct {
	node      *typedNode
	arrayType reflect.Type
	values    reflect.Value
	next      int
}

type decoderOperationState struct {
	first    decoderReceiverArena
	overflow *decoderReceiverOverflow
	batch    int
	maps     []decoderMapScratch
}

// decoderReceiverOverflow is allocated only for a graph that reaches more
// than one standard unmarshaler type in the same repeated element. The common
// one-type case keeps a single compact state object.
type decoderReceiverOverflow struct {
	arenas [decoderReceiverArenaSlots - 1]decoderReceiverArena
	count  int
}

// prepareDecoderReceivers builds typed receiver-array types once and then
// propagates the presence bit through the compiled graph. The fixed-point pass
// handles recursive Go types without recursion-order assumptions.
func prepareDecoderReceivers(root *typedNode) {
	var nodes []*typedNode
	seen := make(map[*typedNode]bool)
	var collect func(*typedNode)
	collect = func(node *typedNode) {
		if node == nil || seen[node] {
			return
		}
		seen[node] = true
		nodes = append(nodes, node)
		if (node.kind == typedUnmarshalerJSON || node.kind == typedUnmarshalerText) && node.typ != timeReflectType {
			if decoderReceiverArrayType(node) != nil {
				node.decHasReceiver = true
			}
			return
		}
		switch node.kind {
		case typedStruct:
			for i := range node.fields {
				collect(node.fields[i].node)
			}
			if node.inlineMap != nil {
				collect(node.inlineMap.elem)
			}
		case typedSlice, typedArray, typedMap, typedPointer:
			collect(node.elem)
		}
	}
	collect(root)

	for changed := true; changed; {
		changed = false
		for _, node := range nodes {
			if node.decHasReceiver {
				continue
			}
			has := false
			switch node.kind {
			case typedStruct:
				for i := range node.fields {
					if node.fields[i].node.decHasReceiver {
						has = true
						break
					}
				}
				if !has && node.inlineMap != nil {
					has = node.inlineMap.elem.decHasReceiver
				}
			case typedSlice, typedArray, typedMap, typedPointer:
				has = node.elem != nil && node.elem.decHasReceiver
			}
			if has {
				node.decHasReceiver = true
				changed = true
			}
		}
	}
}

func decoderReceiverArrayType(node *typedNode) reflect.Type {
	receiverType := node.typ
	if receiverType.Kind() == reflect.Pointer {
		receiverType = receiverType.Elem()
	}
	count := decoderReceiverArenaValues
	if size := receiverType.Size(); size != 0 && uintptr(count)*size > decoderReceiverArenaBytes {
		count = int(uintptr(decoderReceiverArenaBytes) / size)
	}
	if count < 2 {
		return nil
	}
	return reflect.ArrayOf(count, receiverType)
}

func (c *decoderCursor) beginReceiverBatch() bool {
	if c.state == nil {
		c.state = new(decoderState)
	}
	if c.state.operation == nil {
		c.state.operation = new(decoderOperationState)
	}
	c.state.operation.batch++
	return true
}

func (c *decoderCursor) endReceiverBatch(active bool) {
	if active {
		c.state.operation.batch--
	}
}

func (c *decoderCursor) nextReceiverShadow(node *typedNode, source reflect.Value) (reflect.Value, bool) {
	if c.state == nil || c.state.operation == nil || c.state.operation.batch == 0 || !node.decHasReceiver {
		return reflect.Value{}, false
	}
	state := c.state.operation
	arena := &state.first
	if arena.node == nil {
		arena.node = node
		arena.arrayType = decoderReceiverArrayType(node)
	} else if arena.node != node {
		overflow := state.overflow
		if overflow == nil {
			overflow = new(decoderReceiverOverflow)
			state.overflow = overflow
		}
		arena = nil
		for i := 0; i < overflow.count; i++ {
			if overflow.arenas[i].node == node {
				arena = &overflow.arenas[i]
				break
			}
		}
		if arena == nil {
			if overflow.count == len(overflow.arenas) {
				return reflect.Value{}, false
			}
			arena = &overflow.arenas[overflow.count]
			overflow.count++
			arena.node = node
			arena.arrayType = decoderReceiverArrayType(node)
		}
	}
	if arena.arrayType == nil {
		return reflect.Value{}, false
	}
	if !arena.values.IsValid() || arena.next == arena.values.Len() {
		arena.values = reflect.New(arena.arrayType).Elem()
		arena.next = 0
	}
	value := arena.values.Index(arena.next)
	arena.next++
	value.Set(source)
	return value.Addr(), true
}

func (state *decoderOperationState) resetReceivers() {
	// Receiver arrays are operation-local because user methods may retain an
	// element. Clear every reflect.Value before the metadata is pooled; retained
	// element pointers keep their typed backing alive independently.
	state.first = decoderReceiverArena{}
	if state.overflow != nil {
		for i := range state.overflow.arenas {
			state.overflow.arenas[i] = decoderReceiverArena{}
		}
		state.overflow.count = 0
	}
	state.batch = 0
}

package vibejson

// finalizeDecodeFields refreshes operations captured while recursive child
// nodes were still being compiled. Custom methods and Replace dispatch are
// selected after structural compilation, so field snapshots can otherwise
// bypass their completed node's operation. Run once before publishing a plan.
func (c *typedCompiler) finalizeDecodeFields() {
	for _, node := range c.nodes {
		for i := range node.fields {
			field := &node.fields[i]
			// Quoted fields deliberately override their node's scalar operation.
			if field.op == typedOpQuoted || field.op == field.node.op {
				continue
			}
			field.op = field.node.op
			// Structural shortcuts were selected using the old operation.
			// Only changed records need the general executor; ordinary records
			// retain their compiled specialization.
			node.structuralFast = false
			node.decShape &^= typedDecShapeMask
		}
	}
}

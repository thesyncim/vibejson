package storeio

const pendingWriteSuperseded uint8 = 1

// coalesceManualBatchLocked returns only an older alternate-superblock staging
// buffer. A newer buffered generation makes that descriptor unreachable by any
// future checkpoint, while the decoded state retained by live snapshots does
// not borrow it.
//
// Data and state-root pages deliberately remain owned by their batches. A
// snapshot of an older buffered generation may fault one of those pages after
// the checkpoint marks dirty cache entries durable; the committer has neither
// reader nor snapshot lease knowledge with which to prove such a page dead.
func (c *Committer) coalesceManualBatchLocked(
	batch *Batch,
	generation uint64,
) {
	if c == nil || !c.options.ManualCheckpoint || batch == nil ||
		batch.materialized {
		return
	}
	if previous := c.pendingRoot; previous != nil &&
		previous.generation > c.checkpointThrough.Load() &&
		previous.generation < generation &&
		previous.root.pendingFlags&pendingWriteSuperseded == 0 {
		previous.root.pendingFlags |= pendingWriteSuperseded
		c.freeBuffers.push(uint32(previous.root.Buffer))
		c.supersededRootWrites.Add(1)
		c.supersededRootBytes.Add(uint64(previous.root.Length))
	}
	c.pendingRoot = batch
}

// rebuildManualPendingLocked forgets committed batches and finds the newest
// alternate root still outside the completed checkpoint cut.
func (c *Committer) rebuildManualPendingLocked() {
	if c == nil || !c.options.ManualCheckpoint {
		return
	}
	c.pendingRoot = nil
	head, tail := c.head.Load(), c.tail.Load()
	for position := head; position < tail; position++ {
		batch := c.pending[position&c.pendingMask]
		if batch == nil || batch.state.Load() != batchPublished ||
			batch.root.pendingFlags&pendingWriteSuperseded != 0 {
			continue
		}
		c.pendingRoot = batch
	}
}

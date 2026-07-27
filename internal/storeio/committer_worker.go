package storeio

import (
	"os"
	"runtime"
	"slices"
	"time"
)

func (c *Committer) stopAccepting() {
	c.closing.Store(true)
	for c.publishers.Load() != 0 {
		runtime.Gosched()
	}
}

func (c *Committer) run(file *os.File, initialized chan<- committerInit, open deviceOpener) {
	locked := c.deviceOptions.Backend != BackendPortable
	if locked {
		runtime.LockOSThread()
		c.deviceOptions.SingleIssuer = true
	}
	device, err := open(file, c.deviceOptions)
	if err != nil {
		if locked {
			runtime.UnlockOSThread()
		}
		c.setFailure(err)
		initialized <- committerInit{err: err}
		close(c.done)
		c.broadcast()
		return
	}
	if locked && device.Backend() != BackendIOUring {
		runtime.UnlockOSThread()
		locked = false
	}
	if locked {
		defer runtime.UnlockOSThread()
	}
	c.device = device
	c.backend = device.Backend()
	c.buffers = make([][]byte, c.bufferCount)
	for i := range c.buffers {
		buffer, bufferErr := device.Buffer(i)
		if bufferErr != nil {
			_ = device.Close()
			c.setFailure(bufferErr)
			initialized <- committerInit{err: bufferErr}
			close(c.done)
			c.broadcast()
			return
		}
		c.buffers[i] = buffer
	}
	initialized <- committerInit{}
	defer func() {
		if closeErr := device.Close(); closeErr != nil {
			c.setFailure(closeErr)
		}
		close(c.done)
		c.broadcast()
	}()

	var coalesce *time.Timer
	if c.options.CoalesceDelay != 0 {
		coalesce = time.NewTimer(time.Hour)
		coalesce.Stop()
		defer coalesce.Stop()
	}
	for {
		if !c.waitForCheckpointRequest() {
			return
		}
		checkpointTarget := c.checkpointThrough.Load()
		batch, ok := c.nextBatch(true)
		if !ok {
			return
		}
		if batch.materialized {
			if err := c.commitMaterialized(device, batch); err != nil {
				c.setFailure(err)
				c.release(batch)
				c.drainFailed()
				return
			}
			continue
		}
		// The first publication has already returned to an asynchronous
		// producer. A short bounded window lets nearby generations share both
		// durability barriers; Close interrupts it and drains immediately.
		//
		// Waiting unconditionally is what made this window unusable for a lone
		// synchronous writer, whose next Put cannot even begin until this commit
		// is durable: the window could never find anything to group and instead
		// added its full length to every acknowledged commit.
		if coalesce != nil && !c.closing.Load() && c.coalesceWorthwhile() {
			coalesce.Reset(c.options.CoalesceDelay)
			select {
			case <-coalesce.C:
			case <-c.stop:
				coalesce.Stop()
			}
		}
		c.groupScratch = c.groupScratch[:1]
		c.groupScratch[0] = batch
		c.commitScratch = c.commitScratch[:len(batch.pages)]
		copy(c.commitScratch, batch.pages)
		latest := batch
		groupLimit := c.options.GroupLimit
		if c.options.ManualCheckpoint {
			groupLimit = cap(c.groupScratch)
		}
		for len(c.groupScratch) < groupLimit {
			next, exists := c.peekBatch()
			if !exists || next.materialized ||
				(c.options.ManualCheckpoint && next.generation > checkpointTarget) ||
				len(c.commitScratch)+len(next.pages) > cap(c.commitScratch) {
				break
			}
			next, _ = c.nextBatch(false)
			groupIndex := len(c.groupScratch)
			c.groupScratch = c.groupScratch[:groupIndex+1]
			c.groupScratch[groupIndex] = next
			writeIndex := len(c.commitScratch)
			c.commitScratch = c.commitScratch[:writeIndex+len(next.pages)]
			copy(c.commitScratch[writeIndex:], next.pages)
			latest = next
		}
		// Every grouped generation contains a checksummed PageStateRoot, but
		// the single alternate superblock committed below can name only the
		// newest one. Earlier state pages are never consulted by live
		// snapshots (which retain decoded roots), so suppressing them is safe.
		// Other same-logical-id pages are deliberately retained: an
		// intermediate snapshot may still need their physical generations.
		//
		// The one intermediate root that must still be written is the one
		// holding the group's highest extent. The superblock publishes a
		// FileEnd covering every extent this group allocated, and recovery
		// rejects a store whose file is shorter than its own FileEnd as
		// truncated. Dropping the only write that reaches that high leaves a
		// file that is exactly one page short of the root it just published,
		// which is unopenable rather than merely stale.
		latestState := -1
		highest := uint64(0)
		for index := range c.commitScratch {
			write := c.commitScratch[index]
			if write.kind == PageStateRoot {
				latestState = index
			}
			highest = max(highest, uint64(write.Offset)+uint64(write.Length))
		}
		if latestState >= 0 {
			out := c.commitScratch[:0]
			for index, write := range c.commitScratch {
				extends := uint64(write.Offset)+uint64(write.Length) == highest
				if write.kind == PageStateRoot && index != latestState && !extends {
					c.suppressedRootWrites.Add(1)
					c.suppressedRootBytes.Add(uint64(write.Length))
					continue
				}
				out = append(out, write)
			}
			c.commitScratch = out
		}
		slices.SortFunc(c.commitScratch, func(a, b Write) int {
			if a.Offset < b.Offset {
				return -1
			}
			if a.Offset > b.Offset {
				return 1
			}
			return 0
		})
		// Always preserve the last durable superblock as the recovery fallback.
		// Grouping may publish generation N+2 directly over durable N, so the
		// generation-derived slot prepared by SetSuperblock is not authoritative.
		// Toggle only after Device.Commit completes both durability barriers.
		rootSlot := c.nextRootSlot.Load()
		if latest.rootGeneration != 0 {
			layout, layoutErr := MutableStoreLayout(latest.root.Length)
			if layoutErr != nil {
				c.setFailure(layoutErr)
				for _, grouped := range c.groupScratch {
					c.release(grouped)
				}
				c.drainFailed()
				return
			}
			latest.root.Offset = int64(layout.RootOffsets[rootSlot])
		}
		committedBytes := uint64(latest.root.Length)
		for _, write := range c.commitScratch {
			committedBytes += uint64(write.Length)
		}
		if err := device.Commit(c.commitScratch, latest.root); err != nil {
			c.setFailure(err)
			for _, grouped := range c.groupScratch {
				c.release(grouped)
			}
			c.drainFailed()
			return
		}
		if latest.rootGeneration != 0 {
			c.nextRootSlot.Store(rootSlot ^ 1)
			fallback := c.durable.Load()
			if fallback == 0 {
				fallback = latest.generation
			}
			c.fallback.Store(fallback)
		}
		groupSize := uint32(len(c.groupScratch))
		c.deviceBytes.Add(committedBytes)
		c.deviceCommits.Add(1)
		c.batchesDone.Add(uint64(groupSize))
		for old := c.largestGroup.Load(); groupSize > old && !c.largestGroup.CompareAndSwap(old, groupSize); old = c.largestGroup.Load() {
		}
		c.durable.Store(latest.generation)
		if callbacks := c.callbacks.Load(); callbacks != nil &&
			callbacks.durable != nil {
			callbacks.durable(latest.generation)
		}
		settledGeneration := latest.generation
		for _, grouped := range c.groupScratch {
			c.release(grouped)
		}
		// A producer that has not recorded its state yet rechecks durable after
		// Publish, while Wait remains behind the completed callback and staging
		// recycle. Keeping these two generations separate closes both sides of
		// the publication race and makes a successful checkpoint immediately
		// reusable as bounded admission capacity.
		c.settled.Store(settledGeneration)
		c.broadcast()
	}
}

// waitForCheckpointRequest keeps a manual committer completely off the device
// until Flush or Close authorizes the generation at the head of the queue.
// The request is a generation cut: publications newer than it remain buffered
// for a later checkpoint.
func (c *Committer) waitForCheckpointRequest() bool {
	if !c.options.ManualCheckpoint {
		return true
	}
	for {
		head := c.head.Load()
		if head != c.tail.Load() {
			batch := c.pending[head&c.pendingMask]
			if batch != nil && batch.generation <= c.checkpointThrough.Load() {
				return true
			}
		}
		c.workerWait.Store(1)
		head = c.head.Load()
		if head != c.tail.Load() {
			batch := c.pending[head&c.pendingMask]
			if batch != nil && batch.generation <= c.checkpointThrough.Load() {
				c.workerWait.Store(0)
				continue
			}
		}
		select {
		case <-c.wake:
		case <-c.stop:
			c.workerWait.Store(0)
			if c.head.Load() == c.tail.Load() {
				return false
			}
			continue
		}
		c.workerWait.Store(0)
	}
}

// coalesceWorthwhile reports whether waiting for company can pay for itself.
// Another generation already queued makes grouping certain. Otherwise the
// window is free only while nobody is blocked on this fence: a caller inside
// Wait has not been acknowledged yet, so every microsecond spent hoping for a
// neighbour is added directly to its latency, and a lone synchronous writer
// cannot even begin the neighbour it is being made to wait for.
func (c *Committer) coalesceWorthwhile() bool {
	return c.head.Load() != c.tail.Load() || c.waiters.Load() == 0
}

func (c *Committer) nextBatch(wait bool) (*Batch, bool) {
	for {
		if c.options.ManualCheckpoint {
			c.manualMu.Lock()
		}
		head := c.head.Load()
		if head != c.tail.Load() {
			batch := c.pending[head&c.pendingMask]
			c.pending[head&c.pendingMask] = nil
			c.head.Store(head + 1)
			if c.options.ManualCheckpoint {
				c.manualMu.Unlock()
			}
			return batch, true
		}
		if c.options.ManualCheckpoint {
			c.manualMu.Unlock()
		}
		if !wait {
			return nil, false
		}
		c.workerWait.Store(1)
		if c.head.Load() != c.tail.Load() {
			c.workerWait.Store(0)
			continue
		}
		select {
		case <-c.wake:
		case <-c.stop:
			c.workerWait.Store(0)
			if c.head.Load() == c.tail.Load() {
				return nil, false
			}
			continue
		}
		c.workerWait.Store(0)
	}
}

func (c *Committer) peekBatch() (*Batch, bool) {
	head := c.head.Load()
	if head == c.tail.Load() {
		return nil, false
	}
	return c.pending[head&c.pendingMask], true
}

func (c *Committer) drainFailed() {
	for {
		batch, ok := c.nextBatch(false)
		if !ok {
			return
		}
		c.release(batch)
	}
}

func (c *Committer) setFailure(err error) {
	if err == nil {
		return
	}
	c.failOnce.Do(func() {
		c.failure.Store(&commitFailure{err: err})
		if callbacks := c.callbacks.Load(); callbacks != nil &&
			callbacks.failed != nil {
			callbacks.failed(err)
		}
		close(c.failureNotified)
		c.stopAccepting()
		close(c.failed)
		c.broadcast()
	})
}

func (c *Committer) release(batch *Batch) {
	if c.options.ManualCheckpoint {
		c.manualMu.Lock()
		defer c.manualMu.Unlock()
	}
	for _, write := range batch.pages {
		c.freeBuffers.push(uint32(write.Buffer))
	}
	if batch.root.pendingFlags&pendingWriteSuperseded == 0 {
		c.freeBuffers.push(uint32(batch.root.Buffer))
	}
	if batch.materialized {
		c.freeBuffers.push(uint32(batch.journal.Buffer))
	}
	batch.pages = batch.pages[:0]
	batch.root = Write{}
	batch.rootGeneration = 0
	batch.journal = Write{}
	batch.journalSequence = 0
	batch.journalSlot = 0
	batch.journalCapsuleChecksum = 0
	batch.materializationPatchCount = 0
	batch.materializationTargetMask = 0
	batch.materializationPatchChecksums = [MaterializationJournalMaxPatches]uint32{}
	batch.materialized = false
	batch.generation = 0
	batch.state.Store(batchFree)
	c.freeBatches.push(batch.index)
	if c.options.ManualCheckpoint {
		c.rebuildManualPendingLocked()
	}
}

func (c *Committer) releasePartial(batch *Batch, acquired int) {
	pageCount := len(batch.pages)
	for i := 0; i < acquired; i++ {
		if i == pageCount {
			c.freeBuffers.push(uint32(batch.root.Buffer))
		} else {
			c.freeBuffers.push(uint32(batch.pages[i].Buffer))
		}
	}
	batch.pages = batch.pages[:0]
	batch.root = Write{}
	batch.rootGeneration = 0
	batch.state.Store(batchFree)
	c.freeBatches.push(batch.index)
}

func (c *Committer) broadcast() {
	c.waitMu.Lock()
	c.wait.Broadcast()
	c.waitMu.Unlock()
}

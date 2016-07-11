// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/keybase/client/go/logger"
	"golang.org/x/net/context"
)

type fbmHelper interface {
	getMDForFBM(ctx context.Context) (*IFCERFTRootMetadata, error)
	finalizeGCOp(ctx context.Context, gco *gcOp) error
}

const (
	// How many pointers to downgrade in a single Archive/Delete call.
	numPointersToDowngradePerChunk = 20
	// Once the number of pointers being deleted in a single gc op
	// passes this threshold, we'll stop garbage collection at the
	// current revision.
	numPointersPerGCThreshold = 100
	// The most revisions to consider for each QR run.
	numMaxRevisionsPerQR = 100
)

// folderBlockManager is a helper class for managing the blocks in a
// particular TLF.  It archives historical blocks and reclaims quota
// usage, all in the background.
type folderBlockManager struct {
	config       IFCERFTConfig
	log          logger.Logger
	shutdownChan chan struct{}
	id           IFCERFTTlfID

	// A queue of MD updates for this folder that need to have their
	// unref's blocks archived
	archiveChan chan *IFCERFTRootMetadata

	archivePauseChan chan (<-chan struct{})

	// archiveGroup tracks the outstanding archives.
	archiveGroup RepeatedWaitGroup

	archiveCancelLock sync.Mutex
	archiveCancel     context.CancelFunc

	// blocksToDeleteAfterError is a list of blocks, for a given
	// metadata revision, that may have been Put as part of a failed
	// MD write.  These blocks should be deleted as soon as we know
	// for sure that the MD write isn't visible to others.
	// The lock should only be held immediately around accessing the
	// list.  TODO: Persist these to disk?
	blocksToDeleteLock       sync.Mutex
	blocksToDeleteAfterError map[*IFCERFTRootMetadata][]IFCERFTBlockPointer

	// forceReclamation forces the manager to start a reclamation
	// process.
	forceReclamationChan chan struct{}

	// reclamationGroup tracks the outstanding quota reclamations.
	reclamationGroup RepeatedWaitGroup

	reclamationCancelLock sync.Mutex
	reclamationCancel     context.CancelFunc

	helper fbmHelper

	// Keep track of the last reclamation time, for testing.
	lastReclamationTimeLock sync.Mutex
	lastReclamationTime     time.Time

	// Remembers what happened last time during quota reclamation;
	// should only be accessed by the QR goroutine.
	lastQRHeadRev      IFCERFTMetadataRevision
	lastQROldEnoughRev IFCERFTMetadataRevision
	wasLastQRComplete  bool
}

func newFolderBlockManager(config IFCERFTConfig, fb IFCERFTFolderBranch, helper fbmHelper) *folderBlockManager {
	tlfStringFull := fb.Tlf.String()
	log := config.MakeLogger(fmt.Sprintf("FBM %s", tlfStringFull[:8]))
	fbm := &folderBlockManager{
		config:                   config,
		log:                      log,
		shutdownChan:             make(chan struct{}),
		id:                       fb.Tlf,
		archiveChan:              make(chan *IFCERFTRootMetadata, 25),
		archivePauseChan:         make(chan (<-chan struct{})),
		blocksToDeleteAfterError: make(map[*IFCERFTRootMetadata][]IFCERFTBlockPointer),
		forceReclamationChan:     make(chan struct{}, 1),
		helper:                   helper,
	}
	// Pass in the BlockOps here so that the archive goroutine
	// doesn't do possibly-racy-in-tests access to
	// fbm.config.BlockOps().
	go fbm.archiveBlocksInBackground()
	if fb.Branch == IFCERFTMasterBranch {
		go fbm.reclaimQuotaInBackground()
	}
	return fbm
}

func (fbm *folderBlockManager) setArchiveCancel(cancel context.CancelFunc) {
	fbm.archiveCancelLock.Lock()
	defer fbm.archiveCancelLock.Unlock()
	fbm.archiveCancel = cancel
}

func (fbm *folderBlockManager) cancelArchive() {
	archiveCancel := func() context.CancelFunc {
		fbm.archiveCancelLock.Lock()
		defer fbm.archiveCancelLock.Unlock()
		archiveCancel := fbm.archiveCancel
		fbm.archiveCancel = nil
		return archiveCancel
	}()
	if archiveCancel != nil {
		archiveCancel()
	}
}

func (fbm *folderBlockManager) setReclamationCancel(cancel context.CancelFunc) {
	fbm.reclamationCancelLock.Lock()
	defer fbm.reclamationCancelLock.Unlock()
	fbm.reclamationCancel = cancel
}

func (fbm *folderBlockManager) cancelReclamation() {
	reclamationCancel := func() context.CancelFunc {
		fbm.reclamationCancelLock.Lock()
		defer fbm.reclamationCancelLock.Unlock()
		reclamationCancel := fbm.reclamationCancel
		fbm.reclamationCancel = nil
		return reclamationCancel
	}()
	if reclamationCancel != nil {
		reclamationCancel()
	}
}

func (fbm *folderBlockManager) shutdown() {
	close(fbm.shutdownChan)
	fbm.cancelArchive()
	fbm.cancelReclamation()
}

// cleanUpBlockState cleans up any blocks that may have been orphaned
// by a failure during or after blocks have been sent to the
// server. This is usually used in a defer right before a call to
// fbo.doBlockPuts like so:
//
//  defer func() {
//    if err != nil {
//      ...cleanUpBlockState(md, bps)
//    }
//  }()
//
//  ... = ...doBlockPuts(ctx, md, *bps)
func (fbm *folderBlockManager) cleanUpBlockState(
	md *IFCERFTRootMetadata, bps *blockPutState) {
	fbm.blocksToDeleteLock.Lock()
	defer fbm.blocksToDeleteLock.Unlock()
	fbm.log.CDebugf(nil, "Clean up md %d %s", md.Revision, md.MergedStatus())
	for _, bs := range bps.blockStates {
		fbm.blocksToDeleteAfterError[md] =
			append(fbm.blocksToDeleteAfterError[md], bs.blockPtr)
	}
}

func (fbm *folderBlockManager) archiveUnrefBlocks(md *IFCERFTRootMetadata) {
	// Don't archive for unmerged revisions, because conflict
	// resolution might undo some of the unreferences.
	if md.MergedStatus() != IFCERFTMerged {
		return
	}

	fbm.archiveGroup.Add(1)
	fbm.archiveChan <- md
}

// archiveUnrefBlocksNoWait enqueues the MD for archiving without
// blocking.  By the time it returns, the archive group has been
// incremented so future waits will block on this archive.  This
// method is for internal use within folderBlockManager only.
func (fbm *folderBlockManager) archiveUnrefBlocksNoWait(md *IFCERFTRootMetadata) {
	// Don't archive for unmerged revisions, because conflict
	// resolution might undo some of the unreferences.
	if md.MergedStatus() != IFCERFTMerged {
		return
	}

	fbm.archiveGroup.Add(1)

	// Don't block if the channel is full; instead do the send in a
	// background goroutine.  We've already done the Add above, so the
	// wait calls should all work just fine.
	select {
	case fbm.archiveChan <- md:
		return
	default:
		go func() { fbm.archiveChan <- md }()
	}
}

func (fbm *folderBlockManager) waitForArchives(ctx context.Context) error {
	return fbm.archiveGroup.Wait(ctx)
}

func (fbm *folderBlockManager) waitForQuotaReclamations(
	ctx context.Context) error {
	return fbm.reclamationGroup.Wait(ctx)
}

func (fbm *folderBlockManager) forceQuotaReclamation() {
	fbm.reclamationGroup.Add(1)
	select {
	case fbm.forceReclamationChan <- struct{}{}:
	default:
		fbm.reclamationGroup.Done()
	}
}

// doChunkedDowngrades sends batched archive or delete messages to the
// block server for the given block pointers.  For deletes, it returns
// a list of block IDs that no longer have any references.
func (fbm *folderBlockManager) doChunkedDowngrades(ctx context.Context,
	md *IFCERFTRootMetadata, ptrs []IFCERFTBlockPointer, archive bool) (
	[]BlockID, error) {
	fbm.log.CDebugf(ctx, "Downgrading %d pointers (archive=%t)",
		len(ptrs), archive)
	bops := fbm.config.BlockOps()

	// Round up to find the number of chunks.
	numChunks := (len(ptrs) + numPointersToDowngradePerChunk - 1) /
		numPointersToDowngradePerChunk
	numWorkers := numChunks
	if numWorkers > maxParallelBlockPuts {
		numWorkers = maxParallelBlockPuts
	}
	chunks := make(chan []IFCERFTBlockPointer, numChunks)

	var wg sync.WaitGroup
	defer wg.Wait()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type workerResult struct {
		zeroRefCounts []BlockID
		err           error
	}

	chunkResults := make(chan workerResult, numChunks)
	worker := func() {
		defer wg.Done()
		for chunk := range chunks {
			var res workerResult
			fbm.log.CDebugf(ctx, "Downgrading chunk of %d pointers", len(chunk))
			if archive {
				res.err = bops.Archive(ctx, md, chunk)
			} else {
				var liveCounts map[BlockID]int
				liveCounts, res.err = bops.Delete(ctx, md, chunk)
				if res.err == nil {
					for id, count := range liveCounts {
						if count == 0 {
							res.zeroRefCounts = append(res.zeroRefCounts, id)
						}
					}
				}
			}
			chunkResults <- res
			select {
			// return early if the context has been canceled
			case <-ctx.Done():
				return
			default:
			}
		}
	}
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go worker()
	}

	for start := 0; start < len(ptrs); start += numPointersToDowngradePerChunk {
		end := start + numPointersToDowngradePerChunk
		if end > len(ptrs) {
			end = len(ptrs)
		}
		chunks <- ptrs[start:end]
	}
	close(chunks)

	var zeroRefCounts []BlockID
	for i := 0; i < numChunks; i++ {
		result := <-chunkResults
		if result.err != nil {
			// deferred cancel will stop the other workers.
			return nil, result.err
		}
		zeroRefCounts = append(zeroRefCounts, result.zeroRefCounts...)
	}
	return zeroRefCounts, nil
}

// deleteBlockRefs sends batched delete messages to the block server
// for the given block pointers.  It returns a list of block IDs that
// no longer have any references.
func (fbm *folderBlockManager) deleteBlockRefs(ctx context.Context,
	md *IFCERFTRootMetadata, ptrs []IFCERFTBlockPointer) ([]BlockID, error) {
	return fbm.doChunkedDowngrades(ctx, md, ptrs, false)
}

func (fbm *folderBlockManager) processBlocksToDelete(ctx context.Context) error {
	// also attempt to delete any error references
	var toDelete map[*IFCERFTRootMetadata][]IFCERFTBlockPointer
	func() {
		fbm.blocksToDeleteLock.Lock()
		defer fbm.blocksToDeleteLock.Unlock()
		toDelete = fbm.blocksToDeleteAfterError
		fbm.blocksToDeleteAfterError =
			make(map[*IFCERFTRootMetadata][]IFCERFTBlockPointer)
	}()

	if len(toDelete) == 0 {
		return nil
	}

	toDeleteAgain := make(map[*IFCERFTRootMetadata][]IFCERFTBlockPointer)
	for md, ptrs := range toDelete {
		fbm.log.CDebugf(ctx, "Checking deleted blocks for revision %d",
			md.Revision)
		// Make sure that the MD didn't actually become
		// part of the folder history.  (This could happen
		// if the Sync was canceled while the MD put was
		// outstanding.)
		rmds, err := getMDRange(ctx, fbm.config, fbm.id, md.BID,
			md.Revision, md.Revision, md.MergedStatus())
		if err != nil || len(rmds) == 0 {
			toDeleteAgain[md] = ptrs
			continue
		}
		dirsEqual, err := CodecEqual(fbm.config.Codec(),
			rmds[0].data.Dir, md.data.Dir)
		if err != nil {
			fbm.log.CErrorf(ctx, "Error when comparing dirs: %v", err)
		} else if dirsEqual {
			// This md is part of the history of the folder,
			// so we shouldn't delete the blocks.
			fbm.log.CDebugf(ctx, "Not deleting blocks from revision %d",
				md.Revision)
			// But, since this MD put seems to have succeeded, we
			// should archive it.
			fbm.log.CDebugf(ctx, "Archiving successful MD revision %d",
				rmds[0].Revision)
			// Don't block on archiving the MD, because that could
			// lead to deadlock.
			fbm.archiveUnrefBlocksNoWait(rmds[0])
			continue
		}

		// Otherwise something else has been written over
		// this MD, so get rid of the blocks.
		fbm.log.CDebugf(ctx, "Cleaning up blocks for failed revision %d",
			md.Revision)

		_, err = fbm.deleteBlockRefs(ctx, md, ptrs)
		// Ignore permanent errors
		_, isPermErr := err.(BServerError)
		_, isNonceNonExistentErr := err.(BServerErrorNonceNonExistent)
		if err != nil {
			fbm.log.CWarningf(ctx, "Couldn't delete some ref in batch %v: %v", ptrs, err)
			if !isPermErr && !isNonceNonExistentErr {
				toDeleteAgain[md] = ptrs
			}
		}
	}

	if len(toDeleteAgain) > 0 {
		func() {
			fbm.blocksToDeleteLock.Lock()
			defer fbm.blocksToDeleteLock.Unlock()
			for md, ptrs := range toDeleteAgain {
				fbm.blocksToDeleteAfterError[md] =
					append(fbm.blocksToDeleteAfterError[md], ptrs...)
			}
		}()
	}

	return nil
}

// CtxFBMTagKey is the type used for unique context tags within
// folderBlockManager
type CtxFBMTagKey int

const (
	// CtxFBMIDKey is the type of the tag for unique operation IDs
	// within folderBlockManager.
	CtxFBMIDKey CtxFBMTagKey = iota
)

// CtxFBMOpID is the display name for the unique operation
// folderBlockManager ID tag.
const CtxFBMOpID = "FBMID"

func (fbm *folderBlockManager) ctxWithFBMID(
	ctx context.Context) context.Context {
	return ctxWithRandomID(ctx, CtxFBMIDKey, CtxFBMOpID, fbm.log)
}

// Run the passed function with a context that's canceled on shutdown.
func (fbm *folderBlockManager) runUnlessShutdown(
	fn func(ctx context.Context) error) error {
	ctx := fbm.ctxWithFBMID(context.Background())
	ctx, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()
	errChan := make(chan error, 1)
	go func() {
		errChan <- fn(ctx)
	}()

	select {
	case err := <-errChan:
		return err
	case <-fbm.shutdownChan:
		return errors.New("shutdown received")
	}
}

func (fbm *folderBlockManager) archiveBlockRefs(ctx context.Context,
	md *IFCERFTRootMetadata, ptrs []IFCERFTBlockPointer) error {
	_, err := fbm.doChunkedDowngrades(ctx, md, ptrs, true)
	return err
}

func (fbm *folderBlockManager) archiveBlocksInBackground() {
	for {
		select {
		case md := <-fbm.archiveChan:
			var ptrs []IFCERFTBlockPointer
			for _, op := range md.data.Changes.Ops {
				ptrs = append(ptrs, op.Unrefs()...)
				for _, update := range op.AllUpdates() {
					// It's legal for there to be an "update" between
					// two identical pointers (usually because of
					// conflict resolution), so ignore that for
					// archival purposes.
					if update.Ref != update.Unref {
						ptrs = append(ptrs, update.Unref)
					}
				}
			}
			fbm.runUnlessShutdown(func(ctx context.Context) (err error) {
				defer fbm.archiveGroup.Done()
				// This func doesn't take any locks, though it can
				// block md writes due to the buffered channel.  So
				// use the long timeout to make sure things get
				// unblocked eventually, but no need for a short timeout.
				ctx, cancel := context.WithTimeout(ctx, backgroundTaskTimeout)
				fbm.setArchiveCancel(cancel)
				defer fbm.cancelArchive()

				fbm.log.CDebugf(ctx, "Archiving %d block pointers as a result "+
					"of revision %d", len(ptrs), md.Revision)
				err = fbm.archiveBlockRefs(ctx, md, ptrs)
				if err != nil {
					fbm.log.CWarningf(ctx, "Couldn't archive blocks: %v", err)
					return err
				}

				// Also see if we can delete any blocks.
				if err := fbm.processBlocksToDelete(ctx); err != nil {
					fbm.log.CDebugf(ctx, "Error deleting blocks: %v", err)
					return err
				}

				return nil
			})
		case unpause := <-fbm.archivePauseChan:
			fbm.runUnlessShutdown(func(ctx context.Context) (err error) {
				fbm.log.CInfof(ctx, "Archives paused")
				// wait to be unpaused
				select {
				case <-unpause:
					fbm.log.CInfof(ctx, "Archives unpaused")
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			})
		case <-fbm.shutdownChan:
			return
		}
	}
}

func (fbm *folderBlockManager) isOldEnough(rmd *IFCERFTRootMetadata) bool {
	// Trust the client-provided timestamp -- it's
	// possible that a writer with a bad clock could cause
	// another writer to clear out quotas early.  That's
	// ok, there's nothing we can really do about that.
	//
	// TODO: rmd.data.Dir.Mtime does not necessarily reflect when the
	// MD was made, since it only gets updated if the root directory
	// mtime needs to be updated.  As a result, some updates may be
	// cleaned up earlier than desired.  We need to find a more stable
	// way to record MD update time (KBFS-821).
	mtime := time.Unix(0, rmd.data.Dir.Mtime)
	unrefAge := fbm.config.QuotaReclamationMinUnrefAge()
	return mtime.Add(unrefAge).Before(fbm.config.Clock().Now())
}

// getMostRecentOldEnoughAndGCRevisions returns the most recent MD
// that's older than the unref age, as well as the latest revision
// that was scrubbed by the previous gc op.
func (fbm *folderBlockManager) getMostRecentOldEnoughAndGCRevisions(
	ctx context.Context, head *IFCERFTRootMetadata) (
	mostRecentOldEnoughRev, lastGCRev IFCERFTMetadataRevision, err error) {
	// Walk backwards until we find one that is old enough.  Also,
	// look out for the previous gcOp.
	currHead := head.Revision
	mostRecentOldEnoughRev = IFCERFTMetadataRevisionUninitialized
	lastGCRev = IFCERFTMetadataRevisionUninitialized
	for {
		startRev := currHead - maxMDsAtATime + 1 // (MetadataRevision is signed)
		if startRev < IFCERFTMetadataRevisionInitial {
			startRev = IFCERFTMetadataRevisionInitial
		}

		rmds, err := getMDRange(ctx, fbm.config, fbm.id, IFCERFTNullBranchID, startRev,
			currHead, IFCERFTMerged)
		if err != nil {
			return IFCERFTMetadataRevisionUninitialized,
				IFCERFTMetadataRevisionUninitialized, err
		}

		numNew := len(rmds)
		for i := len(rmds) - 1; i >= 0; i-- {
			rmd := rmds[i]
			if mostRecentOldEnoughRev == IFCERFTMetadataRevisionUninitialized &&
				fbm.isOldEnough(rmd) {
				fbm.log.CDebugf(ctx, "Revision %d is older than the unref "+
					"age %s", rmd.Revision,
					fbm.config.QuotaReclamationMinUnrefAge())
				mostRecentOldEnoughRev = rmd.Revision
			}

			if lastGCRev == IFCERFTMetadataRevisionUninitialized {
				for j := len(rmd.data.Changes.Ops) - 1; j >= 0; j-- {
					gcOp, ok := rmd.data.Changes.Ops[j].(*gcOp)
					if !ok {
						continue
					}
					fbm.log.CDebugf(ctx, "Found last gc op: %s", gcOp)
					lastGCRev = gcOp.LatestRev
					break
				}
			}

			// Once both return values are set, we are done
			if mostRecentOldEnoughRev != IFCERFTMetadataRevisionUninitialized &&
				lastGCRev != IFCERFTMetadataRevisionUninitialized {
				return mostRecentOldEnoughRev, lastGCRev, nil
			}
		}

		if numNew > 0 {
			currHead = rmds[0].Revision - 1
		}

		if numNew < maxMDsAtATime || currHead < IFCERFTMetadataRevisionInitial {
			break
		}
	}

	return mostRecentOldEnoughRev, lastGCRev, nil
}

// getUnrefBlocks returns a slice containing all the block pointers
// that were unreferenced after the earliestRev, up to and including
// those in latestRev.  If the number of pointers is too large, it
// will shorten the range of the revisions being reclaimed, and return
// the latest revision represented in the returned slice of pointers.
func (fbm *folderBlockManager) getUnreferencedBlocks(
	ctx context.Context, latestRev, earliestRev IFCERFTMetadataRevision) (
	ptrs []IFCERFTBlockPointer, lastRevConsidered IFCERFTMetadataRevision, complete bool, err error) {
	fbm.log.CDebugf(ctx, "Getting unreferenced blocks between revisions "+
		"%d and %d", earliestRev, latestRev)
	defer func() {
		if err == nil {
			fbm.log.CDebugf(ctx, "Found %d pointers to clean between "+
				"revisions %d and %d", len(ptrs), earliestRev, latestRev)
		}
	}()

	if latestRev <= earliestRev {
		// Nothing to do.
		fbm.log.CDebugf(ctx, "Latest rev %d is included in the previous "+
			"gc op (%d)", latestRev, earliestRev)
		return nil, IFCERFTMetadataRevisionUninitialized, true, nil
	}

	// Walk backward, starting from latestRev, until just after
	// earliestRev, gathering block pointers.
	currHead := latestRev
	revStartPositions := make(map[IFCERFTMetadataRevision]int)
outer:
	for {
		startRev := currHead - maxMDsAtATime + 1 // (MetadataRevision is signed)
		if startRev < IFCERFTMetadataRevisionInitial {
			startRev = IFCERFTMetadataRevisionInitial
		}

		rmds, err := getMDRange(ctx, fbm.config, fbm.id, IFCERFTNullBranchID, startRev,
			currHead, IFCERFTMerged)
		if err != nil {
			return nil, IFCERFTMetadataRevisionUninitialized, false, err
		}

		numNew := len(rmds)
		for i := len(rmds) - 1; i >= 0; i-- {
			rmd := rmds[i]
			if rmd.Revision <= earliestRev {
				break outer
			}
			// Save the latest revision starting at this position:
			revStartPositions[rmd.Revision] = len(ptrs)
			for _, op := range rmd.data.Changes.Ops {
				if _, ok := op.(*gcOp); ok {
					continue
				}
				ptrs = append(ptrs, op.Unrefs()...)
				for _, update := range op.AllUpdates() {
					// It's legal for there to be an "update" between
					// two identical pointers (usually because of
					// conflict resolution), so ignore that for quota
					// reclamation purposes.
					if update.Ref != update.Unref {
						ptrs = append(ptrs, update.Unref)
					}
				}
			}
			// TODO: when can we clean up the MD's unembedded block
			// changes pointer?  It's not safe until we know for sure
			// that all existing clients have received the latest
			// update (and also that there are no outstanding staged
			// branches).  Let's do that as part of the bigger issue
			// KBFS-793 -- for now we have to leak those blocks.
		}

		if numNew > 0 {
			currHead = rmds[0].Revision - 1
		}

		if numNew < maxMDsAtATime || currHead < IFCERFTMetadataRevisionInitial {
			break
		}
	}

	complete = true
	if len(ptrs) > numPointersPerGCThreshold {
		// Find the earliest revision to clean up that lets us send at
		// least numPointersPerGCThreshold pointers.  The earliest
		// pointers are at the end of the list, so subtract the
		// threshold from the back.
		threshStart := len(ptrs) - numPointersPerGCThreshold
		origLatestRev := latestRev
		origPtrsLen := len(ptrs)
		// TODO: optimize by keeping rev->pos mappings in sorted order.
		for rev, i := range revStartPositions {
			if i < threshStart && rev < latestRev {
				latestRev = rev
			}
		}
		if latestRev < origLatestRev {
			ptrs = ptrs[revStartPositions[latestRev]:]
			fbm.log.CDebugf(ctx, "Shortening GC range from [%d:%d] to [%d:%d],"+
				" reducing pointers from %d to %d", earliestRev, origLatestRev,
				earliestRev, latestRev, origPtrsLen, len(ptrs))
			complete = false
		}
	}

	return ptrs, latestRev, complete, nil
}

func (fbm *folderBlockManager) finalizeReclamation(ctx context.Context,
	ptrs []IFCERFTBlockPointer, zeroRefCounts []BlockID,
	latestRev IFCERFTMetadataRevision) error {
	gco := newGCOp(latestRev)
	for _, id := range zeroRefCounts {
		gco.AddUnrefBlock(IFCERFTBlockPointer{ID: id})
	}
	fbm.log.CDebugf(ctx, "Finalizing reclamation %s with %d ptrs", gco,
		len(ptrs))
	// finalizeGCOp could wait indefinitely on locks, so run it in a
	// goroutine.
	return runUnlessCanceled(ctx,
		func() error { return fbm.helper.finalizeGCOp(ctx, gco) })
}

func (fbm *folderBlockManager) isQRNecessary(head *IFCERFTRootMetadata) bool {
	if head == nil {
		return false
	}

	// Do QR if:
	//   * The head has changed since last time, OR
	//   * The last QR did not completely clean every available thing
	if head.Revision != fbm.lastQRHeadRev || !fbm.wasLastQRComplete {
		return true
	}

	// Do QR if the head was not reclaimable at the last QR time, but
	// is old enough now.
	return fbm.lastQRHeadRev > fbm.lastQROldEnoughRev && fbm.isOldEnough(head)
}

func (fbm *folderBlockManager) doReclamation(timer *time.Timer) (err error) {
	ctx, cancel := context.WithCancel(fbm.ctxWithFBMID(context.Background()))
	fbm.setReclamationCancel(cancel)
	defer fbm.cancelReclamation()
	defer timer.Reset(fbm.config.QuotaReclamationPeriod())
	defer fbm.reclamationGroup.Done()

	// Don't set a context deadline.  For users that have written a
	// lot of updates since their last QR, this might involve fetching
	// a lot of MD updates in small chunks.  It doesn't hold locks for
	// any considerable amount of time, so it should be safe to let it
	// run indefinitely.

	// First get the current head, and see if we're staged or not.
	head, err := fbm.helper.getMDForFBM(ctx)
	if err != nil {
		return err
	} else if err := head.IsReadableOrError(ctx, fbm.config); err != nil {
		return err
	} else if head.MergedStatus() != IFCERFTMerged {
		return errors.New("Skipping quota reclamation while unstaged")
	}

	// Make sure we're a writer
	username, uid, err := fbm.config.KBPKI().GetCurrentUserInfo(ctx)
	if err != nil {
		return err
	}
	if !head.GetTlfHandle().IsWriter(uid) {
		return IFCERFTNewWriteAccessError(head.GetTlfHandle(), username)
	}

	if !fbm.isQRNecessary(head) {
		// Nothing has changed since last time, so no need to do any QR.
		return nil
	}
	var mostRecentOldEnoughRev IFCERFTMetadataRevision
	var complete bool
	defer func() {
		// Remember the QR we just performed.
		if err == nil && head != nil {
			fbm.lastQRHeadRev = head.Revision
			fbm.lastQROldEnoughRev = mostRecentOldEnoughRev
			fbm.wasLastQRComplete = complete
		}
	}()

	// Then grab the lock for this folder, so we're the only one doing
	// garbage collection for a while.
	locked, err := fbm.config.MDServer().TruncateLock(ctx, fbm.id)
	if err != nil {
		return err
	}
	if !locked {
		fbm.log.CDebugf(ctx, "Couldn't get the truncate lock")
		return fmt.Errorf("Couldn't get the truncate lock for folder %d",
			fbm.id)
	}
	defer func() {
		unlocked, unlockErr := fbm.config.MDServer().TruncateUnlock(ctx, fbm.id)
		if unlockErr != nil {
			fbm.log.CDebugf(ctx, "Couldn't release the truncate lock: %v",
				unlockErr)
		}
		if !unlocked {
			fbm.log.CDebugf(ctx, "Couldn't unlock the truncate lock")
		}
	}()

	mostRecentOldEnoughRev, lastGCRev, err :=
		fbm.getMostRecentOldEnoughAndGCRevisions(ctx, head)
	if err != nil {
		return err
	}
	if mostRecentOldEnoughRev == IFCERFTMetadataRevisionUninitialized ||
		mostRecentOldEnoughRev <= lastGCRev {
		// TODO: need a log level more fine-grained than Debug to
		// print out that we're not doing reclamation.
		complete = true
		return nil
	}

	// Don't try to do too many at a time.
	shortened := false
	if mostRecentOldEnoughRev-lastGCRev > numMaxRevisionsPerQR {
		mostRecentOldEnoughRev = lastGCRev + numMaxRevisionsPerQR
		shortened = true
	}

	// Don't print these until we know for sure that we'll be
	// reclaiming some quota, to avoid log pollution.
	fbm.log.CDebugf(ctx, "Starting quota reclamation process")
	defer func() {
		fbm.log.CDebugf(ctx, "Ending quota reclamation process: %v", err)
		fbm.lastReclamationTimeLock.Lock()
		defer fbm.lastReclamationTimeLock.Unlock()
		fbm.lastReclamationTime = fbm.config.Clock().Now()
	}()

	ptrs, latestRev, complete, err :=
		fbm.getUnreferencedBlocks(ctx, mostRecentOldEnoughRev, lastGCRev)
	if err != nil {
		return err
	}
	if len(ptrs) == 0 && !shortened {
		complete = true
		return nil
	}

	zeroRefCounts, err := fbm.deleteBlockRefs(ctx, head, ptrs)
	if err != nil {
		return err
	}

	return fbm.finalizeReclamation(ctx, ptrs, zeroRefCounts, latestRev)
}

func (fbm *folderBlockManager) reclaimQuotaInBackground() {
	timer := time.NewTimer(fbm.config.QuotaReclamationPeriod())
	timerChan := timer.C
	for {
		// Don't let the timer fire if auto-reclamation is turned off.
		if fbm.config.QuotaReclamationPeriod().Seconds() == 0 {
			timer.Stop()
			// Use a channel that will never fire instead.
			timerChan = make(chan time.Time)
		}
		select {
		case <-fbm.shutdownChan:
			return
		case <-timerChan:
			fbm.reclamationGroup.Add(1)
		case <-fbm.forceReclamationChan:
		}

		err := fbm.doReclamation(timer)
		if _, ok := err.(IFCERFTWriteAccessError); ok {
			// If we got a write access error, don't bother with the
			// timer anymore. Don't completely shut down, since we
			// don't want forced reclamations to hang.
			timer.Stop()
			timerChan = make(chan time.Time)
		}
	}
}

func (fbm *folderBlockManager) getLastReclamationTime() time.Time {
	fbm.lastReclamationTimeLock.Lock()
	defer fbm.lastReclamationTimeLock.Unlock()
	return fbm.lastReclamationTime
}

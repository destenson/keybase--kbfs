// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"fmt"
	"time"

	"github.com/keybase/client/go/logger"
	"github.com/keybase/client/go/protocol"
	"golang.org/x/net/context"
)

type overallBlockState int

const (
	// cleanState: no outstanding local writes.
	cleanState overallBlockState = iota
	// dirtyState: there are outstanding local writes that haven't yet been
	// synced.
	dirtyState
)

// blockReqType indicates whether an operation makes block
// modifications or not
type blockReqType int

const (
	// A block read request.
	blockRead blockReqType = iota
	// A block write request.
	blockWrite
)

type syncInfo struct {
	oldInfo    IFCERFTBlockInfo
	op         *syncOp
	unrefs     []IFCERFTBlockInfo
	bps        *blockPutState
	refBytes   uint64
	unrefBytes uint64
}

func (si *syncInfo) DeepCopy(codec IFCERFTCodec) (*syncInfo, error) {
	newSi := &syncInfo{
		oldInfo:    si.oldInfo,
		refBytes:   si.refBytes,
		unrefBytes: si.unrefBytes,
	}
	newSi.unrefs = make([]IFCERFTBlockInfo, len(si.unrefs))
	copy(newSi.unrefs, si.unrefs)
	if si.bps != nil {
		newSi.bps = si.bps.DeepCopy()
	}
	if si.op != nil {
		err := CodecUpdate(codec, &newSi.op, si.op)
		if err != nil {
			return nil, err
		}
	}
	return newSi, nil
}

// folderBlockOps contains all the fields that must be synchronized by
// blockLock. It will eventually also contain all the methods that
// must be synchronized by blockLock, so that folderBranchOps will
// have no knowledge of blockLock.
//
// -- And now, a primer on tracking dirty bytes --
//
// The DirtyBlockCache tracks the number of bytes that are dirtied
// system-wide, as the number of bytes that haven't yet been synced
// ("unsynced"), and a number of bytes that haven't yet been resolved
// yet because the overall file Sync hasn't finished yet ("total").
// This data helps us decide when we need to block incoming Writes, in
// order to keep memory usage from exploding.
//
// It's the responsibility of folderBlockOps (and its helper struct
// dirtyFile) to update these totals in DirtyBlockCache for the
// individual files within this TLF.  This is complicated by a few things:
//   * New writes to a file are "deferred" while a Sync is happening, and
//     are replayed after the Sync finishes.
//   * Syncs can be canceled or error out halfway through syncing the blocks,
//     leaving the file in a dirty state until the next Sync.
//   * Syncs can fail with a /recoverable/ error, in which case they get
//     retried automatically by folderBranchOps.  In that case, the retried
//     Sync also sucks in any outstanding deferred writes.
//
// With all that in mind, here is the rough breakdown of how this
// bytes-tracking is implemented:
//   * On a Write/Truncate to a block, folderBranchOps counts all the
//     newly-dirtied bytes in a file as "unsynced".  That is, if the block was
//     already in the dirty cache (and not already being synced), only
//     extensions to the block count as "unsynced" bytes.
//   * When a Sync starts, dirtyFile remembers the total of bytes being synced,
//     and the size of each block being synced.
//   * When each block put finishes successfully, dirtyFile subtracts the size
//     of that block from "unsynced".
//   * When a Sync finishes successfully, the total sum of bytes in that sync
//     are subtracted from the "total" dirty bytes outstanding.
//   * If a Sync fails, but some blocks were put successfully, those blocks
//     are "re-dirtied", which means they count as unsynced bytes again.
//     dirtyFile handles this.
//   * When a Write/Truncate is deferred due to an ongoing Sync, its bytes
//     still count towards the "unsynced" total.  In fact, this essentially
//     creates a new copy of those blocks, and the whole size of that block
//     (not just the newly-dirtied bytes) count for the total.  However,
//     when the write gets replayed, folderBlockOps first subtracts those bytes
//     from the system-wide numbers, since they are about to be replayed.
//   * When a Sync is retried after a recoverable failure, dirtyFile adds
//     the newly-dirtied deferred bytes to the system-wide numbers, since they
//     are now being assimilated into this Sync.
//   * dirtyFile also exposes a concept of "orphaned" blocks.  These are child
//     blocks being synced that are now referenced via a new, permanent block
//     ID from the parent indirect block.  This matters for when hard failures
//     occur during a Sync -- the blocks will no longer be accessible under
//     their previous old pointers, and so dirtyFile needs to know their old
//     bytes can be cleaned up now.
type folderBlockOps struct {
	config       IFCERFTConfig
	log          logger.Logger
	folderBranch IFCERFTFolderBranch
	observers    *observerList

	// forceSyncChan can be sent on to trigger an immediate
	// Sync().  It is a blocking channel.
	forceSyncChan chan<- struct{}

	// protects access to blocks in this folder and all fields
	// below.
	blockLock blockLock

	// Which files are currently dirty and have dirty blocks that are either
	// currently syncing, or waiting to be sync'd.
	dirtyFiles map[IFCERFTBlockPointer]*dirtyFile

	// For writes and truncates, track the unsynced to-be-unref'd
	// block infos, per-path.
	unrefCache map[IFCERFTBlockRef]*syncInfo
	// For writes and truncates, track the modified (but not yet
	// committed) directory entries. Maps the entry blockRef to a
	// modified entry.
	deCache map[IFCERFTBlockRef]DirEntry

	// Writes and truncates for blocks that were being sync'd, and
	// need to be replayed after the sync finishes on top of the new
	// versions of the blocks.
	deferredWrites []func(context.Context, *lockState, *IFCERFTRootMetadata, IFCERFTPath) error
	// Blocks that need to be deleted from the dirty cache before any
	// deferred writes are replayed.
	deferredDirtyDeletes []IFCERFTBlockPointer

	// set to true if this write or truncate should be deferred
	doDeferWrite bool

	// nodeCache itself is goroutine-safe, but write/truncate must
	// call PathFromNode() only under blockLock (see nodeCache
	// comments in folder_branch_ops.go).
	nodeCache IFCERFTNodeCache
}

// Only exported methods of folderBlockOps should be used outside of this
// file.
//
// Although, temporarily, folderBranchOps is allowed to reach in and
// manipulate folderBlockOps fields and methods directly.

func (fbo *folderBlockOps) id() IFCERFTTlfID {
	return fbo.folderBranch.Tlf
}

func (fbo *folderBlockOps) branch() IFCERFTBranchName {
	return fbo.folderBranch.Branch
}

// GetState returns the overall block state of this TLF.
func (fbo *folderBlockOps) GetState(lState *lockState) overallBlockState {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	if len(fbo.deCache) == 0 {
		return cleanState
	}
	return dirtyState
}

func (fbo *folderBlockOps) getBlockFromDirtyOrCleanCache(ptr IFCERFTBlockPointer, branch IFCERFTBranchName) (IFCERFTBlock, error) {
	// Check the dirty cache first.
	if block, err := fbo.config.DirtyBlockCache().Get(ptr, branch); err == nil {
		return block, nil
	}

	return fbo.config.BlockCache().Get(ptr)
}

// getBlockHelperLocked retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server. If
// notifyPath is valid and the block isn't cached, trigger a read
// notification.
//
// This must be called only by get{File,Dir}BlockHelperLocked().
func (fbo *folderBlockOps) getBlockHelperLocked(ctx context.Context,
	lState *lockState, md *IFCERFTRootMetadata, ptr IFCERFTBlockPointer, branch IFCERFTBranchName, newBlock makeNewBlock, doCache bool, notifyPath IFCERFTPath) (
	IFCERFTBlock, error) {
	fbo.blockLock.AssertAnyLocked(lState)

	if !ptr.IsValid() {
		return nil, IFCERFTInvalidBlockRefError{ptr.ref()}
	}

	if block, err := fbo.getBlockFromDirtyOrCleanCache(
		ptr, branch); err == nil {
		return block, nil
	}

	// TODO: add an optimization here that will avoid fetching the
	// same block twice from over the network

	// fetch the block, and add to cache
	block := newBlock()

	bops := fbo.config.BlockOps()

	if notifyPath.IsValid() {
		fbo.config.Reporter().Notify(ctx, readNotification(notifyPath, false))
		defer fbo.config.Reporter().Notify(ctx,
			readNotification(notifyPath, true))
	}

	// Unlock the blockLock while we wait for the network, only if
	// it's locked for reading.  If it's locked for writing, that
	// indicates we are performing an atomic write operation, and we
	// need to ensure that nothing else comes in and modifies the
	// blocks, so don't unlock.
	var err error
	fbo.blockLock.DoRUnlockedIfPossible(lState, func(*lockState) {
		err = bops.Get(ctx, md, ptr, block)
	})
	if err != nil {
		return nil, err
	}

	if doCache {
		if err := fbo.config.BlockCache().Put(ptr, fbo.id(), block,
			IFCERFTTransientEntry); err != nil {
			return nil, err
		}
	}
	return block, nil
}

// getFileBlockHelperLocked retrieves the block pointed to by ptr,
// which must be valid, either from an internal cache, the block
// cache, or from the server. An error is returned if the retrieved
// block is not a file block.
//
// This must be called only by GetFileBlockForReading(),
// getFileBlockLocked(), and getFileLocked().
//
// p is used only when reporting errors and sending read
// notifications, and can be empty.
func (fbo *folderBlockOps) getFileBlockHelperLocked(ctx context.Context,
	lState *lockState, md *IFCERFTRootMetadata, ptr IFCERFTBlockPointer, branch IFCERFTBranchName, p IFCERFTPath) (
	*FileBlock, error) {
	fbo.blockLock.AssertAnyLocked(lState)

	block, err := fbo.getBlockHelperLocked(
		ctx, lState, md, ptr, branch, NewFileBlock, true, p)
	if err != nil {
		return nil, err
	}

	fblock, ok := block.(*FileBlock)
	if !ok {
		return nil, IFCERFTNotFileBlockError{ptr, branch, p}
	}

	return fblock, nil
}

// GetBlockForReading retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server.  The
// returned block may have a generic type (not DirBlock or FileBlock).
//
// This should be called for "internal" operations, like conflict
// resolution and state checking, which don't know what kind of block
// the pointer refers to.  The block will not be cached, if it wasn't
// in the cache already.
func (fbo *folderBlockOps) GetBlockForReading(ctx context.Context,
	lState *lockState, md *IFCERFTRootMetadata, ptr IFCERFTBlockPointer, branch IFCERFTBranchName) (
	IFCERFTBlock, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.getBlockHelperLocked(ctx, lState, md, ptr, branch,
		NewCommonBlock, false, IFCERFTPath{})
}

// getDirBlockHelperLocked retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server. An error
// is returned if the retrieved block is not a dir block.
//
// This must be called only by GetDirBlockForReading() and
// getDirLocked().
//
// p is used only when reporting errors, and can be empty.
func (fbo *folderBlockOps) getDirBlockHelperLocked(ctx context.Context,
	lState *lockState, md *IFCERFTRootMetadata, ptr IFCERFTBlockPointer, branch IFCERFTBranchName, p IFCERFTPath) (*DirBlock, error) {
	fbo.blockLock.AssertAnyLocked(lState)

	// Pass in an empty notify path because notifications should only
	// trigger for file reads.
	block, err := fbo.getBlockHelperLocked(
		ctx, lState, md, ptr, branch, NewDirBlock, true, IFCERFTPath{})
	if err != nil {
		return nil, err
	}

	dblock, ok := block.(*DirBlock)
	if !ok {
		return nil, IFCERFTNotDirBlockError{ptr, branch, p}
	}

	return dblock, nil
}

// GetFileBlockForReading retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server. An error
// is returned if the retrieved block is not a file block.
//
// This should be called for "internal" operations, like conflict
// resolution and state checking. "Real" operations should use
// getFileBlockLocked() and getFileLocked() instead.
//
// p is used only when reporting errors, and can be empty.
func (fbo *folderBlockOps) GetFileBlockForReading(ctx context.Context,
	lState *lockState, md *IFCERFTRootMetadata, ptr IFCERFTBlockPointer, branch IFCERFTBranchName, p IFCERFTPath) (*FileBlock, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.getFileBlockHelperLocked(ctx, lState, md, ptr, branch, p)
}

// GetDirBlockForReading retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server. An error
// is returned if the retrieved block is not a dir block.
//
// This should be called for "internal" operations, like conflict
// resolution and state checking. "Real" operations should use
// getDirLocked() instead.
//
// p is used only when reporting errors, and can be empty.
func (fbo *folderBlockOps) GetDirBlockForReading(ctx context.Context,
	lState *lockState, md *IFCERFTRootMetadata, ptr IFCERFTBlockPointer, branch IFCERFTBranchName, p IFCERFTPath) (*DirBlock, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.getDirBlockHelperLocked(ctx, lState, md, ptr, branch, p)
}

// getFileBlockLocked retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server. An error
// is returned if the retrieved block is not a file block.
//
// The given path must be valid, and the given pointer must be its
// tail pointer or an indirect pointer from it. A read notification is
// triggered for the given path only if the block isn't in the cache.
//
// This shouldn't be called for "internal" operations, like conflict
// resolution and state checking -- use GetFileBlockForReading() for
// those instead.
//
// When rtype == blockWrite and the cached version of the block is
// currently clean, or the block is currently being synced, this
// method makes a copy of the file block and returns it.  If this
// method might be called again for the same block within a single
// operation, it is the caller's responsibility to write that block
// back to the cache as dirty.
//
// Note that blockLock must be locked exactly when rtype ==
// blockWrite, and must be r-locked otherwise.  (This differs from
// getDirLocked.)  This is because a write operation (like write,
// truncate and sync which lock blockLock) fetching a file block will
// almost always need to modify that block, and so will pass in
// blockWrite.
func (fbo *folderBlockOps) getFileBlockLocked(ctx context.Context,
	lState *lockState, md *IFCERFTRootMetadata, ptr IFCERFTBlockPointer, file IFCERFTPath, rtype blockReqType) (*FileBlock, error) {
	if rtype == blockRead {
		fbo.blockLock.AssertRLocked(lState)
	} else {
		fbo.blockLock.AssertLocked(lState)
	}

	// Callers should have already done this check, but it doesn't
	// hurt to do it again.
	if !file.IsValid() {
		return nil, IFCERFTInvalidPathError{file}
	}

	fblock, err := fbo.getFileBlockHelperLocked(
		ctx, lState, md, ptr, file.Branch, file)
	if err != nil {
		return nil, err
	}

	if rtype == blockWrite {
		// Copy the block if it's for writing, and either the
		// block is not yet dirty or the block is currently
		// being sync'd and needs a copy even though it's
		// already dirty.
		df := fbo.dirtyFiles[file.TailPointer()]
		if !fbo.config.DirtyBlockCache().IsDirty(ptr, file.Branch) ||
			(df != nil && df.blockNeedsCopy(ptr)) {
			fblock, err = fblock.DeepCopy(fbo.config.Codec())
			if err != nil {
				return nil, err
			}
		}
	}
	return fblock, nil
}

// getFileLocked is getFileBlockLocked called with file.tailPointer().
func (fbo *folderBlockOps) getFileLocked(ctx context.Context,
	lState *lockState, md *IFCERFTRootMetadata, file IFCERFTPath, rtype blockReqType) (*FileBlock, error) {
	return fbo.getFileBlockLocked(
		ctx, lState, md, file.TailPointer(), file, rtype)
}

// GetIndirectFileBlockInfos returns a list of BlockInfos for all
// indirect blocks of the given file.
func (fbo *folderBlockOps) GetIndirectFileBlockInfos(ctx context.Context,
	lState *lockState, md *IFCERFTRootMetadata, file IFCERFTPath) ([]IFCERFTBlockInfo, error) {
	// TODO: handle multiple levels of indirection.
	fBlock, err := func() (*FileBlock, error) {
		fbo.blockLock.RLock(lState)
		defer fbo.blockLock.RUnlock(lState)
		return fbo.getFileBlockLocked(
			ctx, lState, md, file.TailPointer(), file, blockRead)
	}()
	if err != nil {
		return nil, err
	}
	if !fBlock.IsInd {
		return nil, nil
	}
	blockInfos := make([]IFCERFTBlockInfo, len(fBlock.IPtrs))
	for i, ptr := range fBlock.IPtrs {
		blockInfos[i] = ptr.IFCERFTBlockInfo
	}
	return blockInfos, nil
}

// getDirLocked retrieves the block pointed to by the tail pointer of
// the given path, which must be valid, either from the cache or from
// the server. An error is returned if the retrieved block is not a
// dir block.
//
// This shouldn't be called for "internal" operations, like conflict
// resolution and state checking -- use GetDirBlockForReading() for
// those instead.
//
// When rtype == blockWrite and the cached version of the block is
// currently clean, this method makes a copy of the directory block
// and returns it.  If this method might be called again for the same
// block within a single operation, it is the caller's responsibility
// to write that block back to the cache as dirty.
//
// Note that blockLock must be either r-locked or locked, but
// independently of rtype. (This differs from getFileLocked and
// getFileBlockLocked.) File write operations (which lock blockLock)
// don't need a copy of parent dir blocks, and non-file write
// operations do need to copy dir blocks for modifications.
func (fbo *folderBlockOps) getDirLocked(ctx context.Context,
	lState *lockState, md *IFCERFTRootMetadata, dir IFCERFTPath, rtype blockReqType) (
	*DirBlock, error) {
	fbo.blockLock.AssertAnyLocked(lState)

	// Callers should have already done this check, but it doesn't
	// hurt to do it again.
	if !dir.IsValid() {
		return nil, IFCERFTInvalidPathError{dir}
	}

	// Get the block for the last element in the path.
	dblock, err := fbo.getDirBlockHelperLocked(
		ctx, lState, md, dir.TailPointer(), dir.Branch, dir)
	if err != nil {
		return nil, err
	}

	if rtype == blockWrite && !fbo.config.DirtyBlockCache().IsDirty(
		dir.TailPointer(), dir.Branch) {
		// Copy the block if it's for writing and the block is
		// not yet dirty.
		dblock, err = dblock.DeepCopy(fbo.config.Codec())
		if err != nil {
			return nil, err
		}
	}
	return dblock, nil
}

// GetDir retrieves the block pointed to by the tail pointer of the
// given path, which must be valid, either from the cache or from the
// server. An error is returned if the retrieved block is not a dir
// block.
//
// This shouldn't be called for "internal" operations, like conflict
// resolution and state checking -- use GetDirBlockForReading() for
// those instead.
//
// When rtype == blockWrite and the cached version of the block is
// currently clean, this method makes a copy of the directory block
// and returns it.  If this method might be called again for the same
// block within a single operation, it is the caller's responsibility
// to write that block back to the cache as dirty.
func (fbo *folderBlockOps) GetDir(
	ctx context.Context, lState *lockState, md *IFCERFTRootMetadata, dir IFCERFTPath, rtype blockReqType) (*DirBlock, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.getDirLocked(ctx, lState, md, dir, rtype)
}

func (fbo *folderBlockOps) getFileBlockAtOffsetLocked(ctx context.Context,
	lState *lockState, md *IFCERFTRootMetadata, file IFCERFTPath, topBlock *FileBlock,
	off int64, rtype blockReqType) (
	ptr IFCERFTBlockPointer, parentBlock *FileBlock, indexInParent int,
	block *FileBlock, nextBlockStartOff, startOff int64, err error) {
	fbo.blockLock.AssertAnyLocked(lState)

	// find the block matching the offset, if it exists
	ptr = file.TailPointer()
	block = topBlock
	nextBlockStartOff = -1
	startOff = 0
	// search until it's not an indirect block
	for block.IsInd {
		nextIndex := len(block.IPtrs) - 1
		for i, ptr := range block.IPtrs {
			if ptr.Off == off {
				// small optimization to avoid iterating past the right ptr
				nextIndex = i
				break
			} else if ptr.Off > off {
				// i can never be 0, because the first ptr always has
				// an offset at the beginning of the range
				nextIndex = i - 1
				break
			}
		}
		nextPtr := block.IPtrs[nextIndex]
		parentBlock = block
		indexInParent = nextIndex
		startOff = nextPtr.Off
		// there is more to read if we ever took a path through a
		// ptr that wasn't the final ptr in its respective list
		if nextIndex != len(block.IPtrs)-1 {
			nextBlockStartOff = block.IPtrs[nextIndex+1].Off
		}
		ptr = nextPtr.IFCERFTBlockPointer
		if block, err = fbo.getFileBlockLocked(ctx, lState, md, ptr, file, rtype); err != nil {
			return
		}
	}

	return
}

// updateWithDirtyEntriesLocked checks if the given DirBlock has any
// entries that are in deCache (i.e., entries pointing to dirty
// files). If so, it makes a copy with all such entries replaced with
// the ones in deCache and returns it. If not, it just returns the
// given one.
func (fbo *folderBlockOps) updateWithDirtyEntriesLocked(ctx context.Context,
	lState *lockState, block *DirBlock) (*DirBlock, error) {
	fbo.blockLock.AssertAnyLocked(lState)
	// see if this directory has any outstanding writes/truncates that
	// require an updated DirEntry

	// Save some time for the common case of having no dirty
	// files.
	if len(fbo.deCache) == 0 {
		return block, nil
	}

	var dblockCopy *DirBlock
	for k, v := range block.Children {
		de, ok := fbo.deCache[v.ref()]
		if !ok {
			continue
		}

		if dblockCopy == nil {
			var err error
			dblockCopy, err = block.DeepCopy(fbo.config.Codec())
			if err != nil {
				return nil, err
			}
		}

		dblockCopy.Children[k] = de
	}

	if dblockCopy == nil {
		return block, nil
	}

	return dblockCopy, nil
}

// getDirtyDirLocked composes getDirLocked and
// updatedWithDirtyEntriesLocked. Note that a dirty dir means that it
// has entries possibly pointing to dirty files, not that it's dirty
// itself.
func (fbo *folderBlockOps) getDirtyDirLocked(ctx context.Context,
	lState *lockState, md *IFCERFTRootMetadata, dir IFCERFTPath, rtype blockReqType) (
	*DirBlock, error) {
	fbo.blockLock.AssertAnyLocked(lState)

	dblock, err := fbo.getDirLocked(ctx, lState, md, dir, rtype)
	if err != nil {
		return nil, err
	}

	return fbo.updateWithDirtyEntriesLocked(ctx, lState, dblock)
}

// GetDirtyDirChildren returns a map of EntryInfos for the (possibly
// dirty) children entries of the given directory.
func (fbo *folderBlockOps) GetDirtyDirChildren(
	ctx context.Context, lState *lockState, md *IFCERFTRootMetadata, dir IFCERFTPath) (
	map[string]IFCERFTEntryInfo, error) {
	dblock, err := func() (*DirBlock, error) {
		fbo.blockLock.RLock(lState)
		defer fbo.blockLock.RUnlock(lState)
		dblock, err := fbo.getDirtyDirLocked(
			ctx, lState, md, dir, blockRead)
		if err != nil {
			return nil, err
		}
		return dblock, nil
	}()
	if err != nil {
		return nil, err
	}

	children := make(map[string]IFCERFTEntryInfo)
	for k, de := range dblock.Children {
		children[k] = de.IFCERFTEntryInfo
	}
	return children, nil
}

// file must have a valid parent.
func (fbo *folderBlockOps) getDirtyParentAndEntryLocked(ctx context.Context,
	lState *lockState, md *IFCERFTRootMetadata, file IFCERFTPath, rtype blockReqType) (
	*DirBlock, DirEntry, error) {
	fbo.blockLock.AssertAnyLocked(lState)

	if !file.HasValidParent() {
		return nil, DirEntry{}, IFCERFTInvalidParentPathError{file}
	}

	parentPath := file.ParentPath()
	dblock, err := fbo.getDirtyDirLocked(
		ctx, lState, md, *parentPath, rtype)
	if err != nil {
		return nil, DirEntry{}, err
	}

	// make sure it exists
	name := file.TailName()
	de, ok := dblock.Children[name]
	if !ok {
		return nil, DirEntry{}, IFCERFTNoSuchNameError{name}
	}

	return dblock, de, err
}

// GetDirtyParentAndEntry returns a copy of the parent DirBlock
// (suitable for modification) of the given file, which may contain
// entries pointing to other dirty files, and its possibly-dirty
// DirEntry in that directory. file must have a valid parent. Use
// GetDirtyEntry() if you only need the DirEntry.
func (fbo *folderBlockOps) GetDirtyParentAndEntry(
	ctx context.Context, lState *lockState, md *IFCERFTRootMetadata, file IFCERFTPath) (
	*DirBlock, DirEntry, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.getDirtyParentAndEntryLocked(
		ctx, lState, md, file, blockWrite)
}

// file must have a valid parent.
func (fbo *folderBlockOps) getDirtyEntryLocked(ctx context.Context,
	lState *lockState, md *IFCERFTRootMetadata, file IFCERFTPath) (DirEntry, error) {
	// TODO: Since we only need a single DirEntry, avoid having to
	// look up every entry in the DirBlock.
	_, de, err := fbo.getDirtyParentAndEntryLocked(
		ctx, lState, md, file, blockRead)
	return de, err
}

// GetDirtyEntry returns the possibly-dirty DirEntry of the given file
// in its parent DirBlock. file must have a valid parent.
func (fbo *folderBlockOps) GetDirtyEntry(
	ctx context.Context, lState *lockState, md *IFCERFTRootMetadata, file IFCERFTPath) (DirEntry, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.getDirtyEntryLocked(ctx, lState, md, file)
}

func (fbo *folderBlockOps) getOrCreateDirtyFileLocked(lState *lockState,
	file IFCERFTPath) *dirtyFile {
	fbo.blockLock.AssertLocked(lState)
	ptr := file.TailPointer()
	df := fbo.dirtyFiles[ptr]
	if df == nil {
		df = newDirtyFile(file, fbo.config.DirtyBlockCache())
		fbo.dirtyFiles[ptr] = df
	}
	return df
}

// cacheBlockIfNotYetDirtyLocked puts a block into the cache, but only
// does so if the block isn't already marked as dirty in the cache.
// This is useful when operating on a dirty copy of a block that may
// already be in the cache.
func (fbo *folderBlockOps) cacheBlockIfNotYetDirtyLocked(
	lState *lockState, ptr IFCERFTBlockPointer, file IFCERFTPath, block IFCERFTBlock) error {
	fbo.blockLock.AssertLocked(lState)
	df := fbo.getOrCreateDirtyFileLocked(lState, file)
	needsCaching, isSyncing := df.setBlockDirty(ptr)

	if needsCaching {
		err := fbo.config.DirtyBlockCache().Put(ptr, file.Branch, block)
		if err != nil {
			return err
		}
	}

	if isSyncing {
		fbo.doDeferWrite = true
	}
	return nil
}

func (fbo *folderBlockOps) newRightBlockLocked(
	ctx context.Context, lState *lockState, ptr IFCERFTBlockPointer, file IFCERFTPath, pblock *FileBlock,
	off int64, md *IFCERFTRootMetadata) error {
	fbo.blockLock.AssertLocked(lState)

	newRID, err := fbo.config.Crypto().MakeTemporaryBlockID()
	if err != nil {
		return err
	}
	_, uid, err := fbo.config.KBPKI().GetCurrentUserInfo(ctx)
	if err != nil {
		return err
	}
	rblock := &FileBlock{}

	newPtr := IFCERFTBlockPointer{
		ID:      newRID,
		KeyGen:  md.LatestKeyGeneration(),
		DataVer: DefaultNewBlockDataVersion(fbo.config, false),
		IFCERFTBlockContext: IFCERFTBlockContext{
			Creator:  uid,
			RefNonce: IFCERFTZeroBlockRefNonce,
		},
	}

	pblock.IPtrs = append(pblock.IPtrs, IndirectFilePtr{
		IFCERFTBlockInfo: IFCERFTBlockInfo{
			IFCERFTBlockPointer: newPtr,
			EncodedSize:         0,
		},
		Off: off,
	})

	if err = fbo.cacheBlockIfNotYetDirtyLocked(
		lState, newPtr, file, rblock); err != nil {
		return err
	}
	if err = fbo.cacheBlockIfNotYetDirtyLocked(
		lState, ptr, file, pblock); err != nil {
		return err
	}
	return nil
}

func (fbo *folderBlockOps) getOrCreateSyncInfoLocked(
	lState *lockState, de DirEntry) *syncInfo {
	fbo.blockLock.AssertLocked(lState)
	ref := de.ref()
	si, ok := fbo.unrefCache[ref]
	if !ok {
		si = &syncInfo{
			oldInfo: de.IFCERFTBlockInfo,
			op:      newSyncOp(de.IFCERFTBlockPointer),
		}
		fbo.unrefCache[ref] = si
	}
	return si
}

func (fbo *folderBlockOps) mergeUnrefCacheLocked(
	lState *lockState, file IFCERFTPath, md *IFCERFTRootMetadata) {
	fbo.blockLock.AssertRLocked(lState)
	fileRef := file.TailPointer().ref()
	for _, info := range fbo.unrefCache[fileRef].unrefs {
		// it's ok if we push the same ptr.ID/RefNonce multiple times,
		// because the subsequent ones should have a QuotaSize of 0.
		md.AddUnrefBlock(info)
	}
}

// GetDirtyRefs returns a list of references of all known dirty
// blocks.
func (fbo *folderBlockOps) GetDirtyRefs(lState *lockState) []IFCERFTBlockRef {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	var dirtyRefs []IFCERFTBlockRef
	for ref := range fbo.deCache {
		dirtyRefs = append(dirtyRefs, ref)
	}
	return dirtyRefs
}

// fixChildBlocksAfterRecoverableError should be called when a sync
// failed with a recoverable block error on a multi-block file.  It
// makes sure that any outstanding dirty versions of the file are
// fixed up to reflect the fact that some of the indirect pointers now
// need to change.
func (fbo *folderBlockOps) fixChildBlocksAfterRecoverableError(
	ctx context.Context, lState *lockState, file IFCERFTPath, redirtyOnRecoverableError map[IFCERFTBlockPointer]IFCERFTBlockPointer) {
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)

	// If a copy of the top indirect block was made, we need to
	// redirty all the sync'd blocks under their new IDs, so that
	// future syncs will know they failed.
	df := fbo.dirtyFiles[file.TailPointer()]
	if df != nil {
		// Un-orphan old blocks, since we are reverting back to the
		// previous state.
		for _, oldPtr := range redirtyOnRecoverableError {
			fbo.log.CDebugf(ctx, "Un-orphaning %v", oldPtr)
			df.setBlockOrphaned(oldPtr, false)
		}
	}
	if df == nil || !df.isBlockDirty(file.TailPointer()) ||
		!df.isBlockSyncing(file.TailPointer()) {
		return
	}

	dirtyBcache := fbo.config.DirtyBlockCache()
	topBlock, err := dirtyBcache.Get(file.TailPointer(), fbo.branch())
	fblock, ok := topBlock.(*FileBlock)
	if err != nil || !ok {
		fbo.log.CWarningf(ctx, "Couldn't find dirtied "+
			"top-block for %v: %v", file.TailPointer(), err)
		return
	}

	for newPtr, oldPtr := range redirtyOnRecoverableError {
		found := false
		for i, iptr := range fblock.IPtrs {
			if iptr.IFCERFTBlockPointer == newPtr {
				found = true
				fblock.IPtrs[i].EncodedSize = 0
			}
		}
		if !found {
			continue
		}

		fbo.log.CDebugf(ctx, "Re-dirtying %v (and deleting dirty block %v)",
			newPtr, oldPtr)
		// These blocks would have been permanent, so they're
		// definitely still in the cache.
		b, err := fbo.config.BlockCache().Get(newPtr)
		if err != nil {
			fbo.log.CWarningf(ctx, "Couldn't re-dirty %v: %v", newPtr, err)
			continue
		}
		if err = fbo.cacheBlockIfNotYetDirtyLocked(
			lState, newPtr, file, b); err != nil {
			fbo.log.CWarningf(ctx, "Couldn't re-dirty %v: %v", newPtr, err)
		}
		err = dirtyBcache.Delete(oldPtr, fbo.branch())
		if err != nil {
			fbo.log.CDebugf(ctx, "Couldn't del-dirty %v: %v", oldPtr, err)
		}
	}
}

func (fbo *folderBlockOps) nowUnixNano() int64 {
	return fbo.config.Clock().Now().UnixNano()
}

// PrepRename prepares the given rename operation. It returns copies
// of the old and new parent block (which may be the same), what is to
// be the new DirEntry, and a local block cache. It also modifies md,
// which must be a copy.
func (fbo *folderBlockOps) PrepRename(
	ctx context.Context, lState *lockState, md *IFCERFTRootMetadata, oldParent IFCERFTPath, oldName string, newParent IFCERFTPath, newName string) (
	oldPBlock, newPBlock *DirBlock, newDe DirEntry, lbc localBcache,
	err error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)

	// look up in the old path
	oldPBlock, err = fbo.getDirLocked(
		ctx, lState, md, oldParent, blockWrite)
	if err != nil {
		return nil, nil, DirEntry{}, nil, err
	}
	newDe, ok := oldPBlock.Children[oldName]
	// does the name exist?
	if !ok {
		return nil, nil, DirEntry{}, nil, IFCERFTNoSuchNameError{oldName}
	}

	md.AddOp(newRenameOp(oldName, oldParent.TailPointer(), newName,
		newParent.TailPointer(), newDe.IFCERFTBlockPointer, newDe.Type))

	lbc = make(localBcache)
	// TODO: Write a SameBlock() function that can deal properly with
	// dedup'd blocks that share an ID but can be updated separately.
	if oldParent.TailPointer().ID == newParent.TailPointer().ID {
		newPBlock = oldPBlock
	} else {
		newPBlock, err = fbo.getDirLocked(
			ctx, lState, md, newParent, blockWrite)
		if err != nil {
			return nil, nil, DirEntry{}, nil, err
		}
		now := fbo.nowUnixNano()

		oldGrandparent := *oldParent.ParentPath()
		if len(oldGrandparent.path) > 0 {
			// Update the old parent's mtime/ctime, unless the
			// oldGrandparent is the same as newParent (in which
			// case, the syncBlockAndCheckEmbedLocked call by the
			// caller will take care of it).
			if oldGrandparent.TailPointer().ID != newParent.TailPointer().ID {
				b, err := fbo.getDirLocked(ctx, lState, md, oldGrandparent, blockWrite)
				if err != nil {
					return nil, nil, DirEntry{}, nil, err
				}
				if de, ok := b.Children[oldParent.TailName()]; ok {
					de.Ctime = now
					de.Mtime = now
					b.Children[oldParent.TailName()] = de
					// Put this block back into the local cache as dirty
					lbc[oldGrandparent.TailPointer()] = b
				}
			}
		} else {
			md.data.Dir.Ctime = now
			md.data.Dir.Mtime = now
		}
	}
	return oldPBlock, newPBlock, newDe, lbc, nil
}

// The amount that the read timeout is smaller than the global one.
const readTimeoutSmallerBy = 2 * time.Second

// Read reads from the given file into the given buffer at the given
// offset. It returns the number of bytes read and nil, or 0 and the
// error if there was one.
func (fbo *folderBlockOps) Read(
	ctx context.Context, lState *lockState, md *IFCERFTRootMetadata, file IFCERFTPath, dest []byte, off int64) (int64, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)

	// getFileLocked already checks read permissions
	fblock, err := fbo.getFileLocked(ctx, lState, md, file, blockRead)
	if err != nil {
		return 0, err
	}

	// If we have a large enough timeout add a temporary timeout that is
	// readTimeoutSmallerBy. Use that for reading so short reads get returned
	// upstream without triggering the global timeout.
	now := time.Now()
	deadline, haveTimeout := ctx.Deadline()
	if haveTimeout {
		rem := deadline.Sub(now) - readTimeoutSmallerBy
		if rem > 0 {
			var cancel func()
			ctx, cancel = context.WithTimeout(ctx, rem)
			defer cancel()
		}
	}

	nRead := int64(0)
	n := int64(len(dest))

	for nRead < n {
		nextByte := nRead + off
		toRead := n - nRead
		_, _, _, block, nextBlockOff, startOff, err := fbo.getFileBlockAtOffsetLocked(
			ctx, lState, md, file, fblock, nextByte, blockRead)
		if err != nil {
			// If we hit a timeout while reading then return the bytes already read
			// and no error. If the upstream tries to do something blocking they
			// will encounter the error with the context again. Only do this
			// if we have already read some bytes to avoid pathological cases
			// causing a reader looping.
			if err == context.DeadlineExceeded && nRead > 0 {
				fbo.log.CDebugf(ctx, "Read short: read %d bytes of %d\n", nRead, n)
				return nRead, nil
			}
			return 0, err
		}
		blockLen := int64(len(block.Contents))
		lastByteInBlock := startOff + blockLen

		if nextByte >= lastByteInBlock {
			if nextBlockOff > 0 {
				fill := nextBlockOff - nextByte
				if fill > toRead {
					fill = toRead
				}
				fbo.log.CDebugf(ctx, "Read from hole: nextByte=%d lastByteInBlock=%d fill=%d\n", nextByte, lastByteInBlock, fill)
				if fill <= 0 {
					fbo.log.CErrorf(ctx, "Read invalid file fill <= 0 while reading hole")
					return nRead, IFCERFTBadSplitError{}
				}
				for i := 0; i < int(fill); i++ {
					dest[int(nRead)+i] = 0
				}
				nRead += fill
				continue
			}
			return nRead, nil
		} else if toRead > lastByteInBlock-nextByte {
			toRead = lastByteInBlock - nextByte
		}

		firstByteToRead := nextByte - startOff
		copy(dest[nRead:nRead+toRead],
			block.Contents[firstByteToRead:toRead+firstByteToRead])
		nRead += toRead
	}

	return n, nil
}

func (fbo *folderBlockOps) maybeWaitOnDeferredWrites(
	ctx context.Context, lState *lockState, file IFCERFTNode, c IFCERFTDirtyPermChan) error {
	var errListener chan error
	err := func() error {
		fbo.blockLock.Lock(lState)
		defer fbo.blockLock.Unlock(lState)
		filePath, err := fbo.pathFromNodeForBlockWriteLocked(lState, file)
		if err != nil {
			return err
		}
		df := fbo.getOrCreateDirtyFileLocked(lState, filePath)
		errListener = make(chan error, 1)
		df.addErrListener(errListener)
		return nil
	}()
	if err != nil {
		return err
	}

	logTimer := time.After(100 * time.Millisecond)
	doLogUnblocked := false
	for {
		select {
		case <-c:
			if doLogUnblocked {
				fbo.log.CDebugf(ctx, "Write unblocked")
			}
			// Make sure there aren't any queued errors.
			select {
			case err := <-errListener:
				return err
			default:
			}
			return nil
		case <-logTimer:
			// Print a log message once if it's taking too long.
			fbo.log.CDebugf(ctx,
				"Blocking a write because of a full dirty buffer")
			doLogUnblocked = true
		case err := <-errListener:
			// XXX: should we ignore non-fatal errors (like
			// context.Canceled), or errors that are specific only to
			// some other file being sync'd (e.g., "recoverable" block
			// errors from which we couldn't recover)?
			return err
		}
	}
}

func (fbo *folderBlockOps) pathFromNodeForBlockWriteLocked(
	lState *lockState, n IFCERFTNode) (IFCERFTPath, error) {
	fbo.blockLock.AssertLocked(lState)
	p := fbo.nodeCache.PathFromNode(n)
	if !p.IsValid() {
		return IFCERFTPath{}, IFCERFTInvalidPathError{p}
	}
	return p, nil
}

// writeGetFileLocked checks write permissions explicitly for
// writeDataLocked, truncateLocked etc and returns
func (fbo *folderBlockOps) writeGetFileLocked(
	ctx context.Context, lState *lockState, md *IFCERFTRootMetadata, file IFCERFTPath) (*FileBlock, keybase1.UID, error) {
	fbo.blockLock.AssertLocked(lState)

	username, uid, err := fbo.config.KBPKI().GetCurrentUserInfo(ctx)
	if err != nil {
		return nil, "", err
	}
	if !md.GetTlfHandle().IsWriter(uid) {
		return nil, "", IFCERFTNewWriteAccessError(md.GetTlfHandle(), username)
	}
	fblock, err := fbo.getFileLocked(ctx, lState, md, file, blockWrite)
	if err != nil {
		return nil, "", err
	}
	return fblock, uid, nil
}

// createIndirectBlockLocked creates a new indirect block and
// pick a new id for the existing block, and use the existing block's ID for
// the new indirect block that becomes the parent.
func (fbo *folderBlockOps) createIndirectBlockLocked(lState *lockState,
	md *IFCERFTRootMetadata, file IFCERFTPath, uid keybase1.UID, dver IFCERFTDataVer) (
	*FileBlock, error) {

	newID, err := fbo.config.Crypto().MakeTemporaryBlockID()
	if err != nil {
		return nil, err
	}
	fblock := &FileBlock{
		CommonBlock: CommonBlock{
			IsInd: true,
		},
		IPtrs: []IndirectFilePtr{
			{
				IFCERFTBlockInfo: IFCERFTBlockInfo{
					IFCERFTBlockPointer: IFCERFTBlockPointer{
						ID:      newID,
						KeyGen:  md.LatestKeyGeneration(),
						DataVer: dver,
						IFCERFTBlockContext: IFCERFTBlockContext{
							Creator:  uid,
							RefNonce: IFCERFTZeroBlockRefNonce,
						},
					},
					EncodedSize: 0,
				},
				Off: 0,
			},
		},
	}

	df := fbo.getOrCreateDirtyFileLocked(lState, file)
	// Mark the old block ID as not dirty, so that we will treat the
	// old block ID as newly dirtied in cacheBlockIfNotYetDirtyLocked.
	df.setBlockNotDirty(file.TailPointer())
	err = fbo.cacheBlockIfNotYetDirtyLocked(lState, file.TailPointer(), file,
		fblock)
	if err != nil {
		return nil, err
	}
	return fblock, nil
}

// Returns the set of blocks dirtied during this write that might need
// to be cleaned up if the write is deferred.
func (fbo *folderBlockOps) writeDataLocked(
	ctx context.Context, lState *lockState, md *IFCERFTRootMetadata, file IFCERFTPath, data []byte, off int64) (latestWrite WriteRange, dirtyPtrs []IFCERFTBlockPointer, newlyDirtiedChildBytes int64, err error) {
	if sz := off + int64(len(data)); uint64(sz) > fbo.config.MaxFileBytes() {
		return WriteRange{}, nil, 0,
			IFCERFTFileTooBigError{file, sz, fbo.config.MaxFileBytes()}
	}

	fblock, uid, err := fbo.writeGetFileLocked(ctx, lState, md, file)
	if err != nil {
		return WriteRange{}, nil, 0, err
	}

	dirtyBcache := fbo.config.DirtyBlockCache()
	bsplit := fbo.config.BlockSplitter()
	n := int64(len(data))
	nCopied := int64(0)
	df := fbo.getOrCreateDirtyFileLocked(lState, file)
	defer func() {
		// Always update unsynced bytes and potentially force a sync,
		// even on an error, since the previously-dirty bytes stay in
		// the cache.
		df.updateNotYetSyncingBytes(newlyDirtiedChildBytes)
		if dirtyBcache.ShouldForceSync() {
			select {
			// If we can't send on the channel, that means a sync is
			// already in progress.
			case fbo.forceSyncChan <- struct{}{}:
				fbo.log.CDebugf(ctx, "Forcing a sync due to full buffer")
			default:
			}
		}
	}()

	de, err := fbo.getDirtyEntryLocked(ctx, lState, md, file)
	if err != nil {
		return WriteRange{}, nil, 0, err
	}
	oldSize := de.Size

	si := fbo.getOrCreateSyncInfoLocked(lState, de)
	for nCopied < n {
		ptr, parentBlock, indexInParent, block, nextBlockOff, startOff, err :=
			fbo.getFileBlockAtOffsetLocked(
				ctx, lState, md, file, fblock,
				off+nCopied, blockWrite)
		if err != nil {
			return WriteRange{}, nil, newlyDirtiedChildBytes, err
		}

		oldLen := len(block.Contents)
		wasDirty := dirtyBcache.IsDirty(ptr, file.Branch)

		// Take care not to write past the beginning of the next block
		// by using max.
		max := len(data)
		if nextBlockOff > 0 {
			if room := int(nextBlockOff - off); room < max {
				max = room
			}
		}
		nCopied += bsplit.CopyUntilSplit(block, nextBlockOff < 0, data[nCopied:max],
			off+nCopied-startOff)

		// TODO: support multiple levels of indirection.  Right now the
		// code only does one but it should be straightforward to
		// generalize, just annoying

		// if we need another block but there are no more, then make one
		if nCopied < n && nextBlockOff < 0 {
			// If the block doesn't already have a parent block, make one.
			if ptr == file.TailPointer() {
				fblock, err = fbo.createIndirectBlockLocked(lState, md, file,
					uid, DefaultNewBlockDataVersion(fbo.config, false))
				if err != nil {
					return WriteRange{}, nil, newlyDirtiedChildBytes, err
				}
				ptr = fblock.IPtrs[0].IFCERFTBlockPointer
			}

			// Make a new right block and update the parent's
			// indirect block list
			err = fbo.newRightBlockLocked(ctx, lState, file.TailPointer(),
				file, fblock, startOff+int64(len(block.Contents)), md)
			if err != nil {
				return WriteRange{}, nil, newlyDirtiedChildBytes, err
			}
		} else if nCopied < n && off+nCopied < nextBlockOff {
			// We need a new block to be inserted here
			err = fbo.newRightBlockLocked(ctx, lState, file.TailPointer(),
				file, fblock, startOff+int64(len(block.Contents)), md)
			if err != nil {
				return WriteRange{}, nil, newlyDirtiedChildBytes, err
			}
			// And push the indirect pointers to right
			newb := fblock.IPtrs[len(fblock.IPtrs)-1]
			copy(fblock.IPtrs[indexInParent+2:], fblock.IPtrs[indexInParent+1:])
			fblock.IPtrs[indexInParent+1] = newb
		}

		// Only in the last block does the file size grow.
		if oldLen != len(block.Contents) && nextBlockOff < 0 {
			de.EncodedSize = 0
			// update the file info
			de.Size += uint64(len(block.Contents) - oldLen)
			fbo.deCache[file.TailPointer().ref()] = de
		}

		// Calculate the amount of bytes we've newly-dirtied as part
		// of this write.
		newlyDirtiedChildBytes += int64(len(block.Contents))
		if wasDirty {
			newlyDirtiedChildBytes -= int64(oldLen)
		}

		if parentBlock != nil {
			// remember how many bytes it was
			si.unrefs = append(si.unrefs,
				parentBlock.IPtrs[indexInParent].IFCERFTBlockInfo)
			parentBlock.IPtrs[indexInParent].EncodedSize = 0
		}
		// keep the old block ID while it's dirty
		if err = fbo.cacheBlockIfNotYetDirtyLocked(lState, ptr, file,
			block); err != nil {
			return WriteRange{}, nil, newlyDirtiedChildBytes, err
		}
		dirtyPtrs = append(dirtyPtrs, ptr)
	}

	if fblock.IsInd {
		// Always make the top block dirty, so we will sync its
		// indirect blocks.  This has the added benefit of ensuring
		// that any write to a file while it's being sync'd will be
		// deferred, even if it's to a block that's not currently
		// being sync'd, since this top-most block will always be in
		// the dirtyFiles map.
		if err = fbo.cacheBlockIfNotYetDirtyLocked(lState,
			file.TailPointer(), file, fblock); err != nil {
			return WriteRange{}, nil, newlyDirtiedChildBytes, err
		}
		dirtyPtrs = append(dirtyPtrs, file.TailPointer())

		if fbo.doDeferWrite && de.Size > oldSize {
			df.addDeferredNewBytes(int64(de.Size - oldSize))
		}
	}
	latestWrite = si.op.addWrite(uint64(off), uint64(len(data)))

	return latestWrite, dirtyPtrs, newlyDirtiedChildBytes, nil
}

// Write writes the given data to the given file. May block if there
// is too much unflushed data; in that case, it will be unblocked by a
// future sync.
func (fbo *folderBlockOps) Write(
	ctx context.Context, lState *lockState, md *IFCERFTRootMetadata, file IFCERFTNode, data []byte, off int64) error {
	// If there is too much unflushed data, we should wait until some
	// of it gets flush so our memory usage doesn't grow without
	// bound.
	c, err := fbo.config.DirtyBlockCache().RequestPermissionToDirty(ctx,
		int64(len(data)))
	if err != nil {
		return err
	}
	defer fbo.config.DirtyBlockCache().UpdateUnsyncedBytes(-int64(len(data)),
		false)
	err = fbo.maybeWaitOnDeferredWrites(ctx, lState, file, c)
	if err != nil {
		return err
	}

	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)

	filePath, err := fbo.pathFromNodeForBlockWriteLocked(lState, file)
	if err != nil {
		return err
	}

	defer func() {
		fbo.doDeferWrite = false
	}()

	latestWrite, dirtyPtrs, newlyDirtiedChildBytes, err := fbo.writeDataLocked(
		ctx, lState, md, filePath, data, off)
	if err != nil {
		return err
	}

	fbo.observers.localChange(ctx, file, latestWrite)

	if fbo.doDeferWrite {
		// There's an ongoing sync, and this write altered dirty
		// blocks that are in the process of syncing.  So, we have to
		// redo this write once the sync is complete, using the new
		// file path.
		//
		// There is probably a less terrible of doing this that
		// doesn't involve so much copying and rewriting, but this is
		// the most obviously correct way.
		dataCopy := make([]byte, len(data))
		copy(dataCopy, data)
		fbo.log.CDebugf(ctx, "Deferring a write to file %v off=%d len=%d",
			filePath.TailPointer(), off, len(data))
		fbo.deferredDirtyDeletes = append(fbo.deferredDirtyDeletes,
			dirtyPtrs...)
		fbo.deferredWrites = append(fbo.deferredWrites,
			func(ctx context.Context, lState *lockState, rmd *IFCERFTRootMetadata, f IFCERFTPath) error {
				// We are about to re-dirty these bytes, so mark that
				// they will no longer be synced via the old file.
				df := fbo.getOrCreateDirtyFileLocked(lState, filePath)
				df.updateNotYetSyncingBytes(-newlyDirtiedChildBytes)

				// Write the data again.  We know this won't be
				// deferred, so no need to check the new ptrs.
				_, _, _, err = fbo.writeDataLocked(
					ctx, lState, rmd, f, dataCopy, off)
				return err
			})
	}

	return nil
}

// truncateExtendLocked is called by truncateLocked to extend a file and
// creates a hole.
func (fbo *folderBlockOps) truncateExtendLocked(
	ctx context.Context, lState *lockState, md *IFCERFTRootMetadata, file IFCERFTPath, size uint64) (WriteRange, []IFCERFTBlockPointer, error) {

	if size > fbo.config.MaxFileBytes() {
		return WriteRange{}, nil, IFCERFTFileTooBigError{file, int64(size), fbo.config.MaxFileBytes()}
	}

	fblock, uid, err := fbo.writeGetFileLocked(ctx, lState, md, file)
	if err != nil {
		return WriteRange{}, nil, err
	}

	var dirtyPtrs []IFCERFTBlockPointer

	fbo.log.CDebugf(ctx, "truncateExtendLocked: extending fblock %#v", fblock)
	if !fblock.IsInd {
		fbo.log.CDebugf(ctx, "truncateExtendLocked: making block indirect %v", file.TailPointer())
		old := fblock
		fblock, err = fbo.createIndirectBlockLocked(lState, md, file, uid,
			DefaultNewBlockDataVersion(fbo.config, true))
		if err != nil {
			return WriteRange{}, nil, err
		}
		fblock.IPtrs[0].Holes = true
		err = fbo.cacheBlockIfNotYetDirtyLocked(lState,
			fblock.IPtrs[0].IFCERFTBlockPointer, file, old)
		if err != nil {
			return WriteRange{}, nil, err
		}
		dirtyPtrs = append(dirtyPtrs, fblock.IPtrs[0].IFCERFTBlockPointer)
		fbo.log.CDebugf(ctx, "truncateExtendLocked: new zero data block %v", fblock.IPtrs[0].IFCERFTBlockPointer)
	}

	// TODO: support multiple levels of indirection.  Right now the
	// code only does one but it should be straightforward to
	// generalize, just annoying

	err = fbo.newRightBlockLocked(ctx, lState, file.TailPointer(),
		file, fblock, int64(size), md)
	if err != nil {
		return WriteRange{}, nil, err
	}
	dirtyPtrs = append(dirtyPtrs, fblock.IPtrs[len(fblock.IPtrs)-1].IFCERFTBlockPointer)
	fbo.log.CDebugf(ctx, "truncateExtendLocked: new right data block %v",
		fblock.IPtrs[len(fblock.IPtrs)-1].IFCERFTBlockPointer)

	de, err := fbo.getDirtyEntryLocked(ctx, lState, md, file)
	if err != nil {
		return WriteRange{}, nil, err
	}

	si := fbo.getOrCreateSyncInfoLocked(lState, de)

	de.EncodedSize = 0
	// update the file info
	de.Size = size
	fbo.deCache[file.TailPointer().ref()] = de

	// Mark all for presense of holes, one would be enough,
	// but this is more robust and easy.
	for i := range fblock.IPtrs {
		fblock.IPtrs[i].Holes = true
	}
	// Always make the top block dirty, so we will sync its
	// indirect blocks.  This has the added benefit of ensuring
	// that any write to a file while it's being sync'd will be
	// deferred, even if it's to a block that's not currently
	// being sync'd, since this top-most block will always be in
	// the fileBlockStates map.
	err = fbo.cacheBlockIfNotYetDirtyLocked(lState,
		file.TailPointer(), file, fblock)
	if err != nil {
		return WriteRange{}, nil, err
	}
	dirtyPtrs = append(dirtyPtrs, file.TailPointer())
	latestWrite := si.op.addTruncate(size)

	if fbo.config.DirtyBlockCache().ShouldForceSync() {
		select {
		// If we can't send on the channel, that means a sync is
		// already in progress
		case fbo.forceSyncChan <- struct{}{}:
			fbo.log.CDebugf(ctx, "Forcing a sync due to full buffer")
		default:
		}
	}

	fbo.log.CDebugf(ctx, "truncateExtendLocked: done")
	return latestWrite, dirtyPtrs, nil
}

// truncateExtendCutoffPoint is the amount of data in extending
// truncate that will trigger the extending with a hole algorithm.
const truncateExtendCutoffPoint = 128 * 1024

// Returns the set of newly-ID'd blocks created during this truncate
// that might need to be cleaned up if the truncate is deferred.
func (fbo *folderBlockOps) truncateLocked(
	ctx context.Context, lState *lockState, md *IFCERFTRootMetadata, file IFCERFTPath, size uint64) (*WriteRange, []IFCERFTBlockPointer, int64, error) {
	fblock, _, err := fbo.writeGetFileLocked(ctx, lState, md, file)
	if err != nil {
		return &WriteRange{}, nil, 0, err
	}

	// find the block where the file should now end
	iSize := int64(size) // TODO: deal with overflow
	ptr, parentBlock, indexInParent, block, nextBlockOff, startOff, err :=
		fbo.getFileBlockAtOffsetLocked(
			ctx, lState, md, file, fblock, iSize, blockWrite)

	currLen := int64(startOff) + int64(len(block.Contents))
	if currLen+truncateExtendCutoffPoint < iSize {
		latestWrite, dirtyPtrs, err := fbo.truncateExtendLocked(
			ctx, lState, md, file, uint64(iSize))
		if err != nil {
			return &latestWrite, dirtyPtrs, 0, err
		}
		return &latestWrite, dirtyPtrs, 0, err
	} else if currLen < iSize {
		moreNeeded := iSize - currLen
		latestWrite, dirtyPtrs, newlyDirtiedChildBytes, err :=
			fbo.writeDataLocked(ctx, lState, md, file,
				make([]byte, moreNeeded, moreNeeded), currLen)
		if err != nil {
			return &latestWrite, dirtyPtrs, newlyDirtiedChildBytes, err
		}
		return &latestWrite, dirtyPtrs, newlyDirtiedChildBytes, err
	} else if currLen == iSize && nextBlockOff < 0 {
		// same size!
		return nil, nil, 0, nil
	}

	// update the local entry size
	de, err := fbo.getDirtyEntryLocked(ctx, lState, md, file)
	if err != nil {
		return nil, nil, 0, err
	}

	oldLen := len(block.Contents)
	dirtyBcache := fbo.config.DirtyBlockCache()
	wasDirty := dirtyBcache.IsDirty(ptr, file.Branch)

	// otherwise, we need to delete some data (and possibly entire blocks)
	block.Contents = append([]byte(nil), block.Contents[:iSize-startOff]...)

	newlyDirtiedChildBytes := int64(len(block.Contents))
	if wasDirty {
		newlyDirtiedChildBytes -= int64(oldLen) // negative
	}
	df := fbo.getOrCreateDirtyFileLocked(lState, file)
	df.updateNotYetSyncingBytes(newlyDirtiedChildBytes)

	si := fbo.getOrCreateSyncInfoLocked(lState, de)
	if nextBlockOff > 0 {
		// TODO: if indexInParent == 0, we can remove the level of indirection
		for _, ptr := range parentBlock.IPtrs[indexInParent+1:] {
			si.unrefs = append(si.unrefs, ptr.IFCERFTBlockInfo)
		}
		parentBlock.IPtrs = parentBlock.IPtrs[:indexInParent+1]
		// always make the parent block dirty, so we will sync it
		if err = fbo.cacheBlockIfNotYetDirtyLocked(lState,
			file.TailPointer(), file, parentBlock); err != nil {
			return nil, nil, newlyDirtiedChildBytes, err
		}
	}

	if fblock.IsInd {
		// Always make the top block dirty, so we will sync its
		// indirect blocks.  This has the added benefit of ensuring
		// that any truncate to a file while it's being sync'd will be
		// deferred, even if it's to a block that's not currently
		// being sync'd, since this top-most block will always be in
		// the dirtyFiles map.
		if err = fbo.cacheBlockIfNotYetDirtyLocked(lState,
			file.TailPointer(), file, fblock); err != nil {
			return nil, nil, newlyDirtiedChildBytes, err
		}
	}

	if parentBlock != nil {
		// TODO: When we implement more than one level of indirection,
		// make sure that the pointer to parentBlock in the grandparent block
		// has EncodedSize 0.
		si.unrefs = append(si.unrefs,
			parentBlock.IPtrs[indexInParent].IFCERFTBlockInfo)
		parentBlock.IPtrs[indexInParent].EncodedSize = 0
	}

	latestWrite := si.op.addTruncate(size)

	de.EncodedSize = 0
	de.Size = size
	fbo.deCache[file.TailPointer().ref()] = de

	// Keep the old block ID while it's dirty.
	if err = fbo.cacheBlockIfNotYetDirtyLocked(lState,
		ptr, file, block); err != nil {
		return nil, nil, newlyDirtiedChildBytes, err
	}

	return &latestWrite, nil, newlyDirtiedChildBytes, nil
}

// Truncate truncates or extends the given file to the given size.
// May block if there is too much unflushed data; in that case, it
// will be unblocked by a future sync.
func (fbo *folderBlockOps) Truncate(
	ctx context.Context, lState *lockState, md *IFCERFTRootMetadata, file IFCERFTNode, size uint64) error {
	// If there is too much unflushed data, we should wait until some
	// of it gets flush so our memory usage doesn't grow without
	// bound.
	//
	// Assume the whole remaining file will be dirty after this
	// truncate.  TODO: try to figure out how many bytes actually will
	// be dirtied ahead of time?
	c, err := fbo.config.DirtyBlockCache().RequestPermissionToDirty(ctx,
		int64(size))
	if err != nil {
		return err
	}
	defer fbo.config.DirtyBlockCache().UpdateUnsyncedBytes(-int64(size), false)
	err = fbo.maybeWaitOnDeferredWrites(ctx, lState, file, c)
	if err != nil {
		return err
	}

	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)

	filePath, err := fbo.pathFromNodeForBlockWriteLocked(lState, file)
	if err != nil {
		return err
	}

	defer func() {
		fbo.doDeferWrite = false
	}()

	latestWrite, dirtyPtrs, newlyDirtiedChildBytes, err := fbo.truncateLocked(
		ctx, lState, md, filePath, size)
	if err != nil {
		return err
	}

	if latestWrite != nil {
		fbo.observers.localChange(ctx, file, *latestWrite)
	}

	if fbo.doDeferWrite {
		// There's an ongoing sync, and this truncate altered
		// dirty blocks that are in the process of syncing.  So,
		// we have to redo this truncate once the sync is complete,
		// using the new file path.
		fbo.log.CDebugf(ctx, "Deferring a truncate to file %v",
			filePath.TailPointer())
		fbo.deferredDirtyDeletes = append(fbo.deferredDirtyDeletes,
			dirtyPtrs...)
		fbo.deferredWrites = append(fbo.deferredWrites,
			func(ctx context.Context, lState *lockState, rmd *IFCERFTRootMetadata, f IFCERFTPath) error {
				// We are about to re-dirty these bytes, so mark that
				// they will no longer be synced via the old file.
				df := fbo.getOrCreateDirtyFileLocked(lState, filePath)
				df.updateNotYetSyncingBytes(-newlyDirtiedChildBytes)

				// Truncate the file again.  We know this won't be
				// deferred, so no need to check the new ptrs.
				_, _, _, err := fbo.truncateLocked(
					ctx, lState, rmd, f, size)
				return err
			})
	}

	return nil
}

// IsDirty returns whether the given file is dirty; if false is
// returned, then the file doesn't need to be synced.
func (fbo *folderBlockOps) IsDirty(lState *lockState, file IFCERFTPath) bool {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.config.DirtyBlockCache().IsDirty(file.TailPointer(), file.Branch)
}

func (fbo *folderBlockOps) clearCacheInfoLocked(lState *lockState,
	file IFCERFTPath) error {
	fbo.blockLock.AssertLocked(lState)
	ref := file.TailPointer().ref()
	delete(fbo.deCache, ref)
	delete(fbo.unrefCache, ref)
	df := fbo.dirtyFiles[file.TailPointer()]
	if df != nil {
		err := df.finishSync()
		if err != nil {
			return err
		}
		delete(fbo.dirtyFiles, file.TailPointer())
	}
	return nil
}

// ClearCacheInfo removes any cached info for the the given file.
func (fbo *folderBlockOps) ClearCacheInfo(lState *lockState, file IFCERFTPath) error {
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)
	return fbo.clearCacheInfoLocked(lState, file)
}

// revertSyncInfoAfterRecoverableError updates the saved sync info to
// include all the blocks from before the error, except for those that
// have encountered recoverable block errors themselves.
func (fbo *folderBlockOps) revertSyncInfoAfterRecoverableError(
	blocksToRemove []IFCERFTBlockPointer, si, savedSi *syncInfo) {
	// This sync will be retried and needs new blocks, so
	// reset everything in the sync info.
	*si = *savedSi
	if si.bps == nil {
		return
	}

	si.bps.blockStates = nil

	// Mark any bad pointers so they get skipped next time.
	blocksToRemoveSet := make(map[IFCERFTBlockPointer]bool)
	for _, ptr := range blocksToRemove {
		blocksToRemoveSet[ptr] = true
	}

	for _, bs := range savedSi.bps.blockStates {
		// Only save the good pointers
		if !blocksToRemoveSet[bs.blockPtr] {
			si.bps.blockStates = append(si.bps.blockStates, bs)
		}
	}
}

// ReadyBlock is a thin wrapper around BlockOps.Ready() that handles
// checking for duplicates.
func (fbo *folderBlockOps) ReadyBlock(ctx context.Context, md *IFCERFTRootMetadata, block IFCERFTBlock, uid keybase1.UID) (
	info IFCERFTBlockInfo, plainSize int, readyBlockData IFCERFTReadyBlockData, err error) {
	var ptr IFCERFTBlockPointer
	if fBlock, ok := block.(*FileBlock); ok && !fBlock.IsInd {
		// first see if we are duplicating any known blocks in this folder
		ptr, err = fbo.config.BlockCache().CheckForKnownPtr(fbo.id(), fBlock)
		if err != nil {
			return
		}
	}

	// Ready the block, even in the case where we can reuse an
	// existing block, just so that we know what the size of the
	// encrypted data will be.
	id, plainSize, readyBlockData, err :=
		fbo.config.BlockOps().Ready(ctx, md, block)
	if err != nil {
		return
	}

	if ptr.IsInitialized() {
		ptr.RefNonce, err = fbo.config.Crypto().MakeBlockRefNonce()
		if err != nil {
			return
		}
		ptr.SetWriter(uid)
	} else {
		ptr = IFCERFTBlockPointer{
			ID:      id,
			KeyGen:  md.LatestKeyGeneration(),
			DataVer: block.DataVersion(),
			IFCERFTBlockContext: IFCERFTBlockContext{
				Creator:  uid,
				RefNonce: IFCERFTZeroBlockRefNonce,
			},
		}
	}

	info = IFCERFTBlockInfo{
		IFCERFTBlockPointer: ptr,
		EncodedSize:         uint32(readyBlockData.GetEncodedSize()),
	}
	return
}

// fileSyncState holds state for a sync operation for a single
// file.
type fileSyncState struct {
	// If fblock is non-nil, the (dirty, indirect, cached) block
	// it points to will be set to savedFblock on a recoverable
	// error.
	fblock, savedFblock *FileBlock

	// redirtyOnRecoverableError, which is non-nil only when fblock is
	// non-nil, contains pointers that need to be re-dirtied if the
	// top block gets copied during the sync, and a recoverable error
	// happens.  Maps to the old block pointer for the block, which
	// would need a DirtyBlockCache.Delete.
	redirtyOnRecoverableError map[IFCERFTBlockPointer]IFCERFTBlockPointer

	// If si is non-nil, its updated state will be reset on
	// error. Also, if the error is recoverable, it will be
	// reverted to savedSi.
	//
	// TODO: Working with si in this way is racy, since si is a
	// member of unrefCache.
	si, savedSi *syncInfo

	// oldFileBlockPtrs is a list of transient entries in the
	// block cache for the file, which should be removed when the
	// sync finishes.
	oldFileBlockPtrs []IFCERFTBlockPointer

	// newIndirectFileBlockPtrs is a list of permanent entries
	// added to the block cache for the file, which should be
	// removed after the blocks have been sent to the server.
	// They are not removed on an error, because in that case the
	// file is still dirty locally and may get another chance to
	// be sync'd.
	//
	// TODO: This can be a list of IDs instead.
	newIndirectFileBlockPtrs []IFCERFTBlockPointer
}

// startSyncWriteLocked contains the portion of StartSync() that's
// done while write-locking blockLock.
func (fbo *folderBlockOps) startSyncWriteLocked(ctx context.Context,
	lState *lockState, md *IFCERFTRootMetadata, uid keybase1.UID, file IFCERFTPath) (
	fblock *FileBlock, bps *blockPutState, syncState fileSyncState,
	err error) {
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)

	// update the parent directories, and write all the new blocks out
	// to disk
	fblock, err = fbo.getFileLocked(ctx, lState, md, file, blockWrite)
	if err != nil {
		return nil, nil, syncState, err
	}

	fileRef := file.TailPointer().ref()
	si, ok := fbo.unrefCache[fileRef]
	if !ok {
		return nil, nil, syncState, fmt.Errorf("No syncOp found for file ref %v", fileRef)
	}

	md.AddOp(si.op)

	// Fill in syncState.
	if fblock.IsInd {
		fblockCopy, err := fblock.DeepCopy(fbo.config.Codec())
		if err != nil {
			return nil, nil, syncState, err
		}
		syncState.fblock = fblock
		syncState.savedFblock = fblockCopy
		syncState.redirtyOnRecoverableError = make(map[IFCERFTBlockPointer]IFCERFTBlockPointer)
	}
	syncState.si = si
	syncState.savedSi, err = si.DeepCopy(fbo.config.Codec())
	if err != nil {
		return nil, nil, syncState, err
	}

	if si.bps == nil {
		si.bps = newBlockPutState(1)
	} else {
		// reinstate byte accounting from the previous Sync
		md.RefBytes = si.refBytes
		md.DiskUsage += si.refBytes
		md.UnrefBytes = si.unrefBytes
		md.DiskUsage -= si.unrefBytes
		syncState.newIndirectFileBlockPtrs = append(
			syncState.newIndirectFileBlockPtrs, si.op.Refs()...)
	}
	defer func() {
		si.refBytes = md.RefBytes
		si.unrefBytes = md.UnrefBytes
	}()

	bcache := fbo.config.BlockCache()
	dirtyBcache := fbo.config.DirtyBlockCache()
	df := fbo.getOrCreateDirtyFileLocked(lState, file)
	// If there were any deferred writes from before we started this
	// sync, it implies that we are retrying a previous sync, and will
	// be assimilating those bytes into this sync.
	// TODO: clear the deferred writes slice in that case?
	df.assimilateDeferredNewBytes()

	// Note: below we add possibly updated file blocks as "unref" and
	// "ref" blocks.  This is fine, since conflict resolution or
	// notifications will never happen within a file.

	// if this is an indirect block:
	//   1) check if each dirty block is split at the right place.
	//   2) if it needs fewer bytes, prepend the extra bytes to the next
	//      block (making a new one if it doesn't exist), and the next block
	//      gets marked dirty
	//   3) if it needs more bytes, then use copyUntilSplit() to fetch bytes
	//      from the next block (if there is one), remove the copied bytes
	//      from the next block and mark it dirty
	//   4) Then go through once more, and ready and finalize each
	//      dirty block, updating its ID in the indirect pointer list
	bsplit := fbo.config.BlockSplitter()
	if fblock.IsInd {
		// TODO: Verify that any getFileBlock... calls here
		// only use the dirty cache and not the network, since
		// the blocks are be dirty.
		for i := 0; i < len(fblock.IPtrs); i++ {
			ptr := fblock.IPtrs[i]
			isDirty := dirtyBcache.IsDirty(ptr.IFCERFTBlockPointer, file.Branch)
			if (ptr.EncodedSize > 0) && isDirty {
				return nil, nil, syncState, IFCERFTInconsistentEncodedSizeError{ptr.IFCERFTBlockInfo}
			}
			if isDirty {
				_, _, _, block, nextBlockOff, _, err :=
					fbo.getFileBlockAtOffsetLocked(
						ctx, lState, md, file, fblock,
						ptr.Off, blockWrite)
				if err != nil {
					return nil, nil, syncState, err
				}

				splitAt := bsplit.CheckSplit(block)
				switch {
				case splitAt == 0:
					continue
				case splitAt > 0:
					endOfBlock := ptr.Off + int64(len(block.Contents))
					extraBytes := block.Contents[splitAt:]
					block.Contents = block.Contents[:splitAt]
					// put the extra bytes in front of the next block
					if nextBlockOff < 0 {
						// need to make a new block
						if err := fbo.newRightBlockLocked(
							ctx, lState, file.TailPointer(), file, fblock,
							endOfBlock, md); err != nil {
							return nil, nil, syncState, err
						}
					}
					rPtr, _, _, rblock, _, _, err :=
						fbo.getFileBlockAtOffsetLocked(
							ctx, lState, md, file, fblock,
							endOfBlock, blockWrite)
					if err != nil {
						return nil, nil, syncState, err
					}
					rblock.Contents = append(extraBytes, rblock.Contents...)
					if err = fbo.cacheBlockIfNotYetDirtyLocked(
						lState, rPtr, file, rblock); err != nil {
						return nil, nil, syncState, err
					}
					fblock.IPtrs[i+1].Off = ptr.Off + int64(len(block.Contents))
					md.AddUnrefBlock(fblock.IPtrs[i+1].IFCERFTBlockInfo)
					fblock.IPtrs[i+1].EncodedSize = 0
				case splitAt < 0:
					if nextBlockOff < 0 {
						// end of the line
						continue
					}

					endOfBlock := ptr.Off + int64(len(block.Contents))
					rPtr, _, _, rblock, _, _, err :=
						fbo.getFileBlockAtOffsetLocked(
							ctx, lState, md, file, fblock,
							endOfBlock, blockWrite)
					if err != nil {
						return nil, nil, syncState, err
					}
					// copy some of that block's data into this block
					nCopied := bsplit.CopyUntilSplit(block, false,
						rblock.Contents, int64(len(block.Contents)))
					rblock.Contents = rblock.Contents[nCopied:]
					if len(rblock.Contents) > 0 {
						if err = fbo.cacheBlockIfNotYetDirtyLocked(
							lState, rPtr, file, rblock); err != nil {
							return nil, nil, syncState, err
						}
						fblock.IPtrs[i+1].Off =
							ptr.Off + int64(len(block.Contents))
						md.AddUnrefBlock(fblock.IPtrs[i+1].IFCERFTBlockInfo)
						fblock.IPtrs[i+1].EncodedSize = 0
					} else {
						// TODO: delete the block, and if we're down
						// to just one indirect block, remove the
						// layer of indirection
						//
						// TODO: When we implement more than one level
						// of indirection, make sure that the pointer
						// to the parent block in the grandparent
						// block has EncodedSize 0.
						md.AddUnrefBlock(fblock.IPtrs[i+1].IFCERFTBlockInfo)
						fblock.IPtrs =
							append(fblock.IPtrs[:i+1], fblock.IPtrs[i+2:]...)
					}
				}
			}
		}

		for i, ptr := range fblock.IPtrs {
			localPtr := ptr.IFCERFTBlockPointer
			isDirty := dirtyBcache.IsDirty(localPtr, file.Branch)
			if (ptr.EncodedSize > 0) && isDirty {
				return nil, nil, syncState, IFCERFTInconsistentEncodedSizeError{ptr.IFCERFTBlockInfo}
			}
			if isDirty {
				_, _, _, block, _, _, err := fbo.getFileBlockAtOffsetLocked(
					ctx, lState, md, file, fblock, ptr.Off, blockWrite)
				if err != nil {
					return nil, nil, syncState, err
				}

				newInfo, _, readyBlockData, err :=
					fbo.ReadyBlock(ctx, md, block, uid)
				if err != nil {
					return nil, nil, syncState, err
				}

				syncState.newIndirectFileBlockPtrs = append(syncState.newIndirectFileBlockPtrs, newInfo.IFCERFTBlockPointer)
				err = bcache.Put(newInfo.IFCERFTBlockPointer, fbo.id(), block, IFCERFTPermanentEntry)
				if err != nil {
					return nil, nil, syncState, err
				}
				df.setBlockOrphaned(ptr.IFCERFTBlockPointer, true)

				// Defer the DirtyBlockCache.Delete until after the
				// new path is ready, in case anyone tries to read the
				// dirty file in the meantime.
				syncState.oldFileBlockPtrs =
					append(syncState.oldFileBlockPtrs, localPtr)

				fblock.IPtrs[i].IFCERFTBlockInfo = newInfo
				md.AddRefBlock(newInfo)
				si.bps.addNewBlock(newInfo.IFCERFTBlockPointer, block, readyBlockData,
					func() error {
						return df.setBlockSynced(localPtr)
					})
				err = df.setBlockSyncing(localPtr)
				if err != nil {
					return nil, nil, syncState, err
				}
				syncState.redirtyOnRecoverableError[newInfo.IFCERFTBlockPointer] = localPtr
			}
		}
	}

	err = df.setBlockSyncing(file.TailPointer())
	if err != nil {
		return nil, nil, syncState, err
	}
	syncState.oldFileBlockPtrs = append(syncState.oldFileBlockPtrs, file.TailPointer())
	// TODO: Returning si.bps in this way is racy, since si is a
	// member of unrefCache.
	return fblock, si.bps, syncState, nil
}

func (fbo *folderBlockOps) makeLocalBcache(ctx context.Context,
	lState *lockState, md *IFCERFTRootMetadata, file IFCERFTPath, si *syncInfo) (
	lbc localBcache, err error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)

	parentPath := file.ParentPath()

	dblock, err := fbo.getDirLocked(
		ctx, lState, md, *parentPath, blockWrite)
	if err != nil {
		return nil, err
	}

	// add in the cached unref pieces and fixup the dir entry
	fbo.mergeUnrefCacheLocked(lState, file, md)

	fileRef := file.TailPointer().ref()

	lbc = make(localBcache)

	// update the file's directory entry to the cached copy
	if de, ok := fbo.deCache[fileRef]; ok {
		// remember the old info
		de.EncodedSize = si.oldInfo.EncodedSize
		dblock.Children[file.TailName()] = de
		lbc[parentPath.TailPointer()] = dblock
	}

	return lbc, nil
}

// StartSync starts a sync for the given file. It returns the new
// FileBlock which has the readied top-level block which includes all
// writes since the last sync. Must be used with CleanupSyncState()
// and FinishSync() like so:
//
// 	fblock, bps, lbc, syncState, err :=
//		...fbo.StartSync(ctx, lState, md, uid, file)
//	defer func() {
//		...fbo.CleanupSyncState(
//			ctx, lState, file, ..., syncState, err)
//	}()
//	if err != nil {
//		...
//	}
//      ...
//
//	... = ...fbo.FinishSync(ctx, lState, file, ..., syncState)
func (fbo *folderBlockOps) StartSync(ctx context.Context,
	lState *lockState, md *IFCERFTRootMetadata, uid keybase1.UID, file IFCERFTPath) (
	fblock *FileBlock, bps *blockPutState, lbc localBcache,
	syncState fileSyncState, err error) {
	fblock, bps, syncState, err = fbo.startSyncWriteLocked(
		ctx, lState, md, uid, file)
	if err != nil {
		return nil, nil, nil, syncState, err
	}

	lbc, err = fbo.makeLocalBcache(ctx, lState, md, file, syncState.si)
	if err != nil {
		return nil, nil, nil, syncState, err
	}
	return fblock, bps, lbc, syncState, err
}

// Does any clean-up for a sync of the given file, given an error
// (which may be nil) that happens during or after StartSync() and
// before FinishSync(). blocksToRemove may be nil.
func (fbo *folderBlockOps) CleanupSyncState(
	ctx context.Context, lState *lockState,
	file IFCERFTPath, blocksToRemove []IFCERFTBlockPointer, result fileSyncState, err error) {
	if err == nil {
		return
	}

	// Notify error listeners before we reset the dirty blocks and
	// permissions to be granted.
	fbo.notifyErrListeners(lState, file.TailPointer(), err)

	// If there was an error, we need to back out any changes that
	// might have been filled into the sync op, because it could
	// get reused again in a later Sync call.
	if result.si != nil {
		result.si.op.resetUpdateState()
	}
	if isRecoverableBlockError(err) {
		if result.si != nil {
			fbo.revertSyncInfoAfterRecoverableError(
				blocksToRemove, result.si, result.savedSi)
		}
		if result.fblock != nil {
			*result.fblock = *result.savedFblock
			fbo.fixChildBlocksAfterRecoverableError(
				ctx, lState, file,
				result.redirtyOnRecoverableError)
		}
	}

	// The sync is over, due to an error, so reset the map so that we
	// don't defer any subsequent writes.
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)
	// Old syncing blocks are now just dirty
	if df := fbo.dirtyFiles[file.TailPointer()]; df != nil {
		df.resetSyncingBlocksToDirty()
	}

	// TODO: Clear deferredWrites and deferredDirtyDeletes?
}

// FinishSync finishes the sync process for a file, given the state
// from StartSync. Specifically, it re-applies any writes that
// happened since the call to StartSync.
func (fbo *folderBlockOps) FinishSync(
	ctx context.Context, lState *lockState,
	oldPath, newPath IFCERFTPath, md *IFCERFTRootMetadata, syncState fileSyncState) (stillDirty bool, err error) {
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)

	dirtyBcache := fbo.config.DirtyBlockCache()
	for _, ptr := range syncState.oldFileBlockPtrs {
		if err := dirtyBcache.Delete(ptr, fbo.branch()); err != nil {
			return true, err
		}
	}

	bcache := fbo.config.BlockCache()
	for _, ptr := range syncState.newIndirectFileBlockPtrs {
		err := bcache.DeletePermanent(ptr.ID)
		if err != nil {
			fbo.log.CWarningf(ctx, "Error when deleting %v from cache: %v",
				ptr.ID, err)
		}
	}

	// Redo any writes or truncates that happened to our file while
	// the sync was happening.
	deletes := fbo.deferredDirtyDeletes
	writes := fbo.deferredWrites
	stillDirty = len(fbo.deferredWrites) != 0
	fbo.deferredDirtyDeletes = nil
	fbo.deferredWrites = nil

	// Clear any dirty blocks that resulted from a write/truncate
	// happening during the sync, since we're redoing them below.
	for _, ptr := range deletes {
		if err := dirtyBcache.Delete(ptr, fbo.branch()); err != nil {
			return true, err
		}
	}

	for _, f := range writes {
		err = f(ctx, lState, md, newPath)
		if err != nil {
			// It's a little weird to return an error from a deferred
			// write here. Hopefully that will never happen.
			return true, err
		}
	}

	// Clear cached info for the old path.  We are guaranteed that any
	// concurrent write to this file was deferred, even if it was to a
	// block that wasn't currently being sync'd, since the top-most
	// block is always in dirtyFiles and is always dirtied during a
	// write/truncate.
	//
	// Also, we can get rid of all the sync state that might have
	// happened during the sync, since we will replay the writes
	// below anyway.
	if err := fbo.clearCacheInfoLocked(lState, oldPath); err != nil {
		return true, err
	}

	return stillDirty, nil
}

// notifyErrListeners notifies any write operations that are blocked
// on a file so that they can learn about unrecoverable sync errors.
func (fbo *folderBlockOps) notifyErrListeners(lState *lockState,
	ptr IFCERFTBlockPointer, err error) {
	if isRecoverableBlockError(err) {
		// Don't bother any listeners with this error, since the sync
		// will be retried.  Unless the sync has reached its retry
		// limit, but in that case the listeners will just proceed as
		// normal once the dirty block cache bytes are freed, and
		// that's ok since this error isn't fatal.
		return
	}
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)
	df := fbo.dirtyFiles[ptr]
	if df != nil {
		df.notifyErrListeners(err)
	}
}

// searchForNodesInDirLocked recursively tries to find a path, and
// ultimately a node, to ptr, given the set of pointers that were
// updated in a particular operation.  The keys in nodeMap make up the
// set of BlockPointers that are being searched for, and nodeMap is
// updated in place to include the corresponding discovered nodes.
//
// Returns the number of nodes found by this invocation.
func (fbo *folderBlockOps) searchForNodesInDirLocked(ctx context.Context,
	lState *lockState, cache IFCERFTNodeCache, newPtrs map[IFCERFTBlockPointer]bool,
	md *IFCERFTRootMetadata, currDir IFCERFTPath, nodeMap map[IFCERFTBlockPointer]IFCERFTNode, numNodesFoundSoFar int) (int, error) {
	fbo.blockLock.AssertAnyLocked(lState)

	dirBlock, err := fbo.getDirLocked(
		ctx, lState, md, currDir, blockRead)
	if err != nil {
		return 0, err
	}

	if numNodesFoundSoFar >= len(nodeMap) {
		return 0, nil
	}

	numNodesFound := 0
	for name, de := range dirBlock.Children {
		if _, ok := nodeMap[de.IFCERFTBlockPointer]; ok {
			childPath := currDir.ChildPath(name, de.IFCERFTBlockPointer)
			// make a node for every pathnode
			var n IFCERFTNode
			for _, pn := range childPath.path {
				n, err = cache.GetOrCreate(pn.IFCERFTBlockPointer, pn.Name, n)
				if err != nil {
					return 0, err
				}
			}
			nodeMap[de.IFCERFTBlockPointer] = n
			numNodesFound++
			if numNodesFoundSoFar+numNodesFound >= len(nodeMap) {
				return numNodesFound, nil
			}
		}

		// otherwise, recurse if this represents an updated block
		if _, ok := newPtrs[de.IFCERFTBlockPointer]; de.Type == IFCERFTDir && ok {
			childPath := currDir.ChildPath(name, de.IFCERFTBlockPointer)
			n, err := fbo.searchForNodesInDirLocked(ctx, lState, cache, newPtrs, md,
				childPath, nodeMap, numNodesFoundSoFar+numNodesFound)
			if err != nil {
				return 0, err
			}
			numNodesFound += n
			if numNodesFoundSoFar+numNodesFound >= len(nodeMap) {
				return numNodesFound, nil
			}
		}
	}

	return numNodesFound, nil
}

// SearchForNodes tries to resolve all the given pointers to a Node
// object, using only the updated pointers specified in newPtrs.
// Returns an error if any subset of the pointer paths do not exist;
// it is the caller's responsibility to decide to error on particular
// unresolved nodes.
func (fbo *folderBlockOps) SearchForNodes(ctx context.Context,
	cache IFCERFTNodeCache, ptrs []IFCERFTBlockPointer, newPtrs map[IFCERFTBlockPointer]bool,
	md *IFCERFTRootMetadata) (map[IFCERFTBlockPointer]IFCERFTNode, error) {
	lState := makeFBOLockState()
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)

	nodeMap := make(map[IFCERFTBlockPointer]IFCERFTNode)
	for _, ptr := range ptrs {
		nodeMap[ptr] = nil
	}

	if len(ptrs) == 0 {
		return nodeMap, nil
	}

	// Start with the root node
	rootPtr := md.data.Dir.IFCERFTBlockPointer
	var node IFCERFTNode
	if cache == fbo.nodeCache {
		// Root node should already exist.
		node = cache.Get(rootPtr.ref())
	} else {
		// Root node may or may not exist.
		var err error
		node, err = cache.GetOrCreate(rootPtr,
			string(md.GetTlfHandle().GetCanonicalName()), nil)
		if err != nil {
			return nil, err
		}
	}
	if node == nil {
		return nil, fmt.Errorf("Cannot find root node corresponding to %v",
			rootPtr)
	}

	// are they looking for the root directory?
	numNodesFound := 0
	if _, ok := nodeMap[rootPtr]; ok {
		nodeMap[rootPtr] = node
		numNodesFound++
		if numNodesFound >= len(nodeMap) {
			return nodeMap, nil
		}
	}

	rootPath := cache.PathFromNode(node)
	if len(rootPath.path) != 1 {
		return nil, fmt.Errorf("Invalid root path for %v: %s",
			md.data.Dir.IFCERFTBlockPointer, rootPath)
	}

	_, err := fbo.searchForNodesInDirLocked(ctx, lState, cache, newPtrs, md, rootPath,
		nodeMap, numNodesFound)
	if err != nil {
		return nil, err
	}

	// Return the whole map even if some nodes weren't found.
	return nodeMap, nil
}

// getUndirtiedEntry returns the clean entry for the given path
// corresponding to a cached dirty entry. If there is no dirty or
// clean entry, nil is returned.
func (fbo *folderBlockOps) getUndirtiedEntry(
	ctx context.Context, lState *lockState, md *IFCERFTRootMetadata, file IFCERFTPath) (*DirEntry, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)

	_, ok := fbo.deCache[file.TailPointer().ref()]
	if !ok {
		return nil, nil
	}

	// Get the undirtied dir block.
	dblock, err := fbo.getDirLocked(
		ctx, lState, md, *file.ParentPath(), blockRead)
	if err != nil {
		return nil, err
	}

	undirtiedEntry, ok := dblock.Children[file.TailName()]
	if !ok {
		return nil, nil
	}

	return &undirtiedEntry, nil
}

func (fbo *folderBlockOps) setCachedAttr(
	ctx context.Context, lState *lockState,
	ref IFCERFTBlockRef, op *setAttrOp, realEntry *DirEntry) {
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)

	fileEntry, ok := fbo.deCache[ref]
	if !ok {
		return
	}

	switch op.Attr {
	case exAttr:
		fileEntry.Type = realEntry.Type
	case mtimeAttr:
		fileEntry.Mtime = realEntry.Mtime
	}
	fbo.deCache[ref] = fileEntry
}

// UpdateCachedEntryAttributes updates any cached entry for the given
// path according to the given op. The node for the path is returned
// if there is one.
func (fbo *folderBlockOps) UpdateCachedEntryAttributes(
	ctx context.Context, lState *lockState, md *IFCERFTRootMetadata, dir IFCERFTPath, op *setAttrOp) (IFCERFTNode, error) {
	childPath := dir.ChildPathNoPtr(op.Name)

	// find the node for the actual change; requires looking up
	// the child entry to get the BlockPointer, unfortunately.
	de, err := fbo.GetDirtyEntry(ctx, lState, md, childPath)
	if err != nil {
		return nil, err
	}

	childNode := fbo.nodeCache.Get(de.ref())
	if childNode == nil {
		// Nothing to do, since the cache entry won't be
		// accessible from any node.
		return nil, nil
	}

	childPath = dir.ChildPath(op.Name, de.IFCERFTBlockPointer)

	// If there's a cache entry, we need to update it, so try and
	// fetch the undirtied entry.
	cleanEntry, err := fbo.getUndirtiedEntry(ctx, lState, md, childPath)
	if err != nil {
		return nil, err
	}

	if cleanEntry != nil {
		fbo.setCachedAttr(ctx, lState, de.ref(), op, cleanEntry)
	}

	return childNode, nil
}

func (fbo *folderBlockOps) getDeferredWriteCountForTest(lState *lockState) int {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return len(fbo.deferredWrites)
}

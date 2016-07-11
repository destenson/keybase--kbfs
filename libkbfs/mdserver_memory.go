// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/keybase/client/go/logger"
	keybase1 "github.com/keybase/client/go/protocol"
	"golang.org/x/net/context"
)

// An mdHandleKey is an encoded BareTlfHandle.
type mdHandleKey string

type mdBlockKey struct {
	tlfID    IFCERFTTlfID
	branchID IFCERFTBranchID
}

type mdBranchKey struct {
	tlfID     IFCERFTTlfID
	deviceKID keybase1.KID
}

type mdBlockMem struct {
	// An encoded RootMetdataSigned.
	encodedMd []byte
	timestamp time.Time
}

type mdBlockMemList struct {
	initialRevision IFCERFTMetadataRevision
	blocks          []mdBlockMem
}

type mdServerMemShared struct {
	// Protects all *db variables and truncateLockManager. After
	// Shutdown() is called, all *db variables and
	// truncateLockManager are nil.
	lock sync.RWMutex
	// Bare TLF handle -> TLF ID
	handleDb map[mdHandleKey]IFCERFTTlfID
	// TLF ID -> latest bare TLF handle
	latestHandleDb map[IFCERFTTlfID]IFCERFTBareTlfHandle
	// (TLF ID, branch ID) -> list of MDs
	mdDb map[mdBlockKey]mdBlockMemList
	// (TLF ID, device KID) -> branch ID
	branchDb            map[mdBranchKey]IFCERFTBranchID
	truncateLockManager *mdServerLocalTruncateLockManager

	updateManager *mdServerLocalUpdateManager
}

// MDServerMemory just stores metadata objects in memory.
type MDServerMemory struct {
	config IFCERFTConfig
	log    logger.Logger

	*mdServerMemShared
}

var _ mdServerLocal = (*MDServerMemory)(nil)

// NewMDServerMemory constructs a new MDServerMemory object that stores
// all data in-memory.
func NewMDServerMemory(config IFCERFTConfig) (*MDServerMemory, error) {
	handleDb := make(map[mdHandleKey]IFCERFTTlfID)
	latestHandleDb := make(map[IFCERFTTlfID]IFCERFTBareTlfHandle)
	mdDb := make(map[mdBlockKey]mdBlockMemList)
	branchDb := make(map[mdBranchKey]IFCERFTBranchID)
	log := config.MakeLogger("")
	truncateLockManager := newMDServerLocalTruncatedLockManager()
	shared := mdServerMemShared{
		handleDb:            handleDb,
		latestHandleDb:      latestHandleDb,
		mdDb:                mdDb,
		branchDb:            branchDb,
		truncateLockManager: &truncateLockManager,
		updateManager:       newMDServerLocalUpdateManager(),
	}
	mdserv := &MDServerMemory{config, log, &shared}
	return mdserv, nil
}

var errMDServerMemoryShutdown = errors.New("MDServerMemory is shutdown")

func (md *MDServerMemory) getHandleID(ctx context.Context, handle IFCERFTBareTlfHandle, mStatus IFCERFTMergeStatus) (tlfID IFCERFTTlfID, created bool, err error) {
	handleBytes, err := md.config.Codec().Encode(handle)
	if err != nil {
		return IFCERFTNullTlfID, false, MDServerError{err}
	}

	md.lock.Lock()
	defer md.lock.Unlock()
	if md.handleDb == nil {
		return IFCERFTNullTlfID, false, errMDServerDiskShutdown
	}

	id, ok := md.handleDb[mdHandleKey(handleBytes)]
	if ok {
		return id, false, nil
	}

	// Non-readers shouldn't be able to create the dir.
	_, uid, err := md.config.KBPKI().GetCurrentUserInfo(ctx)
	if err != nil {
		return IFCERFTNullTlfID, false, MDServerError{err}
	}
	if !handle.IsReader(uid) {
		return IFCERFTNullTlfID, false, MDServerErrorUnauthorized{}
	}

	// Allocate a new random ID.
	id, err = md.config.Crypto().MakeRandomTlfID(handle.IsPublic())
	if err != nil {
		return IFCERFTNullTlfID, false, MDServerError{err}
	}

	md.handleDb[mdHandleKey(handleBytes)] = id
	md.latestHandleDb[id] = handle
	return id, true, nil
}

// GetForHandle implements the MDServer interface for MDServerMemory.
func (md *MDServerMemory) GetForHandle(ctx context.Context, handle IFCERFTBareTlfHandle, mStatus IFCERFTMergeStatus) (IFCERFTTlfID, *IFCERFTRootMetadataSigned, error) {
	id, created, err := md.getHandleID(ctx, handle, mStatus)
	if err != nil {
		return IFCERFTNullTlfID, nil, err
	}

	if created {
		return id, nil, nil
	}

	rmds, err := md.GetForTLF(ctx, id, IFCERFTNullBranchID, mStatus)
	if err != nil {
		return IFCERFTNullTlfID, nil, err
	}
	return id, rmds, nil
}

func (md *MDServerMemory) checkGetParams(
	ctx context.Context, id IFCERFTTlfID, bid IFCERFTBranchID, mStatus IFCERFTMergeStatus) (
	newBid IFCERFTBranchID, err error) {
	if mStatus == IFCERFTMerged && bid != IFCERFTNullBranchID {
		return IFCERFTNullBranchID, MDServerErrorBadRequest{Reason: "Invalid branch ID"}
	}

	// Check permissions

	mergedMasterHead, err :=
		md.getHeadForTLF(ctx, id, IFCERFTNullBranchID, IFCERFTMerged)
	if err != nil {
		return IFCERFTNullBranchID, MDServerError{err}
	}

	_, currentUID, err := md.config.KBPKI().GetCurrentUserInfo(ctx)
	if err != nil {
		return IFCERFTNullBranchID, MDServerError{err}
	}

	ok, err := isReader(currentUID, mergedMasterHead)
	if err != nil {
		return IFCERFTNullBranchID, MDServerError{err}
	}
	if !ok {
		return IFCERFTNullBranchID, MDServerErrorUnauthorized{}
	}

	// Lookup the branch ID if not supplied
	if mStatus == IFCERFTUnmerged && bid == IFCERFTNullBranchID {
		return md.getBranchID(ctx, id)
	}

	return bid, nil
}

// GetForTLF implements the MDServer interface for MDServerMemory.
func (md *MDServerMemory) GetForTLF(ctx context.Context, id IFCERFTTlfID, bid IFCERFTBranchID, mStatus IFCERFTMergeStatus) (*IFCERFTRootMetadataSigned, error) {
	bid, err := md.checkGetParams(ctx, id, bid, mStatus)
	if err != nil {
		return nil, err
	}
	if mStatus == IFCERFTUnmerged && bid == IFCERFTNullBranchID {
		return nil, nil
	}

	rmds, err := md.getHeadForTLF(ctx, id, bid, mStatus)
	if err != nil {
		return nil, MDServerError{err}
	}
	return rmds, nil
}

func (md *MDServerMemory) getHeadForTLF(ctx context.Context, id IFCERFTTlfID, bid IFCERFTBranchID, mStatus IFCERFTMergeStatus) (*IFCERFTRootMetadataSigned, error) {
	key, err := md.getMDKey(id, bid, mStatus)
	if err != nil {
		return nil, err
	}
	md.lock.Lock()
	defer md.lock.Unlock()
	if md.mdDb == nil {
		return nil, errMDServerMemoryShutdown
	}

	blockList, ok := md.mdDb[key]
	if !ok {
		return nil, nil
	}
	blocks := blockList.blocks
	var rmds IFCERFTRootMetadataSigned
	err = md.config.Codec().Decode(blocks[len(blocks)-1].encodedMd, &rmds)
	if err != nil {
		return nil, err
	}
	return &rmds, nil
}

func (md *MDServerMemory) getMDKey(
	id IFCERFTTlfID, bid IFCERFTBranchID, mStatus IFCERFTMergeStatus) (mdBlockKey, error) {
	if (mStatus == IFCERFTMerged) != (bid == IFCERFTNullBranchID) {
		return mdBlockKey{},
			fmt.Errorf("mstatus=%v is inconsistent with bid=%v",
				mStatus, bid)
	}
	return mdBlockKey{id, bid}, nil
}

func (md *MDServerMemory) getBranchKey(ctx context.Context, id IFCERFTTlfID) (
	mdBranchKey, error) {
	// add device KID
	deviceKID, err := md.getCurrentDeviceKID(ctx)
	if err != nil {
		return mdBranchKey{}, err
	}
	return mdBranchKey{id, deviceKID}, nil
}

func (md *MDServerMemory) getCurrentDeviceKID(ctx context.Context) (keybase1.KID, error) {
	key, err := md.config.KBPKI().GetCurrentCryptPublicKey(ctx)
	if err != nil {
		return keybase1.KID(""), err
	}
	return key.kid, nil
}

// GetRange implements the MDServer interface for MDServerMemory.
func (md *MDServerMemory) GetRange(ctx context.Context, id IFCERFTTlfID, bid IFCERFTBranchID, mStatus IFCERFTMergeStatus, start, stop IFCERFTMetadataRevision) (
	[]*IFCERFTRootMetadataSigned, error) {
	md.log.CDebugf(ctx, "GetRange %d %d (%s)", start, stop, mStatus)
	bid, err := md.checkGetParams(ctx, id, bid, mStatus)
	if err != nil {
		return nil, err
	}
	if mStatus == IFCERFTUnmerged && bid == IFCERFTNullBranchID {
		return nil, nil
	}

	key, err := md.getMDKey(id, bid, mStatus)
	if err != nil {
		return nil, MDServerError{err}
	}

	md.lock.Lock()
	defer md.lock.Unlock()
	if md.mdDb == nil {
		return nil, errMDServerMemoryShutdown
	}

	blockList, ok := md.mdDb[key]
	if !ok {
		return nil, nil
	}

	startI := int(start - blockList.initialRevision)
	if startI < 0 {
		startI = 0
	}
	endI := int(stop - blockList.initialRevision + 1)
	blocks := blockList.blocks
	if endI > len(blocks) {
		endI = len(blocks)
	}

	var rmdses []*IFCERFTRootMetadataSigned
	for i := startI; i < endI; i++ {
		var rmds IFCERFTRootMetadataSigned
		err = md.config.Codec().Decode(blocks[i].encodedMd, &rmds)
		if err != nil {
			return nil, MDServerError{err}
		}
		expectedRevision := blockList.initialRevision + IFCERFTMetadataRevision(i)
		if expectedRevision != rmds.MD.Revision {
			panic(fmt.Errorf("expected revision %v, got %v",
				expectedRevision, rmds.MD.Revision))
		}
		rmdses = append(rmdses, &rmds)
	}

	return rmdses, nil
}

// Put implements the MDServer interface for MDServerMemory.
func (md *MDServerMemory) Put(ctx context.Context, rmds *IFCERFTRootMetadataSigned) error {
	mStatus := rmds.MD.MergedStatus()
	bid := rmds.MD.BID

	if (mStatus == IFCERFTMerged) != (bid == IFCERFTNullBranchID) {
		return MDServerErrorBadRequest{Reason: "Invalid branch ID"}
	}

	id := rmds.MD.ID

	// Check permissions

	_, currentUID, err := md.config.KBPKI().GetCurrentUserInfo(ctx)
	if err != nil {
		return MDServerError{err}
	}

	mergedMasterHead, err :=
		md.getHeadForTLF(ctx, id, IFCERFTNullBranchID, IFCERFTMerged)
	if err != nil {
		return MDServerError{err}
	}

	ok, err := isWriterOrValidRekey(
		md.config.Codec(), currentUID, mergedMasterHead, rmds)
	if err != nil {
		return MDServerError{err}
	}
	if !ok {
		return MDServerErrorUnauthorized{}
	}

	head, err := md.getHeadForTLF(ctx, id, bid, mStatus)
	if err != nil {
		return MDServerError{err}
	}

	var recordBranchID bool

	if mStatus == IFCERFTUnmerged && head == nil {
		// currHead for unmerged history might be on the main branch
		prevRev := rmds.MD.Revision - 1
		rmdses, err := md.GetRange(ctx, id, IFCERFTNullBranchID, IFCERFTMerged, prevRev, prevRev)
		if err != nil {
			return MDServerError{err}
		}
		if len(rmdses) != 1 {
			return MDServerError{
				Err: fmt.Errorf("Expected 1 MD block got %d", len(rmdses)),
			}
		}
		head = rmdses[0]
		recordBranchID = true
	}

	// Consistency checks
	if head != nil {
		err := head.MD.CheckValidSuccessorForServer(
			md.config.Crypto(), &rmds.MD)
		if err != nil {
			return err
		}
	}

	// Record branch ID
	if recordBranchID {
		branchKey, err := md.getBranchKey(ctx, id)
		if err != nil {
			return MDServerError{err}
		}
		err = func() error {
			md.lock.Lock()
			defer md.lock.Unlock()
			if md.branchDb == nil {
				return errMDServerMemoryShutdown
			}
			md.branchDb[branchKey] = bid
			return nil
		}()
		if err != nil {
			return err
		}
	}

	encodedMd, err := md.config.Codec().Encode(rmds)
	if err != nil {
		return MDServerError{err}
	}

	block := mdBlockMem{encodedMd, md.config.Clock().Now()}

	// Add an entry with the revision key.
	revKey, err := md.getMDKey(id, bid, mStatus)
	if err != nil {
		return MDServerError{err}
	}

	md.lock.Lock()
	defer md.lock.Unlock()
	if md.mdDb == nil {
		return errMDServerMemoryShutdown
	}

	blockList, ok := md.mdDb[revKey]
	if ok {
		blockList.blocks = append(blockList.blocks, block)
		md.mdDb[revKey] = blockList
	} else {
		md.mdDb[revKey] = mdBlockMemList{
			initialRevision: rmds.MD.Revision,
			blocks:          []mdBlockMem{block},
		}
	}

	if mStatus == IFCERFTMerged &&
		// Don't send notifies if it's just a rekey (the real mdserver
		// sends a "folder needs rekey" notification in this case).
		!(rmds.MD.IsRekeySet() && rmds.MD.IsWriterMetadataCopiedSet()) {
		md.updateManager.setHead(id, md)
	}

	return nil
}

// PruneBranch implements the MDServer interface for MDServerMemory.
func (md *MDServerMemory) PruneBranch(ctx context.Context, id IFCERFTTlfID, bid IFCERFTBranchID) error {
	if bid == IFCERFTNullBranchID {
		return MDServerErrorBadRequest{Reason: "Invalid branch ID"}
	}

	currBID, err := md.getBranchID(ctx, id)
	if err != nil {
		return err
	}
	if currBID == IFCERFTNullBranchID || bid != currBID {
		return MDServerErrorBadRequest{Reason: "Invalid branch ID"}
	}

	// Don't actually delete unmerged history. This is intentional to be consistent
	// with the mdserver behavior-- it garbage collects discarded branches in the
	// background.
	branchKey, err := md.getBranchKey(ctx, id)
	if err != nil {
		return MDServerError{err}
	}
	md.lock.Lock()
	defer md.lock.Unlock()
	if md.mdDb == nil {
		return errMDServerMemoryShutdown
	}

	delete(md.branchDb, branchKey)
	return nil
}

func (md *MDServerMemory) getBranchID(ctx context.Context, id IFCERFTTlfID) (IFCERFTBranchID, error) {
	branchKey, err := md.getBranchKey(ctx, id)
	if err != nil {
		return IFCERFTNullBranchID, MDServerError{err}
	}
	md.lock.Lock()
	defer md.lock.Unlock()
	if md.branchDb == nil {
		return IFCERFTNullBranchID, errMDServerMemoryShutdown
	}

	bid, ok := md.branchDb[branchKey]
	if !ok {
		return IFCERFTNullBranchID, nil
	}
	return bid, nil
}

// RegisterForUpdate implements the MDServer interface for MDServerMemory.
func (md *MDServerMemory) RegisterForUpdate(ctx context.Context, id IFCERFTTlfID, currHead IFCERFTMetadataRevision) (<-chan error, error) {
	// are we already past this revision?  If so, fire observer
	// immediately
	currMergedHeadRev, err := md.getCurrentMergedHeadRevision(ctx, id)
	if err != nil {
		return nil, err
	}

	c := md.updateManager.registerForUpdate(id, currHead, currMergedHeadRev, md)
	return c, nil
}

func (md *MDServerMemory) getCurrentDeviceKIDBytes(ctx context.Context) (
	[]byte, error) {
	buf := &bytes.Buffer{}
	deviceKID, err := md.getCurrentDeviceKID(ctx)
	if err != nil {
		return []byte{}, err
	}
	_, err = buf.Write(deviceKID.ToBytes())
	if err != nil {
		return []byte{}, err
	}
	return buf.Bytes(), nil
}

// TruncateLock implements the MDServer interface for MDServerMemory.
func (md *MDServerMemory) TruncateLock(ctx context.Context, id IFCERFTTlfID) (
	bool, error) {
	md.lock.Lock()
	defer md.lock.Unlock()
	if md.truncateLockManager == nil {
		return false, errMDServerMemoryShutdown
	}

	myKID, err := md.getCurrentDeviceKID(ctx)
	if err != nil {
		return false, err
	}

	return md.truncateLockManager.truncateLock(myKID, id)
}

// TruncateUnlock implements the MDServer interface for MDServerMemory.
func (md *MDServerMemory) TruncateUnlock(ctx context.Context, id IFCERFTTlfID) (
	bool, error) {
	md.lock.Lock()
	defer md.lock.Unlock()
	if md.truncateLockManager == nil {
		return false, errMDServerMemoryShutdown
	}

	myKID, err := md.getCurrentDeviceKID(ctx)
	if err != nil {
		return false, err
	}

	return md.truncateLockManager.truncateUnlock(myKID, id)
}

// Shutdown implements the MDServer interface for MDServerMemory.
func (md *MDServerMemory) Shutdown() {
	md.lock.Lock()
	defer md.lock.Unlock()
	md.handleDb = nil
	md.latestHandleDb = nil
	md.branchDb = nil
	md.truncateLockManager = nil
}

// IsConnected implements the MDServer interface for MDServerMemory.
func (md *MDServerMemory) IsConnected() bool {
	return !md.isShutdown()
}

// RefreshAuthToken implements the MDServer interface for MDServerMemory.
func (md *MDServerMemory) RefreshAuthToken(ctx context.Context) {}

// This should only be used for testing with an in-memory server.
func (md *MDServerMemory) copy(config IFCERFTConfig) mdServerLocal {
	// NOTE: observers and sessionHeads are copied shallowly on
	// purpose, so that the MD server that gets a Put will notify all
	// observers correctly no matter where they got on the list.
	log := config.MakeLogger("")
	return &MDServerMemory{config, log, md.mdServerMemShared}
}

// isShutdown returns whether the logical, shared MDServer instance
// has been shut down.
func (md *MDServerMemory) isShutdown() bool {
	md.lock.RLock()
	defer md.lock.RUnlock()
	return md.handleDb == nil
}

// DisableRekeyUpdatesForTesting implements the MDServer interface.
func (md *MDServerMemory) DisableRekeyUpdatesForTesting() {
	// Nothing to do.
}

// CheckForRekeys implements the MDServer interface.
func (md *MDServerMemory) CheckForRekeys(ctx context.Context) <-chan error {
	// Nothing to do
	c := make(chan error, 1)
	c <- nil
	return c
}

func (md *MDServerMemory) addNewAssertionForTest(uid keybase1.UID,
	newAssertion keybase1.SocialAssertion) error {
	md.lock.Lock()
	defer md.lock.Unlock()
	if md.handleDb == nil {
		return errMDServerMemoryShutdown
	}

	// Iterate through all the handles, and add handles for ones
	// containing newAssertion to now include the uid.
	for hBytes, id := range md.handleDb {
		var h IFCERFTBareTlfHandle
		err := md.config.Codec().Decode([]byte(hBytes), &h)
		if err != nil {
			return err
		}
		assertions := map[keybase1.SocialAssertion]keybase1.UID{
			newAssertion: uid,
		}
		newH := h.ResolveAssertions(assertions)
		if reflect.DeepEqual(h, newH) {
			continue
		}
		newHBytes, err := md.config.Codec().Encode(newH)
		if err != nil {
			return err
		}
		md.handleDb[mdHandleKey(newHBytes)] = id
	}
	return nil
}

func (md *MDServerMemory) getCurrentMergedHeadRevision(
	ctx context.Context, id IFCERFTTlfID) (rev IFCERFTMetadataRevision, err error) {
	head, err := md.GetForTLF(ctx, id, IFCERFTNullBranchID, IFCERFTMerged)
	if err != nil {
		return 0, err
	}
	if head != nil {
		rev = head.MD.Revision
	}
	return
}

// GetLatestHandleForTLF implements the MDServer interface for MDServerMemory.
func (md *MDServerMemory) GetLatestHandleForTLF(_ context.Context, id IFCERFTTlfID) (
	IFCERFTBareTlfHandle, error) {
	md.lock.RLock()
	defer md.lock.RUnlock()
	if md.latestHandleDb == nil {
		return IFCERFTBareTlfHandle{}, errMDServerMemoryShutdown
	}

	return md.latestHandleDb[id], nil
}

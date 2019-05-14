// Copyright (c) 2017, Jonathan Chappelow
// See LICENSE for details.

package blockdata

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/fonero-project/fnod/chaincfg"
	"github.com/fonero-project/fnod/chaincfg/chainhash"
	"github.com/fonero-project/fnod/fnojson"
	"github.com/fonero-project/fnod/fnoutil"
	"github.com/fonero-project/fnod/rpcclient"
	"github.com/fonero-project/fnod/wire"
	apitypes "github.com/fonero-project/fnodata/api/types"
	"github.com/fonero-project/fnodata/db/dbtypes"
	"github.com/fonero-project/fnodata/stakedb"
	"github.com/fonero-project/fnodata/txhelpers"
)

// BlockData contains all the data collected by a Collector and stored
// by a BlockDataSaver. TODO: consider if pointers are desirable here.
type BlockData struct {
	Header           fnojson.GetBlockHeaderVerboseResult
	Connections      int32
	FeeInfo          fnojson.FeeInfoBlock
	CurrentStakeDiff fnojson.GetStakeDifficultyResult
	EstStakeDiff     fnojson.EstimateStakeDiffResult
	PoolInfo         *apitypes.TicketPoolInfo
	ExtraInfo        apitypes.BlockExplorerExtraInfo
	BlockchainInfo   *fnojson.GetBlockChainInfoResult
	PriceWindowNum   int
	IdxBlockInWindow int
	WinningTickets   []string
}

// ToStakeInfoExtended returns an apitypes.StakeInfoExtended object from the
// blockdata
func (b *BlockData) ToStakeInfoExtended() apitypes.StakeInfoExtended {
	return apitypes.StakeInfoExtended{
		Hash:             b.Header.Hash,
		Feeinfo:          b.FeeInfo,
		StakeDiff:        b.CurrentStakeDiff.CurrentStakeDifficulty,
		PriceWindowNum:   b.PriceWindowNum,
		IdxBlockInWindow: b.IdxBlockInWindow,
		PoolInfo:         b.PoolInfo,
	}
}

// ToStakeInfoExtendedEstimates returns an apitypes.StakeInfoExtendedEstimates
// object from the blockdata
func (b *BlockData) ToStakeInfoExtendedEstimates() apitypes.StakeInfoExtendedEstimates {
	return apitypes.StakeInfoExtendedEstimates{
		Hash:    b.Header.Hash,
		Feeinfo: b.FeeInfo,
		StakeDiff: apitypes.StakeDiff{
			GetStakeDifficultyResult: b.CurrentStakeDiff,
			Estimates:                b.EstStakeDiff,
			IdxBlockInWindow:         b.IdxBlockInWindow,
			PriceWindowNum:           b.PriceWindowNum,
		},
		// PriceWindowNum and Idx... are repeated here since this is a kludge
		PriceWindowNum:   b.PriceWindowNum,
		IdxBlockInWindow: b.IdxBlockInWindow,
		PoolInfo:         b.PoolInfo,
	}
}

// ToBlockSummary returns an apitypes.BlockDataBasic object from the blockdata
func (b *BlockData) ToBlockSummary() apitypes.BlockDataBasic {
	t := dbtypes.NewTimeDefFromUNIX(b.Header.Time)
	return apitypes.BlockDataBasic{
		Height:     b.Header.Height,
		Size:       b.Header.Size,
		Hash:       b.Header.Hash,
		Difficulty: b.Header.Difficulty,
		StakeDiff:  b.Header.SBits,
		Time:       apitypes.TimeAPI{S: t},
		PoolInfo:   b.PoolInfo,
	}
}

// ToBlockExplorerSummary returns a BlockExplorerBasic
func (b *BlockData) ToBlockExplorerSummary() apitypes.BlockExplorerBasic {
	extra := b.ExtraInfo
	t := dbtypes.NewTimeDefFromUNIX(b.Header.Time)
	return apitypes.BlockExplorerBasic{
		Height:                 b.Header.Height,
		Size:                   b.Header.Size,
		Voters:                 b.Header.Voters,
		Revocations:            b.Header.Revocations,
		FreshStake:             b.Header.FreshStake,
		StakeDiff:              b.Header.SBits,
		BlockExplorerExtraInfo: extra,
		Time:                   t,
	}
}

// Collector models a structure for the source of the blockdata
type Collector struct {
	mtx          sync.Mutex
	fnodChainSvr *rpcclient.Client
	netParams    *chaincfg.Params
	stakeDB      *stakedb.StakeDatabase
}

// NewCollector creates a new Collector.
func NewCollector(fnodChainSvr *rpcclient.Client, params *chaincfg.Params,
	stakeDB *stakedb.StakeDatabase) *Collector {
	return &Collector{
		fnodChainSvr: fnodChainSvr,
		netParams:    params,
		stakeDB:      stakeDB,
	}
}

// CollectAPITypes uses CollectBlockInfo to collect block data, then organizes
// it into the BlockDataBasic and StakeInfoExtended and fnodataapi types.
func (t *Collector) CollectAPITypes(hash *chainhash.Hash) (*apitypes.BlockDataBasic, *apitypes.StakeInfoExtended) {
	blockDataBasic, feeInfoBlock, _, _, _, err := t.CollectBlockInfo(hash)
	if err != nil {
		return nil, nil
	}

	height := int64(blockDataBasic.Height)
	winSize := t.netParams.StakeDiffWindowSize

	stakeInfoExtended := &apitypes.StakeInfoExtended{
		Hash:             blockDataBasic.Hash,
		Feeinfo:          *feeInfoBlock,
		StakeDiff:        blockDataBasic.StakeDiff,
		PriceWindowNum:   int(height / winSize),
		IdxBlockInWindow: int(height%winSize) + 1,
		PoolInfo:         blockDataBasic.PoolInfo,
	}

	return blockDataBasic, stakeInfoExtended
}

// CollectBlockInfo uses the chain server and the stake DB to collect most of
// the block data required by Collect() that is specific to the block with the
// given hash.
func (t *Collector) CollectBlockInfo(hash *chainhash.Hash) (*apitypes.BlockDataBasic,
	*fnojson.FeeInfoBlock, *fnojson.GetBlockHeaderVerboseResult,
	*apitypes.BlockExplorerExtraInfo, *wire.MsgBlock, error) {
	// Retrieve block from fnod.
	msgBlock, err := t.fnodChainSvr.GetBlock(hash)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	height := msgBlock.Header.Height
	block := fnoutil.NewBlock(msgBlock)
	txLen := len(block.Transactions())

	// Coin supply and block subsidy. If either RPC fails, do not immediately
	// return. Attempt acquisition of other data for this block.
	coinSupply, err := t.fnodChainSvr.GetCoinSupply()
	if err != nil {
		log.Error("GetCoinSupply failed: ", err)
	}
	nbSubsidy, err := t.fnodChainSvr.GetBlockSubsidy(int64(msgBlock.Header.Height)+1, 5)
	if err != nil {
		log.Errorf("GetBlockSubsidy for %d failed: %v", msgBlock.Header.Height, err)
	}

	// Block header
	blockHeaderResults, err := t.fnodChainSvr.GetBlockHeaderVerbose(hash)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	isSideChain := blockHeaderResults.Confirmations == -1

	// Ticket pool info (value, size, avg)
	var ticketPoolInfo *apitypes.TicketPoolInfo
	var found bool
	if ticketPoolInfo, found = t.stakeDB.PoolInfo(*hash); !found {
		// If unable to get ticket pool info for this block, stakedb does
		// not have it. This is expected for side chain blocks, so do not
		// log in that case.
		if !isSideChain {
			log.Infof("Unable to find block (%v) in pool info cache, trying best block.", hash)
		}
		ticketPoolInfo = t.stakeDB.PoolInfoBest()
		if ticketPoolInfo.Height != height {
			if !isSideChain {
				log.Warnf("Ticket pool info not available for block %v.", hash)
			}
			ticketPoolInfo = nil
		}
	}

	// Fee info
	feeInfoBlock := txhelpers.FeeRateInfoBlock(block)
	if feeInfoBlock == nil {
		log.Error("FeeInfoBlock failed")
	}

	// Work/Stake difficulty
	header := msgBlock.Header
	diff := txhelpers.GetDifficultyRatio(header.Bits, t.netParams)
	sdiff := fnoutil.Amount(header.SBits).ToCoin()

	// Output
	blockdata := &apitypes.BlockDataBasic{
		Height:     height,
		Size:       uint32(block.MsgBlock().SerializeSize()),
		Hash:       hash.String(),
		Difficulty: diff,
		StakeDiff:  sdiff,
		Time:       apitypes.TimeAPI{S: dbtypes.NewTimeDef(header.Timestamp)},
		PoolInfo:   ticketPoolInfo,
	}
	extrainfo := &apitypes.BlockExplorerExtraInfo{
		TxLen:            txLen,
		CoinSupply:       int64(coinSupply),
		NextBlockSubsidy: nbSubsidy,
	}
	return blockdata, feeInfoBlock, blockHeaderResults, extrainfo, msgBlock, err
}

// CollectHash collects chain data at the block with the specified hash.
func (t *Collector) CollectHash(hash *chainhash.Hash) (*BlockData, *wire.MsgBlock, error) {
	// In case of a very fast block, make sure previous call to collect is not
	// still running, or fnod may be mad.
	t.mtx.Lock()
	defer t.mtx.Unlock()

	// Time this function
	defer func(start time.Time) {
		log.Debugf("Collector.CollectHash() completed in %v", time.Since(start))
	}(time.Now())

	// Info specific to the block hash
	blockDataBasic, feeInfoBlock, blockHeaderVerbose, extra, msgBlock, err :=
		t.CollectBlockInfo(hash)
	if err != nil {
		return nil, nil, err
	}

	// Number of peer connection to chain server
	numConn, err := t.fnodChainSvr.GetConnectionCount()
	if err != nil {
		log.Warn("Unable to get connection count: ", err)
	}

	// Blockchain info (e.g. syncheight, verificationprogress, chainwork,
	// bestblockhash, initialblockdownload, maxblocksize, deployments, etc.).
	chainInfo, err := t.fnodChainSvr.GetBlockChainInfo()
	if err != nil {
		log.Warn("Unable to get blockchain info: ", err)
	}
	// GetBlockChainInfo is only valid for for chain tip.
	if chainInfo.BestBlockHash != hash.String() {
		chainInfo = nil
	}

	// Output
	height := int64(blockDataBasic.Height)
	winSize := t.netParams.StakeDiffWindowSize
	blockdata := &BlockData{
		Header:           *blockHeaderVerbose,
		Connections:      int32(numConn),
		FeeInfo:          *feeInfoBlock,
		CurrentStakeDiff: fnojson.GetStakeDifficultyResult{CurrentStakeDifficulty: blockDataBasic.StakeDiff},
		EstStakeDiff:     fnojson.EstimateStakeDiffResult{},
		PoolInfo:         blockDataBasic.PoolInfo,
		ExtraInfo:        *extra,
		BlockchainInfo:   chainInfo,
		PriceWindowNum:   int(height / winSize),
		IdxBlockInWindow: int(height%winSize) + 1,
	}

	return blockdata, msgBlock, err
}

// Collect collects chain data at the current best block.
func (t *Collector) Collect() (*BlockData, *wire.MsgBlock, error) {
	// In case of a very fast block, make sure previous call to collect is not
	// still running, or fnod may be mad.
	t.mtx.Lock()
	defer t.mtx.Unlock()

	// Time this function
	defer func(start time.Time) {
		log.Debugf("Collector.Collect() completed in %v", time.Since(start))
	}(time.Now())

	// Run first client call with a timeout.
	type bciRes struct {
		err            error
		blockchainInfo *fnojson.GetBlockChainInfoResult
	}
	toch := make(chan bciRes)

	// Pull and store relevant data about the blockchain (e.g. syncheight,
	// verificationprogress, chainwork, bestblockhash, initialblockdownload,
	// maxblocksize, deployments, etc.).
	go func() {
		blockchainInfo, err := t.fnodChainSvr.GetBlockChainInfo()
		toch <- bciRes{err, blockchainInfo}
	}()

	var bci bciRes
	select {
	case bci = <-toch:
	case <-time.After(time.Second * 10):
		log.Errorf("Timeout waiting for fnod.")
		return nil, nil, errors.New("Timeout")
	}

	if bci.err != nil {
		return nil, nil, fmt.Errorf("unable to get blockchain info: %v", bci.err)
	}

	hash, err := chainhash.NewHashFromStr(bci.blockchainInfo.BestBlockHash)
	if err != nil {
		return nil, nil,
			fmt.Errorf("invalid best block hash from getblockchaininfo: %v", err)
	}

	// Stake difficulty
	stakeDiff, err := t.fnodChainSvr.GetStakeDifficulty()
	if err != nil {
		return nil, nil, err
	}

	// estimatestakediff
	estStakeDiff, err := t.fnodChainSvr.EstimateStakeDiff(nil)
	if err != nil {
		log.Warn("estimatestakediff is broken: ", err)
		estStakeDiff = &fnojson.EstimateStakeDiffResult{}
	}

	// Info specific to the block hash
	blockDataBasic, feeInfoBlock, blockHeaderVerbose, extra, msgBlock, err :=
		t.CollectBlockInfo(hash)
	if err != nil {
		return nil, nil, err
	}

	// Number of peer connection to chain server
	numConn, err := t.fnodChainSvr.GetConnectionCount()
	if err != nil {
		log.Warn("Unable to get connection count: ", err)
	}

	// Output
	height := int64(blockDataBasic.Height)
	winSize := t.netParams.StakeDiffWindowSize
	blockdata := &BlockData{
		Header:           *blockHeaderVerbose,
		Connections:      int32(numConn),
		FeeInfo:          *feeInfoBlock,
		CurrentStakeDiff: *stakeDiff,
		EstStakeDiff:     *estStakeDiff,
		ExtraInfo:        *extra,
		BlockchainInfo:   bci.blockchainInfo,
		PoolInfo:         blockDataBasic.PoolInfo,
		PriceWindowNum:   int(height / winSize),
		IdxBlockInWindow: int(height%winSize) + 1,
	}

	return blockdata, msgBlock, err
}

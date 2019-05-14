package dbtypes

import (
	"fmt"
	"math"

	"github.com/fonero-project/fnod/chaincfg"
	"github.com/fonero-project/fnod/wire"
	"github.com/fonero-project/fnodata/txhelpers"
)

// MsgBlockToDBBlock creates a dbtypes.Block from a wire.MsgBlock
func MsgBlockToDBBlock(msgBlock *wire.MsgBlock, chainParams *chaincfg.Params) *Block {
	// Create the dbtypes.Block structure
	blockHeader := msgBlock.Header

	// convert each transaction hash to a hex string
	var txHashStrs []string
	txHashes := msgBlock.TxHashes()
	for i := range txHashes {
		txHashStrs = append(txHashStrs, txHashes[i].String())
	}

	var stxHashStrs []string
	stxHashes := msgBlock.STxHashes()
	for i := range stxHashes {
		stxHashStrs = append(stxHashStrs, stxHashes[i].String())
	}

	// Assemble the block
	return &Block{
		Hash:       blockHeader.BlockHash().String(),
		Size:       uint32(msgBlock.SerializeSize()),
		Height:     blockHeader.Height,
		Version:    uint32(blockHeader.Version),
		MerkleRoot: blockHeader.MerkleRoot.String(),
		StakeRoot:  blockHeader.StakeRoot.String(),
		NumTx:      uint32(len(msgBlock.Transactions) + len(msgBlock.STransactions)),
		// nil []int64 for TxDbIDs
		NumRegTx:     uint32(len(msgBlock.Transactions)),
		Tx:           txHashStrs,
		NumStakeTx:   uint32(len(msgBlock.STransactions)),
		STx:          stxHashStrs,
		Time:         uint64(blockHeader.Timestamp.Unix()),
		Nonce:        uint64(blockHeader.Nonce),
		VoteBits:     blockHeader.VoteBits,
		FinalState:   blockHeader.FinalState[:],
		Voters:       blockHeader.Voters,
		FreshStake:   blockHeader.FreshStake,
		Revocations:  blockHeader.Revocations,
		PoolSize:     blockHeader.PoolSize,
		Bits:         blockHeader.Bits,
		SBits:        uint64(blockHeader.SBits),
		Difficulty:   txhelpers.GetDifficultyRatio(blockHeader.Bits, chainParams),
		ExtraData:    blockHeader.ExtraData[:],
		StakeVersion: blockHeader.StakeVersion,
		PreviousHash: blockHeader.PrevBlock.String(),
	}
}

// ChartGroupingToInterval converts the chartGrouping value to an actual time
// interval based on the gregorian calendar. AllChartGrouping returns 1 while
// the unknown chartGrouping returns -1 and an error. All the other time
// interval values is returned in terms of seconds.
func ChartGroupingToInterval(grouping ChartGrouping) (float64, error) {
	var hr = 3600.0
	switch grouping {
	case AllChartGrouping:
		return 1, nil

	case DayChartGrouping:
		return hr * 24, nil

	case WeekChartGrouping:
		return hr * 24 * 7, nil

	case MonthChartGrouping:
		return hr * 24 * 30.436875, nil

	case YearChartGrouping:
		return hr * 24 * 30.436875 * 12, nil

	default:
		return -1, fmt.Errorf(`unknown chart grouping "%d"`, grouping)
	}
}

// CalculateHashRate calculates the hashrate from the difficulty value and
// the targetTimePerBlock in seconds. The hashrate returned is in form PetaHash
// per second (PH/s).
func CalculateHashRate(difficulty, targetTimePerBlock float64) float64 {
	return ((difficulty * math.Pow(2, 32)) / targetTimePerBlock) / 1000000
}

// CalculateWindowIndex calculates the window index from the quotient of a block
// height and the chainParams.StakeDiffWindowSize.
func CalculateWindowIndex(height, stakeDiffWindowSize int64) int64 {
	// A window being a group of blocks whose count is defined by
	// chainParams.StakeDiffWindowSize, the first window starts from block 1 to
	// block 144 instead of block 0 to block 143. To obtain the accurate window
	// index value, we should add 1 to the quotient obtained by dividing the block
	// height with the chainParams.StakeDiffWindowSize value; if the float precision
	// is greater than zero. The precision is equal to zero only when the block
	// height value is divisible by the window size.
	windowVal := float64(height) / float64(stakeDiffWindowSize)
	index := int64(windowVal)
	if windowVal != math.Floor(windowVal) || windowVal == 0 {
		index += 1
	}
	return index
}

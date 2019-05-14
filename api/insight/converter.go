// Copyright (c) 2018, The Fonero developers
// Copyright (c) 2017, The fnodata developers
// See LICENSE for details.

package insight

import (
	"github.com/fonero-project/fnod/blockchain"
	"github.com/fonero-project/fnod/fnojson"
	"github.com/fonero-project/fnod/fnoutil"
	apitypes "github.com/fonero-project/fnodata/api/types"
)

// TxConverter converts fnod-tx to insight tx
func (iapi *InsightApi) TxConverter(txs []*fnojson.TxRawResult) ([]apitypes.InsightTx, error) {
	return iapi.FnoToInsightTxns(txs, false, false, false)
}

// FnoToInsightTxns converts a fnojson TxRawResult to a InsightTx. The asm,
// scriptSig, and spending status may be skipped by setting the appropriate
// input arguments.
func (iapi *InsightApi) FnoToInsightTxns(txs []*fnojson.TxRawResult, noAsm, noScriptSig, noSpent bool) ([]apitypes.InsightTx, error) {
	newTxs := make([]apitypes.InsightTx, 0, len(txs))
	for _, tx := range txs {
		// Build new InsightTx
		txNew := apitypes.InsightTx{
			Txid:          tx.Txid,
			Version:       tx.Version,
			Locktime:      tx.LockTime,
			Blockhash:     tx.BlockHash,
			Blockheight:   tx.BlockHeight,
			Confirmations: tx.Confirmations,
			Time:          tx.Time,
			Blocktime:     tx.Blocktime,
			Size:          uint32(len(tx.Hex) / 2),
		}

		// Vins fill
		var vInSum float64
		for vinID, vin := range tx.Vin {
			InsightVin := &apitypes.InsightVin{
				Txid:     vin.Txid,
				Vout:     vin.Vout,
				Sequence: vin.Sequence,
				N:        vinID,
				Value:    vin.AmountIn,
				CoinBase: vin.Coinbase,
			}

			// init ScriptPubKey
			if !noScriptSig {
				InsightVin.ScriptSig = new(apitypes.InsightScriptSig)
				if vin.ScriptSig != nil {
					if !noAsm {
						InsightVin.ScriptSig.Asm = vin.ScriptSig.Asm
					}
					InsightVin.ScriptSig.Hex = vin.ScriptSig.Hex
				}
			}

			// Note: this only gathers information from the database, which does
			// not include mempool transactions.
			_, addresses, value, err := iapi.BlockData.ChainDB.AddressIDsByOutpoint(vin.Txid, vin.Vout)
			if err == nil {
				if len(addresses) > 0 {
					// Update Vin due to FNOD AMOUNTIN - START
					// NOTE THIS IS ONLY USEFUL FOR INPUT AMOUNTS THAT ARE NOT ALSO FROM MEMPOOL
					if tx.Confirmations == 0 {
						InsightVin.Value = fnoutil.Amount(value).ToCoin()
					}
					// Update Vin due to FNOD AMOUNTIN - END
					InsightVin.Addr = addresses[0]
				}
			}
			fnoamt, _ := fnoutil.NewAmount(InsightVin.Value)
			InsightVin.ValueSat = int64(fnoamt)

			vInSum += InsightVin.Value
			txNew.Vins = append(txNew.Vins, InsightVin)

		}

		// Vout fill
		var vOutSum float64
		for _, v := range tx.Vout {
			InsightVout := &apitypes.InsightVout{
				Value: v.Value,
				N:     v.N,
				ScriptPubKey: apitypes.InsightScriptPubKey{
					Addresses: v.ScriptPubKey.Addresses,
					Type:      v.ScriptPubKey.Type,
					Hex:       v.ScriptPubKey.Hex,
				},
			}
			if !noAsm {
				InsightVout.ScriptPubKey.Asm = v.ScriptPubKey.Asm
			}

			txNew.Vouts = append(txNew.Vouts, InsightVout)
			vOutSum += v.Value
		}

		fnoamt, _ := fnoutil.NewAmount(vOutSum)
		txNew.ValueOut = fnoamt.ToCoin()

		fnoamt, _ = fnoutil.NewAmount(vInSum)
		txNew.ValueIn = fnoamt.ToCoin()

		fnoamt, _ = fnoutil.NewAmount(txNew.ValueIn - txNew.ValueOut)
		txNew.Fees = fnoamt.ToCoin()

		// Return true if coinbase value is not empty, return 0 at some fields.
		if txNew.Vins != nil && txNew.Vins[0].CoinBase != "" {
			txNew.IsCoinBase = true
			txNew.ValueIn = 0
			txNew.Fees = 0
			for _, v := range txNew.Vins {
				v.Value = 0
				v.ValueSat = 0
			}
		}

		if !noSpent {
			// Populate the spending status of all vouts. Note: this only
			// gathers information from the database, which does not include
			// mempool transactions.
			addrFull, err := iapi.BlockData.ChainDB.SpendDetailsForFundingTx(txNew.Txid)
			if err != nil {
				return nil, err
			}
			for _, dbaddr := range addrFull {
				txNew.Vouts[dbaddr.FundingTxVoutIndex].SpentIndex = dbaddr.SpendingTxVinIndex
				txNew.Vouts[dbaddr.FundingTxVoutIndex].SpentTxID = dbaddr.SpendingTxHash
				txNew.Vouts[dbaddr.FundingTxVoutIndex].SpentHeight = dbaddr.BlockHeight
			}
		}
		newTxs = append(newTxs, txNew)
	}
	return newTxs, nil
}

// FnoToInsightBlock converts a fnojson.GetBlockVerboseResult to Insight block.
func (iapi *InsightApi) FnoToInsightBlock(inBlocks []*fnojson.GetBlockVerboseResult) ([]*apitypes.InsightBlockResult, error) {
	RewardAtBlock := func(blocknum int64, voters uint16) float64 {
		subsidyCache := blockchain.NewSubsidyCache(0, iapi.params)
		work := blockchain.CalcBlockWorkSubsidy(subsidyCache, blocknum, voters, iapi.params)
		stake := blockchain.CalcStakeVoteSubsidy(subsidyCache, blocknum, iapi.params) * int64(voters)
		tax := blockchain.CalcBlockTaxSubsidy(subsidyCache, blocknum, voters, iapi.params)
		return fnoutil.Amount(work + stake + tax).ToCoin()
	}

	outBlocks := make([]*apitypes.InsightBlockResult, 0, len(inBlocks))
	for _, inBlock := range inBlocks {
		outBlock := apitypes.InsightBlockResult{
			Hash:          inBlock.Hash,
			Confirmations: inBlock.Confirmations,
			Size:          inBlock.Size,
			Height:        inBlock.Height,
			Version:       inBlock.Version,
			MerkleRoot:    inBlock.MerkleRoot,
			Tx:            append(inBlock.Tx, inBlock.STx...),
			Time:          inBlock.Time,
			Nonce:         inBlock.Nonce,
			Bits:          inBlock.Bits,
			Difficulty:    inBlock.Difficulty,
			PreviousHash:  inBlock.PreviousHash,
			NextHash:      inBlock.NextHash,
			Reward:        RewardAtBlock(inBlock.Height, inBlock.Voters),
			IsMainChain:   inBlock.Height > 0,
		}
		outBlocks = append(outBlocks, &outBlock)
	}
	return outBlocks, nil
}

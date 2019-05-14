// Copyright (c) 2018-2019, The Fonero developers
// Copyright (c) 2017, Jonathan Chappelow
// See LICENSE for details.

package notification

import (
	"github.com/fonero-project/fnod/chaincfg/chainhash"
	"github.com/fonero-project/fnod/fnojson"
	"github.com/fonero-project/fnod/fnoutil"

	"github.com/fonero-project/fnodata/txhelpers"
	"github.com/fonero-project/fnodata/api/insight"
)

const (
	// blockConnChanBuffer is the size of the block connected channel buffer.
	blockConnChanBuffer = 4096

	// newTxChanBuffer is the size of the new transaction channel buffer, for
	// ANY transactions are added into mempool.
	newTxChanBuffer = 4096

	// expNewTxChanBuffer is the size of the new transaction buffer for explorer
	expNewTxChanBuffer = 4096

	// relevantMempoolTxChanBuffer is the size of the new transaction channel
	// buffer, for relevant transactions that are added into mempool.
	//relevantMempoolTxChanBuffer = 2048
)

// NtfnChans collects the chain server notification channels
var NtfnChans struct {
	ConnectChan                       chan *chainhash.Hash
	ReorgChanBlockData                chan *txhelpers.ReorgData
	ConnectChanWiredDB                chan *chainhash.Hash
	ReorgChanWiredDB                  chan *txhelpers.ReorgData
	ConnectChanStakeDB                chan *chainhash.Hash
	ReorgChanStakeDB                  chan *txhelpers.ReorgData
	ConnectChanFnopgDB                chan *chainhash.Hash
	ReorgChanFnopgDB                  chan *txhelpers.ReorgData
	UpdateStatusNodeHeight            chan uint32
	UpdateStatusDBHeight              chan uint32
	SpendTxBlockChan, RecvTxBlockChan chan *txhelpers.BlockWatchedTx
	RelevantTxMempoolChan             chan *fnoutil.Tx
	NewTxChan                         chan *fnojson.TxRawResult
	InsightNewTxChan                  chan *insight.NewTx
	ReorgChartsCache                  chan *txhelpers.ReorgData
}

// MakeNtfnChans create notification channels based on config
func MakeNtfnChans() {
	// If we're monitoring for blocks OR collecting block data, these channels
	// are necessary to handle new block notifications. Otherwise, leave them
	// as nil so that both a send (below) blocks and a receive (in
	// blockConnectedHandler) block. default case makes non-blocking below.
	// quit channel case manages blockConnectedHandlers.
	//NtfnChans.ConnectChan = make(chan *chainhash.Hash, blockConnChanBuffer)

	// WiredDB channel for connecting new blocks
	NtfnChans.ConnectChanWiredDB = make(chan *chainhash.Hash, blockConnChanBuffer)

	// Stake DB channel for connecting new blocks - BLOCKING!
	//NtfnChans.ConnectChanStakeDB = make(chan *chainhash.Hash)

	NtfnChans.ConnectChanFnopgDB = make(chan *chainhash.Hash, blockConnChanBuffer)

	// Reorg data channels
	NtfnChans.ReorgChanBlockData = make(chan *txhelpers.ReorgData)
	NtfnChans.ReorgChanWiredDB = make(chan *txhelpers.ReorgData)
	NtfnChans.ReorgChanStakeDB = make(chan *txhelpers.ReorgData)
	NtfnChans.ReorgChanFnopgDB = make(chan *txhelpers.ReorgData)

	// To update app status
	NtfnChans.UpdateStatusNodeHeight = make(chan uint32, blockConnChanBuffer)
	NtfnChans.UpdateStatusDBHeight = make(chan uint32, blockConnChanBuffer)

	// watchaddress
	// if len(cfg.WatchAddresses) > 0 {
	// // recv/SpendTxBlockChan come with connected blocks
	// 	NtfnChans.RecvTxBlockChan = make(chan *txhelpers.BlockWatchedTx, blockConnChanBuffer)
	// 	NtfnChans.SpendTxBlockChan = make(chan *txhelpers.BlockWatchedTx, blockConnChanBuffer)
	// 	NtfnChans.RelevantTxMempoolChan = make(chan *fnoutil.Tx, relevantMempoolTxChanBuffer)
	// }

	// New mempool tx chan for general purpose mempool monitor/collector/saver.
	NtfnChans.NewTxChan = make(chan *fnojson.TxRawResult, newTxChanBuffer)

	NtfnChans.InsightNewTxChan = make(chan *insight.NewTx, expNewTxChanBuffer)
	NtfnChans.ReorgChartsCache = make(chan *txhelpers.ReorgData)
}

// CloseNtfnChans close all notification channels
func CloseNtfnChans() {
	if NtfnChans.ConnectChan != nil {
		close(NtfnChans.ConnectChan)
	}
	if NtfnChans.ConnectChanWiredDB != nil {
		close(NtfnChans.ConnectChanWiredDB)
	}
	if NtfnChans.ConnectChanStakeDB != nil {
		close(NtfnChans.ConnectChanStakeDB)
	}
	if NtfnChans.ConnectChanFnopgDB != nil {
		close(NtfnChans.ConnectChanFnopgDB)
	}

	if NtfnChans.ReorgChanBlockData != nil {
		close(NtfnChans.ReorgChanBlockData)
	}
	if NtfnChans.ReorgChanWiredDB != nil {
		close(NtfnChans.ReorgChanWiredDB)
	}
	if NtfnChans.ReorgChanStakeDB != nil {
		close(NtfnChans.ReorgChanStakeDB)
	}
	if NtfnChans.ReorgChanFnopgDB != nil {
		close(NtfnChans.ReorgChanFnopgDB)
	}

	if NtfnChans.UpdateStatusNodeHeight != nil {
		close(NtfnChans.UpdateStatusNodeHeight)
	}
	if NtfnChans.UpdateStatusDBHeight != nil {
		close(NtfnChans.UpdateStatusDBHeight)
	}

	if NtfnChans.NewTxChan != nil {
		close(NtfnChans.NewTxChan)
	}
	if NtfnChans.RelevantTxMempoolChan != nil {
		close(NtfnChans.RelevantTxMempoolChan)
	}

	if NtfnChans.SpendTxBlockChan != nil {
		close(NtfnChans.SpendTxBlockChan)
	}
	if NtfnChans.RecvTxBlockChan != nil {
		close(NtfnChans.RecvTxBlockChan)
	}

	if NtfnChans.InsightNewTxChan != nil {
		close(NtfnChans.InsightNewTxChan)
	}

	if NtfnChans.ReorgChartsCache != nil {
		close(NtfnChans.ReorgChartsCache)
	}
}

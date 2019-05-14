// Copyright (c) 2018-2019, The Fonero developers
// Copyright (c) 2017, The fnodata developers
// See LICENSE for details.

package pubsub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/fonero-project/fnod/chaincfg"
	"github.com/fonero-project/fnod/fnojson"
	"github.com/fonero-project/fnod/fnoutil"
	"github.com/fonero-project/fnod/txscript"
	"github.com/fonero-project/fnod/wire"
	"github.com/fonero-project/fnodata/blockdata"
	"github.com/fonero-project/fnodata/db/dbtypes"
	"github.com/fonero-project/fnodata/explorer/types"
	exptypes "github.com/fonero-project/fnodata/explorer/types"
	"github.com/fonero-project/fnodata/mempool"
	pstypes "github.com/fonero-project/fnodata/pubsub/types"
	"github.com/fonero-project/fnodata/txhelpers"
	"golang.org/x/net/websocket"
)

const (
	wsWriteTimeout = 10 * time.Second
	wsReadTimeout  = 20 * time.Second
)

// wsDataSource defines the interface for collecting required data.
type wsDataSource interface {
	GetExplorerBlock(hash string) *exptypes.BlockInfo
	DecodeRawTransaction(txhex string) (*fnojson.TxRawResult, error)
	SendRawTransaction(txhex string) (string, error)
	GetChainParams() *chaincfg.Params
	// UnconfirmedTxnsForAddress(address string) (*txhelpers.AddressOutpoints, int64, error)
	GetMempool() []exptypes.MempoolTx
	BlockSubsidy(height int64, voters uint16) *fnojson.GetBlockSubsidyResult
	RetreiveDifficulty(timestamp int64) float64
}

// State represents the current state of block chain.
type State struct {
	// State is read locked by the send loop, and read/write locked when
	// occasional updates are made.
	mtx sync.RWMutex

	// GeneralInfo contains a variety of high level status information. Much of
	// GeneralInfo is constant, set in the constructor, while many fields are
	// set when Store provides new block details.
	GeneralInfo *exptypes.HomeInfo

	// BlockInfo contains details on the most recent block. It is updated when
	// Store provides new block details.
	BlockInfo *exptypes.BlockInfo

	// BlockchainInfo contains the result of the getblockchaininfo RPC. It is
	// updated when Store provides new block details.
	BlockchainInfo *fnojson.GetBlockChainInfoResult
}

type connection struct {
	sync.WaitGroup
	ws     *websocket.Conn
	client *clientHubSpoke
}

// PubSubHub manages the collection and distribution of block chain and mempool
// data to WebSocket clients.
type PubSubHub struct {
	sourceBase wsDataSource
	wsHub      *WebsocketHub
	state      *State
	params     *chaincfg.Params
	invsMtx    sync.RWMutex
	invs       *exptypes.MempoolInfo
}

// NewPubSubHub constructs a PubSubHub given a primary and auxiliary data
// source. The primary data source is required, while the aux. source may be
// nil, which indicates a "lite" mode of operation. The WebSocketHub is
// automatically started.
func NewPubSubHub(dataSource wsDataSource) (*PubSubHub, error) {
	psh := new(PubSubHub)
	psh.sourceBase = dataSource

	// Allocate Mempool fields.
	psh.invs = new(exptypes.MempoolInfo)

	// Retrieve chain parameters.
	params := psh.sourceBase.GetChainParams()
	psh.params = params

	// Development subsidy address of the current network
	devSubsidyAddress, err := dbtypes.DevSubsidyAddress(params)
	if err != nil {
		return nil, fmt.Errorf("bad project fund address: %v", err)
	}

	psh.state = &State{
		// Set the constant parameters of GeneralInfo.
		GeneralInfo: &exptypes.HomeInfo{
			DevAddress: devSubsidyAddress,
			Params: exptypes.ChainParams{
				WindowSize:       params.StakeDiffWindowSize,
				RewardWindowSize: params.SubsidyReductionInterval,
				BlockTime:        params.TargetTimePerBlock.Nanoseconds(),
				MeanVotingBlocks: txhelpers.CalcMeanVotingBlocks(params),
			},
			PoolInfo: exptypes.TicketPoolInfo{
				Target: params.TicketPoolSize * params.TicketsPerBlock,
			},
		},
		// BlockInfo and BlockchainInfo are set by Store()
	}

	psh.wsHub = NewWebsocketHub()
	go psh.wsHub.Run()

	return psh, nil
}

// StopWebsocketHub stops the websocket hub.
func (psh *PubSubHub) StopWebsocketHub() {
	if psh == nil {
		return
	}
	log.Info("Stopping websocket hub.")
	psh.wsHub.Stop()
}

// Ready checks if the WebSocketHub is ready.
func (psh *PubSubHub) Ready() bool {
	return psh.wsHub.Ready()
}

// SetReady updates the ready status of the WebSocketHub.
func (psh *PubSubHub) SetReady(ready bool) {
	psh.wsHub.SetReady(ready)
}

// HubRelay returns the channel used to signal to the WebSocketHub. See
// pstypes.HubSignal for valid signals.
func (psh *PubSubHub) HubRelay() chan pstypes.HubMessage {
	return psh.wsHub.HubRelay
}

// MempoolInventory safely retrieves the current mempool inventory.
func (psh *PubSubHub) MempoolInventory() *types.MempoolInfo {
	psh.invsMtx.RLock()
	defer psh.invsMtx.RUnlock()
	return psh.invs
}

// closeWS attempts to close a websocket.Conn, logging errors other than those
// with messages containing ErrWsClosed.
func closeWS(ws *websocket.Conn) {
	err := ws.Close()
	// Do not log error if connection is just closed
	if err != nil && !pstypes.IsWSClosedErr(err) && !pstypes.IsIOTimeoutErr(err) {
		log.Errorf("Failed to close websocket: %v", err)
	}
}

// receiveLoop receives and processes incoming messages from active websocket
// connections. When receiveLoop returns, the client connection is unregistered
// with the WebSocketHub. receiveLoop should be started as a goroutine, after
// conn.Add(1) and before a conn.Wait().
func (psh *PubSubHub) receiveLoop(conn *connection) {
	ws := conn.ws
	defer closeWS(ws)

	// When connection is done, unregister the client, which closes the client's
	// updateSig channel.
	defer psh.wsHub.UnregisterClient(conn.client)

	defer conn.client.cl.unsubscribeAll()

	// receiveLoop should be started after conn.Add(1) and before a conn.Wait().
	defer conn.Done()

	// Receive messages on the websocket.Conn until it is closed.
	for {
		// Set this Conn's read deadline.
		err := ws.SetReadDeadline(time.Now().Add(wsReadTimeout))
		if err != nil && !pstypes.IsWSClosedErr(err) {
			log.Warnf("SetReadDeadline: %v", err)
		}

		// Wait to receive a message on the websocket
		msg := new(pstypes.WebSocketMessage)
		if err := websocket.JSON.Receive(ws, &msg); err != nil {
			// Keep listening for new messages if the read deadline has passed.
			if pstypes.IsIOTimeoutErr(err) {
				//log.Tracef("No data read from client in %v. Trying again.", wsReadTimeout)
				continue
			}
			// EOF is a common client disconnected error.
			if err.Error() != "EOF" {
				log.Warnf("websocket client receive error: %v", err)
			}
			return
		}

		// Handle the received message according to its event ID.
		resp := pstypes.WebSocketMessage{
			EventId: msg.EventId + "Resp",
		}

		// Reject messages that exceed the limit.
		if len(msg.Message) > psh.wsHub.requestLimit {
			log.Debug("Request size over limit")
			resp.Message = "Request too large"
			continue
		}

		// Determine response based on EventId and Message content.
		switch msg.EventId {
		case "subscribe":
			sig, sigMsg, valid := pstypes.ValidateSubscription(msg.Message)
			if !valid {
				log.Debugf("Invalid subscribe signal: %.40s...", msg.Message)
				resp.Message = "invalid subscription"
				break
			}

			err = conn.client.cl.subscribe(pstypes.HubMessage{Signal: sig, Msg: sigMsg})
			if err != nil {
				log.Debugf("Failed to subscribe: %.40s...", msg.Message)
				resp.Message = "invalid subscription"
				break
			}
			log.Debugf("Client subscribed for: %v.", msg.Message)
			resp.Message = msg.Message + " subscribe ok"

		case "unsubscribe":
			sig, sigMsg, valid := pstypes.ValidateSubscription(msg.Message)
			if !valid {
				log.Debugf("Invalid unsubscribe signal: %.40s...", msg.Message)
				resp.Message = "invalid subscription"
				break
			}

			err = conn.client.cl.unsubscribe(pstypes.HubMessage{Signal: sig, Msg: sigMsg})
			if err != nil {
				log.Debugf("Failed to unsubscribe from: %.40s...", msg.Message)
				resp.Message = "invalid subscription"
				break
			}
			log.Debugf("Client unsubscribed from: %v.", msg.Message)
			resp.Message = msg.Message + " unsubscribe ok"

		case "decodetx":
			log.Debugf("Received decodetx signal for hex: %.40s...", msg.Message)
			tx, err := psh.sourceBase.DecodeRawTransaction(msg.Message)
			if err == nil {
				b, err := json.MarshalIndent(tx, "", "    ")
				if err != nil {
					log.Warn("Invalid JSON message: ", err)
					resp.Message = "Error: Could not encode JSON message"
					break
				}
				resp.Message = string(b)
			} else {
				log.Debugf("Could not decode raw tx")
				resp.Message = fmt.Sprintf("Error: %v", err)
			}

		case "sendtx":
			log.Debugf("Received sendtx signal for hex: %.40s...", msg.Message)
			txid, err := psh.sourceBase.SendRawTransaction(msg.Message)
			if err != nil {
				resp.Message = fmt.Sprintf("Error: %v", err)
			} else {
				resp.Message = fmt.Sprintf("Transaction sent: %s", txid)
			}

		case "getmempooltxs": // TODO: maybe disable this case
			// construct mempool object with properties required in template
			inv := psh.MempoolInventory()
			mempoolInfo := inv.Trim() // Trim locks the inventory.

			psh.state.mtx.RLock()
			mempoolInfo.Subsidy = psh.state.GeneralInfo.NBlockSubsidy
			psh.state.mtx.RUnlock()

			b, err := json.Marshal(mempoolInfo)
			if err != nil {
				log.Warn("Invalid JSON message: ", err)
				resp.Message = "Error: Could not encode JSON message"
				break
			}
			resp.Message = string(b)

		case "ping":
			log.Tracef("We've been pinged: %.40s...", msg.Message)
			// No response to ping
			continue

		default:
			log.Warnf("Unrecognized event ID: %v", msg.EventId)
			// ignore unrecognized events
			continue
		}

		// Send the response.
		err = ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		if err != nil && !pstypes.IsWSClosedErr(err) {
			log.Warnf("SetWriteDeadline: %v", err)
		}
		if err := websocket.JSON.Send(ws, resp); err != nil {
			// Do not log the error if the connection is just closed.
			if !pstypes.IsWSClosedErr(err) {
				log.Debugf("Failed to encode WebSocketMessage (reply) %s: %v",
					resp.EventId, err)
			}
			// If the send failed, the client is probably gone, quit the
			// receive loop, closing the websocket.Conn.
			return
		}
	} // for {
}

// sendLoop receives signals from WebSocketHub via the connections unique signal
// channel, and sends the relevant data to the client.
func (psh *PubSubHub) sendLoop(conn *connection) {
	// If returning because the WebSocketHub sent a quit signal, the receive
	// loop may still be waiting for a message, so it is necessary to close the
	// websocket.Conn in this case.
	ws := conn.ws
	defer closeWS(ws)

	// sendLoop should be started after conn.Add(1), and before a conn.Wait().
	defer conn.Done()

	// Use this client's unique channel to receive signals from the
	// WebSocketHub, which broadcasts signals to all clients.
	updateSigChan := *conn.client.c
	clientData := conn.client.cl
	buff := new(bytes.Buffer)

loop:
	for {
		// Wait for signal from the WebSocketHub to update.
		select {
		case sig, ok := <-updateSigChan:
			// Check if the update channel was closed. Either the websocket
			// hub will do it after unregistering the client, or forcibly in
			// response to (http.CloseNotifier).CloseNotify() and only then
			// if the hub has somehow lost track of the client.
			if !ok {
				return
			}

			if !sig.IsValid() {
				log.Errorf("invalid signal to send: %s / %d", sig.Signal.String(), int(sig.Signal))
				continue loop
			}

			if !clientData.isSubscribed(sig) {
				log.Errorf("Client not subscribed for %s events. "+
					"WebSocketHub should have caught this.", sig.Signal.String())
				continue loop // break
			}

			log.Tracef("signaling client: %p", conn.client.c) // ID by address

			// Respond to the websocket client.
			pushMsg := pstypes.WebSocketMessage{
				EventId: sig.Signal.String(),
				// Message is set in switch statement below.
			}

			// JSON encoder for the Message.
			buff.Reset()
			enc := json.NewEncoder(buff)

			switch sig.Signal {
			case sigAddressTx:
				// sig was already validated, but do it again here in case the
				// type changed without changing the type assertion here.
				am, ok := sig.Msg.(*pstypes.AddressMessage)
				if !ok {
					log.Errorf("sigAddressTx did not store a *AddressMessage in Msg.")
					continue loop
				}
				err := enc.Encode(am)
				if err != nil {
					log.Warnf("Encode(AddressMessage) failed: %v", err)
				}

				pushMsg.Message = buff.String()
			case sigNewBlock:
				psh.state.mtx.RLock()
				if psh.state.BlockInfo == nil {
					psh.state.mtx.RUnlock()
					break // from switch to send empty message
				}
				err := enc.Encode(exptypes.WebsocketBlock{
					Block: psh.state.BlockInfo,
					Extra: psh.state.GeneralInfo,
				})
				psh.state.mtx.RUnlock()
				if err != nil {
					log.Warnf("Encode(WebsocketBlock) failed: %v", err)
				}

				pushMsg.Message = buff.String()

			case sigMempoolUpdate:
				// You probably want the sigNewTX event. sigMempoolUpdate sends
				// a summary of mempool contents, and the NumLatestMempoolTxns
				// latest transactions.
				inv := psh.MempoolInventory()
				if inv == nil {
					break // from switch to send empty message
				}
				inv.RLock()
				err := enc.Encode(inv.MempoolShort)
				inv.RUnlock()
				if err != nil {
					log.Warnf("Encode(MempoolShort) failed: %v", err)
				}

				pushMsg.Message = buff.String()

			case sigPingAndUserCount:
				// ping and send user count
				pushMsg.Message = strconv.Itoa(psh.wsHub.NumClients())

			case sigNewTxs:
				// Marshal this client's tx buffer if it is not empty.
				clientData.newTxs.Lock()
				if len(clientData.newTxs.t) == 0 {
					clientData.newTxs.Unlock()
					continue loop // break sigselect
				}
				err := enc.Encode(clientData.newTxs.t)

				// Reinit the tx buffer.
				clientData.newTxs.t = make(pstypes.TxList, 0, NewTxBufferSize)
				clientData.newTxs.Unlock()
				if err != nil {
					log.Warnf("Encode([]*exptypes.MempoolTx) failed: %v", err)
				}

				pushMsg.Message = buff.String()

			// case sigSyncStatus:
			// 	err := enc.Encode(explorer.SyncStatus())
			// 	if err != nil {
			// 		log.Warnf("Encode(SyncStatus()) failed: %v", err)
			// 	}
			// 	pushMsg.Message = buff.String()

			default:
				log.Errorf("Not sending a %v to the client.", sig)
				continue loop // break sigselect
			} // switch sig

			// Send the message.
			err := ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if err != nil && !pstypes.IsWSClosedErr(err) {
				log.Warnf("SetWriteDeadline failed: %v", err)
			}
			if err = websocket.JSON.Send(ws, pushMsg); err != nil {
				// Do not log the error if the connection is just closed.
				if !pstypes.IsWSClosedErr(err) {
					log.Debugf("Failed to encode WebSocketMessage (push) %v: %v", sig, err)
				}
				// If the send failed, the client is probably gone, quit the
				// send loop, unregistering the client from the websocket hub.
				log.Errorf("websocket.JSON.Send of %v failed: %v", pushMsg, err)
				return
			}

		// end of case sig, ok := <-updateSigChan

		case <-psh.wsHub.quitWSHandler:
			return
		} // select
	} // for { a.k.a. loop:
}

// WebSocketHandler is the http.HandlerFunc for new websocket connections. The
// connection is registered with the WebSocketHub, and the send/receive loops
// are launched.
func (psh *PubSubHub) WebSocketHandler(w http.ResponseWriter, r *http.Request) {
	wsHandler := websocket.Handler(func(ws *websocket.Conn) {
		// Set the max payload size for this connection.
		ws.MaxPayloadBytes = psh.wsHub.requestLimit

		// Register websocket client.
		ch := psh.wsHub.NewClientHubSpoke()

		// The receive loop will be sitting on websocket.JSON.Receive, while the
		// send loop will be waiting for signals from the WebSocketHub. One must
		// close the other depending on whether the connection was closed/lost,
		// or the WebSocketHub quit or forcibly unregistered the client. The
		// receive loop unregisters the client (thus closing the update signal
		// channel) when the connection is closed and it returns. The send loop
		// closes the websocket.Conn on return, which will interrupt the receive
		// loop from its waiting to receive data on the connection.

		conn := &connection{
			client: ch,
			ws:     ws,
		}

		// Start listening for websocket messages from client, returning when
		// the connection is closed.
		conn.Add(1)
		go psh.receiveLoop(conn)

		// Send loop (ping, new tx, block, etc. update loop)
		conn.Add(1)
		go psh.sendLoop(conn)

		// Hang out until the send and receive loops have quit.
		conn.Wait()
	})

	// Use a websocket.Server to avoid checking Origin.
	wsServer := websocket.Server{
		Handler: wsHandler,
	}
	wsServer.ServeHTTP(w, r)
}

// StoreMPData stores mempool data. It is advisable to pass a copy of the
// []types.MempoolTx so that it may be modified (e.g. sorted) without affecting
// other MempoolDataSavers. The struct pointed to may be shared, so it should
// not be modified.
func (psh *PubSubHub) StoreMPData(_ *mempool.StakeData, _ []exptypes.MempoolTx, inv *exptypes.MempoolInfo) {
	// Get exclusive access to the Mempool field.
	psh.invsMtx.Lock()
	psh.invs = inv
	psh.invsMtx.Unlock()
	log.Debugf("Updated mempool details for the pubsubhub.")
}

// Store processes and stores new block data, then signals to the WebSocketHub
// that the new data is available.
func (psh *PubSubHub) Store(blockData *blockdata.BlockData, msgBlock *wire.MsgBlock) error {
	// Retrieve block data for the passed block hash.
	newBlockData := psh.sourceBase.GetExplorerBlock(msgBlock.BlockHash().String())

	// Use the latest block's blocktime to get the last 24hr timestamp.
	day := 24 * time.Hour
	targetTimePerBlock := float64(psh.params.TargetTimePerBlock)

	// Hashrate change over last day
	timestamp := newBlockData.BlockTime.T.Add(-day).Unix()
	last24hrDifficulty := psh.sourceBase.RetreiveDifficulty(timestamp)
	last24HrHashRate := dbtypes.CalculateHashRate(last24hrDifficulty, targetTimePerBlock)

	// Hashrate change over last month
	timestamp = newBlockData.BlockTime.T.Add(-30 * day).Unix()
	lastMonthDifficulty := psh.sourceBase.RetreiveDifficulty(timestamp)
	lastMonthHashRate := dbtypes.CalculateHashRate(lastMonthDifficulty, targetTimePerBlock)

	difficulty := blockData.Header.Difficulty
	hashrate := dbtypes.CalculateHashRate(difficulty, targetTimePerBlock)

	// If BlockData contains non-nil PoolInfo, compute actual percentage of FNO
	// supply staked.
	stakePerc := 45.0
	if blockData.PoolInfo != nil {
		stakePerc = blockData.PoolInfo.Value / fnoutil.Amount(blockData.ExtraInfo.CoinSupply).ToCoin()
	}

	// Update pageData with block data and chain (home) info.
	p := psh.state
	p.mtx.Lock()

	// Store current block and blockchain data.
	p.BlockInfo = newBlockData
	p.BlockchainInfo = blockData.BlockchainInfo

	// Update GeneralInfo, keeping constant parameters set in NewPubSubHub.
	p.GeneralInfo.HashRate = hashrate
	p.GeneralInfo.HashRateChangeDay = 100 * (hashrate - last24HrHashRate) / last24HrHashRate
	p.GeneralInfo.HashRateChangeMonth = 100 * (hashrate - lastMonthHashRate) / lastMonthHashRate
	p.GeneralInfo.CoinSupply = blockData.ExtraInfo.CoinSupply
	p.GeneralInfo.StakeDiff = blockData.CurrentStakeDiff.CurrentStakeDifficulty
	p.GeneralInfo.NextExpectedStakeDiff = blockData.EstStakeDiff.Expected
	p.GeneralInfo.NextExpectedBoundsMin = blockData.EstStakeDiff.Min
	p.GeneralInfo.NextExpectedBoundsMax = blockData.EstStakeDiff.Max
	p.GeneralInfo.IdxBlockInWindow = blockData.IdxBlockInWindow
	p.GeneralInfo.IdxInRewardWindow = int(newBlockData.Height % psh.params.SubsidyReductionInterval)
	p.GeneralInfo.Difficulty = difficulty
	p.GeneralInfo.NBlockSubsidy.Dev = blockData.ExtraInfo.NextBlockSubsidy.Developer
	p.GeneralInfo.NBlockSubsidy.PoS = blockData.ExtraInfo.NextBlockSubsidy.PoS
	p.GeneralInfo.NBlockSubsidy.PoW = blockData.ExtraInfo.NextBlockSubsidy.PoW
	p.GeneralInfo.NBlockSubsidy.Total = blockData.ExtraInfo.NextBlockSubsidy.Total

	// If BlockData contains non-nil PoolInfo, copy values.
	p.GeneralInfo.PoolInfo = exptypes.TicketPoolInfo{}
	if blockData.PoolInfo != nil {
		tpTarget := psh.params.TicketPoolSize * psh.params.TicketsPerBlock
		p.GeneralInfo.PoolInfo = exptypes.TicketPoolInfo{
			Size:          blockData.PoolInfo.Size,
			Value:         blockData.PoolInfo.Value,
			ValAvg:        blockData.PoolInfo.ValAvg,
			Percentage:    stakePerc * 100,
			PercentTarget: 100 * float64(blockData.PoolInfo.Size) / float64(tpTarget),
			Target:        tpTarget,
		}
	}

	posSubsPerVote := fnoutil.Amount(blockData.ExtraInfo.NextBlockSubsidy.PoS).ToCoin() /
		float64(psh.params.TicketsPerBlock)
	p.GeneralInfo.TicketReward = 100 * posSubsPerVote /
		blockData.CurrentStakeDiff.CurrentStakeDifficulty

	// The actual reward of a ticket needs to also take into consideration the
	// ticket maturity (time from ticket purchase until its eligible to vote)
	// and coinbase maturity (time after vote until funds distributed to ticket
	// holder are available to use).
	avgSSTxToSSGenMaturity := psh.state.GeneralInfo.Params.MeanVotingBlocks +
		int64(psh.params.TicketMaturity) +
		int64(psh.params.CoinbaseMaturity)
	p.GeneralInfo.RewardPeriod = fmt.Sprintf("%.2f days", float64(avgSSTxToSSGenMaturity)*
		psh.params.TargetTimePerBlock.Hours()/24)
	//p.GeneralInfo.ASR = ASR

	p.mtx.Unlock()

	// Signal to the websocket hub that a new block was received, but do not
	// block Store(), and do not hang forever in a goroutine waiting to send.
	go func() {
		select {
		case psh.wsHub.HubRelay <- pstypes.HubMessage{Signal: sigNewBlock}:
		case <-time.After(time.Second * 10):
			log.Errorf("sigNewBlock send failed: Timeout waiting for WebsocketHub.")
		}
	}()

	log.Debugf("Got new block %d for the pubsubhub.", newBlockData.Height)

	// Since the coinbase transaction is generated by the miner, it will never
	// hit mempool. It must be processed now, with the new block.
	coinbaseTx := msgBlock.Transactions[0]
	coinbaseHash := coinbaseTx.TxHash().String()
	// Check each output's pkScript for subscribed addresses.
	for _, out := range coinbaseTx.TxOut {
		_, scriptAddrs, _, err := txscript.ExtractPkScriptAddrs(
			out.Version, out.PkScript, psh.params)
		if err != nil {
			log.Warnf("failed to decode pkScript: %v", err)
			continue
		}

		if len(scriptAddrs) == 0 {
			continue
		}

		for _, scriptAddr := range scriptAddrs {
			addr := scriptAddr.EncodeAddress()
			go func() {
				select {
				case psh.wsHub.HubRelay <- pstypes.HubMessage{
					Signal: sigAddressTx,
					Msg: &pstypes.AddressMessage{
						Address: addr,
						TxHash:  coinbaseHash,
					},
				}:
				case <-time.After(time.Second * 10):
					log.Errorf("sigNewBlock send failed: Timeout waiting for WebsocketHub.")
				}
			}()
		}
	}

	return nil
}

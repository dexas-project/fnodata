// Copyright (c) 2018-2019, The Fonero developers
// Copyright (c) 2017, The fnodata developers
// See LICENSE for details.

// Package explorer handles the block explorer subsystem for generating the
// explorer pages.
package explorer

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fonero-project/fnod/chaincfg"
	"github.com/fonero-project/fnod/chaincfg/chainhash"
	"github.com/fonero-project/fnod/fnojson"
	"github.com/fonero-project/fnod/fnoutil"
	"github.com/fonero-project/fnod/wire"
	"github.com/fonero-project/fnodata/blockdata"
	"github.com/fonero-project/fnodata/db/dbtypes"
	"github.com/fonero-project/fnodata/exchanges"
	"github.com/fonero-project/fnodata/explorer/types"
	"github.com/fonero-project/fnodata/gov/agendas"
	pitypes "github.com/fonero-project/fnodata/gov/politeia/types"
	"github.com/fonero-project/fnodata/mempool"
	pstypes "github.com/fonero-project/fnodata/pubsub/types"
	"github.com/fonero-project/fnodata/txhelpers"
	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/rs/cors"
)

const (
	// maxExplorerRows and minExplorerRows are the limits on the number of
	// blocks/time-window rows that may be shown on the explorer pages.
	maxExplorerRows = 400
	minExplorerRows = 20

	// syncStatusInterval is the frequency with startup synchronization progress
	// signals are sent to websocket clients.
	syncStatusInterval = 2 * time.Second

	// defaultAddressRows is the default number of rows to be shown on the
	// address page table.
	defaultAddressRows int64 = 20

	// MaxAddressRows is an upper limit on the number of rows that may be shown
	// on the address page table.
	MaxAddressRows int64 = 1000
)

// explorerDataSourceLite implements an interface for collecting data for the
// explorer pages
type explorerDataSourceLite interface {
	GetExplorerBlock(hash string) *types.BlockInfo
	GetExplorerBlocks(start int, end int) []*types.BlockBasic
	GetBlockHeight(hash string) (int64, error)
	GetBlockHash(idx int64) (string, error)
	GetExplorerTx(txid string) *types.TxInfo
	GetExplorerAddress(address string, count, offset int64) (*dbtypes.AddressInfo, txhelpers.AddressType, txhelpers.AddressError)
	GetTip() (*types.WebBasicBlock, error)
	DecodeRawTransaction(txhex string) (*fnojson.TxRawResult, error)
	SendRawTransaction(txhex string) (string, error)
	GetHeight() (int64, error)
	GetChainParams() *chaincfg.Params
	GetMempool() []types.MempoolTx
	TxHeight(txid *chainhash.Hash) (height int64)
	BlockSubsidy(height int64, voters uint16) *fnojson.GetBlockSubsidyResult
	GetExplorerFullBlocks(start int, end int) []*types.BlockInfo
	Difficulty() (float64, error)
	RetreiveDifficulty(timestamp int64) float64
}

// explorerDataSource implements extra data retrieval functions that require a
// faster solution than RPC, or additional functionality.
type explorerDataSource interface {
	BlockHeight(hash string) (int64, error)
	Height() int64
	HeightDB() (int64, error)
	BlockHash(height int64) (string, error)
	SpendingTransaction(fundingTx string, vout uint32) (string, uint32, int8, error)
	SpendingTransactions(fundingTxID string) ([]string, []uint32, []uint32, error)
	PoolStatusForTicket(txid string) (dbtypes.TicketSpendType, dbtypes.TicketPoolStatus, error)
	AddressHistory(address string, N, offset int64, txnType dbtypes.AddrTxnViewType) ([]*dbtypes.AddressRow, *dbtypes.AddressBalance, error)
	AddressData(address string, N, offset int64, txnType dbtypes.AddrTxnViewType) (*dbtypes.AddressInfo, error)
	DevBalance() (*dbtypes.AddressBalance, error)
	FillAddressTransactions(addrInfo *dbtypes.AddressInfo) error
	BlockMissedVotes(blockHash string) ([]string, error)
	TicketMiss(ticketHash string) (string, int64, error)
	SideChainBlocks() ([]*dbtypes.BlockStatus, error)
	DisapprovedBlocks() ([]*dbtypes.BlockStatus, error)
	BlockStatus(hash string) (dbtypes.BlockStatus, error)
	BlockFlags(hash string) (bool, bool, error)
	TicketPoolVisualization(interval dbtypes.TimeBasedGrouping) (*dbtypes.PoolTicketsData, *dbtypes.PoolTicketsData, *dbtypes.PoolTicketsData, int64, error)
	TransactionBlocks(hash string) ([]*dbtypes.BlockStatus, []uint32, error)
	Transaction(txHash string) ([]*dbtypes.Tx, error)
	VinsForTx(*dbtypes.Tx) (vins []dbtypes.VinTxProperty, prevPkScripts []string, scriptVersions []uint16, err error)
	VoutsForTx(*dbtypes.Tx) ([]dbtypes.Vout, error)
	PosIntervals(limit, offset uint64) ([]*dbtypes.BlocksGroupedInfo, error)
	TimeBasedIntervals(timeGrouping dbtypes.TimeBasedGrouping, limit, offset uint64) ([]*dbtypes.BlocksGroupedInfo, error)
	AgendasVotesSummary(agendaID string) (summary *dbtypes.AgendaSummary, err error)
	BlockTimeByHeight(height int64) (int64, error)
	LastPiParserSync() time.Time
}

// politeiaBackend implements methods that manage proposals db data.
type politeiaBackend interface {
	LastProposalsSync() int64
	CheckProposalsUpdates() error
	AllProposals(offset, rowsCount int, filterByVoteStatus ...int) (proposals []*pitypes.ProposalInfo, totalCount int, err error)
	ProposalByToken(proposalToken string) (*pitypes.ProposalInfo, error)
	ProposalByRefID(RefID string) (*pitypes.ProposalInfo, error)
}

// agendaBackend implements methods that manage agendas db data.
type agendaBackend interface {
	AgendaInfo(agendaID string) (*agendas.AgendaTagged, error)
	AllAgendas() (agendas []*agendas.AgendaTagged, err error)
	CheckAgendasUpdates(activeVersions map[uint32][]chaincfg.ConsensusDeployment) error
}

// links to be passed with common page data.
type links struct {
	CoinbaseComment string
	POSExplanation  string
	APIDocs         string
	InsightAPIDocs  string
	Github          string
	License         string
	NetParams       string
	BtcAddress      string
	DownloadLink    string
	// Testnet and below are set via fnodata config.
	Testnet       string
	Mainnet       string
	TestnetSearch string
	MainnetSearch string
}

var explorerLinks = &links{
	CoinbaseComment: "https://github.com/fonero-project/fnod/blob/2a18beb4d56fe59d614a7309308d84891a0cba96/chaincfg/genesis.go#L17-L53",
	POSExplanation:  "https://docs.fonero.org/faq/proof-of-stake/general/#9-what-is-proof-of-stake-voting",
	APIDocs:         "https://github.com/fonero-project/fnodata#apis",
	InsightAPIDocs:  "https://github.com/fonero-project/fnodata/blob/master/api/Insight_API_documentation.md",
	Github:          "https://github.com/fonero-project/fnodata",
	License:         "https://github.com/fonero-project/fnodata/blob/master/LICENSE",
	NetParams:       "https://github.com/fonero-project/fnod/blob/master/chaincfg/params.go",
	BtcAddress:      "https://live.blockcypher.com/btc/address/",
	DownloadLink:    "https://fonero.org/downloads/",
}

// TicketStatusText generates the text to display on the explorer's transaction
// page for the "POOL STATUS" field.
func TicketStatusText(s dbtypes.TicketSpendType, p dbtypes.TicketPoolStatus) string {
	switch p {
	case dbtypes.PoolStatusLive:
		return "In Live Ticket Pool"
	case dbtypes.PoolStatusVoted:
		return "Voted"
	case dbtypes.PoolStatusExpired:
		switch s {
		case dbtypes.TicketUnspent:
			return "Expired, Unrevoked"
		case dbtypes.TicketRevoked:
			return "Expired, Revoked"
		default:
			return "invalid ticket state"
		}
	case dbtypes.PoolStatusMissed:
		switch s {
		case dbtypes.TicketUnspent:
			return "Missed, Unrevoked"
		case dbtypes.TicketRevoked:
			return "Missed, Revoked"
		default:
			return "invalid ticket state"
		}
	default:
		return "Immature"
	}
}

type pageData struct {
	sync.RWMutex
	BlockInfo      *types.BlockInfo
	BlockchainInfo *fnojson.GetBlockChainInfoResult
	HomeInfo       *types.HomeInfo
}

type explorerUI struct {
	Mux              *chi.Mux
	blockData        explorerDataSourceLite
	explorerSource   explorerDataSource
	agendasSource    agendaBackend
	voteTracker      *agendas.VoteTracker
	proposalsSource  politeiaBackend
	dbsSyncing       atomic.Value
	devPrefetch      bool
	templates        templates
	wsHub            *WebsocketHub
	pageData         *pageData
	ChainParams      *chaincfg.Params
	Version          string
	NetName          string
	MeanVotingBlocks int64
	xcBot            *exchanges.ExchangeBot
	xcDone           chan struct{}
	// displaySyncStatusPage indicates if the sync status page is the only web
	// page that should be accessible during DB synchronization.
	displaySyncStatusPage atomic.Value
	politeiaAPIURL        string

	invsMtx sync.RWMutex
	invs    *types.MempoolInfo
	premine int64
}

// AreDBsSyncing is a thread-safe way to fetch the boolean in dbsSyncing.
func (exp *explorerUI) AreDBsSyncing() bool {
	syncing, ok := exp.dbsSyncing.Load().(bool)
	return ok && syncing
}

// SetDBsSyncing is a thread-safe way to update dbsSyncing.
func (exp *explorerUI) SetDBsSyncing(syncing bool) {
	exp.dbsSyncing.Store(syncing)
	exp.wsHub.SetDBsSyncing(syncing)
}

func (exp *explorerUI) reloadTemplates() error {
	return exp.templates.reloadTemplates()
}

// See reloadsig*.go for an exported method
func (exp *explorerUI) reloadTemplatesSig(sig os.Signal) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, sig)

	go func() {
		for {
			sigr := <-sigChan
			log.Infof("Received %s", sig)
			if sigr == sig {
				if err := exp.reloadTemplates(); err != nil {
					log.Error(err)
					continue
				}
				log.Infof("Explorer UI html templates reparsed.")
			}
		}
	}()
}

// StopWebsocketHub stops the websocket hub
func (exp *explorerUI) StopWebsocketHub() {
	if exp == nil {
		return
	}
	log.Info("Stopping websocket hub.")
	exp.wsHub.Stop()
	close(exp.xcDone)
}

// ExplorerConfig is the configuration settings for explorerUI.
type ExplorerConfig struct {
	DataSource        explorerDataSourceLite
	PrimaryDataSource explorerDataSource
	UseRealIP         bool
	AppVersion        string
	DevPrefetch       bool
	Viewsfolder       string
	XcBot             *exchanges.ExchangeBot
	AgendasSource     agendaBackend
	Tracker           *agendas.VoteTracker
	ProposalsSource   politeiaBackend
	PoliteiaURL       string
	MainnetLink       string
	TestnetLink       string
}

// New returns an initialized instance of explorerUI
func New(cfg *ExplorerConfig) *explorerUI {
	exp := new(explorerUI)
	exp.Mux = chi.NewRouter()
	exp.blockData = cfg.DataSource
	exp.explorerSource = cfg.PrimaryDataSource
	// Allocate Mempool fields.
	exp.invs = new(types.MempoolInfo)
	exp.Version = cfg.AppVersion
	exp.devPrefetch = cfg.DevPrefetch
	exp.xcBot = cfg.XcBot
	exp.xcDone = make(chan struct{})
	exp.agendasSource = cfg.AgendasSource
	exp.voteTracker = cfg.Tracker
	exp.proposalsSource = cfg.ProposalsSource
	exp.politeiaAPIURL = cfg.PoliteiaURL
	explorerLinks.Mainnet = cfg.MainnetLink
	explorerLinks.Testnet = cfg.TestnetLink
	explorerLinks.MainnetSearch = cfg.MainnetLink + "search?search="
	explorerLinks.TestnetSearch = cfg.TestnetLink + "search?search="

	// explorerDataSource is an interface that could have a value of pointer type.
	if exp.explorerSource == nil || reflect.ValueOf(exp.explorerSource).IsNil() {
		log.Errorf("An explorerDataSource (PostgreSQL backend) is required.")
		return nil
	}

	if cfg.UseRealIP {
		exp.Mux.Use(middleware.RealIP)
	}

	params := exp.blockData.GetChainParams()
	exp.ChainParams = params
	exp.NetName = netName(exp.ChainParams)
	exp.MeanVotingBlocks = txhelpers.CalcMeanVotingBlocks(params)
	exp.premine = params.BlockOneSubsidy()

	// Development subsidy address of the current network
	devSubsidyAddress, err := dbtypes.DevSubsidyAddress(params)
	if err != nil {
		log.Warnf("explorer.New: %v", err)
	}
	log.Debugf("Organization address: %s", devSubsidyAddress)

	exp.pageData = &pageData{
		BlockInfo: new(types.BlockInfo),
		HomeInfo: &types.HomeInfo{
			DevAddress: devSubsidyAddress,
			Params: types.ChainParams{
				WindowSize:       exp.ChainParams.StakeDiffWindowSize,
				RewardWindowSize: exp.ChainParams.SubsidyReductionInterval,
				BlockTime:        exp.ChainParams.TargetTimePerBlock.Nanoseconds(),
				MeanVotingBlocks: exp.MeanVotingBlocks,
			},
			PoolInfo: types.TicketPoolInfo{
				Target: exp.ChainParams.TicketPoolSize * exp.ChainParams.TicketsPerBlock,
			},
		},
	}

	log.Infof("Mean Voting Blocks calculated: %d", exp.pageData.HomeInfo.Params.MeanVotingBlocks)

	commonTemplates := []string{"extras"}
	exp.templates = newTemplates(cfg.Viewsfolder, commonTemplates, makeTemplateFuncMap(exp.ChainParams))

	tmpls := []string{"home", "explorer", "mempool", "block", "tx", "address",
		"rawtx", "status", "parameters", "agenda", "agendas", "charts",
		"sidechains", "disapproved", "ticketpool", "nexthome", "statistics",
		"windows", "timelisting", "addresstable", "proposals", "proposal",
		"market", "insight_root"}

	for _, name := range tmpls {
		if err := exp.templates.addTemplate(name); err != nil {
			log.Errorf("Unable to create new html template: %v", err)
			return nil
		}
	}

	exp.addRoutes()

	exp.wsHub = NewWebsocketHub()

	go exp.wsHub.run()

	go exp.watchExchanges()

	return exp
}

// Height returns the height of the current block data.
func (exp *explorerUI) Height() int64 {
	exp.pageData.RLock()
	defer exp.pageData.RUnlock()

	if exp.pageData.BlockInfo.BlockBasic == nil {
		// If exp.pageData.BlockInfo.BlockBasic has not yet been set return:
		return -1
	}

	return exp.pageData.BlockInfo.Height
}

// LastBlock returns the last block hash, height and time.
func (exp *explorerUI) LastBlock() (lastBlockHash string, lastBlock int64, lastBlockTime int64) {
	exp.pageData.RLock()
	defer exp.pageData.RUnlock()

	if exp.pageData.BlockInfo.BlockBasic == nil {
		// If exp.pageData.BlockInfo.BlockBasic has not yet been set return:
		lastBlock, lastBlockTime = -1, -1
		return
	}

	lastBlock = exp.pageData.BlockInfo.Height
	lastBlockTime = exp.pageData.BlockInfo.BlockTime.UNIX()
	lastBlockHash = exp.pageData.BlockInfo.Hash
	return
}

// MempoolInventory safely retrieves the current mempool inventory.
func (exp *explorerUI) MempoolInventory() *types.MempoolInfo {
	exp.invsMtx.RLock()
	defer exp.invsMtx.RUnlock()
	return exp.invs
}

// MempoolID safely fetches the current mempool inventory ID.
func (exp *explorerUI) MempoolID() uint64 {
	exp.invsMtx.RLock()
	defer exp.invsMtx.RUnlock()
	return exp.invs.ID()
}

// MempoolSignal returns the mempool signal channel, which is to be used by the
// mempool package's MempoolMonitor as a send-only channel.
func (exp *explorerUI) MempoolSignal() chan<- pstypes.HubMessage {
	return exp.wsHub.HubRelay
}

// StoreMPData stores mempool data. It is advisable to pass a copy of the
// []types.MempoolTx so that it may be modified (e.g. sorted) without affecting
// other MempoolDataSavers.
func (exp *explorerUI) StoreMPData(_ *mempool.StakeData, _ []types.MempoolTx, inv *types.MempoolInfo) {
	// Get exclusive access to the Mempool field.
	exp.invsMtx.Lock()
	exp.invs = inv
	exp.invsMtx.Unlock()
	log.Debugf("Updated mempool details for the explorerUI.")
}

func (exp *explorerUI) Store(blockData *blockdata.BlockData, msgBlock *wire.MsgBlock) error {
	// Retrieve block data for the passed block hash.
	newBlockData := exp.blockData.GetExplorerBlock(msgBlock.BlockHash().String())

	// Use the latest block's blocktime to get the last 24hr timestamp.
	day := 24 * time.Hour
	targetTimePerBlock := float64(exp.ChainParams.TargetTimePerBlock)

	// Hashrate change over last day
	timestamp := newBlockData.BlockTime.T.Add(-day).Unix()
	last24hrDifficulty := exp.blockData.RetreiveDifficulty(timestamp)
	last24HrHashRate := dbtypes.CalculateHashRate(last24hrDifficulty, targetTimePerBlock)

	// Hashrate change over last month
	timestamp = newBlockData.BlockTime.T.Add(-30 * day).Unix()
	lastMonthDifficulty := exp.blockData.RetreiveDifficulty(timestamp)
	lastMonthHashRate := dbtypes.CalculateHashRate(lastMonthDifficulty, targetTimePerBlock)

	difficulty := blockData.Header.Difficulty
	hashrate := dbtypes.CalculateHashRate(difficulty, targetTimePerBlock)

	// If BlockData contains non-nil PoolInfo, compute actual percentage of FNO
	// supply staked.
	stakePerc := 45.0
	if blockData.PoolInfo != nil {
		stakePerc = blockData.PoolInfo.Value / fnoutil.Amount(blockData.ExtraInfo.CoinSupply).ToCoin()
	}
	// Simulate the annual staking rate
	ASR, _ := exp.simulateASR(1000, false, stakePerc,
		fnoutil.Amount(blockData.ExtraInfo.CoinSupply).ToCoin(),
		float64(newBlockData.Height),
		blockData.CurrentStakeDiff.CurrentStakeDifficulty)

	// Trigger a vote info refresh
	go exp.voteTracker.Refresh()

	// Update pageData with block data and chain (home) info.
	p := exp.pageData
	p.Lock()

	// Store current block and blockchain data.
	p.BlockInfo = newBlockData
	p.BlockchainInfo = blockData.BlockchainInfo

	// Update HomeInfo.
	p.HomeInfo.HashRate = hashrate
	p.HomeInfo.HashRateChangeDay = 100 * (hashrate - last24HrHashRate) / last24HrHashRate
	p.HomeInfo.HashRateChangeMonth = 100 * (hashrate - lastMonthHashRate) / lastMonthHashRate
	p.HomeInfo.CoinSupply = blockData.ExtraInfo.CoinSupply
	p.HomeInfo.StakeDiff = blockData.CurrentStakeDiff.CurrentStakeDifficulty
	p.HomeInfo.NextExpectedStakeDiff = blockData.EstStakeDiff.Expected
	p.HomeInfo.NextExpectedBoundsMin = blockData.EstStakeDiff.Min
	p.HomeInfo.NextExpectedBoundsMax = blockData.EstStakeDiff.Max
	p.HomeInfo.IdxBlockInWindow = blockData.IdxBlockInWindow
	p.HomeInfo.IdxInRewardWindow = int(newBlockData.Height % exp.ChainParams.SubsidyReductionInterval)
	p.HomeInfo.Difficulty = difficulty
	p.HomeInfo.NBlockSubsidy.Dev = blockData.ExtraInfo.NextBlockSubsidy.Developer
	p.HomeInfo.NBlockSubsidy.PoS = blockData.ExtraInfo.NextBlockSubsidy.PoS
	p.HomeInfo.NBlockSubsidy.PoW = blockData.ExtraInfo.NextBlockSubsidy.PoW
	p.HomeInfo.NBlockSubsidy.Total = blockData.ExtraInfo.NextBlockSubsidy.Total

	// If BlockData contains non-nil PoolInfo, copy values.
	p.HomeInfo.PoolInfo = types.TicketPoolInfo{}
	if blockData.PoolInfo != nil {
		tpTarget := exp.ChainParams.TicketPoolSize * exp.ChainParams.TicketsPerBlock
		p.HomeInfo.PoolInfo = types.TicketPoolInfo{
			Size:          blockData.PoolInfo.Size,
			Value:         blockData.PoolInfo.Value,
			ValAvg:        blockData.PoolInfo.ValAvg,
			Percentage:    stakePerc * 100,
			PercentTarget: 100 * float64(blockData.PoolInfo.Size) / float64(tpTarget),
			Target:        tpTarget,
		}
	}

	posSubsPerVote := fnoutil.Amount(blockData.ExtraInfo.NextBlockSubsidy.PoS).ToCoin() /
		float64(exp.ChainParams.TicketsPerBlock)
	p.HomeInfo.TicketReward = 100 * posSubsPerVote /
		blockData.CurrentStakeDiff.CurrentStakeDifficulty

	// The actual reward of a ticket needs to also take into consideration the
	// ticket maturity (time from ticket purchase until its eligible to vote)
	// and coinbase maturity (time after vote until funds distributed to ticket
	// holder are available to use).
	avgSSTxToSSGenMaturity := exp.MeanVotingBlocks +
		int64(exp.ChainParams.TicketMaturity) +
		int64(exp.ChainParams.CoinbaseMaturity)
	p.HomeInfo.RewardPeriod = fmt.Sprintf("%.2f days", float64(avgSSTxToSSGenMaturity)*
		exp.ChainParams.TargetTimePerBlock.Hours()/24)
	p.HomeInfo.ASR = ASR

	// If exchange monitoring is enabled, set the exchange rate.
	if exp.xcBot != nil {
		p.HomeInfo.ExchangeRate = exp.xcBot.Conversion(1.0)
	}

	p.Unlock()

	if exp.devPrefetch {
		go exp.updateDevFundBalance()
	}

	// Do not run updates if blockchain sync is running.
	if !exp.AreDBsSyncing() {
		// Politeia updates happen hourly thus if every blocks takes an average
		// of 5 minutes to mine then 12 blocks take approximately 1hr.
		// https://docs.fonero.org/advanced/navigating-politeia-data/#voting-and-comment-data
		if newBlockData.Height%12 == 0 {
			// Update the proposal DB. This is run asynchronously since it involves
			// a query to Politeia (a remote system) and we do not want to block
			// execution.
			go func() {
				err := exp.proposalsSource.CheckProposalsUpdates()
				if err != nil {
					log.Error(err)
				}
			}()
		}

		// Update in every 5 blocks implys that in approximately every 25mins
		// an update will be queried.
		if newBlockData.Height%5 == 0 {
			// Update the Agendas DB. Run this asynchronously to avoid
			// blocking other processes.
			go func() {
				err := exp.agendasSource.CheckAgendasUpdates(exp.ChainParams.Deployments)
				if err != nil {
					log.Error(err)
				}
			}()
		}
	}

	// Signal to the websocket hub that a new block was received, but do not
	// block Store(), and do not hang forever in a goroutine waiting to send.
	go func() {
		select {
		case exp.wsHub.HubRelay <- pstypes.HubMessage{Signal: sigNewBlock}:
		case <-time.After(time.Second * 10):
			log.Errorf("sigNewBlock send failed: Timeout waiting for WebsocketHub.")
		}
	}()

	log.Debugf("Got new block %d for the explorer.", newBlockData.Height)

	return nil
}

func (exp *explorerUI) updateDevFundBalance() {
	// yield processor to other goroutines
	runtime.Gosched()

	devBalance, err := exp.explorerSource.DevBalance()
	if err == nil && devBalance != nil {
		exp.pageData.Lock()
		exp.pageData.HomeInfo.DevFund = devBalance.TotalUnspent
		exp.pageData.Unlock()
	} else {
		log.Errorf("explorerUI.updateDevFundBalance failed: %v", err)
	}
}

func (exp *explorerUI) addRoutes() {
	exp.Mux.Use(middleware.Logger)
	exp.Mux.Use(middleware.Recoverer)
	corsMW := cors.Default()
	exp.Mux.Use(corsMW.Handler)

	redirect := func(url string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			x := chi.URLParam(r, "x")
			if x != "" {
				x = "/" + x
			}
			http.Redirect(w, r, "/"+url+x, http.StatusPermanentRedirect)
		}
	}
	exp.Mux.Get("/", redirect("blocks"))

	exp.Mux.Get("/block/{x}", redirect("block"))

	exp.Mux.Get("/tx/{x}", redirect("tx"))

	exp.Mux.Get("/address/{x}", redirect("address"))

	exp.Mux.Get("/decodetx", redirect("decodetx"))

	exp.Mux.Get("/stats", redirect("statistics"))
}

// Simulate ticket purchase and re-investment over a full year for a given
// starting amount of FNO and calculation parameters.  Generate a TEXT table of
// the simulation results that can optionally be used for future expansion of
// fnodata functionality.
func (exp *explorerUI) simulateASR(StartingFNOBalance float64, IntegerTicketQty bool,
	CurrentStakePercent float64, ActualCoinbase float64, CurrentBlockNum float64,
	ActualTicketPrice float64) (ASR float64, ReturnTable string) {

	// Calculations are only useful on mainnet.  Short circuit calculations if
	// on any other version of chain params.
	if exp.ChainParams.Name != "mainnet" {
		return 0, ""
	}

	BlocksPerDay := 86400 / exp.ChainParams.TargetTimePerBlock.Seconds()
	BlocksPerYear := 365 * BlocksPerDay
	TicketsPurchased := float64(0)

	StakeRewardAtBlock := func(blocknum float64) float64 {
		// Option 1:  RPC Call
		Subsidy := exp.blockData.BlockSubsidy(int64(blocknum), 1)
		return fnoutil.Amount(Subsidy.PoS).ToCoin()

		// Option 2:  Calculation
		// epoch := math.Floor(blocknum / float64(exp.ChainParams.SubsidyReductionInterval))
		// RewardProportionPerVote := float64(exp.ChainParams.StakeRewardProportion) / (10 * float64(exp.ChainParams.TicketsPerBlock))
		// return float64(RewardProportionPerVote) * fnoutil.Amount(exp.ChainParams.BaseSubsidy).ToCoin() *
		// 	math.Pow(float64(exp.ChainParams.MulSubsidy)/float64(exp.ChainParams.DivSubsidy), epoch)
	}

	MaxCoinSupplyAtBlock := func(blocknum float64) float64 {
		// 4th order poly best fit curve to Fonero mainnet emissions plot.
		// Curve fit was done with 0 Y intercept and Pre-Mine added after.

		return (-9E-19*math.Pow(blocknum, 4) +
			7E-12*math.Pow(blocknum, 3) -
			2E-05*math.Pow(blocknum, 2) +
			29.757*blocknum + 76963 +
			1680000) // Premine 1.68M

	}

	CoinAdjustmentFactor := ActualCoinbase / MaxCoinSupplyAtBlock(CurrentBlockNum)

	TheoreticalTicketPrice := func(blocknum float64) float64 {
		ProjectedCoinsCirculating := MaxCoinSupplyAtBlock(blocknum) * CoinAdjustmentFactor * CurrentStakePercent
		TicketPoolSize := (float64(exp.MeanVotingBlocks) + float64(exp.ChainParams.TicketMaturity) +
			float64(exp.ChainParams.CoinbaseMaturity)) * float64(exp.ChainParams.TicketsPerBlock)
		return ProjectedCoinsCirculating / TicketPoolSize

	}
	TicketAdjustmentFactor := ActualTicketPrice / TheoreticalTicketPrice(CurrentBlockNum)

	// Prepare for simulation
	simblock := CurrentBlockNum
	TicketPrice := ActualTicketPrice
	FNOBalance := StartingFNOBalance

	ReturnTable += fmt.Sprintf("\n\nBLOCKNUM        FNO  TICKETS TKT_PRICE TKT_REWRD  ACTION\n")
	ReturnTable += fmt.Sprintf("%8d  %9.2f %8.1f %9.2f %9.2f    INIT\n",
		int64(simblock), FNOBalance, TicketsPurchased,
		TicketPrice, StakeRewardAtBlock(simblock))

	for simblock < (BlocksPerYear + CurrentBlockNum) {

		// Simulate a Purchase on simblock
		TicketPrice = TheoreticalTicketPrice(simblock) * TicketAdjustmentFactor

		if IntegerTicketQty {
			// Use this to simulate integer qtys of tickets up to max funds
			TicketsPurchased = math.Floor(FNOBalance / TicketPrice)
		} else {
			// Use this to simulate ALL funds used to buy tickets - even fractional tickets
			// which is actually not possible
			TicketsPurchased = (FNOBalance / TicketPrice)
		}

		FNOBalance -= (TicketPrice * TicketsPurchased)
		ReturnTable += fmt.Sprintf("%8d  %9.2f %8.1f %9.2f %9.2f     BUY\n",
			int64(simblock), FNOBalance, TicketsPurchased,
			TicketPrice, StakeRewardAtBlock(simblock))

		// Move forward to average vote
		simblock += (float64(exp.ChainParams.TicketMaturity) + float64(exp.MeanVotingBlocks))
		ReturnTable += fmt.Sprintf("%8d  %9.2f %8.1f %9.2f %9.2f    VOTE\n",
			int64(simblock), FNOBalance, TicketsPurchased,
			(TheoreticalTicketPrice(simblock) * TicketAdjustmentFactor), StakeRewardAtBlock(simblock))

		// Simulate return of funds
		FNOBalance += (TicketPrice * TicketsPurchased)

		// Simulate reward
		FNOBalance += (StakeRewardAtBlock(simblock) * TicketsPurchased)
		TicketsPurchased = 0

		// Move forward to coinbase maturity
		simblock += float64(exp.ChainParams.CoinbaseMaturity)

		ReturnTable += fmt.Sprintf("%8d  %9.2f %8.1f %9.2f %9.2f  REWARD\n",
			int64(simblock), FNOBalance, TicketsPurchased,
			(TheoreticalTicketPrice(simblock) * TicketAdjustmentFactor), StakeRewardAtBlock(simblock))

		// Need to receive funds before we can use them again so add 1 block
		simblock++
	}

	// Scale down to exactly 365 days
	SimulationReward := ((FNOBalance - StartingFNOBalance) / StartingFNOBalance) * 100
	ASR = (BlocksPerYear / (simblock - CurrentBlockNum)) * SimulationReward
	ReturnTable += fmt.Sprintf("ASR over 365 Days is %.2f.\n", ASR)
	return
}

func (exp *explorerUI) watchExchanges() {
	if exp.xcBot == nil {
		return
	}
	xcChans := exp.xcBot.UpdateChannels()

	sendXcUpdate := func(isFiat bool, token string, updater *exchanges.ExchangeState) {
		xcState := exp.xcBot.State()
		update := &WebsocketExchangeUpdate{
			Updater: WebsocketMiniExchange{
				Token:  token,
				Price:  updater.Price,
				Volume: updater.Volume,
				Change: updater.Change,
			},
			IsFiatIndex: isFiat,
			BtcIndex:    exp.xcBot.BtcIndex,
			Price:       xcState.Price,
			BtcPrice:    xcState.BtcPrice,
			Volume:      xcState.Volume,
		}
		select {
		case exp.wsHub.xcChan <- update:
		default:
			log.Warnf("Failed to send WebsocketExchangeUpdate on WebsocketHub channel")
		}
	}

	for {
		select {
		case update := <-xcChans.Exchange:
			sendXcUpdate(false, update.Token, update.State)
		case update := <-xcChans.Index:
			indexState, found := exp.xcBot.State().FiatIndices[update.Token]
			if !found {
				log.Errorf("Index state not found when preparing websocket udpate")
				continue
			}
			sendXcUpdate(true, update.Token, indexState)
		case <-xcChans.Quit:
			log.Warnf("ExchangeBot has quit.")
			return
		case <-exp.xcDone:
			return
		}
	}
}

func (exp *explorerUI) getExchangeState() *exchanges.ExchangeBotState {
	if exp.xcBot == nil || exp.xcBot.IsFailed() {
		return nil
	}
	return exp.xcBot.State()
}

// mempoolTime is the TimeDef that the transaction was received in FNOData, or
// else a zero-valued TimeDef if no transaction is found.
func (exp *explorerUI) mempoolTime(txid string) types.TimeDef {
	exp.invsMtx.RLock()
	defer exp.invsMtx.RUnlock()
	tx, found := exp.invs.Tx(txid)
	if !found {
		return types.NewTimeDefFromUNIX(0)
	}
	return types.NewTimeDefFromUNIX(tx.Time)
}

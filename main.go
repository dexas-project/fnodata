// Copyright (c) 2018-2019, The Fonero developers
// Copyright (c) 2017, Jonathan Chappelow
// See LICENSE for details.

package main

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	"github.com/fonero-project/fnod/chaincfg/chainhash"
	"github.com/fonero-project/fnod/rpcclient"
	"github.com/fonero-project/fnodata/blockdata"
	"github.com/fonero-project/fnodata/db/cache"
	"github.com/fonero-project/fnodata/db/dbtypes"
	"github.com/fonero-project/fnodata/db/fnopg"
	"github.com/fonero-project/fnodata/db/fnosqlite"
	"github.com/fonero-project/fnodata/exchanges"
	"github.com/fonero-project/fnodata/gov/agendas"
	"github.com/fonero-project/fnodata/gov/politeia"
	"github.com/fonero-project/fnodata/mempool"
	m "github.com/fonero-project/fnodata/middleware"
	"github.com/fonero-project/fnodata/pubsub"
	pstypes "github.com/fonero-project/fnodata/pubsub/types"
	"github.com/fonero-project/fnodata/rpcutils"
	"github.com/fonero-project/fnodata/semver"
	"github.com/fonero-project/fnodata/stakedb"
	"github.com/fonero-project/fnodata/txhelpers"
	"github.com/fonero-project/fnodata/api"
	"github.com/fonero-project/fnodata/api/insight"
	"github.com/fonero-project/fnodata/explorer"
	notify "github.com/fonero-project/fnodata/notification"
	"github.com/fonero-project/fnodata/version"
	"github.com/dmigwi/go-piparser/proposals"
	"github.com/go-chi/chi"
	"github.com/google/gops/agent"
)

func main() {
	// Create a context that is cancelled when a shutdown request is received
	// via requestShutdown.
	ctx := withShutdownCancel(context.Background())
	// Listen for both interrupt signals and shutdown requests.
	go shutdownListener()

	if err := _main(ctx); err != nil {
		if logRotator != nil {
			log.Error(err)
		}
		os.Exit(1)
	}
	os.Exit(0)
}

// _main does all the work. Deferred functions do not run after os.Exit(), so
// main wraps this function, which returns a code.
func _main(ctx context.Context) error {
	// Parse the configuration file, and setup logger.
	cfg, err := loadConfig()
	if err != nil {
		fmt.Printf("Failed to load fnodata config: %s\n", err.Error())
		return err
	}
	defer func() {
		if logRotator != nil {
			logRotator.Close()
		}
	}()

	if cfg.CPUProfile != "" {
		var f *os.File
		f, err = os.Create(cfg.CPUProfile)
		if err != nil {
			return err
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	if cfg.UseGops {
		// Start gops diagnostic agent, with shutdown cleanup.
		if err = agent.Listen(agent.Options{}); err != nil {
			return err
		}
		defer agent.Close()
	}

	// Display app version.
	log.Infof("%s version %v (Go version %s)", version.AppName,
		version.Version(), runtime.Version())

	// Setup the notification handlers.
	notify.MakeNtfnChans()

	// Connect to fnod RPC server using a websocket.
	ntfnHandlers, collectionQueue := notify.MakeNodeNtfnHandlers()
	fnodClient, nodeVer, err := connectNodeRPC(cfg, ntfnHandlers)
	if err != nil || fnodClient == nil {
		return fmt.Errorf("Connection to fnod failed: %v", err)
	}

	defer func() {
		if fnodClient != nil {
			log.Infof("Closing connection to fnod.")
			fnodClient.Shutdown()
			fnodClient.WaitForShutdown()
		}

		// The individial hander's loops should close the notifications channels
		// on quit, but do it here too to be sure.
		notify.CloseNtfnChans()

		log.Infof("Bye!")
		time.Sleep(250 * time.Millisecond)
	}()

	// Display connected network (e.g. mainnet, testnet, simnet).
	curnet, err := fnodClient.GetCurrentNet()
	if err != nil {
		return fmt.Errorf("Unable to get current network from fnod: %v", err)
	}
	log.Infof("Connected to fnod (JSON-RPC API v%s) on %v",
		nodeVer.String(), curnet.String())

	if curnet != activeNet.Net {
		log.Criticalf("Network of connected node, %s, does not match expected "+
			"network, %s.", activeNet.Net, curnet)
		return fmt.Errorf("expected network %s, got %s", activeNet.Net, curnet)
	}

	// StakeDatabase
	stakeDB, stakeDBHeight, err := stakedb.NewStakeDatabase(fnodClient, activeChain, cfg.DataDir)
	if err != nil {
		log.Errorf("Unable to create stake DB: %v", err)
		if stakeDBHeight >= 0 {
			log.Infof("Attempting to recover stake DB...")
			stakeDB, err = stakedb.LoadAndRecover(fnodClient, activeChain, cfg.DataDir, stakeDBHeight-288)
			//stakeDBHeight = int64(stakeDB.Height())
		}
		if err != nil {
			if stakeDB != nil {
				_ = stakeDB.Close()
			}
			return fmt.Errorf("StakeDatabase recovery failed: %v", err)
		}
	}
	defer stakeDB.Close()

	// SQLite
	dbPath := filepath.Join(cfg.DataDir, cfg.DBFileName)
	dbInfo := fnosqlite.DBInfo{FileName: dbPath}
	baseDB, err := fnosqlite.InitWiredDB(&dbInfo, stakeDB,
		notify.NtfnChans.UpdateStatusDBHeight, fnodClient, activeChain, requestShutdown)
	if err != nil {
		return fmt.Errorf("Unable to initialize SQLite database: %v", err)
	}
	log.Infof("SQLite DB successfully opened: %s", cfg.DBFileName)
	defer baseDB.Close()

	baseDB.DB.SetMaxOpenConns(cfg.SQLiteMaxConns)

	if err = baseDB.ReportHeights(); err != nil {
		return fmt.Errorf("Possible SQLite corruption: %v", err)
	}

	log.Infof("Setting up the Politeia's proposals clone repository. Please wait...")
	// If repoName and repoOwner are set to empty strings the defaults are used.
	parser, err := proposals.NewParser(cfg.PiPropRepoOwner, cfg.PiPropRepoName, cfg.DataDir)
	if err != nil {
		// since this piparser isn't a requirement to run the explorer, its
		// failure should not block the system from running.
		log.Error(err)
	}

	// Auxiliary DB (PostgreSQL)
	var newPGIndexes, updateAllAddresses, updateAllVotes bool
	pgHost, pgPort := cfg.PGHost, ""
	if !strings.HasPrefix(pgHost, "/") {
		pgHost, pgPort, err = net.SplitHostPort(cfg.PGHost)
		if err != nil {
			return fmt.Errorf("SplitHostPort failed: %v", err)
		}
	}
	dbi := fnopg.DBInfo{
		Host:         pgHost,
		Port:         pgPort,
		User:         cfg.PGUser,
		Pass:         cfg.PGPass,
		DBName:       cfg.PGDBName,
		QueryTimeout: cfg.PGQueryTimeout,
	}

	// If using {netname} then replace it with netName(activeNet).
	dbi.DBName = strings.Replace(dbi.DBName, "{netname}", netName(activeNet), -1)

	// Rough estimate of capacity in rows, using size of struct plus some
	// for the string buffer of the Address field.
	rowCap := cfg.AddrCacheCap / int(32+reflect.TypeOf(dbtypes.AddressRowCompact{}).Size())
	log.Infof("Address cache capacity: %d rows, %d bytes", rowCap, cfg.AddrCacheCap)

	// Open and upgrade the database.
	mpChecker := rpcutils.NewMempoolAddressChecker(fnodClient, activeChain)
	chainDB, err := fnopg.NewChainDBWithCancel(ctx, &dbi, activeChain,
		stakeDB, !cfg.NoDevPrefetch, cfg.HidePGConfig, rowCap,
		mpChecker, parser, fnodClient)
	if chainDB != nil {
		defer chainDB.Close()
	}
	if err != nil {
		return err
	}

	// Wrap ChainDB with an RPC client. TODO: redefine or remove ChainDBRPC.
	pgDB, err := fnopg.NewChainDBRPC(chainDB, fnodClient)
	if err != nil {
		return err
	}

	if cfg.DropIndexes {
		log.Info("Dropping all table indexing and quitting...")
		err = pgDB.DeindexAll()
		requestShutdown()
		return err
	}

	// Check for missing indexes.
	missingIndexes, descs, err := pgDB.MissingIndexes()
	if err != nil {
		return err
	}

	// If any indexes are missing, forcibly drop any existing indexes, and
	// create them all after block sync.
	if len(missingIndexes) > 0 {
		newPGIndexes = true
		updateAllAddresses = true
		// Warn if this is not a fresh sync.
		if pgDB.Height() > 0 {
			log.Warnf("Some table indexes not found!")
			for im, mi := range missingIndexes {
				log.Warnf(` - Missing Index "%s": "%s"`, mi, descs[im])
			}
			log.Warnf("Forcing new index creation and addresses table spending info update.")
		}
	}

	// Heights gets the current height of each DB, the minimum of the DB heights
	// (dbHeight), and the chain server height.
	Heights := func() (dbHeight, nodeHeight, baseDBHeight, pgDBHeight int64, err error) {
		_, nodeHeight, err = fnodClient.GetBestBlock()
		if err != nil {
			err = fmt.Errorf("unable to get block from node: %v", err)
			return
		}

		baseDBHeight, err = baseDB.GetHeight()
		if err != nil {
			if err != sql.ErrNoRows {
				log.Errorf("baseDB.GetHeight failed: %v", err)
				return
			}
			// err == sql.ErrNoRows is not an error
			err = nil
			log.Infof("baseDB block summary table is empty.")
		}
		log.Debugf("baseDB height: %d", baseDBHeight)
		dbHeight = baseDBHeight

		pgDBHeight, err = pgDB.HeightDB()
		if err != nil {
			if err != sql.ErrNoRows {
				log.Errorf("pgDB.HeightDB failed: %v", err)
				return
			}
			// err == sql.ErrNoRows is not an error, and pgDBHeight == -1
			err = nil
			log.Infof("pgDB block summary table is empty.")
		}
		log.Debugf("pgDB height: %d", pgDBHeight)
		if baseDBHeight > pgDBHeight {
			dbHeight = pgDBHeight
		}

		return
	}

	// Check for database tip blocks that have been orphaned. If any are found,
	// purge blocks to get to a common ancestor. Only message when purging more
	// than requested in the configuration settings.
	blocksToPurge := cfg.PurgeNBestBlocks
	_, _, baseHeight, auxHeight, err := Heights()
	if err != nil {
		return fmt.Errorf("Failed to get Heights for tip check: %v", err)
	}

	if baseHeight > -1 {
		orphaned, err := rpcutils.OrphanedTipLength(ctx, fnodClient, baseHeight, baseDB.DB.RetrieveBlockHash)
		if err != nil {
			return fmt.Errorf("Failed to compare tip blocks for the base DB: %v", err)
		}
		if int(orphaned) > blocksToPurge {
			blocksToPurge = int(orphaned)
			log.Infof("Orphaned tip detected on base DB. Purging %d blocks", blocksToPurge)
		}
	}

	if auxHeight > -1 {
		orphaned, err := rpcutils.OrphanedTipLength(ctx, fnodClient, auxHeight, pgDB.BlockHash)
		if err != nil {
			return fmt.Errorf("Failed to compare tip blocks for the aux DB: %v", err)
		}
		if int(orphaned) > blocksToPurge {
			blocksToPurge = int(orphaned)
			log.Infof("Orphaned tip detected on aux DB. Purging %d blocks", blocksToPurge)
		}
	}

	// Give a chance to abort a purge.
	if shutdownRequested(ctx) {
		return nil
	}

	if blocksToPurge > 0 {
		// The number of blocks to purge for each DB is computed so that the DBs
		// will end on the same height.
		_, _, baseDBHeight, pgDBHeight, err := Heights()
		if err != nil {
			return fmt.Errorf("Heights failed: %v", err)
		}
		// Determine the largest DB height.
		maxHeight := baseDBHeight
		if pgDBHeight > maxHeight {
			maxHeight = pgDBHeight
		}
		// The final best block after purge.
		purgeToBlock := maxHeight - int64(blocksToPurge)

		// Purge from SQLite, using either the "blocks above" or "N best main
		// chain" approach.
		var heightDB, nRemovedSummary int64
		if cfg.FastSQLitePurge {
			log.Infof("Purging SQLite data for the blocks above %d...",
				purgeToBlock)
			nRemovedSummary, _, err = baseDB.PurgeBlocksAboveHeight(purgeToBlock)
			if err != nil && err != sql.ErrNoRows {
				return fmt.Errorf("failed to purge to block %d from SQLite: %v",
					purgeToBlock, err)
			}
			heightDB = purgeToBlock
		} else {
			// Purge NBase blocks from base DB.
			NBase := baseDBHeight - purgeToBlock
			log.Infof("Purging SQLite data for the %d best blocks...", NBase)
			nRemovedSummary, _, heightDB, _, err = baseDB.PurgeBestBlocks(NBase)
			if err != nil {
				return fmt.Errorf("failed to purge %d blocks from SQLite: %v",
					NBase, err)
			}
		}

		// The number of rows removed from the summary table and stake table may
		// be different if the DB was corrupted, but it is not important to log
		// for the tables separately.
		log.Infof("Successfully purged data for %d blocks from SQLite "+
			"(new height = %d).", nRemovedSummary, heightDB)

		// Purge NAux blocks from auxiliary DB.
		NAux := pgDBHeight - purgeToBlock
		log.Infof("Purging PostgreSQL data for the %d best blocks...", NAux)
		s, heightDB, err := pgDB.PurgeBestBlocks(NAux)
		if err != nil {
			return fmt.Errorf("Failed to purge %d blocks from PostgreSQL: %v",
				NAux, err)
		}
		if s != nil {
			log.Infof("Successfully purged data for %d blocks from PostgreSQL "+
				"(new height = %d):\n%v", s.Blocks, heightDB, s)
		} // otherwise likely sql.ErrNoRows (heightDB was already -1)
	}

	// Get the last block added to the aux DB.
	lastBlockPG, err := pgDB.HeightDB()
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("Unable to get height from PostgreSQL DB: %v", err)
	}

	// Allow WiredDB/stakedb to catch up to the pgDB, but after
	// fetchToHeight, WiredDB must receive block signals from pgDB, and
	// stakedb must send connect signals to pgDB.
	fetchToHeight := lastBlockPG + 1

	// For consistency with StakeDatabase, a non-negative height is needed.
	heightDB := lastBlockPG
	if heightDB < 0 {
		heightDB = 0
	}

	charts := cache.NewChartData(uint32(heightDB), time.Unix(pgDB.GenesisStamp(), 0),
		activeChain, ctx)
	pgDB.RegisterCharts(charts)
	baseDB.RegisterCharts(charts)

	// Aux DB height and stakedb height must be equal. StakeDatabase will
	// catch up automatically if it is behind, but we must rewind it here if
	// it is ahead of pgDB. For pgDB to receive notification from
	// StakeDatabase when the required blocks are connected, the
	// StakeDatabase must be at the same height or lower than pgDB.
	stakeDBHeight = int64(stakeDB.Height())
	if stakeDBHeight > heightDB {
		// Have baseDB rewind it's the StakeDatabase. stakeDBHeight is
		// always rewound to a height of zero even when lastBlockPG is -1,
		// hence we rewind to heightDB.
		log.Infof("Rewinding StakeDatabase from block %d to %d.",
			stakeDBHeight, heightDB)
		stakeDBHeight, err = baseDB.RewindStakeDB(ctx, heightDB)
		if err != nil {
			return fmt.Errorf("RewindStakeDB failed: %v", err)
		}

		// Verify that the StakeDatabase is at the intended height.
		if stakeDBHeight != heightDB {
			return fmt.Errorf("failed to rewind stakedb: got %d, expecting %d",
				stakeDBHeight, heightDB)
		}
	}

	// TODO: just use getblockchaininfo to see if it still syncing and what
	// height the network's best block is at.
	blockHash, nodeHeight, err := fnodClient.GetBestBlock()
	if err != nil {
		return fmt.Errorf("Unable to get block from node: %v", err)
	}

	block, err := fnodClient.GetBlockHeader(blockHash)
	if err != nil {
		return fmt.Errorf("unable to fetch the block from the node: %v", err)
	}

	// bestBlockAge is the time since the fnod best block was mined.
	bestBlockAge := time.Since(block.Timestamp).Minutes()

	// Since mining a block take approximately ChainParams.TargetTimePerBlock then the
	// expected height of the best block from fnod now should be this.
	expectedHeight := int64(bestBlockAge/float64(activeChain.TargetTimePerBlock)) + nodeHeight

	// Estimate how far pgDB is behind the node.
	blocksBehind := expectedHeight - lastBlockPG
	if blocksBehind < 0 {
		return fmt.Errorf("Node is still syncing. Node height = %d, "+
			"DB height = %d", expectedHeight, heightDB)
	}

	// PG gets winning tickets out of baseDB's pool info cache, so it must
	// be big enough to hold the needed blocks' info, and charged with the
	// data from disk. The cache is updated on each block connect.
	tpcSize := int(blocksBehind) + 200
	log.Debugf("Setting ticket pool cache capacity to %d blocks", tpcSize)
	err = stakeDB.SetPoolCacheCapacity(tpcSize)
	if err != nil {
		return err
	}

	// Charge stakedb pool info cache, including previous PG blocks, up to
	// best in sqlite.
	if err = baseDB.ChargePoolInfoCache(heightDB - 2); err != nil {
		return fmt.Errorf("Failed to charge pool info cache: %v", err)
	}

	// Fetch the latest blockchain info, which is needed to update the
	// agendas db while db sync is in progress.
	bci, err := baseDB.BlockchainInfo()
	if err != nil {
		return fmt.Errorf("failed to fetch the latest blockchain info")
	}

	// Update the current chain state in the ChainDBRPC
	pgDB.UpdateChainState(bci)

	// Block data collector. Needs a StakeDatabase too.
	collector := blockdata.NewCollector(fnodClient, activeChain, stakeDB)
	if collector == nil {
		return fmt.Errorf("Failed to create block data collector")
	}

	// Build a slice of each required saver type for each data source.
	blockDataSavers := []blockdata.BlockDataSaver{pgDB}
	blockDataSavers = append(blockDataSavers, baseDB)

	mempoolSavers := []mempool.MempoolDataSaver{baseDB.MPC} // mempool.MempoolDataCache

	// Allow Ctrl-C to halt startup here.
	if shutdownRequested(ctx) {
		return nil
	}

	// WaitGroup for monitoring goroutines
	var wg sync.WaitGroup

	// ExchangeBot
	var xcBot *exchanges.ExchangeBot
	if cfg.EnableExchangeBot && activeChain.Name != "mainnet" {
		log.Warnf("disabling exchange monitoring. only available on mainnet")
		cfg.EnableExchangeBot = false
	}
	if cfg.EnableExchangeBot {
		botCfg := exchanges.ExchangeBotConfig{
			BtcIndex:       cfg.ExchangeCurrency,
			MasterBot:      cfg.RateMaster,
			MasterCertFile: cfg.RateCertificate,
		}
		if cfg.DisabledExchanges != "" {
			botCfg.Disabled = strings.Split(cfg.DisabledExchanges, ",")
		}
		xcBot, err = exchanges.NewExchangeBot(&botCfg)
		if err != nil {
			log.Errorf("Could not create exchange monitor. Exchange info will be disabled: %v", err)
		} else {
			var xcList, prepend string
			for k := range xcBot.Exchanges {
				xcList += prepend + k
				prepend = ", "
			}
			log.Infof("ExchangeBot monitoring %s", xcList)
			wg.Add(1)
			go xcBot.Start(ctx, &wg)
		}
	}

	// Creates a new or loads an existing agendas db instance that helps to
	// store and retrieves agendas data. Agendas votes are On-Chain
	// transactions that appear in the fonero blockchain. If corrupted data is
	// is found, its deleted pending the data update that restores valid data.
	agendasInstance, err := agendas.NewAgendasDB(fnodClient,
		filepath.Join(cfg.DataDir, cfg.AgendasDBFileName))
	if err != nil {
		return fmt.Errorf("failed to create new agendas db instance: %v", err)
	}

	// Retrieve blockchain deployment updates and add them to the agendas db.
	// activeChain.Deployments contains a list of all agendas supported in the
	// current environment.
	if err = agendasInstance.CheckAgendasUpdates(activeChain.Deployments); err != nil {
		return fmt.Errorf("updating agendas db failed: %v", err)
	}

	// Creates a new or loads an existing proposals db instance that helps to
	// store and retrieve proposals data. Proposals votes is Off-Chain
	// data stored in github repositories away from the fonero blockchain. It also
	// creates a new http client needed to query Politeia API endpoints.
	proposalsInstance, err := politeia.NewProposalsDB(cfg.PoliteiaAPIURL,
		filepath.Join(cfg.DataDir, cfg.ProposalsFileName))
	if err != nil {
		return fmt.Errorf("failed to create new proposals db instance: %v", err)
	}

	// Retrieve newly added proposals and add them to the proposals db.
	// Proposal db update is made asynchronously to ensure that the system works
	// even when the Politeia API endpoint set is down.
	go func() {
		if err := proposalsInstance.CheckProposalsUpdates(); err != nil {
			log.Errorf("updating proposals db failed: %v", err)
		}
	}()

	// A vote tracker tracks current block and stake versions and votes.
	tracker, err := agendas.NewVoteTracker(activeChain, fnodClient,
		pgDB.AgendaVoteCounts, activeChain.Deployments)
	if err != nil {
		return fmt.Errorf("Unable to initialize vote tracker: %v", err)
	}

	// Create the explorer system.
	explore := explorer.New(&explorer.ExplorerConfig{
		DataSource:        baseDB,
		PrimaryDataSource: pgDB,
		UseRealIP:         cfg.UseRealIP,
		AppVersion:        version.Version(),
		DevPrefetch:       !cfg.NoDevPrefetch,
		Viewsfolder:       "views",
		XcBot:             xcBot,
		AgendasSource:     agendasInstance,
		Tracker:           tracker,
		ProposalsSource:   proposalsInstance,
		PoliteiaURL:       cfg.PoliteiaAPIURL,
		MainnetLink:       cfg.MainnetLink,
		TestnetLink:       cfg.TestnetLink,
	})
	// TODO: allow views config
	if explore == nil {
		return fmt.Errorf("failed to create new explorer (templates missing?)")
	}
	explore.UseSIGToReloadTemplates()
	defer explore.StopWebsocketHub()

	blockDataSavers = append(blockDataSavers, explore)
	mempoolSavers = append(mempoolSavers, explore)

	// Create the pub sub hub.
	psHub, err := pubsub.NewPubSubHub(baseDB)
	if err != nil {
		return fmt.Errorf("failed to create new pubsubhub: %v", err)
	}
	defer psHub.StopWebsocketHub()

	blockDataSavers = append(blockDataSavers, psHub)
	mempoolSavers = append(mempoolSavers, psHub) // individial transactions are from mempool monitor

	// Create the mempool data collector.
	mpoolCollector := mempool.NewMempoolDataCollector(fnodClient, activeChain)
	if mpoolCollector == nil {
		// Shutdown goroutines.
		requestShutdown()
		return fmt.Errorf("Failed to create mempool data collector")
	}

	// The MempoolMonitor receives notifications of new transactions on
	// notify.NtfnChans.NewTxChan, and of new blocks on the same channel with a
	// nil transaction message. The mempool monitor will process the
	// transactions, and forward new ones on via the mpDataToPSHub with an
	// appropriate signal to the underlying WebSocketHub on signalToPSHub.
	signalToPSHub := psHub.HubRelay()
	signalToExplorer := explore.MempoolSignal()
	mempoolSigOuts := []chan<- pstypes.HubMessage{signalToPSHub, signalToExplorer}
	mpm, err := mempool.NewMempoolMonitor(ctx, mpoolCollector, mempoolSavers,
		activeChain, &wg, notify.NtfnChans.NewTxChan, mempoolSigOuts, true)
	// Ensure the initial collect/store succeeded.
	if err != nil {
		// Shutdown goroutines.
		requestShutdown()
		return fmt.Errorf("NewMempoolMonitor: %v", err)
	}

	// Use the MempoolMonitor in aux DB to get unconfirmed transaction data.
	pgDB.UseMempoolChecker(mpm)

	// Prepare for sync by setting up the channels for status/progress updates
	// (barLoad) or full explorer page updates (latestBlockHash).

	// barLoad is used to send sync status updates to websocket clients (e.g.
	// browsers with the status page opened) via the goroutines launched by
	// BeginSyncStatusUpdates.
	var barLoad chan *dbtypes.ProgressBarLoad

	// latestBlockHash communicates the hash of block most recently processed
	// during synchronization. This is done if all of the explorer pages (not
	// just the status page) are to be served during sync.
	var latestBlockHash chan *chainhash.Hash

	// Display the blockchain syncing status page if the number of blocks behind
	// the node's best block height are more than the set limit. The sync status
	// page should also be displayed when updateAllAddresses and newPGIndexes
	// are true, indicating maintenance or an initial sync.
	dbHeight, nodeHeight, _, _, err := Heights()
	if err != nil {
		return fmt.Errorf("Heights failed: %v", err)
	}
	blocksBehind = nodeHeight - dbHeight
	log.Debugf("dbHeight: %d / blocksBehind: %d", dbHeight, blocksBehind)
	displaySyncStatusPage := blocksBehind > int64(cfg.SyncStatusLimit) || // over limit
		updateAllAddresses || newPGIndexes // maintenance or initial sync

	// Initiate the sync status monitor and the coordinating goroutines if the
	// sync status is activated, otherwise coordinate updating the full set of
	// explorer pages.
	if displaySyncStatusPage {
		// Start goroutines that keep the update the shared progress bar data,
		// and signal the websocket hub to send progress updates to clients.
		barLoad = make(chan *dbtypes.ProgressBarLoad, 2)
		explore.BeginSyncStatusUpdates(barLoad)
	} else {
		// Start a goroutine to update the explorer pages when the DB sync
		// functions send a new block hash on the following channel.
		latestBlockHash = make(chan *chainhash.Hash, 2)

		// The BlockConnected handler should not be started until after sync.
		go func() {
			// Keep receiving updates until the channel is closed, or a nil Hash
			// pointer received.
			for hash := range latestBlockHash {
				if hash == nil {
					return
				}
				// Fetch the blockdata by block hash.
				d, msgBlock, err := collector.CollectHash(hash)
				if err != nil {
					log.Warnf("failed to fetch blockdata for (%s) hash. error: %v",
						hash.String(), err)
					continue
				}

				// Store the blockdata for the explorer pages.
				if err = explore.Store(d, msgBlock); err != nil {
					log.Warnf("failed to store (%s) hash's blockdata for the explorer pages error: %v",
						hash.String(), err)
				}
			}
		}()

		// Before starting the DB sync, trigger the explorer to display data for
		// the current best block.

		// Retrieve the hash of the best block across every DB.
		latestDBBlockHash, err := fnodClient.GetBlockHash(dbHeight)
		if err != nil {
			return fmt.Errorf("failed to fetch the block at height (%d): %v",
				dbHeight, err)
		}

		// Signal to load this block's data into the explorer. Future signals
		// will come from the sync methods of either baseDB or pgDB.
		latestBlockHash <- latestDBBlockHash
	}

	// Create the Insight socket.io server, and add it to block savers if in
	// full/pg mode. Since insightSocketServer is added into the url before even
	// the sync starts, this implementation cannot be moved to
	// initiateHandlersAndCollectBlocks function.
	insightSocketServer, err := insight.NewSocketServer(notify.NtfnChans.InsightNewTxChan, activeChain)
	if err != nil {
		return fmt.Errorf("Could not create Insight socket.io server: %v", err)
	}
	blockDataSavers = append(blockDataSavers, insightSocketServer)

	// Start fnodata's JSON web API.
	app := api.NewContext(&api.AppContextConfig{
		Client:            fnodClient,
		Params:            activeChain,
		DataSource:        baseDB,
		DBSource:          pgDB,
		JsonIndent:        cfg.IndentJSON,
		XcBot:             xcBot,
		AgendasDBInstance: agendasInstance,
		MaxAddrs:          cfg.MaxCSVAddrs,
		Charts:            charts,
	})
	// Start the notification hander for keeping /status up-to-date.
	wg.Add(1)
	go app.StatusNtfnHandler(ctx, &wg)
	// Initial setting of DBHeight. Subsequently, Store() will send this.
	if dbHeight >= 0 {
		// Do not sent 4294967295 = uint32(-1) if there are no blocks.
		notify.NtfnChans.UpdateStatusDBHeight <- uint32(dbHeight)
	}

	// Configure the URL path to http handler router for the API.
	apiMux := api.NewAPIRouter(app, cfg.UseRealIP, cfg.CompressAPI)

	// File downloads piggy-back on the API.
	fileMux := api.NewFileRouter(app, cfg.UseRealIP)

	// Configure the explorer web pages router.
	webMux := chi.NewRouter()
	webMux.With(explore.SyncStatusPageIntercept).Group(func(r chi.Router) {
		r.Get("/", explore.Home)
		r.Get("/nexthome", explore.NextHome)
	})
	webMux.Get("/ws", explore.RootWebsocket)
	webMux.Get("/ps", psHub.WebSocketHandler)

	// Make the static assets available under a path with the given prefix.
	mountAssetPaths := func(pathPrefix string) {
		if !strings.HasSuffix(pathPrefix, "/") {
			pathPrefix += "/"
		}

		webMux.Get(pathPrefix+"favicon.ico", func(w http.ResponseWriter, r *http.Request) {
			log.Infof("Serving %s", r.URL.String())
			http.ServeFile(w, r, "./public/images/favicon/favicon.ico")
		})

		cacheControlMaxAge := int64(cfg.CacheControlMaxAge)
		FileServer(webMux, pathPrefix+"js", "./public/js", cacheControlMaxAge)
		FileServer(webMux, pathPrefix+"css", "./public/css", cacheControlMaxAge)
		FileServer(webMux, pathPrefix+"fonts", "./public/fonts", cacheControlMaxAge)
		FileServer(webMux, pathPrefix+"images", "./public/images", cacheControlMaxAge)
		FileServer(webMux, pathPrefix+"dist", "./public/dist", cacheControlMaxAge)
	}
	// Mount under root (e.g. /js, /css, etc.).
	mountAssetPaths("/")

	// HTTP profiler
	if cfg.HTTPProfile {
		profPath := cfg.HTTPProfPath
		log.Warnf("Starting the HTTP profiler on path %s.", profPath)
		// http pprof uses http.DefaultServeMux
		http.Handle("/", http.RedirectHandler(profPath+"/debug/pprof/", http.StatusSeeOther))
		webMux.Mount(profPath, http.StripPrefix(profPath, http.DefaultServeMux))
	}

	// SyncStatusAPIIntercept returns a json response if the sync status page is
	// enabled (no the full explorer while syncing).
	webMux.With(explore.SyncStatusAPIIntercept).Group(func(r chi.Router) {
		// Mount the fnodata's REST API.
		r.Mount("/api", apiMux.Mux)
		// Setup and mount the Insight API.
		insightApp := insight.NewInsightApi(fnodClient, pgDB,
			activeChain, mpm, cfg.IndentJSON, cfg.MaxCSVAddrs, app.Status)
		insightApp.SetReqRateLimit(cfg.InsightReqRateLimit)
		insightMux := insight.NewInsightApiRouter(insightApp, cfg.UseRealIP, cfg.CompressAPI)
		r.Mount("/insight/api", insightMux.Mux)

		if insightSocketServer != nil {
			r.Get("/insight/socket.io/", insightSocketServer.ServeHTTP)
		}
	})

	// HTTP Error 503 StatusServiceUnavailable for file requests before sync.
	webMux.With(explore.SyncStatusFileIntercept).Group(func(r chi.Router) {
		r.Mount("/download", fileMux.Mux)
	})

	webMux.With(explore.SyncStatusPageIntercept).Group(func(r chi.Router) {
		r.NotFound(explore.NotFound)

		r.Mount("/explorer", explore.Mux)
		r.Get("/days", explore.DayBlocksListing)
		r.Get("/weeks", explore.WeekBlocksListing)
		r.Get("/months", explore.MonthBlocksListing)
		r.Get("/years", explore.YearBlocksListing)
		r.Get("/blocks", explore.Blocks)
		r.Get("/ticketpricewindows", explore.StakeDiffWindows)
		r.Get("/side", explore.SideChains)
		r.Get("/rejects", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/disapproved", http.StatusPermanentRedirect)
		})
		r.Get("/disapproved", explore.DisapprovedBlocks)
		r.Get("/mempool", explore.Mempool)
		r.Get("/parameters", explore.ParametersPage)
		r.With(explore.BlockHashPathOrIndexCtx).Get("/block/{blockhash}", explore.Block)
		r.With(explorer.TransactionHashCtx).Get("/tx/{txid}", explore.TxPage)
		r.With(explorer.TransactionHashCtx, explorer.TransactionIoIndexCtx).Get("/tx/{txid}/{inout}/{inoutid}", explore.TxPage)
		r.With(explorer.AddressPathCtx).Get("/address/{address}", explore.AddressPage)
		r.With(explorer.AddressPathCtx).Get("/addresstable/{address}", explore.AddressTable)
		r.Get("/agendas", explore.AgendasPage)
		r.With(explorer.AgendaPathCtx).Get("/agenda/{agendaid}", explore.AgendaPage)
		r.Get("/proposals", explore.ProposalsPage)
		r.With(explorer.ProposalPathCtx).Get("/proposal/{proposalrefid}", explore.ProposalPage)
		r.Get("/decodetx", explore.DecodeTxPage)
		r.Get("/search", explore.Search)
		r.Get("/charts", explore.Charts)
		r.Get("/ticketpool", explore.Ticketpool)
		r.Get("/stats", explore.StatsPage)
		r.Get("/market", explore.MarketPage)
		r.Get("/statistics", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/stats", http.StatusPermanentRedirect)
		})
		// MenuFormParser will typically redirect, but going to the homepage as a
		// fallback.
		r.With(explorer.MenuFormParser).Post("/set", explore.Home)
	})

	// Configure a page for the bare "/insight" path. This mounts the static
	// assets under /insight (e.g. /insight/js) to support the page's complete
	// loading when the root mounter is not accessible, such as the case in
	// certain reverse proxy configurations that map /insight as the root path.
	webMux.With(m.OriginalRequestURI).Get("/insight", explore.InsightRootPage)
	// Serve static assets under /insight for when the a reverse proxy prefixes
	// all requests with "/insight". (e.g. /insight/js, /insight/css, etc.).
	mountAssetPaths("/insight")

	// Start the web server.
	listenAndServeProto(ctx, &wg, cfg.APIListen, cfg.APIProto, webMux)

	// Last chance to quit before syncing if the web server could not start.
	if shutdownRequested(ctx) {
		return nil
	}

	log.Infof("Starting blockchain sync...")
	explore.SetDBsSyncing(true)
	psHub.SetReady(false)

	// Coordinate the sync of both sqlite and auxiliary DBs with the network.
	// This closure captures the RPC client and the quit channel.
	getSyncd := func(updateAddys, updateVotes, newPGInds bool,
		fetchHeightInBaseDB int64) (int64, int64, error) {
		// Simultaneously synchronize the ChainDB (PostgreSQL) and the
		// block/stake info DB (sqlite). Results are returned over channels:
		sqliteSyncRes := make(chan dbtypes.SyncResult)
		pgSyncRes := make(chan dbtypes.SyncResult)

		// Use either the plain rpcclient.Client or a rpcutils.BlockPrefetchClient.
		var bf rpcutils.BlockFetcher
		if cfg.BlockPrefetch {
			pfc := rpcutils.NewBlockPrefetchClient(fnodClient)
			defer func() {
				pfc.Stop()
				log.Debugf("Block prefetcher hits = %d, misses = %d.",
					pfc.Hits(), pfc.Misses())
			}()
			bf = pfc
		} else {
			bf = fnodClient
		}

		// Synchronization between DBs via rpcutils.BlockGate
		smartClient := rpcutils.NewBlockGate(bf, 4)

		// stakedb (in baseDB) connects blocks *after* ChainDB retrieves them,
		// but it has to get a notification channel first to receive them. The
		// BlockGate will provide this for blocks after fetchHeightInBaseDB.
		baseDB.SyncDBAsync(ctx, sqliteSyncRes, smartClient, fetchHeightInBaseDB)

		// Now that stakedb is either catching up or waiting for a block, start
		// the pgDB sync, which is the master block getter, retrieving and
		// making available blocks to the baseDB. In return, baseDB maintains a
		// StakeDatabase at the best block's height. For a detailed description
		// on how the DBs' synchronization is coordinated, see the documents in
		// db/fnopg/sync.go.
		go pgDB.SyncChainDBAsync(ctx, pgSyncRes, smartClient,
			updateAddys, updateVotes, newPGInds, latestBlockHash, barLoad)

		// Wait for the results from both of these DBs.
		return waitForSync(ctx, sqliteSyncRes, pgSyncRes)
	}

	baseDBHeight, pgDBHeight, err := getSyncd(updateAllAddresses,
		updateAllVotes, newPGIndexes, fetchToHeight)
	if err != nil {
		requestShutdown()
		return err
	}

	// After sync and indexing, must use upsert statement, which checks for
	// duplicate entries and updates instead of erroring. SyncChainDB should
	// set this on successful sync, but do it again anyway.
	pgDB.EnableDuplicateCheckOnInsert(true)

	// The sync routines may have lengthy tasks, such as table indexing, that
	// follow main sync loop. Before enabling the chain monitors, again ensure
	// the DBs are at the node's best block.
	ensureSync := func() error {
		updateAllAddresses, updateAllVotes, newPGIndexes = false, false, false
		_, height, err := fnodClient.GetBestBlock()
		if err != nil {
			return fmt.Errorf("unable to get block from node: %v", err)
		}

		for baseDBHeight < height {
			fetchToHeight = pgDBHeight + 1
			baseDBHeight, pgDBHeight, err = getSyncd(updateAllAddresses, updateAllVotes,
				newPGIndexes, fetchToHeight)
			if err != nil {
				requestShutdown()
				return err
			}
			_, height, err = fnodClient.GetBestBlock()
			if err != nil {
				return fmt.Errorf("unable to get block from node: %v", err)
			}
		}

		// Update the node height for the status API endpoint.
		select {
		case notify.NtfnChans.UpdateStatusNodeHeight <- uint32(height):
		default:
			log.Errorf("Failed to update node height with API status. Is StatusNtfnHandler started?")
		}
		// WiredDB.resyncDB is responsible for updating DB status via
		// notify.NtfnChans.UpdateStatusDBHeight.

		return nil
	}
	if err = ensureSync(); err != nil {
		return err
	}

	// Exits immediately after the sync completes if SyncAndQuit is to true
	// because all we needed then was the blockchain sync be completed successfully.
	if cfg.SyncAndQuit {
		log.Infof("All ready, at height %d. Quitting.", baseDBHeight)
		return nil
	}

	log.Info("Mainchain sync complete.")

	// Ensure all side chains known by fnod are also present in the base DB
	// and import them if they are not already there.
	if cfg.ImportSideChains {
		log.Info("Primary DB -> Now retrieving side chain blocks from fnod...")
		err := baseDB.ImportSideChains(collector)
		if err != nil {
			log.Errorf("Primary DB -> Error importing side chains: %v", err)
		}
	}

	// Ensure all side chains known by fnod are also present in the auxiliary DB
	// and import them if they are not already there.
	if cfg.ImportSideChains {
		// First identify the side chain blocks that are missing from the DB.
		log.Info("Aux DB -> Retrieving side chain blocks from fnod...")
		sideChainBlocksToStore, nSideChainBlocks, err := pgDB.MissingSideChainBlocks()
		if err != nil {
			return fmt.Errorf("Aux DB -> Unable to determine missing side chain blocks: %v", err)
		}
		nSideChains := len(sideChainBlocksToStore)

		// Importing side chain blocks involves only the aux (postgres) DBs
		// since fnosqlite does not track side chain blocks, and stakedb only
		// supports mainchain. TODO: Get stakedb to work with side chain blocks
		// to get ticket pool info.

		// Collect and store data for each side chain.
		log.Infof("Aux DB -> Importing %d new block(s) from %d known side chains...",
			nSideChainBlocks, nSideChains)
		// Disable recomputing project fund balance, and clearing address
		// balance and counts cache.
		pgDB.InBatchSync = true
		var sideChainsStored, sideChainBlocksStored int
		for _, sideChain := range sideChainBlocksToStore {
			// Process this side chain only if there are blocks in it that need
			// to be stored.
			if len(sideChain.Hashes) == 0 {
				continue
			}
			sideChainsStored++

			// Collect and store data for each block in this side chain.
			for _, hash := range sideChain.Hashes {
				// Validate the block hash.
				blockHash, err := chainhash.NewHashFromStr(hash)
				if err != nil {
					log.Errorf("Aux DB -> Invalid block hash %s: %v.", hash, err)
					continue
				}

				// Collect block data.
				blockData, msgBlock, err := collector.CollectHash(blockHash)
				if err != nil {
					// Do not quit if unable to collect side chain block data.
					log.Errorf("Aux DB -> Unable to collect data for side chain block %s: %v.",
						hash, err)
					continue
				}

				// Get the chainwork
				chainWork, err := rpcutils.GetChainWork(pgDB.Client, blockHash)
				if err != nil {
					log.Errorf("GetChainWork failed (%s): %v", blockHash, err)
					continue
				}

				// PostgreSQL / aux DB
				log.Debugf("Aux DB -> Importing block %s (height %d) into aux DB.",
					blockHash, msgBlock.Header.Height)

				// Stake invalidation is always handled by subsequent block, so
				// add the block as valid. These are all side chain blocks.
				isValid, isMainchain := true, false

				// Existing DB records might be for mainchain and/or valid
				// blocks, so these imported blocks should not data in rows that
				// are conflicting as per the different table constraints and
				// unique indexes.
				updateExistingRecords := false

				// Store data in the aux (fnopg) DB.
				_, _, _, err = pgDB.StoreBlock(msgBlock, blockData.WinningTickets,
					isValid, isMainchain, updateExistingRecords, true, true, chainWork)
				if err != nil {
					// If data collection succeeded, but storage fails, bail out
					// to diagnose the DB trouble.
					return fmt.Errorf("Aux DB -> ChainDBRPC.StoreBlock failed: %v", err)
				}

				sideChainBlocksStored++
			}
		}
		pgDB.InBatchSync = false
		log.Infof("Successfully added %d blocks from %d side chains into fnopg DB.",
			sideChainBlocksStored, sideChainsStored)

		// That may have taken a while, check again for new blocks from network.
		if err = ensureSync(); err != nil {
			return err
		}
	}

	// It initiates the updates fetch process for the proposal votes data after
	// sync for the other tables is complete. It is only run if the system is on
	// full mode. An error in fetching the updates should not stop the system
	// functionality since it could be attributed to the external systems used.
	log.Info("Running updates retrieval for Politeia's Proposals. Please wait...")

	// Fetch updates for Politiea's Proposal history data via the parser.
	commitsCount, err := pgDB.PiProposalsHistory()
	if err != nil {
		log.Errorf("pgDB.PiProposalsHistory failed : %v", err)
	} else {
		log.Infof("%d politeia's proposal (auxiliary db) commits were processed",
			commitsCount)
	}

	log.Infof("All ready, at height %d.", baseDBHeight)
	explore.SetDBsSyncing(false)
	psHub.SetReady(true)

	// Pre-populate charts data using the dumped cache data in the .gob file path
	// provided instead of querying the data from the dbs.
	// Should be invoked before explore.Store to avoid double charts data
	// cache population. This charts pre-population is faster than db querying
	// and can be done before the monitors are fully set up.
	dumpPath := filepath.Join(cfg.DataDir, cfg.ChartsCacheDump)
	charts.Load(dumpPath)
	// Add charts saver method after explorer and any databases.
	blockDataSavers = append(blockDataSavers, blockdata.BlockTrigger{Saver: charts.TriggerUpdate})

	// This dumps the cache charts data into a file for future use on system
	// exit.
	defer charts.Dump(dumpPath)

	// Enable new blocks being stored into the base DB's cache.
	baseDB.EnableCache()

	// Block further usage of the barLoad by sending a nil value
	if barLoad != nil {
		select {
		case barLoad <- nil:
		default:
		}
	}

	// Set that newly sync'd blocks should no longer be stored in the explorer.
	// Monitors that fetch the latest updates from fnod will be launched next.
	if latestBlockHash != nil {
		close(latestBlockHash)
	}

	// Monitors for new blocks, transactions, and reorgs should not run before
	// blockchain syncing and DB indexing completes. If started before then, the
	// DBs will not be prepared to process the notified events. For example, if
	// fnod notifies of block 200000 while fnodata has only reached 1000 in
	// batch synchronization, trying to process that block will be impossible as
	// the entire chain before it is not yet processed. Similarly, if we have
	// already registered for notifications with fnod but the monitors below are
	// not started, notifications will fill up the channels, only to be
	// processed after sync. This is also incorrect since fnod might notify of a
	// bew block 200000, but the batch sync will process that block on its own,
	// causing this to be a duplicate block by the time the monitors begin
	// pulling data out of the full channels.

	// The following configures and starts handlers that monitor for new blocks,
	// changes in the mempool, and handle chain reorg. It also initiates data
	// collection for the explorer.

	// Blockchain monitor for the collector
	addrMap := make(map[string]txhelpers.TxAction) // for support of watched addresses
	// On reorg, only update web UI since fnosqlite's own reorg handler will
	// deal with patching up the block info database.
	reorgBlockDataSavers := []blockdata.BlockDataSaver{explore}
	wsChainMonitor := blockdata.NewChainMonitor(ctx, collector, blockDataSavers,
		reorgBlockDataSavers, &wg, addrMap, notify.NtfnChans.RecvTxBlockChan,
		notify.NtfnChans.ReorgChanBlockData)

	// Blockchain monitor for the stake DB
	sdbChainMonitor := stakeDB.NewChainMonitor(ctx, &wg, notify.NtfnChans.ReorgChanStakeDB)

	// Blockchain monitor for the wired sqlite DB
	WiredDBChainMonitor := baseDB.NewChainMonitor(ctx, collector, &wg,
		notify.NtfnChans.ConnectChanWiredDB, notify.NtfnChans.ReorgChanWiredDB)

	// Blockchain monitor for the aux (PG) DB
	pgDBChainMonitor := pgDB.NewChainMonitor(ctx, &wg,
		notify.NtfnChans.ConnectChanFnopgDB, notify.NtfnChans.ReorgChanFnopgDB)
	if pgDBChainMonitor == nil {
		return fmt.Errorf("Failed to enable fnopg ChainMonitor. *ChainDB is nil.")
	}

	// Setup the synchronous handler functions called by the collectionQueue via
	// OnBlockConnected.
	collectionQueue.SetSynchronousHandlers([]func(*chainhash.Hash) error{
		sdbChainMonitor.ConnectBlock, // 1. Stake DB for pool info
		wsChainMonitor.ConnectBlock,  // 2. blockdata for regular block data collection and storage
	})

	// Initial data summary for web ui. stakedb must be at the same height, so
	// we do this before starting the monitors.
	blockData, msgBlock, err := collector.Collect()
	if err != nil {
		return fmt.Errorf("Block data collection for initial summary failed: %v",
			err.Error())
	}

	// Update the current chain state in the ChainDB.
	pgDB.UpdateChainState(blockData.BlockchainInfo)

	if err = explore.Store(blockData, msgBlock); err != nil {
		return fmt.Errorf("Failed to store initial block data for explorer pages: %v", err.Error())
	}

	if err = psHub.Store(blockData, msgBlock); err != nil {
		return fmt.Errorf("Failed to store initial block data with the PubSubHub: %v", err.Error())
	}

	// Register for notifications from fnod. This also sets the daemon RPC
	// client used by other functions in the notify/notification package (i.e.
	// common ancestor identification in signalReorg).
	cerr := notify.RegisterNodeNtfnHandlers(fnodClient)
	if cerr != nil {
		return fmt.Errorf("RPC client error: %v (%v)", cerr.Error(), cerr.Cause())
	}

	// After this final node sync check, the monitors will handle new blocks.
	// TODO: make this not racy at all by having sync stop at specified block.
	if err = ensureSync(); err != nil {
		return err
	}

	// Set the current best block in the collection queue so that it can verify
	// that subsequent blocks are in the correct sequence.
	bestHash, bestHeight, err := baseDB.GetBestBlockHeightHash()
	if err != nil {
		return fmt.Errorf("Failed to determine base DB's best block: %v", err)
	}
	collectionQueue.SetPreviousBlock(bestHash, bestHeight)

	// Start the monitors' event handlers.

	// blockdata collector handlers
	wg.Add(1)
	// The blockdata reorg handler disables collection during reorg, leaving
	// fnosqlite to do the switch, except for the last block which gets
	// collected and stored via reorgBlockDataSavers (for the explorer UI).
	go wsChainMonitor.ReorgHandler()

	// StakeDatabase
	wg.Add(1)
	go sdbChainMonitor.ReorgHandler()

	// fnosqlite does not handle new blocks except during reorg.
	wg.Add(1)
	go WiredDBChainMonitor.ReorgHandler()

	// fnopg also does not handle new blocks except during reorg.
	wg.Add(1)
	go pgDBChainMonitor.ReorgHandler()

	wg.Add(1)
	// charts cache drops all the records added since the common ancestor
	// before initiating a cache update after all other reorgs have completed.
	go charts.ReorgHandler(&wg, notify.NtfnChans.ReorgChartsCache)

	// Begin listening on notify.NtfnChans.NewTxChan, and forwarding mempool
	// events to psHub via the channels from HubRelays().
	wg.Add(1)

	// TxHandler also gets signaled about new blocks when a nil tx hash is sent
	// on notify.NtfnChans.NewTxChan, which triggers a full mempool refresh
	// followed by CollectAndStore, which provides the parsed data to all
	// mempoolSavers via their StoreMPData method. This should include the
	// PubSubHub and the base DB's MempoolDataCache.
	go mpm.TxHandler(fnodClient)

	// Wait for notification handlers to quit.
	wg.Wait()

	return nil
}

func waitForSync(ctx context.Context, base chan dbtypes.SyncResult, aux chan dbtypes.SyncResult) (int64, int64, error) {
	// First wait for the base DB (sqlite) sync to complete.
	baseRes := <-base
	baseDBHeight := baseRes.Height
	log.Infof("SQLite sync ended at height %d", baseDBHeight)
	// With an error condition in the result, signal for shutdown and wait for
	// the aux. DB (PostgreSQL) to return its result.
	if baseRes.Error != nil {
		log.Errorf("fnosqlite.SyncDBAsync failed at height %d: %v.", baseDBHeight, baseRes.Error)
		requestShutdown()
		auxRes := <-aux
		return baseDBHeight, auxRes.Height, baseRes.Error
	}

	// After a successful sqlite sync result is received, wait for the
	// postgresql sync result.
	auxRes := <-aux
	pgDBHeight := auxRes.Height
	log.Infof("PostgreSQL sync ended at height %d", pgDBHeight)

	// See if shutdown was requested.
	if shutdownRequested(ctx) {
		return baseDBHeight, pgDBHeight, fmt.Errorf("quit signal received during DB sync")
	}

	// Check for pg sync errors and combine the messages if necessary.
	if auxRes.Error != nil {
		if baseRes.Error != nil {
			log.Error("fnosqlite.SyncDBAsync AND fnopg.SyncChainDBAsync "+
				"failed at heights %d and %d, respectively.",
				baseDBHeight, pgDBHeight)
			errCombined := fmt.Errorf("%v, %v", baseRes.Error, auxRes.Error)
			return baseDBHeight, pgDBHeight, errCombined
		}
		log.Errorf("fnopg.SyncChainDBAsync failed at height %d.", pgDBHeight)
		return baseDBHeight, pgDBHeight, auxRes.Error
	}

	// DBs must finish at the same height.
	if pgDBHeight != baseDBHeight {
		return baseDBHeight, pgDBHeight, fmt.Errorf("failed to hit same"+
			"sync height for PostgreSQL (%d) and SQLite (%d)",
			pgDBHeight, baseDBHeight)
	}
	return baseDBHeight, pgDBHeight, nil
}

func connectNodeRPC(cfg *config, ntfnHandlers *rpcclient.NotificationHandlers) (*rpcclient.Client, semver.Semver, error) {
	return rpcutils.ConnectNodeRPC(cfg.FnodServ, cfg.FnodUser, cfg.FnodPass,
		cfg.FnodCert, cfg.DisableDaemonTLS, true, ntfnHandlers)
}

func listenAndServeProto(ctx context.Context, wg *sync.WaitGroup, listen, proto string, mux http.Handler) {
	// Try to bind web server
	server := http.Server{
		Addr:         listen,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,  // slow requests should not hold connections opened
		WriteTimeout: 60 * time.Second, // hung responses must die
	}

	// Add the graceful shutdown to the waitgroup.
	wg.Add(1)
	go func() {
		// Start graceful shutdown of web server on shutdown signal.
		<-ctx.Done()

		// We received an interrupt signal, shut down.
		log.Infof("Gracefully shutting down web server...")
		if err := server.Shutdown(context.Background()); err != nil {
			// Error from closing listeners.
			log.Infof("HTTP server Shutdown: %v", err)
		}

		// wg.Wait can proceed.
		wg.Done()
	}()

	log.Infof("Now serving the explorer and APIs on %s://%v/", proto, listen)
	// Start the server.
	go func() {
		var err error
		if proto == "https" {
			err = server.ListenAndServeTLS("fnodata.cert", "fnodata.key")
		} else {
			err = server.ListenAndServe()
		}
		// If the server dies for any reason other than ErrServerClosed (from
		// graceful server.Shutdown), log the error and request fnodata be
		// shutdown.
		if err != nil && err != http.ErrServerClosed {
			log.Errorf("Failed to start server: %v", err)
			requestShutdown()
		}
	}()

	// If the server successfully binds to a listening port, ListenAndServe*
	// will block until the server is shutdown. Wait here briefly so the startup
	// operations in main can have a chance to bail out.
	time.Sleep(250 * time.Millisecond)
}

// FileServer conveniently sets up a http.FileServer handler to serve static
// files from path on the file system. Directory listings are denied, as are URL
// paths containing "..".
func FileServer(r chi.Router, pathRoot, fsRoot string, cacheControlMaxAge int64) {
	if strings.ContainsAny(pathRoot, "{}*") {
		panic("FileServer does not permit URL parameters.")
	}

	// Define a http.HandlerFunc to serve files but not directory indexes.
	hf := func(w http.ResponseWriter, r *http.Request) {
		// Ensure the path begins with "/".
		upath := r.URL.Path
		if strings.Contains(upath, "..") {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		if !strings.HasPrefix(upath, "/") {
			upath = "/" + upath
			r.URL.Path = upath
		}
		// Strip the path prefix and clean the path.
		upath = path.Clean(strings.TrimPrefix(upath, pathRoot))

		// Deny directory listings (http.ServeFile recognizes index.html and
		// attempts to serve the directory contents instead).
		if strings.HasSuffix(upath, "/index.html") {
			http.NotFound(w, r)
			return
		}

		// Generate the full file system path and test for existence.
		fullFilePath := filepath.Join(fsRoot, upath)
		fi, err := os.Stat(fullFilePath)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		// Deny directory listings
		if fi.IsDir() {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}

		http.ServeFile(w, r, fullFilePath)
	}

	// For the chi.Mux, make sure a path that ends in "/" and append a "*".
	muxRoot := pathRoot
	if pathRoot != "/" && pathRoot[len(pathRoot)-1] != '/' {
		r.Get(pathRoot, http.RedirectHandler(pathRoot+"/", 301).ServeHTTP)
		muxRoot += "/"
	}
	muxRoot += "*"

	// Mount the http.HandlerFunc on the pathRoot.
	r.With(m.CacheControl(cacheControlMaxAge)).Get(muxRoot, hf)
}

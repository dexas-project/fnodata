// Copyright (c) 2018, The Fonero developers
// Copyright (c) 2017, Jonathan Chappelow
// See LICENSE for details.

package rpcutils

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"
	"strconv"

	"github.com/fonero-project/fnod/chaincfg"
	"github.com/fonero-project/fnod/chaincfg/chainhash"
	"github.com/fonero-project/fnod/fnojson"
	"github.com/fonero-project/fnod/fnoutil"
	"github.com/fonero-project/fnod/rpcclient"
	"github.com/fonero-project/fnod/wire"
	apitypes "github.com/fonero-project/fnodata/api/types"
	"github.com/fonero-project/fnodata/semver"
	"github.com/fonero-project/fnodata/txhelpers"
)

// Any of the following fnod RPC API versions are deemed compatible with
// fnodata.
var compatibleChainServerAPIs = []semver.Semver{
	semver.NewSemver(5, 0, 0), // order of reorg and block connected notifications changed
}

var (
	zeroHash            = chainhash.Hash{}
	zeroHashStringBytes = []byte(chainhash.Hash{}.String())

	maxAncestorChainLength = 8192

	ErrAncestorAtGenesis      = errors.New("no ancestor: at genesis")
	ErrAncestorMaxChainLength = errors.New("no ancestor: max chain length reached")
)

// ConnectNodeRPC attempts to create a new websocket connection to a fnod node,
// with the given credentials and optional notification handlers.
func ConnectNodeRPC(host, user, pass, cert string, disableTLS bool,
	ntfnHandlers ...*rpcclient.NotificationHandlers) (*rpcclient.Client, semver.Semver, error) {
	var fnodCerts []byte
	var err error
	var nodeVer semver.Semver
	if !disableTLS {
		fnodCerts, err = ioutil.ReadFile(cert)
		if err != nil {
			log.Errorf("Failed to read fnod cert file at %s: %s\n",
				cert, err.Error())
			return nil, nodeVer, err
		}
		log.Debugf("Attempting to connect to fnod RPC %s as user %s "+
			"using certificate located in %s",
			host, user, cert)
	} else {
		log.Debugf("Attempting to connect to fnod RPC %s as user %s (no TLS)",
			host, user)
	}

	connCfgDaemon := &rpcclient.ConnConfig{
		Host:         host,
		Endpoint:     "ws", // websocket
		User:         user,
		Pass:         pass,
		Certificates: fnodCerts,
		DisableTLS:   disableTLS,
	}

	var ntfnHdlrs *rpcclient.NotificationHandlers
	if len(ntfnHandlers) > 0 {
		if len(ntfnHandlers) > 1 {
			return nil, nodeVer, fmt.Errorf("invalid notification handler argument")
		}
		ntfnHdlrs = ntfnHandlers[0]
	}
	fnodClient, err := rpcclient.New(connCfgDaemon, ntfnHdlrs)
	if err != nil {
		return nil, nodeVer, fmt.Errorf("Failed to start fnod RPC client: %s", err.Error())
	}

	// Ensure the RPC server has a compatible API version.
	ver, err := fnodClient.Version()
	if err != nil {
		log.Error("Unable to get RPC version: ", err)
		return nil, nodeVer, fmt.Errorf("unable to get node RPC version")
	}

	fnodVer := ver["fnodjsonrpcapi"]
	nodeVer = semver.NewSemver(fnodVer.Major, fnodVer.Minor, fnodVer.Patch)

	// Check if the fnod RPC API version is compatible with fnodata.
	isApiCompat := semver.AnyCompatible(compatibleChainServerAPIs, nodeVer)
	if !isApiCompat {
		return nil, nodeVer, fmt.Errorf("Node JSON-RPC server does not have "+
			"a compatible API version. Advertises %v but requires one of: %v",
			nodeVer, compatibleChainServerAPIs)
	}

	return fnodClient, nodeVer, nil
}

// BuildBlockHeaderVerbose creates a *fnojson.GetBlockHeaderVerboseResult from
// an input *wire.BlockHeader and current best block height, which is used to
// compute confirmations.  The next block hash may optionally be provided.
func BuildBlockHeaderVerbose(header *wire.BlockHeader, params *chaincfg.Params,
	currentHeight int64, nextHash ...string) *fnojson.GetBlockHeaderVerboseResult {
	if header == nil {
		return nil
	}

	diffRatio := txhelpers.GetDifficultyRatio(header.Bits, params)

	var next string
	if len(nextHash) > 0 {
		next = nextHash[0]
	}

	blockHeaderResult := fnojson.GetBlockHeaderVerboseResult{
		Hash:          header.BlockHash().String(),
		Confirmations: currentHeight - int64(header.Height),
		Version:       header.Version,
		PreviousHash:  header.PrevBlock.String(),
		MerkleRoot:    header.MerkleRoot.String(),
		StakeRoot:     header.StakeRoot.String(),
		VoteBits:      header.VoteBits,
		FinalState:    hex.EncodeToString(header.FinalState[:]),
		Voters:        header.Voters,
		FreshStake:    header.FreshStake,
		Revocations:   header.Revocations,
		PoolSize:      header.PoolSize,
		Bits:          strconv.FormatInt(int64(header.Bits), 16),
		SBits:         fnoutil.Amount(header.SBits).ToCoin(),
		Height:        header.Height,
		Size:          header.Size,
		Time:          header.Timestamp.Unix(),
		Nonce:         header.Nonce,
		Difficulty:    diffRatio,
		NextHash:      next,
	}

	return &blockHeaderResult
}

// GetBlockHeaderVerbose creates a *fnojson.GetBlockHeaderVerboseResult for the
// block at height idx via an RPC connection to a chain server.
func GetBlockHeaderVerbose(client *rpcclient.Client, idx int64) *fnojson.GetBlockHeaderVerboseResult {
	blockhash, err := client.GetBlockHash(idx)
	if err != nil {
		log.Errorf("GetBlockHash(%d) failed: %v", idx, err)
		return nil
	}

	blockHeaderVerbose, err := client.GetBlockHeaderVerbose(blockhash)
	if err != nil {
		log.Errorf("GetBlockHeaderVerbose(%v) failed: %v", blockhash, err)
		return nil
	}

	return blockHeaderVerbose
}

// GetBlockHeaderVerboseByString creates a *fnojson.GetBlockHeaderVerboseResult
// for the block specified by hash via an RPC connection to a chain server.
func GetBlockHeaderVerboseByString(client *rpcclient.Client, hash string) *fnojson.GetBlockHeaderVerboseResult {
	blockhash, err := chainhash.NewHashFromStr(hash)
	if err != nil {
		log.Errorf("Invalid block hash %s: %v", blockhash, err)
		return nil
	}

	blockHeaderVerbose, err := client.GetBlockHeaderVerbose(blockhash)
	if err != nil {
		log.Errorf("GetBlockHeaderVerbose(%v) failed: %v", blockhash, err)
		return nil
	}

	return blockHeaderVerbose
}

// GetBlockVerbose creates a *fnojson.GetBlockVerboseResult for the block index
// specified by idx via an RPC connection to a chain server.
func GetBlockVerbose(client *rpcclient.Client, idx int64, verboseTx bool) *fnojson.GetBlockVerboseResult {
	blockhash, err := client.GetBlockHash(idx)
	if err != nil {
		log.Errorf("GetBlockHash(%d) failed: %v", idx, err)
		return nil
	}

	blockVerbose, err := client.GetBlockVerbose(blockhash, verboseTx)
	if err != nil {
		log.Errorf("GetBlockVerbose(%v) failed: %v", blockhash, err)
		return nil
	}

	return blockVerbose
}

// GetBlockVerboseByHash creates a *fnojson.GetBlockVerboseResult for the
// specified block hash via an RPC connection to a chain server.
func GetBlockVerboseByHash(client *rpcclient.Client, hash string, verboseTx bool) *fnojson.GetBlockVerboseResult {
	blockhash, err := chainhash.NewHashFromStr(hash)
	if err != nil {
		log.Errorf("Invalid block hash %s", hash)
		return nil
	}

	blockVerbose, err := client.GetBlockVerbose(blockhash, verboseTx)
	if err != nil {
		log.Errorf("GetBlockVerbose(%v) failed: %v", blockhash, err)
		return nil
	}

	return blockVerbose
}

// GetStakeDiffEstimates combines the results of EstimateStakeDiff and
// GetStakeDifficulty into a *apitypes.StakeDiff.
func GetStakeDiffEstimates(client *rpcclient.Client) *apitypes.StakeDiff {
	stakeDiff, err := client.GetStakeDifficulty()
	if err != nil {
		return nil
	}
	estStakeDiff, err := client.EstimateStakeDiff(nil)
	if err != nil {
		return nil
	}
	stakeDiffEstimates := apitypes.StakeDiff{
		GetStakeDifficultyResult: fnojson.GetStakeDifficultyResult{
			CurrentStakeDifficulty: stakeDiff.CurrentStakeDifficulty,
			NextStakeDifficulty:    stakeDiff.NextStakeDifficulty,
		},
		Estimates: *estStakeDiff,
	}
	return &stakeDiffEstimates
}

// GetBlock gets a block at the given height from a chain server.
func GetBlock(ind int64, client *rpcclient.Client) (*fnoutil.Block, *chainhash.Hash, error) {
	blockhash, err := client.GetBlockHash(ind)
	if err != nil {
		return nil, nil, fmt.Errorf("GetBlockHash(%d) failed: %v", ind, err)
	}

	msgBlock, err := client.GetBlock(blockhash)
	if err != nil {
		return nil, blockhash,
			fmt.Errorf("GetBlock failed (%s): %v", blockhash, err)
	}
	block := fnoutil.NewBlock(msgBlock)

	return block, blockhash, nil
}

// GetBlockByHash gets the block with the given hash from a chain server.
func GetBlockByHash(blockhash *chainhash.Hash, client *rpcclient.Client) (*fnoutil.Block, error) {
	msgBlock, err := client.GetBlock(blockhash)
	if err != nil {
		return nil, fmt.Errorf("GetBlock failed (%s): %v", blockhash, err)
	}
	block := fnoutil.NewBlock(msgBlock)

	return block, nil
}

// SideChains gets a slice of known side chain tips. This corresponds to the
// results of the getchaintips node RPC where the block tip "status" is either
// "valid-headers" or "valid-fork".
func SideChains(client *rpcclient.Client) ([]fnojson.GetChainTipsResult, error) {
	tips, err := client.GetChainTips()
	if err != nil {
		return nil, err
	}

	return sideChainTips(tips), nil
}

func sideChainTips(allTips []fnojson.GetChainTipsResult) (sideTips []fnojson.GetChainTipsResult) {
	for i := range allTips {
		switch allTips[i].Status {
		case "valid-headers", "valid-fork":
			sideTips = append(sideTips, allTips[i])
		}
	}
	return
}

// SideChainFull gets all of the blocks in the side chain with the specified tip
// block hash. The first block in the slice is the lowest height block in the
// side chain, and its previous block is the main/side common ancestor, which is
// not included in the slice since it is main chain. The last block in the slice
// is thus the side chain tip.
func SideChainFull(client *rpcclient.Client, tipHash string) ([]string, error) {
	// Do not assume specified tip hash is even side chain.
	var sideChain []string

	hash := tipHash
	for {
		header := GetBlockHeaderVerboseByString(client, hash)
		if header == nil {
			return nil, fmt.Errorf("GetBlockHeaderVerboseByString failed for block %s", hash)
		}

		// Main chain blocks have Confirmations != -1.
		if header.Confirmations != -1 {
			// The passed block is main chain, not a side chain tip.
			if hash == tipHash {
				return nil, fmt.Errorf("tip block is not on a side chain")
			}
			// This previous block is the main/side common ancestor.
			break
		}

		// This was another side chain block.
		sideChain = append(sideChain, hash)

		// On to previous block
		hash = header.PreviousHash
	}

	// Reverse side chain order so that last element is tip.
	reverseStringSlice(sideChain)

	return sideChain, nil
}

func reverseStringSlice(s []string) {
	N := len(s)
	for i := 0; i <= (N/2)-1; i++ {
		j := N - 1 - i
		s[i], s[j] = s[j], s[i]
	}
}

// GetTransactionVerboseByID get a transaction by transaction id
func GetTransactionVerboseByID(client *rpcclient.Client, txid string) (*fnojson.TxRawResult, error) {
	txhash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		log.Errorf("Invalid transaction hash %s", txid)
		return nil, err
	}

	txraw, err := client.GetRawTransactionVerbose(txhash)
	if err != nil {
		log.Errorf("GetRawTransactionVerbose failed for: %v", txhash)
		return nil, err
	}
	return txraw, nil
}

// SearchRawTransaction fetch transactions the belong to an
// address
func SearchRawTransaction(client *rpcclient.Client, count int, address string) ([]*fnojson.SearchRawTransactionsResult, error) {
	addr, err := fnoutil.DecodeAddress(address)
	if err != nil {
		log.Infof("Invalid address %s: %v", address, err)
		return nil, err
	}
	//change the 1000 000 number demo for now
	txs, err := client.SearchRawTransactionsVerbose(addr, 0, count,
		true, true, nil)
	if err != nil {
		log.Warnf("SearchRawTransaction failed for address %s: %v", addr, err)
	}
	return txs, nil
}

// CommonAncestor attempts to determine the common ancestor block for two chains
// specified by the hash of the chain tip block. The full chains from the tips
// back to but not including the common ancestor are also returned. The first
// element in the chain slices is the lowest block following the common
// ancestor, while the last element is the chain tip. The common ancestor will
// never by one of the chain tips. Thus, if one of the chain tips is on the
// other chain, that block will be shared between the two chains, and the common
// ancestor will be the previous block. However, the intended use of this
// function is to find a common ancestor for two chains with no common blocks.
func CommonAncestor(client *rpcclient.Client, hashA, hashB chainhash.Hash) (*chainhash.Hash, []chainhash.Hash, []chainhash.Hash, error) {
	if client == nil {
		return nil, nil, nil, errors.New("nil RPC client")
	}

	var length int
	var chainA, chainB []chainhash.Hash
	for {
		if length >= maxAncestorChainLength {
			return nil, nil, nil, ErrAncestorMaxChainLength
		}

		// Chain A
		blockA, err := client.GetBlock(&hashA)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("Failed to get block %v: %v", hashA, err)
		}
		heightA := blockA.Header.Height

		// Chain B
		blockB, err := client.GetBlock(&hashB)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("Failed to get block %v: %v", hashB, err)
		}
		heightB := blockB.Header.Height

		// Reach the same height on both chains before checking the loop
		// termination condition. At least one previous block for each chain
		// must be used, so that a chain tip block will not be considered a
		// common ancestor and it will instead be added to a chain slice.
		if heightA > heightB {
			chainA = append([]chainhash.Hash{hashA}, chainA...)
			length++
			hashA = blockA.Header.PrevBlock
			continue
		}
		if heightB > heightA {
			chainB = append([]chainhash.Hash{hashB}, chainB...)
			length++
			hashB = blockB.Header.PrevBlock
			continue
		}

		// Assert heightB == heightA
		if heightB != heightA {
			panic("you broke the code")
		}

		chainA = append([]chainhash.Hash{hashA}, chainA...)
		chainB = append([]chainhash.Hash{hashB}, chainB...)
		length++

		// We are at genesis if the previous block is the zero hash.
		if blockA.Header.PrevBlock == zeroHash {
			return nil, chainA, chainB, ErrAncestorAtGenesis // no common ancestor, but the same block
		}

		hashA = blockA.Header.PrevBlock
		hashB = blockB.Header.PrevBlock

		// break here rather than for condition so inputs with equal hashes get
		// handled properly (with ancestor as previous block and chains
		// including the input blocks.)
		if hashA == hashB {
			break // hashA(==hashB) is the common ancestor.
		}
	}

	// hashA == hashB
	return &hashA, chainA, chainB, nil
}

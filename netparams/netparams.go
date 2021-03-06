// Copyright (c) 2013-2015 The btcsuite developers
// Copyright (c) 2016-2017 The Fonero developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package netparams

import "github.com/fonero-project/fnod/chaincfg"

// Params is used to group parameters for various networks such as the main
// network and test networks.
type Params struct {
	*chaincfg.Params
	JSONRPCClientPort string
	JSONRPCServerPort string
	GRPCServerPort    string
}

// MainNetParams contains parameters specific running fnowallet and
// fnod on the main network (wire.MainNet).
var MainNetParams = Params{
	Params:            &chaincfg.MainNetParams,
	JSONRPCClientPort: "9209",
	JSONRPCServerPort: "9210",
	GRPCServerPort:    "9211",
}

// TestNetParams contains parameters specific running fnowallet and
// fnod on the test network (version 3) (wire.TestNet).
var TestNetParams = Params{
	Params:            &chaincfg.TestNetParams,
	JSONRPCClientPort: "19209",
	JSONRPCServerPort: "19210",
	GRPCServerPort:    "19211",
}

// SimNetParams contains parameters specific to the simulation test network
// (wire.SimNet).
var SimNetParams = Params{
	Params:            &chaincfg.SimNetParams,
	JSONRPCClientPort: "19556",
	JSONRPCServerPort: "19557",
	GRPCServerPort:    "19558",
}

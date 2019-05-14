// Copyright (c) 2018-2019, The Fonero developers
// Copyright (c) 2017, The fnodata developers
// See LICENSE for details.

package pubsub

import (
	"errors"
	"testing"

	pstypes "github.com/fonero-project/fnodata/pubsub/types"
)

func Test_client_subscribe(t *testing.T) {
	tests := []struct {
		name    string
		cl      *client
		hubMsg  pstypes.HubMessage
		wantErr error
	}{
		{"ok newtx", newClient(), pstypes.HubMessage{Signal: sigNewTx}, nil},
		{"ok addr", newClient(), pstypes.HubMessage{
			Signal: sigAddressTx,
			Msg:    &pstypes.AddressMessage{Address: "DsfX4WrSecUwGoRd9B7Lz1JjYssYaVKnjGC"},
		}, nil},
		{"bad addr", newClient(), pstypes.HubMessage{
			Signal: sigAddressTx,
			Msg:    pstypes.AddressMessage{Address: "DsfX4WrSecUwGoRd9B7Lz1JjYssYaVKnjGC"},
		}, errors.New("msg.Msg not a string (SigAddressTx): types.AddressMessage")},
		{"bad addr", newClient(), pstypes.HubMessage{
			Signal: sigAddressTx,
			Msg:    nil,
		}, errors.New("msg.Msg not a string (SigAddressTx): <nil>")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cl.subscribe(tt.hubMsg)
			if (err != nil) != (tt.wantErr != nil) ||
				(err != nil && err.Error() != tt.wantErr.Error()) {
				t.Errorf(`subscribe() error = "%v", wantErr "%v"`, err, tt.wantErr)
				return
			}
		})
	}
}

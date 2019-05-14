package agendas

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/fonero-project/fnod/chaincfg"
	"github.com/fonero-project/fnod/fnojson"
)

type dataSourceStub struct{}

func (source dataSourceStub) GetStakeVersionInfo(version int32) (*fnojson.GetStakeVersionInfoResult, error) {
	if version > 6 {
		return nil, fmt.Errorf(" ")
	}
	h := int64(version * 50000)
	return &fnojson.GetStakeVersionInfoResult{
		CurrentHeight: h,
		Hash:          strconv.Itoa(int(version)),
		Intervals: []fnojson.VersionInterval{
			{
				StartHeight: h - 500,
				EndHeight:   h + 500,
				PoSVersions: []fnojson.VersionCount{
					{
						Version: uint32(version),
						Count:   5,
					},
					{
						Version: uint32(version),
						Count:   100000,
					},
				},
				VoteVersions: []fnojson.VersionCount{
					{
						Version: uint32(version),
						Count:   5,
					},
					{
						Version: uint32(version),
						Count:   100000,
					},
				},
			},
			{
				StartHeight: h - 1500,
				EndHeight:   h - 501,
				PoSVersions: []fnojson.VersionCount{
					{
						Version: uint32(version),
						Count:   5,
					},
					{
						Version: uint32(version),
						Count:   100000,
					},
				},
				VoteVersions: []fnojson.VersionCount{
					{
						Version: uint32(version),
						Count:   5,
					},
					{
						Version: uint32(version),
						Count:   100000,
					},
				},
			},
		},
	}, nil
}

func (source dataSourceStub) GetVoteInfo(version uint32) (*fnojson.GetVoteInfoResult, error) {
	if version > 6 {
		return nil, fmt.Errorf(" ")
	}
	h := int64(version * 50000)
	return &fnojson.GetVoteInfoResult{
		CurrentHeight: h,
		StartHeight:   h - 1500,
		EndHeight:     h + 500,
		Hash:          strconv.Itoa(int(version)),
		VoteVersion:   version,
		Quorum:        4032,
		TotalVotes:    10000,
		Agendas: []fnojson.Agenda{
			{
				ID:             "test agenda",
				Description:    "agenda for testing",
				Mask:           6,
				StartTime:      5,
				ExpireTime:     10,
				Status:         "failed",
				QuorumProgress: 0,
				Choices: []fnojson.Choice{
					{
						ID:          "abstain",
						Description: "abstain voting for change",
						Bits:        0,
						IsAbstain:   true,
						IsNo:        false,
						Count:       0,
						Progress:    0,
					},
					{
						ID:          "no",
						Description: "keep the existing consensus rules",
						Bits:        2,
						IsAbstain:   false,
						IsNo:        true,
						Count:       0,
						Progress:    0,
					},
					{
						ID:          "yes",
						Description: "change to the new consensus rules",
						Bits:        4,
						IsAbstain:   false,
						IsNo:        false,
						Count:       0,
						Progress:    0,
					},
				},
			},
		},
	}, nil
}

func (source dataSourceStub) GetStakeVersions(hash string, count int32) (*fnojson.GetStakeVersionsResult, error) {
	h, _ := strconv.Atoi(hash)
	result := &fnojson.GetStakeVersionsResult{
		StakeVersions: make([]fnojson.StakeVersions, int(count)),
	}
	c := int(count)
	for i := 0; i < c; i++ {
		result.StakeVersions[i] = fnojson.StakeVersions{
			Hash:         strconv.Itoa(h),
			Height:       int64(h),
			BlockVersion: 6,
			StakeVersion: 6,
			Votes:        []fnojson.VersionBits{}, // VoteTracker does not use this
		}
		h--
	}
	return result, nil
}

func counter(hash string) (uint32, uint32, uint32, error) {
	return 1, 2, 3, nil
}

func TestVoteTracker(t *testing.T) {
	data := map[uint32][]chaincfg.ConsensusDeployment{4: []chaincfg.ConsensusDeployment{{StartTime: 1493164800}}}
	tracker, err := NewVoteTracker(&chaincfg.MainNetParams, dataSourceStub{}, counter, data)
	if err != nil {
		t.Errorf("NewVoteTracker error: %v", err)
	}

	summary := tracker.Summary()
	if summary == nil {
		t.Errorf("nil VoteSummary error")
	}
}

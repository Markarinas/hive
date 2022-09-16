package main

import (
	"context"
	"github.com/ethereum/hive/hivesim"
	"github.com/ethereum/hive/optimism"
	"github.com/stretchr/testify/require"
	"time"
)

var depositTests = []*optimism.TestSpec{
	{
		Name: "index simple deposit tx through the portal",
		Run:  simplePortalDepositTest,
	},
}

var withdrawalTests = []*optimism.TestSpec{
	{
		Name: "index simple eth withdrawal",
		Run:  simpleWithdrawalTest,
	},
}

func main() {
	suite := hivesim.Suite{
		Name: "optimism indexer",
		Description: `
Tests indexing deposits and withdrawals against a running node.
`[1:],
	}

	suite.Add(&hivesim.TestSpec{
		Name:        "deposits",
		Description: "Tests deposit indexer.",
		Run:         runAllTests(depositTests),
	})
	suite.Add(&hivesim.TestSpec{
		Name:        "withdrawals",
		Description: "Tests withdrawal indexer.",
		Run:         runAllTests(withdrawalTests),
	})

	sim := hivesim.New()
	hivesim.MustRunSuite(sim, suite)
}

func runAllTests(tests []*optimism.TestSpec) func(t *hivesim.T) {
	return func(t *hivesim.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		d := optimism.NewDevnet(t)
		require.NoError(t, optimism.StartSequencerDevnet(ctx, d, &optimism.SequencerDevnetParams{
			MaxSeqDrift:   10,
			SeqWindowSize: 4,
			ChanTimeout:   30,
			EnableIndexer: true,
		}))

		optimism.RunTests(ctx, t, &optimism.RunTestsParams{
			Devnet:      d,
			Tests:       tests,
			Concurrency: 40,
		})
	}
}

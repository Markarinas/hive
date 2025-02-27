package main

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum-optimism/optimism/op-node/eth"
	"github.com/ethereum-optimism/optimism/op-node/rollup/driver"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/stretchr/testify/require"

	"github.com/ethereum/hive/hivesim"
	"github.com/ethereum/hive/optimism"
)

const replicaCount = 2
const maxReplicaLag = 5

func main() {
	sim := hivesim.New()
	for _, forkName := range optimism.AllOptimismForkConfigs {
		forkName := forkName
		suite := hivesim.Suite{
			Name:        "optimism p2p - " + forkName,
			Description: "This suite runs the P2P tests",
		}

		// Add tests for full nodes.
		suite.Add(&hivesim.TestSpec{
			Name:        "simple p2p testnet",
			Description: `This suite runs the a testnet with P2P set up`,
			Run:         func(t *hivesim.T) { runP2PTests(t, forkName) },
		})
		suite.Add(&hivesim.TestSpec{
			Name:        "tx forwarding",
			Description: `This test verifies that tx forwarding works`,
			Run:         func(t *hivesim.T) { txForwardingTest(t, forkName) },
		})
		hivesim.MustRunSuite(sim, suite)
	}
}

// txForwardingTest verifies that a transaction submitted to a replica with tx forwarding enabled shows up on the sequencer.
// TODO: The transaction shows up with `getTransaction`, but it remains pending and is not mined for some reason.
// This is weird, but fine because it still shows that the transaction is received by the sequencer.
func txForwardingTest(t *hivesim.T, forkName string) {
	d := optimism.NewDevnet(t)
	sender := d.L2Vault.GenerateKey()
	receiver := d.L2Vault.GenerateKey()
	d.InitChain(30, 4, 30, core.GenesisAlloc{sender: {Balance: big.NewInt(params.Ether)}}, 15, forkName)
	d.AddEth1()
	d.WaitUpEth1(0, time.Second*10)

	d.AddOpL2()
	d.AddOpNode(0, 0, true)
	seqNode := d.GetOpL2Engine(0)
	seqClient := d.GetOpL2Engine(0).EthClient()

	d.AddOpL2(hivesim.Params{"HIVE_OP_GETH_SEQUENCER_HTTP": seqNode.HttpRpcEndpoint()})
	d.AddOpNode(0, 1, false)

	d.AddOpBatcher(0, 0, 0, optimism.HiveUnpackParams{}.Params())
	d.AddOpProposer(0, 0, 0)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		d.WaitUpOpL2Engine(0, time.Second*10)
		wg.Done()
	}()
	go func() {
		d.WaitUpOpL2Engine(1, time.Second*10)
		wg.Done()
	}()

	t.Log("waiting for nodes to come up")
	wg.Wait()

	verifClient := d.GetOpL2Engine(1).EthClient()

	baseTx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   optimism.L2ChainIDBig,
		Nonce:     0,
		To:        &receiver,
		Gas:       75000,
		GasTipCap: big.NewInt(10 * params.GWei),
		GasFeeCap: big.NewInt(20 * params.GWei),
		Value:     big.NewInt(0.0001 * params.Ether),
	})

	tx, err := d.L2Vault.SignTransaction(sender, baseTx)
	require.Nil(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, verifClient.SendTransaction(ctx, tx))
	t.Log("sent tx to verifier, waiting for propagation")

	<-time.After(10 * time.Second)

	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, isPending, err := seqClient.TransactionByHash(ctx, tx.Hash())
	if err != nil {
		t.Fatal("transaction did not propagate")
	}
	t.Logf("found transaction on sequencer, isPending: %v", isPending)

	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = optimism.WaitReceiptOK(ctx, seqClient, tx.Hash())
	require.Nil(t, err) // tx should show up on the sequencer
}

// runP2PTests runs the P2P tests between the sequencer and verifier.
func runP2PTests(t *hivesim.T, forkName string) {
	d := optimism.NewDevnet(t)

	d.InitChain(30, 4, 30, nil, 15, forkName)
	d.AddEth1() // l1 eth1 node is required for l2 config init
	d.WaitUpEth1(0, time.Second*10)

	// Start all replicas
	for i := 0; i <= replicaCount; i++ {
		isSeq := i == 0
		d.AddOpL2()
		d.AddOpNode(0, i, isSeq)

		if isSeq {
			d.AddOpBatcher(0, 0, 0, optimism.HiveUnpackParams{}.Params())
			d.AddOpProposer(0, 0, 0)
		}
	}
	t.Log("waiting for nodes to come up")
	// Wait for them to come up. Don't do in a separate thread b/c WaitUpOpL2Engine calls t.FailNow on error
	for i := 0; i <= replicaCount; i++ {
		d.WaitUpOpL2Engine(i, time.Second*10)
	}

	for i := 1; i <= replicaCount; i++ {
		node := d.GetOpNode(i)
		p2pClient := node.P2PClient()

		for j := 0; j <= replicaCount; j++ {
			if i == j {
				continue
			}
			peer := d.GetOpNode(j)
			t.Logf("peering node %d (%s) with %d", j, peer.P2PAddr(), i)
			require.NoError(t, p2pClient.ConnectPeer(context.Background(), peer.P2PAddr()))
		}
	}

	seqEng := d.GetOpL2Engine(0)
	seqEthCl := seqEng.EthClient()
	seqRollupCl := d.GetOpNode(0).RollupClient()
	sender := d.L2Vault.CreateAccount(context.Background(), seqEthCl, big.NewInt(params.Ether))

	// Wait until all replicas have an unsafe head within 2 blocks of the verifier unsafe head
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Log("awaiting initial sync")
	tick := time.NewTicker(250 * time.Millisecond)
waitLoop:
	for {
		select {
		case <-tick.C:
			seqHead, err := seqRollupCl.SyncStatus(ctx)
			require.NoError(t, err)
			if seqHead.UnsafeL2.Number == seqHead.SafeL2.Number {
				t.Log("Sequencer is unsafe head is at safe head", seqHead.SafeL2)
				continue
			}
			ready := true
			for i := 1; i <= replicaCount; i++ {
				repRollupCl := d.GetOpNode(i).RollupClient()
				repHead, err := repRollupCl.SyncStatus(ctx)
				require.NoError(t, err)

				if seqHead.UnsafeL2.Number-repHead.UnsafeL2.Number >= 2 {
					t.Logf("Replica %d is not ready. Seq Unsafe Head: %v, Replica Unsafe Head: %v", i, seqHead.UnsafeL2, repHead.UnsafeL2)
					ready = false
					break
				}
			}
			if ready {
				break waitLoop
			}
		case <-ctx.Done():
			t.Fatal("Context expired while waiting for nodes to come up")
		}
	}
	t.Log("Initial sync done")

	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Minute)
	errCh := make(chan error, 20)
	defer cancel()

	getSyncStat := func(ctx context.Context, i int) (*driver.SyncStatus, error) {
		cl := d.GetOpNode(i).RollupClient()
		seqStat, err := cl.SyncStatus(ctx)
		if err != nil {
			return nil, err
		}
		t.Log(fmt.Sprintf("replica-%d", i),
			"currentL1", seqStat.CurrentL1.TerminalString(),
			"headL1", seqStat.HeadL1.TerminalString(),
			"finalizedL2", seqStat.FinalizedL2.TerminalString(),
			"safeL2", seqStat.SafeL2.TerminalString(),
			"unsafeL2", seqStat.UnsafeL2.TerminalString())
		return seqStat, nil
	}

	// NB: head is from seq, id is from verif
	checkCanon := func(i int, head uint64, id eth.BlockID) error {
		// Convert to ints to stop this from underflowing and inccorectly error if the replica is ahead
		if int(head)-int(id.Number) > maxReplicaLag {
			return fmt.Errorf("replica %d: too far behind sequencer. seq head: %d, replica head: %d", i, head, id.Number)
		}
		bl, err := seqEthCl.BlockByNumber(ctx, big.NewInt(int64(id.Number)))
		if err != nil {
			return fmt.Errorf("replica %d: sequencer does not have block at height %d", i, id.Number)
		}
		if h := bl.Hash(); h != id.Hash {
			return fmt.Errorf("replica %d: sequencer diverged, height %d does not match: sequencer: %s <> verifier: %s", i, id.Number, h, id.Hash)
		}
		return nil
	}

	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		for {
			select {
			case <-tick.C:
				nonce, err := seqEthCl.NonceAt(ctx, sender, nil)
				if err != nil {
					errCh <- err
					return
				}
				tx := types.NewTx(&types.DynamicFeeTx{
					ChainID:   optimism.L2ChainIDBig,
					Nonce:     nonce,
					Gas:       75000,
					GasTipCap: big.NewInt(1),
					GasFeeCap: big.NewInt(2),
					Value:     big.NewInt(0.0001 * params.Ether),
				})
				tx, err = d.L2Vault.SignTransaction(sender, tx)
				if err != nil {
					errCh <- err
					return
				}

				if err := seqEthCl.SendTransaction(ctx, tx); err != nil {
					errCh <- err
					return
				}
				_, err = optimism.WaitReceiptOK(ctx, seqEthCl, tx.Hash())
				if err != nil {
					errCh <- err
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		for {
			select {
			case <-tick.C:
				head, err := seqEthCl.BlockByNumber(ctx, nil)
				if err != nil {
					errCh <- err
					return
				}

				for i := 1; i <= replicaCount; i++ {
					seqStat, err := getSyncStat(ctx, i)
					if err != nil {
						errCh <- err
						return
					}
					if err := checkCanon(i, head.NumberU64(), seqStat.UnsafeL2.ID()); err != nil {
						errCh <- err
						return
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	select {
	case <-time.NewTimer(time.Minute).C:
		break
	case err := <-errCh:
		t.Fatalf("unhandled error: %v", err)
	}

	cancel()
}

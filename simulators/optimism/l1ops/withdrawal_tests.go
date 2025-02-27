package main

import (
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/ethclient/gethclient"

	"github.com/ethereum-optimism/optimism/op-bindings/bindings"
	"github.com/ethereum-optimism/optimism/op-bindings/predeploys"
	"github.com/ethereum-optimism/optimism/op-node/withdrawals"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/hive/hivesim"
	"github.com/ethereum/hive/optimism"
	"github.com/stretchr/testify/require"
)

func simpleWithdrawalTest(t *hivesim.T, env *optimism.TestEnv) {
	l1 := env.Devnet.L1Client(0)
	l2 := env.Devnet.L2Client(0)
	l1Vault := env.Devnet.L1Vault
	l2Vault := env.Devnet.L2Vault
	depositor := l1Vault.CreateAccount(env.TimeoutCtx(time.Minute), l1, big.NewInt(params.Ether))
	l2Vault.InsertKey(l1Vault.FindKey(depositor))

	mintAmount := big.NewInt(0.5 * params.Ether)
	doDeposit(t, env, depositor, mintAmount, false, nil)

	l2Wd, err := bindings.NewL2ToL1MessagePasser(predeploys.L2ToL1MessagePasserAddr, l2)
	require.Nil(t, err, "binding withdrawer on L2")

	withdrawAmount := big.NewInt(0.25 * params.Ether)
	l2Opts := l2Vault.KeyedTransactor(depositor)
	l2Opts.Value = withdrawAmount
	initTx, err := l2Wd.InitiateWithdrawal(l2Opts, depositor, big.NewInt(21000), nil)
	require.NoError(t, err)
	initReceipt, err := optimism.WaitReceiptOK(env.TimeoutCtx(time.Minute), l2, initTx.Hash())
	require.NoError(t, err)
	require.Equal(t, initReceipt.Status, types.ReceiptStatusSuccessful)

	proofBlockNum, err := withdrawals.WaitForOutputRootPublished(env.TimeoutCtx(5*time.Minute), l1, env.Devnet.Deployments.OptimismPortalProxy, initReceipt.BlockNumber)
	require.NoError(t, err)
	finHeader, err := l2.HeaderByNumber(env.Ctx(), big.NewInt(int64(proofBlockNum)))
	require.NoError(t, err)

	proofClient := gethclient.New(env.Devnet.GetOpL2Engine(0).RPC())
	wParams, err := withdrawals.ProveWithdrawalParameters(env.Ctx(), proofClient, l2, initTx.Hash(), finHeader, &env.Devnet.Bindings.BindingsL1.L2OutputOracle.L2OutputOracleCaller)
	require.NoError(t, err)

	portal, err := bindings.NewOptimismPortal(
		env.Devnet.Deployments.OptimismPortalProxy,
		l1,
	)
	require.NoError(t, err)

	startBalanceL1, err := l1.BalanceAt(env.Ctx(), depositor, nil)
	require.NoError(t, err)

	l1Opts := l1Vault.KeyedTransactor(depositor)
	require.NoError(t, err)

	withdrawalTx := bindings.TypesWithdrawalTransaction{
		Nonce:    wParams.Nonce,
		Sender:   wParams.Sender,
		Target:   wParams.Target,
		Value:    wParams.Value,
		GasLimit: wParams.GasLimit,
		Data:     wParams.Data,
	}

	proveTx, err := portal.ProveWithdrawalTransaction(
		l1Opts,
		withdrawalTx,
		wParams.L2OutputIndex,
		wParams.OutputRootProof,
		wParams.WithdrawalProof,
	)
	require.NoError(t, err)

	proveReceipt, err := optimism.WaitReceiptOK(env.TimeoutCtx(time.Minute), l1, proveTx.Hash())
	require.NoError(t, err)
	require.Equal(t, types.ReceiptStatusSuccessful, proveReceipt.Status)

	// Await finalization period
	err = withdrawals.WaitForFinalizationPeriod(
		env.TimeoutCtx(5*time.Minute),
		l1,
		env.Devnet.Deployments.OptimismPortalProxy,
		proveReceipt.BlockNumber,
	)
	require.NoError(t, err)

	finTx, err := portal.FinalizeWithdrawalTransaction(
		l1Opts,
		withdrawalTx,
	)
	require.NoError(t, err)

	finReceipt, err := optimism.WaitReceiptOK(env.TimeoutCtx(time.Minute), l1, finTx.Hash())
	require.NoError(t, err)
	require.Equal(t, types.ReceiptStatusSuccessful, finReceipt.Status)

	endBalanceL1, err := l1.BalanceAt(env.Ctx(), depositor, nil)
	require.NoError(t, err)

	diff := new(big.Int).Sub(endBalanceL1, startBalanceL1)
	requireBetween(t, diff, big.NewInt(0.001*params.Ether))
}

func requireBetween(t require.TestingT, x *big.Int, rg *big.Int) {
	top := new(big.Int).Add(x, rg)
	bot := new(big.Int).Add(x, new(big.Int).Neg(rg))
	require.True(t, x.Cmp(top) == -1, "number must be within %s and %s, got %s", top, bot, x)
	require.True(t, x.Cmp(bot) == 1, "number must be within %s and %s, got %s", top, bot, x)
}

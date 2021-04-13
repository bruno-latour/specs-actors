package test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/filecoin-project/go-bitfield"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/exitcode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/specs-actors/v4/actors/builtin"
	"github.com/filecoin-project/specs-actors/v4/actors/builtin/miner"
	"github.com/filecoin-project/specs-actors/v4/actors/builtin/power"
	"github.com/filecoin-project/specs-actors/v4/actors/runtime/proof"
	"github.com/filecoin-project/specs-actors/v4/actors/states"
	"github.com/filecoin-project/specs-actors/v4/support/ipld"
	tutil "github.com/filecoin-project/specs-actors/v4/support/testing"
	vm "github.com/filecoin-project/specs-actors/v4/support/vm"
)

func TestCommitPoStFlow(t *testing.T) {
	ctx := context.Background()
	v := vm.NewVMWithSingletons(ctx, t, ipld.NewBlockStoreInMemory())
	addrs := vm.CreateAccounts(ctx, t, v, 1, big.Mul(big.NewInt(10_000), big.NewInt(1e18)), 93837778)

	minerBalance := big.Mul(big.NewInt(10_000), vm.FIL)
	sealProof := abi.RegisteredSealProof_StackedDrg32GiBV1_1

	// create miner
	params := power.CreateMinerParams{
		Owner:               addrs[0],
		Worker:              addrs[0],
		WindowPoStProofType: abi.RegisteredPoStProof_StackedDrgWindow32GiBV1,
		Peer:                abi.PeerID("not really a peer id"),
	}
	ret := vm.ApplyOk(t, v, addrs[0], builtin.StoragePowerActorAddr, minerBalance, builtin.MethodsPower.CreateMiner, &params)

	minerAddrs, ok := ret.(*power.CreateMinerReturn)
	require.True(t, ok)

	// advance vm so we can have seal randomness epoch in the past
	v, err := v.WithEpoch(200)
	require.NoError(t, err)

	//
	// precommit sector
	//

	sectorNumber := abi.SectorNumber(100)
	sealedCid := tutil.MakeCID("100", &miner.SealedCIDPrefix)
	sectorSize, err := sealProof.SectorSize()
	require.NoError(t, err)

	preCommitParams := miner.PreCommitSectorParams{
		SealProof:     sealProof,
		SectorNumber:  sectorNumber,
		SealedCID:     sealedCid,
		SealRandEpoch: v.GetEpoch() - 1,
		DealIDs:       nil,
		Expiration:    v.GetEpoch() + miner.MinSectorExpiration + miner.MaxProveCommitDuration[sealProof] + 100,
	}
	vm.ApplyOk(t, v, addrs[0], minerAddrs.RobustAddress, big.Zero(), builtin.MethodsMiner.PreCommitSector, &preCommitParams)

	// assert successful precommit invocation
	vm.ExpectInvocation{
		To:     minerAddrs.IDAddress,
		Method: builtin.MethodsMiner.PreCommitSector,
		Params: vm.ExpectObject(&preCommitParams),
		SubInvocations: []vm.ExpectInvocation{
			{To: builtin.RewardActorAddr, Method: builtin.MethodsReward.ThisEpochReward},
			{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.CurrentTotalPower}},
	}.Matches(t, v.Invocations()[0])

	balances := vm.GetMinerBalances(t, v, minerAddrs.IDAddress)
	assert.True(t, balances.PreCommitDeposit.GreaterThan(big.Zero()))

	// find information about precommited sector
	var minerState miner.State
	err = v.GetState(minerAddrs.IDAddress, &minerState)
	require.NoError(t, err)

	precommit, found, err := minerState.GetPrecommittedSector(v.Store(), sectorNumber)
	require.NoError(t, err)
	require.True(t, found)

	// advance time to max seal duration
	proveTime := v.GetEpoch() + miner.MaxProveCommitDuration[sealProof]
	v, dlInfo := vm.AdvanceByDeadlineTillEpoch(t, v, minerAddrs.IDAddress, proveTime)

	//
	// overdue precommit
	//

	t.Run("missed prove commit results in precommit expiry", func(t *testing.T) {
		// advanced one more deadline so precommit is late
		tv, err := v.WithEpoch(dlInfo.Close)
		require.NoError(t, err)

		// run cron which should expire precommit
		vm.ApplyOk(t, tv, builtin.SystemActorAddr, builtin.CronActorAddr, big.Zero(), builtin.MethodsCron.EpochTick, nil)

		vm.ExpectInvocation{
			To:     builtin.CronActorAddr,
			Method: builtin.MethodsCron.EpochTick,
			SubInvocations: []vm.ExpectInvocation{
				{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.OnEpochTickEnd, SubInvocations: []vm.ExpectInvocation{
					{To: minerAddrs.IDAddress, Method: builtin.MethodsMiner.OnDeferredCronEvent, SubInvocations: []vm.ExpectInvocation{
						{To: builtin.RewardActorAddr, Method: builtin.MethodsReward.ThisEpochReward},
						{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.CurrentTotalPower},

						// The call to burnt funds indicates the overdue precommit has been penalized
						{To: builtin.BurntFundsActorAddr, Method: builtin.MethodSend, Value: vm.ExpectAttoFil(precommit.PreCommitDeposit)},
						{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.EnrollCronEvent},
					}},
					//{To: minerAddrs.IDAddress, Method: builtin.MethodsMiner.ConfirmSectorProofsValid},
					{To: builtin.RewardActorAddr, Method: builtin.MethodsReward.UpdateNetworkKPI},
				}},
				{To: builtin.StorageMarketActorAddr, Method: builtin.MethodsMarket.CronTick},
			},
		}.Matches(t, tv.Invocations()[0])

		// precommit deposit has been reset
		balances := vm.GetMinerBalances(t, tv, minerAddrs.IDAddress)
		assert.Equal(t, big.Zero(), balances.InitialPledge)
		assert.Equal(t, big.Zero(), balances.PreCommitDeposit)

		// no power is added
		networkStats := vm.GetNetworkStats(t, tv)
		assert.Equal(t, big.Zero(), networkStats.TotalBytesCommitted)
		assert.Equal(t, big.Zero(), networkStats.TotalPledgeCollateral)
		assert.Equal(t, big.Zero(), networkStats.TotalRawBytePower)
		assert.Equal(t, big.Zero(), networkStats.TotalQualityAdjPower)
	})

	//
	// prove and verify
	//

	// Prove commit sector after max seal duration
	v, err = v.WithEpoch(proveTime)
	require.NoError(t, err)
	proveCommitParams := miner.ProveCommitSectorParams{
		SectorNumber: sectorNumber,
	}
	vm.ApplyOk(t, v, addrs[0], minerAddrs.RobustAddress, big.Zero(), builtin.MethodsMiner.ProveCommitSector, &proveCommitParams)

	vm.ExpectInvocation{
		To:     minerAddrs.IDAddress,
		Method: builtin.MethodsMiner.ProveCommitSector,
		Params: vm.ExpectObject(&proveCommitParams),
		SubInvocations: []vm.ExpectInvocation{
			{To: builtin.StorageMarketActorAddr, Method: builtin.MethodsMarket.ComputeDataCommitment},
			{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.SubmitPoRepForBulkVerify},
		},
	}.Matches(t, v.Invocations()[0])

	// In the same epoch, trigger cron to validate prove commit
	vm.ApplyOk(t, v, builtin.SystemActorAddr, builtin.CronActorAddr, big.Zero(), builtin.MethodsCron.EpochTick, nil)

	vm.ExpectInvocation{
		To:     builtin.CronActorAddr,
		Method: builtin.MethodsCron.EpochTick,
		SubInvocations: []vm.ExpectInvocation{
			{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.OnEpochTickEnd, SubInvocations: []vm.ExpectInvocation{
				// expect confirm sector proofs valid because we prove committed,
				// but not an on deferred cron event because this is not a deadline boundary
				{To: minerAddrs.IDAddress, Method: builtin.MethodsMiner.ConfirmSectorProofsValid, SubInvocations: []vm.ExpectInvocation{
					{To: builtin.RewardActorAddr, Method: builtin.MethodsReward.ThisEpochReward},
					{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.CurrentTotalPower},
					{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.UpdatePledgeTotal},
				}},
				{To: builtin.RewardActorAddr, Method: builtin.MethodsReward.UpdateNetworkKPI},
			}},
			{To: builtin.StorageMarketActorAddr, Method: builtin.MethodsMarket.CronTick},
		},
	}.Matches(t, v.Invocations()[1])

	// precommit deposit is released, ipr is added
	balances = vm.GetMinerBalances(t, v, minerAddrs.IDAddress)
	assert.True(t, balances.InitialPledge.GreaterThan(big.Zero()))
	assert.Equal(t, big.Zero(), balances.PreCommitDeposit)

	// power is unproven so network stats are unchanged
	networkStats := vm.GetNetworkStats(t, v)
	assert.Equal(t, big.Zero(), networkStats.TotalBytesCommitted)
	assert.True(t, networkStats.TotalPledgeCollateral.GreaterThan(big.Zero()))

	//
	// Submit PoSt
	//

	// advance to proving period
	dlInfo, pIdx, v := vm.AdvanceTillProvingDeadline(t, v, minerAddrs.IDAddress, sectorNumber)
	err = v.GetState(minerAddrs.IDAddress, &minerState)
	require.NoError(t, err)

	sector, found, err := minerState.GetSector(v.Store(), sectorNumber)
	require.NoError(t, err)
	require.True(t, found)

	t.Run("submit PoSt succeeds", func(t *testing.T) {
		tv, err := v.WithEpoch(v.GetEpoch())
		require.NoError(t, err)

		// Submit PoSt
		submitParams := miner.SubmitWindowedPoStParams{
			Deadline: dlInfo.Index,
			Partitions: []miner.PoStPartition{{
				Index:   pIdx,
				Skipped: bitfield.New(),
			}},
			Proofs: []proof.PoStProof{{
				PoStProof: abi.RegisteredPoStProof_StackedDrgWindow32GiBV1,
			}},
			ChainCommitEpoch: dlInfo.Challenge,
			ChainCommitRand:  []byte("not really random"),
		}
		vm.ApplyOk(t, tv, addrs[0], minerAddrs.RobustAddress, big.Zero(), builtin.MethodsMiner.SubmitWindowedPoSt, &submitParams)

		sectorPower := miner.PowerForSector(sectorSize, sector)
		updatePowerParams := &power.UpdateClaimedPowerParams{
			RawByteDelta:         sectorPower.Raw,
			QualityAdjustedDelta: sectorPower.QA,
		}

		vm.ExpectInvocation{
			To:     minerAddrs.IDAddress,
			Method: builtin.MethodsMiner.SubmitWindowedPoSt,
			Params: vm.ExpectObject(&submitParams),
			SubInvocations: []vm.ExpectInvocation{
				// This call to the power actor indicates power has been added for the sector
				{
					To:     builtin.StoragePowerActorAddr,
					Method: builtin.MethodsPower.UpdateClaimedPower,
					Params: vm.ExpectObject(updatePowerParams),
				},
			},
		}.Matches(t, tv.Invocations()[0])

		// miner still has initial pledge
		balances = vm.GetMinerBalances(t, tv, minerAddrs.IDAddress)
		assert.True(t, balances.InitialPledge.GreaterThan(big.Zero()))

		// committed bytes are added (miner would have gained power if minimum requirement were met)
		networkStats := vm.GetNetworkStats(t, tv)
		assert.Equal(t, big.NewInt(int64(sectorSize)), networkStats.TotalBytesCommitted)
		assert.True(t, networkStats.TotalPledgeCollateral.GreaterThan(big.Zero()))

		// Trigger cron to keep reward accounting correct
		vm.ApplyOk(t, tv, builtin.SystemActorAddr, builtin.CronActorAddr, big.Zero(), builtin.MethodsCron.EpochTick, nil)

		stateTree, err := tv.GetStateTree()
		require.NoError(t, err)
		totalBalance, err := tv.GetTotalActorBalance()
		require.NoError(t, err)
		acc, err := states.CheckStateInvariants(stateTree, totalBalance, tv.GetEpoch())
		require.NoError(t, err)
		assert.True(t, acc.IsEmpty(), strings.Join(acc.Messages(), "\n"))
	})

	t.Run("skip sector", func(t *testing.T) {
		tv, err := v.WithEpoch(v.GetEpoch())
		require.NoError(t, err)

		// Submit PoSt
		submitParams := miner.SubmitWindowedPoStParams{
			Deadline: dlInfo.Index,
			Partitions: []miner.PoStPartition{{
				Index:   pIdx,
				Skipped: bitfield.NewFromSet([]uint64{uint64(sectorNumber)}),
			}},
			Proofs: []proof.PoStProof{{
				PoStProof: abi.RegisteredPoStProof_StackedDrgWindow32GiBV1,
			}},
			ChainCommitEpoch: dlInfo.Challenge,
			ChainCommitRand:  []byte("not really random"),
		}
		// PoSt is rejected for skipping all sectors.
		_, code := tv.ApplyMessage(addrs[0], minerAddrs.RobustAddress, big.Zero(), builtin.MethodsMiner.SubmitWindowedPoSt, &submitParams)
		assert.Equal(t, exitcode.ErrIllegalArgument, code)

		vm.ExpectInvocation{
			To:       minerAddrs.IDAddress,
			Method:   builtin.MethodsMiner.SubmitWindowedPoSt,
			Params:   vm.ExpectObject(&submitParams),
			Exitcode: exitcode.ErrIllegalArgument,
		}.Matches(t, tv.Invocations()[0])

		// miner still has initial pledge
		balances = vm.GetMinerBalances(t, v, minerAddrs.IDAddress)
		assert.True(t, balances.InitialPledge.GreaterThan(big.Zero()))

		// network power is unchanged
		networkStats := vm.GetNetworkStats(t, tv)
		assert.Equal(t, big.Zero(), networkStats.TotalBytesCommitted)
		assert.True(t, networkStats.TotalPledgeCollateral.GreaterThan(big.Zero()))
	})

	t.Run("missed first PoSt deadline", func(t *testing.T) {
		// move to proving period end
		tv, err := v.WithEpoch(dlInfo.Last())
		require.NoError(t, err)

		// Run cron to detect missing PoSt
		vm.ApplyOk(t, tv, builtin.SystemActorAddr, builtin.CronActorAddr, big.Zero(), builtin.MethodsCron.EpochTick, nil)

		vm.ExpectInvocation{
			To:     builtin.CronActorAddr,
			Method: builtin.MethodsCron.EpochTick,
			SubInvocations: []vm.ExpectInvocation{
				{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.OnEpochTickEnd, SubInvocations: []vm.ExpectInvocation{
					{To: minerAddrs.IDAddress, Method: builtin.MethodsMiner.OnDeferredCronEvent, SubInvocations: []vm.ExpectInvocation{
						{To: builtin.RewardActorAddr, Method: builtin.MethodsReward.ThisEpochReward},
						{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.CurrentTotalPower},
						{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.EnrollCronEvent},
					}},
					{To: builtin.RewardActorAddr, Method: builtin.MethodsReward.UpdateNetworkKPI},
				}},
				{To: builtin.StorageMarketActorAddr, Method: builtin.MethodsMarket.CronTick},
			},
		}.Matches(t, tv.Invocations()[0])

		// network power is unchanged
		networkStats := vm.GetNetworkStats(t, tv)
		assert.Equal(t, big.Zero(), networkStats.TotalBytesCommitted)
		assert.True(t, networkStats.TotalPledgeCollateral.GreaterThan(big.Zero()))
	})
}

func TestMeasurePoRepGas(t *testing.T) {

	batchSize := 819
	fmt.Printf("Batch Size = %d\n", batchSize)
	printPoRepMsgGas(batchSize)

	ctx := context.Background()
	blkStore := ipld.NewBlockStoreInMemory()
	metrics := ipld.NewMetricsBlockStore(blkStore)
	v := vm.NewVMWithSingletons(ctx, t, metrics)
	v.SetStatsSource(metrics)
	addrs := vm.CreateAccounts(ctx, t, v, 1, big.Mul(big.NewInt(10_000), big.NewInt(1e18)), 93837778)

	minerBalance := big.Mul(big.NewInt(10_000), vm.FIL)
	sealProof := abi.RegisteredSealProof_StackedDrg32GiBV1_1

	// create miner
	params := power.CreateMinerParams{
		Owner:               addrs[0],
		Worker:              addrs[0],
		WindowPoStProofType: abi.RegisteredPoStProof_StackedDrgWindow32GiBV1,
		Peer:                abi.PeerID("not really a peer id"),
	}
	ret := vm.ApplyOk(t, v, addrs[0], builtin.StoragePowerActorAddr, minerBalance, builtin.MethodsPower.CreateMiner, &params)
	minerAddrs, ok := ret.(*power.CreateMinerReturn)
	require.True(t, ok)

	// advance vm so we can have seal randomness epoch in the past
	v, err := v.WithEpoch(abi.ChainEpoch(200))
	require.NoError(t, err)

	//
	// precommit sectors
	//
	sectorNumberBase := 100
	precommits := make([]*miner.SectorPreCommitOnChainInfo, 0)
	sectorNumbers := make([]abi.SectorNumber, 0)
	for i := 0; i <= batchSize-1; i++ {
		sectorNumber := abi.SectorNumber(sectorNumberBase + i)
		sealedCid := tutil.MakeCID(fmt.Sprintf("%d", sectorNumber), &miner.SealedCIDPrefix)

		preCommitParams := miner.PreCommitSectorParams{
			SealProof:     sealProof,
			SectorNumber:  sectorNumber,
			SealedCID:     sealedCid,
			SealRandEpoch: v.GetEpoch() - 1,
			DealIDs:       nil,
			Expiration:    v.GetEpoch() + miner.MinSectorExpiration + miner.MaxProveCommitDuration[sealProof] + 100,
		}
		vm.ApplyOk(t, v, addrs[0], minerAddrs.RobustAddress, big.Zero(), builtin.MethodsMiner.PreCommitSector, &preCommitParams)

		// assert successful precommit invocation
		vm.ExpectInvocation{
			To:     minerAddrs.IDAddress,
			Method: builtin.MethodsMiner.PreCommitSector,
			Params: vm.ExpectObject(&preCommitParams),
			SubInvocations: []vm.ExpectInvocation{
				{To: builtin.RewardActorAddr, Method: builtin.MethodsReward.ThisEpochReward},
				{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.CurrentTotalPower}},
		}.Matches(t, v.Invocations()[i])

		// find information about precommited sector
		var minerState miner.State
		err = v.GetState(minerAddrs.IDAddress, &minerState)
		require.NoError(t, err)

		precommit, found, err := minerState.GetPrecommittedSector(v.Store(), sectorNumber)
		require.NoError(t, err)
		require.True(t, found)
		precommits = append(precommits, precommit)
		sectorNumbers = append(sectorNumbers, sectorNumber)
	}

	balances := vm.GetMinerBalances(t, v, minerAddrs.IDAddress)
	assert.True(t, balances.PreCommitDeposit.GreaterThan(big.Zero()))

	// advance time to max seal duration
	proveTime := v.GetEpoch() + miner.MaxProveCommitDuration[sealProof]
	v, _ = vm.AdvanceByDeadlineTillEpoch(t, v, minerAddrs.IDAddress, proveTime)

	//
	// prove and verify
	//
	v, err = v.WithEpoch(proveTime)
	require.NoError(t, err)
	for i := 0; i <= batchSize-1; i++ {
		// Prove commit sector after max seal duration

		proveCommitParams := miner.ProveCommitSectorParams{
			SectorNumber: sectorNumbers[i],
		}
		vm.ApplyOk(t, v, addrs[0], minerAddrs.RobustAddress, big.Zero(), builtin.MethodsMiner.ProveCommitSector, &proveCommitParams)

		vm.ExpectInvocation{
			To:     minerAddrs.IDAddress,
			Method: builtin.MethodsMiner.ProveCommitSector,
			Params: vm.ExpectObject(&proveCommitParams),
			SubInvocations: []vm.ExpectInvocation{
				{To: builtin.StorageMarketActorAddr, Method: builtin.MethodsMarket.ComputeDataCommitment},
				{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.SubmitPoRepForBulkVerify},
			},
		}.Matches(t, v.Invocations()[i])
	}

	proveCommitKey := vm.MethodKey{Code: builtin.StorageMinerActorCodeID, Method: builtin.MethodsMiner.ProveCommitSector}
	stats := v.GetCallStats()
	printCallStats(proveCommitKey, stats[proveCommitKey], "\n")

	// In the same epoch, trigger cron to validate prove commits
	vm.ApplyOk(t, v, builtin.SystemActorAddr, builtin.CronActorAddr, big.Zero(), builtin.MethodsCron.EpochTick, nil)

	cronKey := vm.MethodKey{Code: builtin.CronActorCodeID, Method: builtin.MethodsCron.EpochTick}
	stats = v.GetCallStats()
	printCallStats(cronKey, stats[cronKey], "\n")

	vm.ExpectInvocation{
		To:     builtin.CronActorAddr,
		Method: builtin.MethodsCron.EpochTick,
		SubInvocations: []vm.ExpectInvocation{
			{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.OnEpochTickEnd, SubInvocations: []vm.ExpectInvocation{
				// expect confirm sector proofs valid because we prove committed,
				// but not an on deferred cron event because this is not a deadline boundary
				{To: minerAddrs.IDAddress, Method: builtin.MethodsMiner.ConfirmSectorProofsValid, SubInvocations: []vm.ExpectInvocation{
					{To: builtin.RewardActorAddr, Method: builtin.MethodsReward.ThisEpochReward},
					{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.CurrentTotalPower},
					{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.UpdatePledgeTotal},
				}},
				{To: builtin.RewardActorAddr, Method: builtin.MethodsReward.UpdateNetworkKPI},
			}},
			{To: builtin.StorageMarketActorAddr, Method: builtin.MethodsMarket.CronTick},
		},
	}.Matches(t, v.Invocations()[batchSize])

	// precommit deposit is released, ipr is added
	balances = vm.GetMinerBalances(t, v, minerAddrs.IDAddress)
	assert.True(t, balances.InitialPledge.GreaterThan(big.Zero()))
	assert.Equal(t, big.Zero(), balances.PreCommitDeposit)

	// power is unproven so network stats are unchanged
	networkStats := vm.GetNetworkStats(t, v)
	assert.Equal(t, big.Zero(), networkStats.TotalBytesCommitted)
	assert.True(t, networkStats.TotalPledgeCollateral.GreaterThan(big.Zero()))

}

func TestMeasureAggregatePorepGas(t *testing.T) {

	batchSize := 819
	fmt.Printf("batch size = %d\n", batchSize)

	ctx := context.Background()
	blkStore := ipld.NewBlockStoreInMemory()
	metrics := ipld.NewMetricsBlockStore(blkStore)
	v := vm.NewVMWithSingletons(ctx, t, metrics)
	v.SetStatsSource(metrics)
	addrs := vm.CreateAccounts(ctx, t, v, 1, big.Mul(big.NewInt(10_000), big.NewInt(1e18)), 93837778)

	minerBalance := big.Mul(big.NewInt(10_000), vm.FIL)
	sealProof := abi.RegisteredSealProof_StackedDrg32GiBV1_1

	// create miner
	params := power.CreateMinerParams{
		Owner:               addrs[0],
		Worker:              addrs[0],
		WindowPoStProofType: abi.RegisteredPoStProof_StackedDrgWindow32GiBV1,
		Peer:                abi.PeerID("not really a peer id"),
	}
	ret := vm.ApplyOk(t, v, addrs[0], builtin.StoragePowerActorAddr, minerBalance, builtin.MethodsPower.CreateMiner, &params)
	minerAddrs, ok := ret.(*power.CreateMinerReturn)
	require.True(t, ok)

	// advance vm so we can have seal randomness epoch in the past
	v, err := v.WithEpoch(abi.ChainEpoch(200))
	require.NoError(t, err)

	//
	// precommit sectors
	//
	sectorNumberBase := 100
	precommits := make([]*miner.SectorPreCommitOnChainInfo, 0)
	sectorNumbers := make([]uint64, 0)
	for i := 0; i <= batchSize-1; i++ {
		sectorNumber := abi.SectorNumber(sectorNumberBase + i)
		sealedCid := tutil.MakeCID(fmt.Sprintf("%d", sectorNumber), &miner.SealedCIDPrefix)

		preCommitParams := miner.PreCommitSectorParams{
			SealProof:     sealProof,
			SectorNumber:  sectorNumber,
			SealedCID:     sealedCid,
			SealRandEpoch: v.GetEpoch() - 1,
			DealIDs:       nil,
			Expiration:    v.GetEpoch() + miner.MinSectorExpiration + miner.MaxProveCommitDuration[sealProof] + 100,
		}
		vm.ApplyOk(t, v, addrs[0], minerAddrs.RobustAddress, big.Zero(), builtin.MethodsMiner.PreCommitSector, &preCommitParams)

		// assert successful precommit invocation
		vm.ExpectInvocation{
			To:     minerAddrs.IDAddress,
			Method: builtin.MethodsMiner.PreCommitSector,
			Params: vm.ExpectObject(&preCommitParams),
			SubInvocations: []vm.ExpectInvocation{
				{To: builtin.RewardActorAddr, Method: builtin.MethodsReward.ThisEpochReward},
				{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.CurrentTotalPower}},
		}.Matches(t, v.Invocations()[i])

		// find information about precommited sector
		var minerState miner.State
		err = v.GetState(minerAddrs.IDAddress, &minerState)
		require.NoError(t, err)

		precommit, found, err := minerState.GetPrecommittedSector(v.Store(), sectorNumber)
		require.NoError(t, err)
		require.True(t, found)
		precommits = append(precommits, precommit)
		sectorNumbers = append(sectorNumbers, uint64(sectorNumber))
	}

	balances := vm.GetMinerBalances(t, v, minerAddrs.IDAddress)
	assert.True(t, balances.PreCommitDeposit.GreaterThan(big.Zero()))

	// advance time to max seal duration
	proveTime := v.GetEpoch() + miner.MaxProveCommitDuration[sealProof]
	v, _ = vm.AdvanceByDeadlineTillEpoch(t, v, minerAddrs.IDAddress, proveTime)

	//
	// prove and verify
	//
	v, err = v.WithEpoch(proveTime)
	require.NoError(t, err)
	sectorNosBf := bitfield.NewFromSet(sectorNumbers)

	proveCommitAggregateParams := miner.ProveCommitAggregateParams{
		SectorNumbers: sectorNosBf,
	}
	vm.ApplyOk(t, v, addrs[0], minerAddrs.RobustAddress, big.Zero(), builtin.MethodsMiner.ProveCommitAggregate, &proveCommitAggregateParams)
	invocs := make([]vm.ExpectInvocation, 0)
	for i := 0; i < batchSize; i++ {
		invocs = append(invocs, vm.ExpectInvocation{
			To: builtin.StorageMarketActorAddr, Method: builtin.MethodsMarket.ComputeDataCommitment,
		})
	}
	invocs = append(invocs, []vm.ExpectInvocation{
		{To: builtin.RewardActorAddr, Method: builtin.MethodsReward.ThisEpochReward},
		{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.CurrentTotalPower},
		{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.UpdatePledgeTotal},
	}...)

	vm.ExpectInvocation{
		To:             minerAddrs.IDAddress,
		Method:         builtin.MethodsMiner.ProveCommitAggregate,
		Params:         vm.ExpectObject(&proveCommitAggregateParams),
		SubInvocations: invocs,
	}.Matches(t, v.Invocations()[0])

	proveCommitAggrKey := vm.MethodKey{Code: builtin.StorageMinerActorCodeID, Method: builtin.MethodsMiner.ProveCommitAggregate}
	stats := v.GetCallStats()
	printCallStats(proveCommitAggrKey, stats[proveCommitAggrKey], "\n")

	// In the same epoch, trigger cron to
	vm.ApplyOk(t, v, builtin.SystemActorAddr, builtin.CronActorAddr, big.Zero(), builtin.MethodsCron.EpochTick, nil)

	cronKey := vm.MethodKey{Code: builtin.CronActorCodeID, Method: builtin.MethodsCron.EpochTick}
	stats = v.GetCallStats()
	printCallStats(cronKey, stats[cronKey], "\n")

	vm.ExpectInvocation{
		To:     builtin.CronActorAddr,
		Method: builtin.MethodsCron.EpochTick,
		SubInvocations: []vm.ExpectInvocation{
			{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.OnEpochTickEnd, SubInvocations: []vm.ExpectInvocation{
				// expect no confirm sector proofs valid because we prove committed with aggregation.
				// expect no on deferred cron event because this is not a deadline boundary
				{To: builtin.RewardActorAddr, Method: builtin.MethodsReward.UpdateNetworkKPI},
			}},
			{To: builtin.StorageMarketActorAddr, Method: builtin.MethodsMarket.CronTick},
		},
	}.Matches(t, v.Invocations()[1])

	// precommit deposit is released, ipr is added
	balances = vm.GetMinerBalances(t, v, minerAddrs.IDAddress)
	assert.True(t, balances.InitialPledge.GreaterThan(big.Zero()))
	assert.Equal(t, big.Zero(), balances.PreCommitDeposit)

	// power is unproven so network stats are unchanged
	networkStats := vm.GetNetworkStats(t, v)
	assert.Equal(t, big.Zero(), networkStats.TotalBytesCommitted)
	assert.True(t, networkStats.TotalPledgeCollateral.GreaterThan(big.Zero()))

}

func printCallStats(method vm.MethodKey, stats *vm.CallStats, indent string) { // nolint:unused
	fmt.Printf("%s%v:%d: calls: %d  gets: %d  puts: %d  read: %d  written: %d  avg gets: %.2f, avg puts: %.2f\n",
		indent, builtin.ActorNameByCode(method.Code), method.Method, stats.Calls, stats.Reads, stats.Writes,
		stats.ReadBytes, stats.WriteBytes, float32(stats.Reads)/float32(stats.Calls),
		float32(stats.Writes)/float32(stats.Calls))

	gasGetObj := uint64(75242)
	gasPutObj := uint64(84070)
	gasPutPerByte := uint64(1)
	gasStorageMultiplier := uint64(1300)
	gasPerCall := uint64(29233)

	ipldGas := stats.Reads*gasGetObj + stats.Writes*gasPutObj + stats.WriteBytes*gasPutPerByte*gasStorageMultiplier
	callGas := stats.Calls * gasPerCall
	fmt.Printf("%v:%d: ipld gas=%d call gas=%d\n", builtin.ActorNameByCode(method.Code), method.Method, ipldGas, callGas)

	if stats.SubStats == nil {
		return
	}

	for m, s := range stats.SubStats {
		printCallStats(m, s, indent+"  ")
	}
}

func printPoRepMsgGas(batchSize int) {
	// Ignore message fields and sector number bytes for both.
	// Ignoring non-parma message fields under estimates both by the same amount
	// Ignoring sector numbers/bitfields underestimates current porep compared to aggregate
	// which is the right direction for finding a starting bound (we can optimize later)
	onChainMessageComputeBase := 38863
	onChainMessageStorageBase := 36
	onChainMessageStoragePerByte := 1
	storageGasMultiplier := 1300
	msgBytes := 1920
	msgGas := onChainMessageComputeBase + (onChainMessageStorageBase+onChainMessageStoragePerByte*msgBytes)*storageGasMultiplier

	allMsgsGas := batchSize * msgGas
	fmt.Printf("%d batchsize: all proof param byte gas: %d\n", batchSize, allMsgsGas)
}

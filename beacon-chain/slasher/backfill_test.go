package slasher

import (
	"context"
	"testing"
	"time"

	types "github.com/prysmaticlabs/eth2-types"
	mock "github.com/prysmaticlabs/prysm/beacon-chain/blockchain/testing"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/blocks"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	dbtest "github.com/prysmaticlabs/prysm/beacon-chain/db/testing"
	"github.com/prysmaticlabs/prysm/beacon-chain/operations/slashings"
	iface "github.com/prysmaticlabs/prysm/beacon-chain/state/interface"
	"github.com/prysmaticlabs/prysm/beacon-chain/state/stategen"
	mockSync "github.com/prysmaticlabs/prysm/beacon-chain/sync/initial-sync/testing"
	pb "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	ethpb "github.com/prysmaticlabs/prysm/proto/eth/v1alpha1"
	"github.com/prysmaticlabs/prysm/proto/eth/v1alpha1/wrapper"
	"github.com/prysmaticlabs/prysm/proto/interfaces"
	"github.com/prysmaticlabs/prysm/shared/blockutil"
	"github.com/prysmaticlabs/prysm/shared/bls"
	"github.com/prysmaticlabs/prysm/shared/event"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/prysmaticlabs/prysm/shared/testutil"
	"github.com/prysmaticlabs/prysm/shared/testutil/require"
	logTest "github.com/sirupsen/logrus/hooks/test"
)

type backfillTestConfig struct {
	numEpochs              types.Epoch
	proposerSlashingAtSlot types.Slot
}

func TestService_waitForBackfill_OK(t *testing.T) {
	hook := logTest.NewGlobal()
	srv := setupBackfillTest(t, &backfillTestConfig{
		numEpochs: types.Epoch(8),
	})
	srv.waitForDataBackfill(types.Epoch(8))
	require.LogsContain(t, hook, "Beginning slasher data backfill from epoch 0 to 8")
}

func TestService_waitForBackfill_DetectsSlashableBlock(t *testing.T) {
	hook := logTest.NewGlobal()
	c := params.BeaconConfig()
	c.SlotsPerEpoch = 4
	params.OverrideBeaconConfig(c)
	srv := setupBackfillTest(t, &backfillTestConfig{
		numEpochs:              types.Epoch(1),
		proposerSlashingAtSlot: 2,
	})
	srv.waitForDataBackfill(types.Epoch(1))
	require.LogsContain(t, hook, "Beginning slasher data backfill from epoch 0 to 1")
	require.LogsContain(t, hook, "Found 1 proposer slashing")
}

func BenchmarkService_backfill(b *testing.B) {
	b.StopTimer()
	srv := setupBackfillTest(b, &backfillTestConfig{
		numEpochs: types.Epoch(8),
	})
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		srv.waitForDataBackfill(8)
	}
}

func setupBackfillTest(tb testing.TB, cfg *backfillTestConfig) *Service {
	ctx := context.Background()
	beaconDB := dbtest.SetupDB(tb)
	slasherDB := dbtest.SetupSlasherDB(tb)

	beaconState, err := testutil.NewBeaconState()
	require.NoError(tb, err)
	currentSlot := types.Slot(0)
	require.NoError(tb, beaconState.SetSlot(currentSlot))
	genesisStateRoot, err := beaconState.HashTreeRoot(ctx)
	require.NoError(tb, err)

	mockChain := &mock.ChainService{
		State: beaconState,
		Slot:  &currentSlot,
	}
	slashingsPool := &slashings.PoolMock{}

	srv, err := New(ctx, &ServiceConfig{
		IndexedAttestationsFeed: new(event.Feed),
		BeaconBlockHeadersFeed:  new(event.Feed),
		StateNotifier:           &mock.MockStateNotifier{},
		Database:                slasherDB,
		BeaconDatabase:          beaconDB,
		HeadStateFetcher:        mockChain,
		SyncChecker:             &mockSync.Sync{IsSyncing: false},
		StateGen:                stategen.New(beaconDB),
		SlashingPoolInserter:    slashingsPool,
	})
	require.NoError(tb, err)

	genesisBlock := blocks.NewGenesisBlock(genesisStateRoot[:])
	genesisRoot, err := genesisBlock.Block.HashTreeRoot()
	require.NoError(tb, err)
	wrapGenesis := wrapper.WrappedPhase0SignedBeaconBlock(genesisBlock)
	require.NoError(tb, srv.serviceCfg.BeaconDatabase.SaveBlock(ctx, wrapGenesis))
	require.NoError(tb, srv.serviceCfg.BeaconDatabase.SaveGenesisBlockRoot(ctx, genesisRoot))
	require.NoError(tb, srv.serviceCfg.StateGen.SaveState(ctx, genesisRoot, beaconState))
	require.NoError(tb, srv.serviceCfg.BeaconDatabase.SaveState(ctx, beaconState, genesisRoot))

	// Set genesis time to a custom number of epochs ago.
	secondsPerSlot := params.BeaconConfig().SecondsPerSlot
	secondsPerEpoch := secondsPerSlot * uint64(params.BeaconConfig().SlotsPerEpoch)
	totalEpochTimeElapsed := uint64(cfg.numEpochs) * secondsPerEpoch
	srv.genesisTime = time.Now().Add(-time.Duration(totalEpochTimeElapsed) * time.Second)

	// Write blocks for every slot from epoch 0 to numEpochs.
	numSlots := uint64(cfg.numEpochs) * uint64(params.BeaconConfig().SlotsPerEpoch)
	blocksBySlot := make(map[types.Slot][]interfaces.SignedBeaconBlock)
	blocksBySlot[0] = []interfaces.SignedBeaconBlock{wrapGenesis}

	// Setup validators in the beacon state for a full test setup.
	numValidators := numSlots
	validators, balances, privKeysByValidator := setupValidators(tb, numValidators)
	require.NoError(tb, beaconState.SetValidators(validators))
	require.NoError(tb, beaconState.SetBalances(balances))

	for i := uint64(1); i <= numSlots; i++ {
		// Create a realistic looking block for the slot.
		slot := types.Slot(i)
		require.NoError(tb, beaconState.SetSlot(slot))
		parentRoot := genesisRoot[:]
		if i > 1 {
			parentRootHtr, err := blocksBySlot[slot-1][0].Block().HashTreeRoot()
			require.NoError(tb, err)
			parentRoot = parentRootHtr[:]
		}

		_, proposerIndexToSlot, err := helpers.CommitteeAssignments(beaconState, helpers.SlotToEpoch(slot))
		require.NoError(tb, err)
		slotToProposerIndex := make(map[types.Slot]types.ValidatorIndex)
		for k, v := range proposerIndexToSlot {
			for _, assignedSlot := range v {
				slotToProposerIndex[assignedSlot] = k
			}
		}

		proposerIdx := slotToProposerIndex[slot]
		blk := generateBlock(
			tb,
			beaconState,
			privKeysByValidator,
			slot,
			proposerIdx,
			parentRoot,
			false, /* not slashable */
		)
		blocksBySlot[slot] = append(blocksBySlot[slot], blk)

		// Save the state.
		blockRoot, err := blk.Block().HashTreeRoot()
		require.NoError(tb, err)
		require.NoError(tb, srv.serviceCfg.StateGen.SaveState(ctx, blockRoot, beaconState))
		require.NoError(tb, srv.serviceCfg.BeaconDatabase.SaveState(ctx, beaconState, blockRoot))

		// If we specify it, create a slashable block at a certain slot.
		if uint64(cfg.proposerSlashingAtSlot) == i && i != 0 {
			tb.Log("Inserting proposer slashing for proposer index and slot", proposerIdx, i)
			slashableBlk := generateBlock(
				tb,
				beaconState,
				privKeysByValidator,
				slot,
				proposerIdx,
				parentRoot,
				true, /* slashable */
			)
			blocksBySlot[slot] = append(blocksBySlot[slot], slashableBlk)
			// Save the state.
			blockRoot, err := slashableBlk.Block().HashTreeRoot()
			require.NoError(tb, err)
			require.NoError(tb, srv.serviceCfg.StateGen.SaveState(ctx, blockRoot, beaconState))
			require.NoError(tb, srv.serviceCfg.BeaconDatabase.SaveState(ctx, beaconState, blockRoot))
		}
	}
	for _, blocks := range blocksBySlot {
		require.NoError(tb, beaconDB.SaveBlocks(ctx, blocks))
	}
	headSlot := types.Slot(numSlots)
	mockChain.Slot = &headSlot
	mockChain.State = beaconState
	srv.serviceCfg.HeadStateFetcher = mockChain
	return srv
}

func setupValidators(t testing.TB, count uint64) ([]*ethpb.Validator, []uint64, map[types.ValidatorIndex]bls.SecretKey) {
	balances := make([]uint64, count)
	validators := make([]*ethpb.Validator, 0, count)
	secretKeysByValidator := make(map[types.ValidatorIndex]bls.SecretKey)
	for i := uint64(0); i < count; i++ {
		privKey, err := bls.RandKey()
		require.NoError(t, err)
		secretKeysByValidator[types.ValidatorIndex(i)] = privKey
		pubKey := privKey.PublicKey().Marshal()
		balances[i] = i
		validators = append(validators, &ethpb.Validator{
			PublicKey:                  pubKey,
			WithdrawalCredentials:      make([]byte, 32),
			EffectiveBalance:           params.BeaconConfig().MaxEffectiveBalance,
			ActivationEligibilityEpoch: 0,
			ActivationEpoch:            0,
			ExitEpoch:                  params.BeaconConfig().FarFutureEpoch,
		})
	}
	return validators, balances, secretKeysByValidator
}

func generateBlock(
	tb testing.TB,
	beaconState iface.BeaconState,
	privKeysByValidator map[types.ValidatorIndex]bls.SecretKey,
	slot types.Slot,
	valIdx types.ValidatorIndex,
	parentRoot []byte,
	slashable bool,
) wrapper.Phase0SignedBeaconBlock {
	var blk *ethpb.SignedBeaconBlock
	if slashable {
		slashableGraffiti := make([]byte, 32)
		copy(slashableGraffiti, "slashme")
		blk = testutil.HydrateSignedBeaconBlock(&ethpb.SignedBeaconBlock{
			Block: testutil.HydrateBeaconBlock(&ethpb.BeaconBlock{
				Slot:          slot,
				ProposerIndex: valIdx,
				ParentRoot:    parentRoot,
				Body: testutil.HydrateBeaconBlockBody(&ethpb.BeaconBlockBody{
					Graffiti: slashableGraffiti,
				}),
			}),
		})
	} else {
		blk = testutil.HydrateSignedBeaconBlock(&ethpb.SignedBeaconBlock{
			Block: testutil.HydrateBeaconBlock(&ethpb.BeaconBlock{
				Slot:          slot,
				ProposerIndex: valIdx,
				ParentRoot:    parentRoot,
			}),
		})
	}
	wrap := wrapper.WrappedPhase0SignedBeaconBlock(blk)
	header, err := blockutil.SignedBeaconBlockHeaderFromBlock(wrap)
	require.NoError(tb, err)
	epoch := helpers.SlotToEpoch(slot)
	sig, err := signBlockHeader(beaconState, header, privKeysByValidator[valIdx], epoch)
	require.NoError(tb, err)
	blk.Signature = sig.Marshal()
	return wrapper.WrappedPhase0SignedBeaconBlock(blk)
}

func signBlockHeader(
	beaconState iface.BeaconState,
	header *ethpb.SignedBeaconBlockHeader,
	privKey bls.SecretKey,
	epoch types.Epoch,
) (bls.Signature, error) {
	domain, err := helpers.Domain(
		beaconState.Fork(),
		epoch,
		params.BeaconConfig().DomainBeaconProposer,
		beaconState.GenesisValidatorRoot(),
	)
	if err != nil {
		return nil, err
	}
	htr, err := header.Header.HashTreeRoot()
	if err != nil {
		return nil, err
	}
	container := &pb.SigningData{
		ObjectRoot: htr[:],
		Domain:     domain,
	}
	signingRoot, err := container.HashTreeRoot()
	if err != nil {
		return nil, err
	}
	return privKey.Sign(signingRoot[:]), nil
}
package state_native

import (
	"fmt"

	ethpb "github.com/prysmaticlabs/prysm/proto/prysm/v1alpha1"
	"github.com/prysmaticlabs/prysm/runtime/version"
)

// PreviousEpochAttestations corresponding to blocks on the beacon chain.
func (b *BeaconState) PreviousEpochAttestations() ([]*ethpb.PendingAttestation, error) {
	if b.version != version.Phase0 {
		return nil, fmt.Errorf("PreviousEpochAttestations is not supported for %s", version.String(b.version))
	}

	if b.previousEpochAttestations == nil {
		return nil, nil
	}

	b.lock.RLock()
	defer b.lock.RUnlock()

	return b.previousEpochAttestationsVal(), nil
}

// previousEpochAttestationsVal corresponding to blocks on the beacon chain.
// This assumes that a lock is already held on BeaconState.
func (b *BeaconState) previousEpochAttestationsVal() []*ethpb.PendingAttestation {
	if b.version != version.Phase0 {
		return nil
	}

	return ethpb.CopyPendingAttestationSlice(b.previousEpochAttestations)
}

// CurrentEpochAttestations corresponding to blocks on the beacon chain.
func (b *BeaconState) CurrentEpochAttestations() ([]*ethpb.PendingAttestation, error) {
	if b.version != version.Phase0 {
		return nil, fmt.Errorf("CurrentEpochAttestations is not supported for %s", version.String(b.version))
	}

	if b.currentEpochAttestations == nil {
		return nil, nil
	}

	b.lock.RLock()
	defer b.lock.RUnlock()

	return b.currentEpochAttestationsVal(), nil
}

// currentEpochAttestations corresponding to blocks on the beacon chain.
// This assumes that a lock is already held on BeaconState.
func (b *BeaconState) currentEpochAttestationsVal() []*ethpb.PendingAttestation {
	if b.version != version.Phase0 {
		return nil
	}

	return ethpb.CopyPendingAttestationSlice(b.currentEpochAttestations)
}